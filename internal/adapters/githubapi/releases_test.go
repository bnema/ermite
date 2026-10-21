// SPDX-License-Identifier: AGPL-3.0-or-later

package githubapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const releasePath = "/repos/acme/proj/releases"
const releaseAssetPath = releasePath + "/assets/7"
const releaseUploadPath = "/uploads/repos/acme/proj/releases/4/assets"

func releaseRequest(t *testing.T, h *Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, Prefix+path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}
func TestReleaseRoutes(t *testing.T) {
	cases := []struct{ path, kind, ref, allow string }{
		{releasePath, "release-list", "", "GET, HEAD, POST"},
		{releasePath + "/latest", "release-latest", "", "GET, HEAD"},
		{releasePath + "/tags/v1/patch", "release-tag", "v1/patch", "GET, HEAD"},
		{releasePath + "/4", "release-id", "4", "GET, HEAD, PATCH, DELETE"},
		{releasePath + "/4/assets", "release-assets", "4", "GET, HEAD"},
		{releaseAssetPath, "release-asset", "7", "GET, HEAD, PATCH, DELETE"},
		{releaseUploadPath, "release-upload", "4", "POST"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			kind, owner, repo, ref, ok := route(tc.path)
			require.True(t, ok)
			require.Equal(t, tc.kind, kind)
			require.Equal(t, "acme", owner)
			require.Equal(t, "proj", repo)
			require.Equal(t, tc.ref, ref)
			api := mocks.NewGitHubReleaseAPI(t)
			h := New(Config{Releases: api})
			for _, method := range []string{"GET", "HEAD", "POST", "PATCH", "DELETE", "PUT", "OPTIONS"} {
				if methodAllowed(method, tc.allow) {
					continue
				}
				w := releaseRequest(t, h, method, tc.path, "", nil)
				require.Equal(t, 405, w.Code)
				require.Equal(t, tc.allow, w.Header().Get("Allow"))
			}
			w := releaseRequest(t, New(Config{}), "GET", tc.path, "", nil)
			require.Equal(t, 404, w.Code)
			require.Equal(t, notFoundBody, w.Body.String())
		})
	}
	for _, path := range []string{releasePath + "/0", releasePath + "/-1", releasePath + "/01", releasePath + "/9223372036854775808", releasePath + "/tags/", releasePath + "/4/extra", releaseUploadPath + "/extra", "/uploads" + releasePath} {
		_, _, _, _, ok := route(path)
		require.False(t, ok, path)
	}
}
func TestReleaseDispatch(t *testing.T) {
	for _, method := range []string{"GET", "HEAD"} {
		t.Run(method, func(t *testing.T) {
			api := mocks.NewGitHubReleaseAPI(t)
			h := New(Config{Releases: api})
			api.EXPECT().ListReleases(mock.Anything, domain.Anonymous(), "acme", "proj", 2, 10).Return([]byte(`[]`), `<next>; rel="next"`, nil).Once()
			w := releaseRequest(t, h, method, releasePath+"?page=2&per_page=10", "", nil)
			require.Equal(t, 200, w.Code)
			require.Equal(t, `<next>; rel="next"`, w.Header().Get("Link"))
			require.Equal(t, "2", w.Header().Get("Content-Length"))
			if method == "HEAD" {
				require.Empty(t, w.Body.String())
			} else {
				require.Equal(t, "[]", w.Body.String())
			}
			for _, tc := range []struct {
				path string
				ref  ports.ReleaseRef
			}{{releasePath + "/latest", ports.ReleaseRef{Latest: true}}, {releasePath + "/tags/v1/x", ports.ReleaseRef{Tag: "v1/x"}}, {releasePath + "/4", ports.ReleaseRef{ID: 4}}} {
				api.EXPECT().GetRelease(mock.Anything, domain.Anonymous(), "acme", "proj", tc.ref).Return([]byte(`{"id":4}`), nil).Once()
				w = releaseRequest(t, h, method, tc.path, "", nil)
				require.Equal(t, 200, w.Code)
			}
			api.EXPECT().ListAssets(mock.Anything, domain.Anonymous(), "acme", "proj", int64(4), 0, 0).Return([]byte(`[]`), "asset-link", nil).Once()
			w = releaseRequest(t, h, method, releasePath+"/4/assets", "", nil)
			require.Equal(t, 200, w.Code)
			require.Equal(t, "asset-link", w.Header().Get("Link"))
			api.EXPECT().GetAsset(mock.Anything, domain.Anonymous(), "acme", "proj", int64(7)).Return([]byte(`{"id":7}`), nil).Once()
			w = releaseRequest(t, h, method, releaseAssetPath, "", map[string]string{"Accept": "application/vnd.github+json"})
			require.Equal(t, 200, w.Code)
		})
	}
	api := mocks.NewGitHubReleaseAPI(t)
	h := New(Config{Releases: api})
	b := []byte(`{"tag_name":"v1"}`)
	api.EXPECT().CreateRelease(mock.Anything, domain.Anonymous(), "acme", "proj", b).Return([]byte(`{"id":4}`), nil).Once()
	w := releaseRequest(t, h, "POST", releasePath, string(b), nil)
	require.Equal(t, 201, w.Code)
	require.Equal(t, `{"id":4}`, w.Body.String())
	api.EXPECT().UpdateRelease(mock.Anything, domain.Anonymous(), "acme", "proj", int64(4), b).Return([]byte(`{"id":4}`), nil).Once()
	w = releaseRequest(t, h, "PATCH", releasePath+"/4", string(b), nil)
	require.Equal(t, 200, w.Code)
	api.EXPECT().UpdateAsset(mock.Anything, domain.Anonymous(), "acme", "proj", int64(7), b).Return([]byte(`{"id":7}`), nil).Once()
	w = releaseRequest(t, h, "PATCH", releaseAssetPath, string(b), nil)
	require.Equal(t, 200, w.Code)
	api.EXPECT().DeleteRelease(mock.Anything, domain.Anonymous(), "acme", "proj", int64(4)).Return(nil).Once()
	w = releaseRequest(t, h, "DELETE", releasePath+"/4", "", nil)
	require.Equal(t, 204, w.Code)
	require.Empty(t, w.Body.String())
	api.EXPECT().DeleteAsset(mock.Anything, domain.Anonymous(), "acme", "proj", int64(7)).Return(nil).Once()
	w = releaseRequest(t, h, "DELETE", releaseAssetPath, "", nil)
	require.Equal(t, 204, w.Code)
	require.Empty(t, w.Body.String())
}
func TestReleaseUpload(t *testing.T) {
	api := mocks.NewGitHubReleaseAPI(t)
	h := New(Config{Releases: api})
	req := httptest.NewRequest("POST", Prefix+releaseUploadPath+"?name=a", strings.NewReader("abc"))
	req.ContentLength = -1
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, 411, w.Code)
	require.Equal(t, `{"message":"Length Required"}`, w.Body.String())
	// HTTP distinguishes an omitted length from an explicitly empty asset.
	w = releaseRequest(t, h, "POST", releaseUploadPath+"?name=empty", "", nil)
	require.Equal(t, 411, w.Code)
	api.EXPECT().UploadAsset(mock.Anything, domain.Anonymous(), "acme", "proj", int64(4), "empty", "", "application/octet-stream", int64(0), mock.Anything).Return([]byte(`{"id":8}`), nil).Once()
	w = releaseRequest(t, h, "POST", releaseUploadPath+"?name=empty", "", map[string]string{"Content-Length": "0"})
	require.Equal(t, 201, w.Code)
	w = releaseRequest(t, h, "POST", releaseUploadPath, "abc", nil)
	require.Equal(t, 422, w.Code)
	for _, ct := range []string{"", "text/plain"} {
		expectedType := ct
		if ct == "" {
			expectedType = "application/octet-stream"
		}
		api.EXPECT().UploadAsset(mock.Anything, domain.Anonymous(), "acme", "proj", int64(4), "a b", "label", expectedType, int64(3), mock.Anything).RunAndReturn(func(_ context.Context, _ domain.Principal, _, _ string, _ int64, _, _, _ string, size int64, r io.Reader) ([]byte, error) {
			got, err := io.ReadAll(r)
			require.NoError(t, err)
			require.Equal(t, "abc", string(got))
			return []byte(`{"id":7}`), nil
		}).Once()
		req = httptest.NewRequest("POST", Prefix+releaseUploadPath+"?name=a%20b&label=label", strings.NewReader("abcdef"))
		req.ContentLength = 3
		req.Header.Set("Content-Type", ct)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, 201, w.Code)
		require.Equal(t, `{"id":7}`, w.Body.String())
	}
	// The real response template expands to precisely the upload route used above.
	b, err := os.ReadFile("testdata/releases/release.json")
	require.NoError(t, err)
	var object struct {
		UploadURL string `json:"upload_url"`
	}
	require.NoError(t, json.Unmarshal(b, &object))
	expanded := strings.Split(object.UploadURL, "{")[0] + "?name=" + url.QueryEscape("archive.tar.gz")
	u, err := url.Parse(expanded)
	require.NoError(t, err)
	kind, owner, repo, ref, ok := route(strings.TrimPrefix(u.Path, Prefix))
	require.True(t, ok)
	require.Equal(t, "release-upload", kind)
	require.Equal(t, "acme", owner)
	require.Equal(t, "releases", repo)
	require.Equal(t, "1", ref)
}
func TestReleaseBodyLimit(t *testing.T) {
	h := New(Config{Releases: mocks.NewGitHubReleaseAPI(t)})
	for _, tc := range []struct{ method, path string }{{"POST", releasePath}, {"PATCH", releasePath + "/4"}, {"PATCH", releaseAssetPath}} {
		w := releaseRequest(t, h, tc.method, tc.path, strings.Repeat("x", (1<<20)+1), nil)
		require.Equal(t, 422, w.Code)
		require.Equal(t, `{"message":"Body too large"}`, w.Body.String())
	}
}
func TestReleaseErrors(t *testing.T) {
	rn, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	job := domain.RepoTokenPrincipal(domain.CIRepoToken{Repo: rn, Permissions: map[string]string{domain.PermContents: domain.PermissionRead}})
	cases := []struct {
		name, kind string
		p          domain.Principal
		err        error
		status     int
		body       string
	}{
		{"hidden draft", "release-id", domain.Anonymous(), domain.ErrNotFound, 404, notFoundBody},
		{"anonymous write", "release-list", domain.Anonymous(), domain.ErrUnauthenticated, 401, `{"message":"Bad credentials"}`},
		{"job read only", "release-list", job, domain.ErrForbidden, 403, `{"message":"Resource not accessible by integration"}`},
		{"stored token", "release-list", domain.Root(), domain.ErrForbidden, 403, `{"message":"Forbidden"}`},
		{"duplicate release", "release-list", domain.Root(), domain.ErrAlreadyExists, 422, `{"message":"Validation Failed","errors":[{"resource":"Release","code":"already_exists","field":"tag_name"}]}`},
		{"duplicate upload", "release-upload", domain.Root(), domain.ErrAlreadyExists, 422, `{"message":"Validation Failed","errors":[{"resource":"ReleaseAsset","code":"already_exists","field":"name"}],"documentation_url":"https://docs.github.com/rest/releases/assets#upload-a-release-asset"}`},
		{"invalid field", "release-list", domain.Root(), fmt.Errorf("%w: make_latest", domain.ErrInvalid), 422, `{"message":"Validation Failed","errors":[{"resource":"Release","code":"invalid","field":"make_latest"}]}`},
		{"invalid", "release-list", domain.Root(), domain.ErrInvalid, 422, `{"message":"Validation Failed","errors":[{"resource":"Release","code":"invalid"}]}`},
		{"tag missing", "release-id", domain.Root(), domain.ErrPreconditionFailed, 422, `{"message":"Validation Failed","errors":[{"resource":"Release","code":"invalid","field":"target_commitish"}]}`},
		{"size", "release-upload", domain.Root(), fmt.Errorf("%w: asset size limit", domain.ErrResourceExhausted), 422, `{"message":"resource exhausted: asset size limit"}`},
		{"count", "release-upload", domain.Root(), fmt.Errorf("%w: release asset count limit is 1000", domain.ErrResourceExhausted), 422, `{"message":"resource exhausted: release asset count limit is 1000"}`},
		{"quota", "release-upload", domain.Root(), fmt.Errorf("%w: release quota is 20 GiB", domain.ErrResourceExhausted), 422, `{"message":"resource exhausted: release quota is 20 GiB"}`},
		{"upload cap", "release-upload", domain.Root(), fmt.Errorf("%w: too many uploads in progress, retry", domain.ErrResourceExhausted), 429, `{"message":"resource exhausted: too many uploads in progress, retry"}`},
		{"internal", "release-id", domain.Root(), errors.New("secret internal error"), 500, `{"message":"Server Error"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			New(Config{}).failRelease(t.Context(), w, tc.p, tc.err, tc.kind, "")
			require.Equal(t, tc.status, w.Code)
			require.Equal(t, tc.body, w.Body.String())
			if tc.status == 429 {
				require.Equal(t, retryAfter, w.Header().Get("Retry-After"))
			}
		})
	}
	// Verify the route actually forwards a use-case visibility failure unchanged.
	api := mocks.NewGitHubReleaseAPI(t)
	api.EXPECT().GetRelease(mock.Anything, domain.Anonymous(), "acme", "proj", ports.ReleaseRef{ID: 4}).Return(nil, domain.ErrNotFound).Once()
	w := releaseRequest(t, New(Config{Releases: api}), "GET", releasePath+"/4", "", nil)
	require.Equal(t, 404, w.Code)
	require.Equal(t, notFoundBody, w.Body.String())
}
func downloadAsset(t *testing.T) domain.ReleaseAsset {
	t.Helper()
	rn, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	return domain.ReleaseAsset{ID: 7, ReleaseID: 4, Repo: rn, Name: "archive.tar.gz", ContentType: "application/gzip", Size: 10, Digest: "sha256:abcdef", UpdatedAt: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
}
func TestReleaseDownload(t *testing.T) {
	cases := []struct {
		name, method, rang, etag string
		status                   int
		offset, length           int64
		body, contentRange       string
	}{
		{"whole", "GET", "", "", 200, 0, 10, "0123456789", ""},
		{"bounded", "GET", "bytes=2-5", "", 206, 2, 4, "2345", "bytes 2-5/10"},
		{"open ended", "GET", "bytes=7-", "", 206, 7, 3, "789", "bytes 7-9/10"},
		{"suffix", "GET", "bytes=-3", "", 206, 7, 3, "789", "bytes 7-9/10"},
		{"large suffix", "GET", "bytes=-30", "", 206, 0, 10, "0123456789", "bytes 0-9/10"},
		{"clamp", "GET", "bytes=7-100", "", 206, 7, 3, "789", "bytes 7-9/10"},
		{"multiple ignored", "GET", "bytes=0-1,3-4", "", 200, 0, 10, "0123456789", ""},
		{"unknown unit ignored", "GET", "items=1-2", "", 200, 0, 10, "0123456789", ""},
		{"out of bounds", "GET", "bytes=10-", "", 416, 0, 0, "", "bytes */10"},
		{"reverse", "GET", "bytes=4-2", "", 416, 0, 0, "", "bytes */10"},
		{"invalid", "GET", "bytes=garbage", "", 416, 0, 0, "", "bytes */10"},
		{"zero suffix", "GET", "bytes=-0", "", 416, 0, 0, "", "bytes */10"},
		{"conditional", "GET", "", `"sha256-abcdef"`, 304, 0, 0, "", ""},
		{"head", "HEAD", "", "", 200, 0, 10, "", ""},
		{"head range", "HEAD", "bytes=2-5", "", 206, 2, 4, "", "bytes 2-5/10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := mocks.NewGitHubReleaseAPI(t)
			dl := mocks.NewReleaseService(t)
			a := downloadAsset(t)
			api.EXPECT().AssetByID(mock.Anything, domain.Anonymous(), "acme", "proj", int64(7)).Return(a, nil).Once()
			if tc.method == "GET" && (tc.status == 200 || tc.status == 206) {
				dl.EXPECT().DownloadAsset(mock.Anything, domain.Anonymous(), a.Repo, ports.AssetRef{ID: 7}, tc.offset, tc.length, mock.Anything).RunAndReturn(func(_ context.Context, _ domain.Principal, _ domain.RepoName, _ ports.AssetRef, offset, length int64, send func(ports.AssetChunk) error) error {
					require.NoError(t, send(ports.AssetChunk{Asset: &a}))
					return send(ports.AssetChunk{Data: []byte("0123456789")[offset : offset+length]})
				}).Once()
			}
			w := releaseRequest(t, New(Config{Releases: api, ReleaseDownload: dl}), tc.method, releaseAssetPath, "", map[string]string{"Accept": "application/vnd.github+json, application/octet-stream", "Range": tc.rang, "If-None-Match": tc.etag})
			require.Equal(t, tc.status, w.Code)
			require.Equal(t, tc.body, w.Body.String())
			require.Equal(t, tc.contentRange, w.Header().Get("Content-Range"))
			require.Equal(t, "application/gzip", w.Header().Get("Content-Type"))
			require.Equal(t, `attachment; filename="archive.tar.gz"`, w.Header().Get("Content-Disposition"))
			require.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
			require.Equal(t, "sandbox", w.Header().Get("Content-Security-Policy"))
			require.Equal(t, "bytes", w.Header().Get("Accept-Ranges"))
			require.Equal(t, `"sha256-abcdef"`, w.Header().Get("ETag"))
			require.Equal(t, "Wed, 21 Oct 2026 12:00:00 GMT", w.Header().Get("Last-Modified"))
			if tc.status == 200 || tc.status == 206 {
				require.Equal(t, fmt.Sprint(tc.length), w.Header().Get("Content-Length"))
			}
		})
	}
}
func TestReleaseDownloadSafety(t *testing.T) {
	for _, ct := range []string{"text/html", "text/html; charset=utf-8", "APPLICATION/JAVASCRIPT", "image/svg+xml"} {
		a := downloadAsset(t)
		a.ContentType = ct
		a.Name = "é space.txt"
		w := httptest.NewRecorder()
		setReleaseDownloadHeaders(w, a)
		require.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
		require.Equal(t, `attachment; filename="_ space.txt"; filename*=UTF-8''%C3%A9%20space.txt`, w.Header().Get("Content-Disposition"))
	}
	require.Equal(t, `attachment; filename="evil.txt"`, releaseContentDisposition("\r\n/\\\"evil.txt"))
}
func TestReleaseDownloadFailures(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprint(started), func(t *testing.T) {
			api := mocks.NewGitHubReleaseAPI(t)
			dl := mocks.NewReleaseService(t)
			a := downloadAsset(t)
			api.EXPECT().AssetByID(mock.Anything, domain.Anonymous(), "acme", "proj", int64(7)).Return(a, nil).Once()
			dl.EXPECT().DownloadAsset(mock.Anything, domain.Anonymous(), a.Repo, ports.AssetRef{ID: 7}, int64(0), int64(10), mock.Anything).RunAndReturn(func(_ context.Context, _ domain.Principal, _ domain.RepoName, _ ports.AssetRef, _, _ int64, send func(ports.AssetChunk) error) error {
				if started {
					require.NoError(t, send(ports.AssetChunk{Data: []byte("x")}))
				}
				return domain.ErrNotFound
			}).Once()
			h := New(Config{Releases: api, ReleaseDownload: dl})
			run := func() {
				w := releaseRequest(t, h, "GET", releaseAssetPath, "", map[string]string{"Accept": "application/octet-stream"})
				require.Equal(t, 404, w.Code)
				require.Equal(t, notFoundBody, w.Body.String())
				require.Empty(t, w.Header().Get("Content-Length"))
				require.Empty(t, w.Header().Get("Content-Range"))
			}
			if started {
				require.PanicsWithValue(t, http.ErrAbortHandler, run)
			} else {
				run()
			}
			require.Empty(t, h.archives)
			require.Empty(t, h.clients)
		})
	}
}
func TestReleaseDownloadSlots(t *testing.T) {
	api := mocks.NewGitHubReleaseAPI(t)
	a := downloadAsset(t)
	api.EXPECT().AssetByID(mock.Anything, domain.Anonymous(), "acme", "proj", int64(7)).Return(a, nil).Times(2)
	h := New(Config{Releases: api, ReleaseDownload: mocks.NewReleaseService(t), MaxArchives: 1, MaxArchivesPerClient: 1})
	release, ok := h.acquire(httptest.NewRecorder(), "192.0.2.1")
	require.True(t, ok)
	defer release()
	w := releaseRequest(t, h, "GET", releaseAssetPath, "", map[string]string{"Accept": "application/octet-stream"})
	require.Equal(t, 429, w.Code)
	require.Equal(t, retryAfter, w.Header().Get("Retry-After"))
	require.Empty(t, w.Header().Get("Content-Length"))
	require.Contains(t, w.Body.String(), "Too many archives")
	// A HEAD uses metadata, even when every download slot is taken.
	w = releaseRequest(t, h, "HEAD", releaseAssetPath, "", map[string]string{"Accept": "application/octet-stream"})
	require.Equal(t, 200, w.Code)
}

func TestReleaseEmptyAndMissingDownload(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int64
		err  error
		want int
	}{
		{"empty", 0, domain.ErrOutOfRange, 200},
		{"missing empty", 0, domain.ErrNotFound, 404},
		{"store range", 10, domain.ErrOutOfRange, 416},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := mocks.NewGitHubReleaseAPI(t)
			dl := mocks.NewReleaseService(t)
			a := downloadAsset(t)
			a.Size = tc.size
			api.EXPECT().AssetByID(mock.Anything, domain.Anonymous(), "acme", "proj", int64(7)).Return(a, nil).Once()
			dl.EXPECT().DownloadAsset(mock.Anything, domain.Anonymous(), a.Repo, ports.AssetRef{ID: 7}, int64(0), tc.size, mock.Anything).Return(tc.err).Once()
			w := releaseRequest(t, New(Config{Releases: api, ReleaseDownload: dl}), "GET", releaseAssetPath, "", map[string]string{"Accept": "application/octet-stream"})
			require.Equal(t, tc.want, w.Code)
			switch tc.want {
			case 200:
				require.Equal(t, "0", w.Header().Get("Content-Length"))
				require.Empty(t, w.Body.String())
			case 416:
				require.Equal(t, "bytes */10", w.Header().Get("Content-Range"))
			case 404:
				require.Equal(t, notFoundBody, w.Body.String())
			}
		})
	}
	// Missing the stream service leaves JSON available but bytes disabled.
	api := mocks.NewGitHubReleaseAPI(t)
	w := releaseRequest(t, New(Config{Releases: api}), "GET", releaseAssetPath, "", map[string]string{"Accept": "application/octet-stream"})
	require.Equal(t, 404, w.Code)
	require.Equal(t, notFoundBody, w.Body.String())
}

func TestReleaseDownloadGlobalSlot(t *testing.T) {
	api := mocks.NewGitHubReleaseAPI(t)
	a := downloadAsset(t)
	api.EXPECT().AssetByID(mock.Anything, domain.Anonymous(), "acme", "proj", int64(7)).Return(a, nil).Once()
	h := New(Config{Releases: api, ReleaseDownload: mocks.NewReleaseService(t), MaxArchives: 1})
	release, ok := h.acquire(httptest.NewRecorder(), "another-client")
	require.True(t, ok)
	defer release()
	w := releaseRequest(t, h, "GET", releaseAssetPath, "", map[string]string{"Accept": "application/octet-stream"})
	require.Equal(t, 503, w.Code)
	require.Equal(t, retryAfter, w.Header().Get("Retry-After"))
	require.Empty(t, w.Header().Get("Content-Length"))
	require.Contains(t, w.Body.String(), "Server busy")
	require.NotContains(t, h.clients, "192.0.2.1")
}
