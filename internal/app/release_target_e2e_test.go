// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"io"
	"net/http"
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

// An unknown draft target is invalid, never not found, through both APIs.
func TestReleaseUnknownTargetIsInvalid(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("proj", domain.VisibilityPublic, "README.md")
	hc := &http.Client{Transport: bearerTransport{secret: adminToken, next: http.DefaultTransport}}
	rel := ermiteconnect.NewReleaseServiceClient(connect.NewClient(connecthttp.NewTransport(hc, h.url("/-/api"), connecthttp.WithReadMaxBytes(0))))
	draft, err := rel.CreateRelease(h.ctx, &pb.CreateReleaseRequest{Repo: "proj", Tag: "v9", Draft: true})
	require.NoError(t, err)
	id := draft.GetRelease().GetId()

	_, err = rel.UpdateRelease(h.ctx, &pb.UpdateReleaseRequest{Repo: "proj", Id: id, TargetCommitish: "no-such-branch", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"target_commitish"}}})
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))

	req, err := http.NewRequestWithContext(h.ctx, "PATCH", h.url(githubapi.Prefix+"/repos/owner/proj/releases/"+strconv.FormatInt(id, 10)), strings.NewReader(`{"target_commitish":"no-such-branch"}`))
	require.NoError(t, err)
	resp, err := hc.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, 422, resp.StatusCode, string(body))
	assert.Contains(t, string(body), `"field":"target_commitish"`)
}
