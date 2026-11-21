// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/adapters/githubapi"
	"bnema.dev/ermite/internal/domain"
)

// release:draft is refused publishing, deleting and published assets through
// the real REST and RPC servers.
func TestReleaseDraftScopeOverHTTP(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("proj", domain.VisibilityPublic, "README.md")
	drafter := h.token([]domain.Scope{domain.ScopeRepoRead, domain.ScopeReleaseDraft})
	clients := func(secret string) (*http.Client, ermiteconnect.ReleaseServiceClient) {
		hc := &http.Client{Transport: bearerTransport{secret: secret, next: http.DefaultTransport}}
		return hc, ermiteconnect.NewReleaseServiceClient(connect.NewClient(connecthttp.NewTransport(hc, h.url("/-/api"), connecthttp.WithReadMaxBytes(0))))
	}
	adminHTTP, _ := clients(adminToken)
	draftHTTP, draftRPC := clients(drafter)
	do := func(hc *http.Client, method, target, ctype, body string) (int, string) {
		t.Helper()
		if !strings.HasPrefix(target, "http") {
			target = h.url(githubapi.Prefix + target)
		}
		req, err := http.NewRequestWithContext(h.ctx, method, target, strings.NewReader(body))
		require.NoError(t, err)
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		resp, err := hc.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(b)
	}
	base := "/repos/owner/proj/releases"
	var pub struct {
		ID        int64  `json:"id"`
		UploadURL string `json:"upload_url"`
	}
	code, body := do(adminHTTP, "POST", base, "", `{"tag_name":"v1","target_commitish":"main"}`)
	require.Equal(t, 201, code, body)
	require.NoError(t, json.Unmarshal([]byte(body), &pub))
	u, err := url.Parse(strings.Split(pub.UploadURL, "{")[0])
	require.NoError(t, err)
	upload := h.url(u.Path) + "?name="
	code, body = do(adminHTTP, "POST", upload+"pub.bin", "application/octet-stream", "p")
	require.Equal(t, 201, code, body)
	var asset struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &asset))

	// REST: 403.
	code, _ = do(draftHTTP, "POST", base, "", `{"tag_name":"v2","target_commitish":"main"}`)
	assert.Equal(t, 403, code, "publish on create")
	code, body = do(draftHTTP, "POST", base, "", `{"tag_name":"v3","draft":true}`)
	require.Equal(t, 201, code, body)
	var draft struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &draft))
	code, _ = do(draftHTTP, "PATCH", base+"/"+strconv.FormatInt(draft.ID, 10), "", `{"draft":false,"target_commitish":"main"}`)
	assert.Equal(t, 403, code, "publish a draft")
	code, _ = do(draftHTTP, "DELETE", base+"/"+strconv.FormatInt(draft.ID, 10), "", "")
	assert.Equal(t, 403, code, "delete")
	code, _ = do(draftHTTP, "POST", upload+"new.bin", "application/octet-stream", "n")
	assert.Equal(t, 403, code, "asset on a published release")
	code, _ = do(draftHTTP, "PATCH", base+"/assets/"+strconv.FormatInt(asset.ID, 10), "", `{"name":"renamed.bin"}`)
	assert.Equal(t, 403, code, "rename a published asset")

	// RPC: PermissionDenied.
	_, err = draftRPC.UpdateRelease(h.ctx, &pb.UpdateReleaseRequest{Repo: "proj", Id: draft.ID, Draft: false, TargetCommitish: "main", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"draft", "target_commitish"}}})
	assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err), "publish")
	_, err = draftRPC.DeleteRelease(h.ctx, &pb.DeleteReleaseRequest{Repo: "proj", Id: draft.ID})
	assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err), "delete")
	_, err = draftRPC.UpdateReleaseAsset(h.ctx, &pb.UpdateReleaseAssetRequest{Repo: "proj", AssetId: asset.ID, Name: "renamed.bin", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"name"}}})
	assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err), "published asset")
}
