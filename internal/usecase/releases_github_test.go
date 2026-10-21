// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"bnema.dev/ermite/internal/adapters/githubapi"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func releaseGitHub(t *testing.T, r *releaseRig) *usecase.ReleasesGitHub {
	t.Helper()
	return usecase.NewReleasesGitHub(r.svc, "acme", "http://localhost:7341/")
}
func releaseGolden(t *testing.T, file string, body []byte) {
	t.Helper()
	var pretty bytes.Buffer
	require.NoError(t, json.Indent(&pretty, body, "", "  "))
	pretty.WriteByte('\n')
	path := filepath.Join("..", "adapters", "githubapi", "testdata", "releases", file+".json")
	if os.Getenv("UPDATE_RELEASE_GOLDENS") == "1" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
		require.NoError(t, os.WriteFile(path, pretty.Bytes(), 0644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), pretty.String())
}
func TestGitHubReleaseGoldens(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{})
	g := releaseGitHub(t, r)
	rel := r.create(t, "v1", false)
	for _, name := range []string{"ermite.tar.gz", "checksums.txt"} {
		_, err := g.UploadAsset(r.ctx, domain.Root(), "acme", r.repo.String(), rel.ID, name, "", "application/octet-stream", 3, strings.NewReader("abc"))
		require.NoError(t, err)
	}
	body, err := g.GetRelease(r.ctx, domain.Anonymous(), "ACME", "RELEASES", ports.ReleaseRef{Tag: "v1"})
	require.NoError(t, err)
	releaseGolden(t, "release", body)
	draft := r.create(t, "next", true)
	body, err = g.GetRelease(r.ctx, domain.Root(), "acme", r.repo.String(), ports.ReleaseRef{ID: draft.ID})
	require.NoError(t, err)
	releaseGolden(t, "draft", body)
	_, err = g.GetRelease(r.ctx, domain.Anonymous(), "acme", r.repo.String(), ports.ReleaseRef{ID: draft.ID})
	require.ErrorIs(t, err, domain.ErrNotFound)
}
func TestGitHubReleaseRequests(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{})
	g := releaseGitHub(t, r)
	repo := r.repo.String()
	p := domain.Root()
	r.resolve(t, "v1", nil)
	body, err := g.CreateRelease(r.ctx, p, "acme", repo, []byte(`{"tag_name":"v1","name":"Title","body":"Notes","draft":true,"prerelease":true,"discussion_category_name":"ignored","make_latest":"legacy"}`))
	require.NoError(t, err)
	var rel struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(body, &rel))
	body, err = g.UpdateRelease(r.ctx, p, "acme", repo, rel.ID, []byte(`{"name":"","draft":false,"prerelease":false,"make_latest":"true","unknown":1}`))
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	require.Equal(t, "", got["name"])
	require.Equal(t, "Notes", got["body"])
	require.Equal(t, false, got["draft"])
	require.Equal(t, false, got["prerelease"])
	require.NotContains(t, got, "make_latest")
	stored, err := r.svc.GetRelease(r.ctx, p, r.repo, ports.ReleaseRef{ID: rel.ID})
	require.NoError(t, err)
	require.True(t, stored.MakeLatest)
	_, err = g.UpdateRelease(r.ctx, p, "acme", repo, rel.ID, []byte(`{"make_latest":"legacy"}`))
	require.NoError(t, err)
	stored, err = r.svc.GetRelease(r.ctx, p, r.repo, ports.ReleaseRef{ID: rel.ID})
	require.NoError(t, err)
	require.True(t, stored.MakeLatest)
	_, err = g.UpdateRelease(r.ctx, p, "acme", repo, rel.ID, []byte(`{"make_latest":"false"}`))
	require.NoError(t, err)
	stored, err = r.svc.GetRelease(r.ctx, p, r.repo, ports.ReleaseRef{ID: rel.ID})
	require.NoError(t, err)
	require.False(t, stored.MakeLatest)
	for _, bad := range []string{`{}`, `null`, `[]`, `{`, `{"tag_name":2}`, `{"tag_name":"v1","draft":"true"}`, `{"tag_name":"v1","name":null}`, `{"tag_name":"v1","make_latest":true}`, `{"tag_name":"v1","make_latest":"yes"}`} {
		_, err = g.CreateRelease(r.ctx, p, "acme", repo, []byte(bad))
		require.ErrorIs(t, err, domain.ErrInvalid, bad)
	}
	_, err = g.CreateRelease(r.ctx, p, "acme", repo, []byte(`{"tag_name":"v1"}`))
	require.ErrorIs(t, err, domain.ErrAlreadyExists)
	_, err = g.GetRelease(r.ctx, p, "other", repo, ports.ReleaseRef{ID: rel.ID})
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = g.GetRelease(r.ctx, p, "acme", "bad/name", ports.ReleaseRef{ID: rel.ID})
	require.ErrorIs(t, err, domain.ErrNotFound)
}
func TestGitHubReleasePagination(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{})
	g := releaseGitHub(t, r)
	for i := 0; i < 5; i++ {
		r.create(t, "v"+strconv.Itoa(i), i == 4)
	}
	for page := 1; page <= 3; page++ {
		b, link, err := g.ListReleases(r.ctx, domain.Root(), "acme", r.repo.String(), page, 2)
		require.NoError(t, err)
		var rows []struct {
			ID int64 `json:"id"`
		}
		require.NoError(t, json.Unmarshal(b, &rows))
		require.Len(t, rows, min(2, 5-(page-1)*2))
		require.Equal(t, int64(5-(page-1)*2), rows[0].ID)
		base := "http://localhost:7341/-/github/api/repos/acme/releases/releases?per_page=2&page="
		switch page {
		case 1:
			require.Equal(t, `<`+base+`2>; rel="next", <`+base+`3>; rel="last"`, link)
		case 2:
			require.Equal(t, `<`+base+`3>; rel="next", <`+base+`3>; rel="last", <`+base+`1>; rel="first", <`+base+`1>; rel="prev"`, link)
		case 3:
			require.Equal(t, `<`+base+`1>; rel="first", <`+base+`2>; rel="prev"`, link)
		}
	}
	b, _, err := g.ListReleases(r.ctx, domain.Anonymous(), "acme", r.repo.String(), 0, 0)
	require.NoError(t, err)
	var rows []any
	require.NoError(t, json.Unmarshal(b, &rows))
	require.Len(t, rows, 4)
	for _, page := range []int{4, math.MaxInt} {
		b, _, err = g.ListReleases(r.ctx, domain.Root(), "acme", r.repo.String(), page, 2)
		require.NoError(t, err)
		require.Equal(t, "[]", string(b))
	}
	b, _, err = g.ListReleases(r.ctx, domain.Root(), "acme", r.repo.String(), 1, 1000)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &rows))
	require.Len(t, rows, 5)
}
func TestGitHubReleaseAssetsAndRetry(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{AssetMax: 10})
	g := releaseGitHub(t, r)
	p := domain.Root()
	repo := r.repo.String()
	rel := r.create(t, "v1/slash", false)
	upload := func(name string) ([]byte, error) {
		return g.UploadAsset(r.ctx, p, "acme", repo, rel.ID, name, "label", "text/plain", 3, strings.NewReader("abc"))
	}
	b, err := upload("é #?.txt")
	require.NoError(t, err)
	var asset struct {
		ID  int64  `json:"id"`
		URL string `json:"browser_download_url"`
	}
	require.NoError(t, json.Unmarshal(b, &asset))
	require.Equal(t, "http://localhost:7341/releases/releases/download/v1%2Fslash/%C3%A9%20%23%3F.txt", asset.URL)
	_, err = upload("é #?.txt")
	require.ErrorIs(t, err, domain.ErrAlreadyExists)
	b, err = g.UpdateAsset(r.ctx, p, "acme", repo, asset.ID, []byte(`{"label":""}`))
	require.NoError(t, err)
	require.Contains(t, string(b), `"label":""`)
	require.Contains(t, string(b), `"name":"é #?.txt"`)
	b, err = g.GetAsset(r.ctx, domain.Anonymous(), "acme", repo, asset.ID)
	require.NoError(t, err)
	require.Contains(t, string(b), `"state":"uploaded"`)
	for i := 0; i < 2; i++ {
		_, err = upload(fmt.Sprint(i))
		require.NoError(t, err)
	}
	b, link, err := g.ListAssets(r.ctx, p, "acme", repo, rel.ID, 1, 2)
	require.NoError(t, err)
	require.Contains(t, link, `rel="next"`)
	var assets []any
	require.NoError(t, json.Unmarshal(b, &assets))
	require.Len(t, assets, 2)
	require.NoError(t, g.DeleteAsset(r.ctx, p, "acme", repo, asset.ID))
	_, err = g.AssetByID(r.ctx, p, "acme", repo, asset.ID)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = upload("é #?.txt")
	require.NoError(t, err)
	_, err = g.UploadAsset(r.ctx, p, "acme", repo, rel.ID, "huge", "", "", 11, strings.NewReader("ignored"))
	require.ErrorIs(t, err, domain.ErrResourceExhausted)
	require.NoError(t, g.DeleteRelease(r.ctx, p, "acme", repo, rel.ID))
	_, err = g.GetRelease(r.ctx, p, "acme", repo, ports.ReleaseRef{ID: rel.ID})
	require.ErrorIs(t, err, domain.ErrNotFound)
}

func TestGitHubReleaseAuthorizationAndLatest(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{})
	g := releaseGitHub(t, r)
	repo := r.repo.String()
	stable := r.create(t, "v1", false)
	r.create(t, "draft", true)
	b, err := g.GetRelease(r.ctx, domain.Anonymous(), "acme", repo, ports.ReleaseRef{Latest: true})
	require.NoError(t, err)
	var got struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(b, &got))
	require.Equal(t, stable.ID, got.ID)
	writer := domain.RepoTokenPrincipal(domain.CIRepoToken{Repo: r.repo, Permissions: map[string]string{domain.PermContents: domain.PermissionWrite}})
	reader := domain.RepoTokenPrincipal(domain.CIRepoToken{Repo: r.repo, Permissions: map[string]string{domain.PermContents: domain.PermissionRead}})
	branched := domain.TokenPrincipal(domain.Token{Scopes: []domain.Scope{domain.ScopeRepoRead, domain.ScopeReleaseWrite}, BranchPrefixes: []string{"feature/"}})
	for _, tc := range []struct {
		p    domain.Principal
		tag  string
		want error
	}{
		{domain.Anonymous(), "anonymous", domain.ErrUnauthenticated},
		{reader, "reader", domain.ErrForbidden},
		{branched, "branch", domain.ErrForbidden},
		{writer, "job", nil},
	} {
		if tc.want == nil {
			r.resolve(t, tc.tag, nil)
		}
		b, err = g.CreateRelease(r.ctx, tc.p, "acme", repo, []byte(`{"tag_name":"`+tc.tag+`"}`))
		if tc.want != nil {
			require.ErrorIs(t, err, tc.want)
			continue
		}
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(b, &got))
		_, err = g.UploadAsset(r.ctx, tc.p, "acme", repo, got.ID, "ci.tar.gz", "", "", 3, strings.NewReader("abc"))
		require.NoError(t, err)
		_, err = g.UploadAsset(r.ctx, reader, "acme", repo, got.ID, "read.tar.gz", "", "", 3, strings.NewReader("abc"))
		require.ErrorIs(t, err, domain.ErrForbidden)
	}
	other, err := domain.ParseRepoName("other")
	require.NoError(t, err)
	require.NoError(t, r.store.Create(r.ctx, domain.Repo{Name: other, Visibility: domain.VisibilityPublic, CreatedAt: now}))
	b, _, err = g.ListAssets(r.ctx, domain.Root(), "acme", other.String(), stable.ID, 1, 30)
	require.ErrorIs(t, err, domain.ErrNotFound)
	require.Nil(t, b)
}

func TestGitHubReleaseTargetAndMissingTag(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{})
	g := releaseGitHub(t, r)
	p := domain.Root()
	repo := r.repo.String()
	r.resolve(t, "missing", domain.ErrNotFound)
	_, err := g.CreateRelease(r.ctx, p, "acme", repo, []byte(`{"tag_name":"missing"}`))
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	b, err := g.CreateRelease(r.ctx, p, "acme", repo, []byte(`{"tag_name":"missing","draft":true}`))
	require.NoError(t, err)
	var got struct {
		ID     int64  `json:"id"`
		Target string `json:"target_commitish"`
	}
	require.NoError(t, json.Unmarshal(b, &got))
	require.Empty(t, got.Target)
	_, err = g.UpdateRelease(r.ctx, p, "acme", repo, got.ID, []byte(`{"draft":false}`))
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	r.resolve(t, "new", domain.ErrNotFound)
	v, err := domain.ParseRevision("main")
	require.NoError(t, err)
	oid, err := domain.ParseObjectID(oidA)
	require.NoError(t, err)
	r.reader.EXPECT().ResolveRevision(r.ctx, r.repo, v).Return(domain.ResolvedRevision{CommitID: oid}, nil).Once()
	r.tags.EXPECT().CreateTag(r.ctx, r.repo, "new", oid).Return(nil).Once()
	r.pushes.EXPECT().Record(r.ctx, p, r.repo, []domain.RefUpdate{{Ref: "refs/tags/new", Old: domain.ZeroOID, New: oidA}}).Return(nil).Once()
	b, err = g.CreateRelease(r.ctx, p, "acme", repo, []byte(`{"tag_name":"new","target_commitish":"main"}`))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &got))
	require.Equal(t, oidA, got.Target)

	// PATCH with target_commitish publishes a draft whose tag is missing.
	r.resolve(t, "patched", domain.ErrNotFound)
	b, err = g.CreateRelease(r.ctx, p, "acme", repo, []byte(`{"tag_name":"patched","draft":true}`))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &got))
	r.reader.EXPECT().ResolveRevision(r.ctx, r.repo, v).Return(domain.ResolvedRevision{CommitID: oid}, nil).Once()
	r.tags.EXPECT().CreateTag(r.ctx, r.repo, "patched", oid).Return(nil).Once()
	r.pushes.EXPECT().Record(r.ctx, p, r.repo, []domain.RefUpdate{{Ref: "refs/tags/patched", Old: domain.ZeroOID, New: oidA}}).Return(nil).Once()
	b, err = g.UpdateRelease(r.ctx, p, "acme", repo, got.ID, []byte(`{"draft":false,"target_commitish":"main"}`))
	require.NoError(t, err)
	require.Contains(t, string(b), `"draft":false`)
	require.Contains(t, string(b), `"target_commitish":"`+oidA+`"`)
}

// A real HTTP round trip through the facade, metadata store and disk blob
// adapter protects the goreleaser/softprops create, retry, overwrite, publish
// sequence, not just each side's mocked contract.
func TestGitHubReleaseHTTPWorkflow(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{})
	g := releaseGitHub(t, r)
	auth := mocks.NewAuthService(t)
	auth.EXPECT().Authenticate(mock.Anything, "dev-token").Return(domain.Root(), nil).Maybe()
	srv := httptest.NewServer(githubapi.New(githubapi.Config{Auth: auth, Releases: g, ReleaseDownload: r.svc}))
	defer srv.Close()
	do := func(method, path, body string, headers map[string]string) (*http.Response, string) {
		req, err := http.NewRequestWithContext(r.ctx, method, srv.URL+githubapi.Prefix+path, strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer dev-token")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := srv.Client().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp, string(b)
	}
	base := "/repos/acme/" + r.repo.String() + "/releases"
	resp, _ := do("GET", base+"/tags/v1", "", nil)
	require.Equal(t, 404, resp.StatusCode)
	r.resolve(t, "v1", nil)
	resp, b := do("POST", base, `{"tag_name":"v1","draft":true}`, nil)
	require.Equal(t, 201, resp.StatusCode)
	var rel struct {
		ID        int64  `json:"id"`
		UploadURL string `json:"upload_url"`
	}
	require.NoError(t, json.Unmarshal([]byte(b), &rel))
	upload := strings.TrimPrefix(strings.Split(rel.UploadURL, "{")[0], "http://localhost:7341"+githubapi.Prefix) + "?name=archive.tar.gz"
	resp, b = do("POST", upload, "0123456789", map[string]string{"Content-Type": "application/gzip"})
	require.Equal(t, 201, resp.StatusCode)
	var a struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(b), &a))
	resp, b = do("POST", upload, "replacement", nil)
	require.Equal(t, 422, resp.StatusCode)
	require.Contains(t, b, `"code":"already_exists"`)
	resp, b = do("GET", base+"/"+strconv.FormatInt(rel.ID, 10)+"/assets", "", nil)
	require.Equal(t, 200, resp.StatusCode)
	require.Contains(t, b, `"name":"archive.tar.gz"`)
	assetURL := base + "/assets/" + strconv.FormatInt(a.ID, 10)
	resp, _ = do("DELETE", assetURL, "", nil)
	require.Equal(t, 204, resp.StatusCode)
	resp, b = do("POST", upload, "abcdefghij", nil)
	require.Equal(t, 201, resp.StatusCode)
	require.NoError(t, json.Unmarshal([]byte(b), &a))
	resp, _ = do("PATCH", base+"/"+strconv.FormatInt(rel.ID, 10), `{"draft":false}`, nil)
	require.Equal(t, 200, resp.StatusCode)
	assetURL = base + "/assets/" + strconv.FormatInt(a.ID, 10)
	resp, b = do("GET", assetURL, "", map[string]string{"Accept": "application/octet-stream", "Range": "bytes=2-5"})
	require.Equal(t, 206, resp.StatusCode)
	require.Equal(t, "cdef", b)
	require.Equal(t, "bytes 2-5/10", resp.Header.Get("Content-Range"))
	require.Equal(t, "4", resp.Header.Get("Content-Length"))
	require.Empty(t, resp.Header.Get("Location"))
	resp, b = do("HEAD", assetURL, "", map[string]string{"Accept": "application/octet-stream"})
	require.Equal(t, 200, resp.StatusCode)
	require.Empty(t, b)
	require.Equal(t, "10", resp.Header.Get("Content-Length"))
	resp, _ = do("GET", assetURL, "", map[string]string{"Accept": "application/octet-stream", "If-None-Match": resp.Header.Get("ETag")})
	require.Equal(t, 304, resp.StatusCode)
}
