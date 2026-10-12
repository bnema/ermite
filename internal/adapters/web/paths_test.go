// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

// pathsSHA is the commit the path list tests ask for.
var pathsSHA = strings.Repeat("a", 40)

// pathsBrowse resolves any full id to itself and lists a.go and b/c.go (or
// paths) for the commit, as the anonymous visitor. lists is how many times
// the list may be built: 0 means ListPaths must not be called.
func pathsBrowse(t testing.TB, lists int, paths []string) *mocks.BrowseService {
	t.Helper()
	browse := mocks.NewBrowseService(t)
	browse.EXPECT().ResolveRevision(mock.Anything, domain.Anonymous(), repoName(t, "r"), mock.Anything).RunAndReturn(
		func(_ context.Context, _ domain.Principal, _ domain.RepoName, rev domain.Revision) (domain.ResolvedRevision, error) {
			return domain.ResolvedRevision{CommitID: objectID(t, rev.String())}, nil
		}).Maybe()
	if lists > 0 {
		if paths == nil {
			paths = []string{"a.go", "b/c.go"}
		}
		browse.EXPECT().ListPaths(mock.Anything, domain.Anonymous(), repoName(t, "r"), mock.Anything).Return(
			domain.PathList{CommitID: objectID(t, pathsSHA), Paths: paths}, nil).Times(lists)
	}
	return browse
}

func getPaths(srv *Server, sha string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/r/paths/"+sha, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// A revalidation is answered after the access check and before the list is
// loaded: it never builds (the mock has no ListPaths to answer). Each variant
// has its own validator.
func TestPathsRevalidationDoesNotBuild(t *testing.T) {
	t.Parallel()
	gzTag, plainTag := `"paths-`+pathsSHA+`-gz"`, `"paths-`+pathsSHA+`"`
	tests := []struct{ name, ae, inm string }{
		{"gzip", "gzip", gzTag},
		{"identity", "", plainTag},
		{"weak", "gzip", "W/" + gzTag},
		{"any", "", "*"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "r")), Browse: pathsBrowse(t, 0, nil)})
			rec := getPaths(srv, pathsSHA, map[string]string{"Accept-Encoding": tt.ae, "If-None-Match": tt.inm})
			assert.Equal(t, http.StatusNotModified, rec.Code)
			assert.Empty(t, rec.Body.String())
			assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
			assert.Equal(t, "Accept-Encoding", rec.Header().Get("Vary"))
			assert.NotEmpty(t, rec.Header().Get("ETag"))
		})
	}
}

// The list is built once, then served from the cache, in the variant the
// client accepts; the validator of the other variant does not match.
func TestPathsServesAndCaches(t *testing.T) {
	t.Parallel()
	gzTag, plainTag := `"paths-`+pathsSHA+`-gz"`, `"paths-`+pathsSHA+`"`
	srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "r")), Browse: pathsBrowse(t, 1, nil)})

	assert.Equal(t, http.StatusOK, getPaths(srv, pathsSHA, map[string]string{"Accept-Encoding": "gzip", "If-None-Match": plainTag}).Code)
	assert.Equal(t, http.StatusOK, getPaths(srv, pathsSHA, map[string]string{"If-None-Match": gzTag}).Code)

	rec := getPaths(srv, pathsSHA, map[string]string{"Accept-Encoding": "gzip"})
	assert.Equal(t, gzTag, rec.Header().Get("ETag"))
	assert.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	zr, err := gzip.NewReader(rec.Body)
	require.NoError(t, err)
	out, err := io.ReadAll(zr)
	require.NoError(t, err)
	assert.JSONEq(t, `{"paths":["a.go","b/c.go"],"truncated":false}`, string(out))

	rec = getPaths(srv, pathsSHA, nil)
	assert.Equal(t, plainTag, rec.Header().Get("ETag"))
	assert.Empty(t, rec.Header().Get("Content-Encoding"))
	assert.JSONEq(t, `{"paths":["a.go","b/c.go"],"truncated":false}`, rec.Body.String())
}

// The commit is checked as the caller of the request, and only a full id that
// resolves to itself is served.
func TestPathsRefusals(t *testing.T) {
	t.Parallel()
	other := strings.Repeat("b", 40)
	tests := []struct {
		name    string
		sha     string
		resolve func() (domain.ResolvedRevision, error)
		want    int
	}{
		{"not a full id", "main", nil, http.StatusNotFound},
		{"abbreviated id", "aaaaaaa", nil, http.StatusNotFound},
		{"commit not found or refused to the caller", pathsSHA, func() (domain.ResolvedRevision, error) { return domain.ResolvedRevision{}, domain.ErrNotFound }, http.StatusNotFound},
		{"a ref named like a commit", pathsSHA, func() (domain.ResolvedRevision, error) {
			return domain.ResolvedRevision{CommitID: objectID(t, other)}, nil
		}, http.StatusNotFound},
		{"backend failure", pathsSHA, func() (domain.ResolvedRevision, error) {
			return domain.ResolvedRevision{}, errors.New("boom")
		}, http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			browse := mocks.NewBrowseService(t)
			if tt.resolve != nil {
				browse.EXPECT().ResolveRevision(mock.Anything, domain.Anonymous(), repoName(t, "r"), mock.Anything).RunAndReturn(
					func(context.Context, domain.Principal, domain.RepoName, domain.Revision) (domain.ResolvedRevision, error) {
						return tt.resolve()
					})
			}
			srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "r")), Browse: browse})
			assert.Equal(t, tt.want, getPaths(srv, tt.sha, nil).Code)
		})
	}
}

// The list is built as the caller of the request: a failure of the use case
// is the answer.
func TestPathsListFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"not found", domain.ErrNotFound, http.StatusNotFound},
		{"other", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			browse := pathsBrowse(t, 0, nil)
			browse.EXPECT().ListPaths(mock.Anything, domain.Anonymous(), repoName(t, "r"), mock.Anything).Return(domain.PathList{}, tt.err)
			srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "r")), Browse: browse})
			assert.Equal(t, tt.want, getPaths(srv, pathsSHA, nil).Code)
		})
	}
}

// The cached list holds no spare capacity; the truncation is reported; a path
// is not HTML, so "<", ">" and "&" are not escaped in the JSON.
func TestBuildPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		paths     []string
		truncated bool
		want      string
	}{
		{"plain", []string{"a.go", "b/c.go"}, false, `{"paths":["a.go","b/c.go"],"truncated":false}`},
		{"empty", nil, false, `{"paths":[],"truncated":false}`},
		{"truncated", []string{"a.go"}, true, `{"paths":["a.go"],"truncated":true}`},
		{"html characters", []string{"a&b/<c>.go"}, false, `{"paths":["a&b/<c>.go"],"truncated":false}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			browse := mocks.NewBrowseService(t)
			browse.EXPECT().ListPaths(mock.Anything, domain.Anonymous(), repoName(t, "r"), mock.Anything).Return(
				domain.PathList{CommitID: objectID(t, pathsSHA), Paths: tt.paths, Truncated: tt.truncated}, nil)
			srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "r")), Browse: browse})
			ctx := domain.ContextWithPrincipal(context.Background(), domain.Anonymous())
			body, err := srv.buildPaths(ctx, repoName(t, "r"), pathsSHA)
			require.NoError(t, err)
			assert.Equal(t, len(body), cap(body))
			zr, err := gzip.NewReader(bytes.NewReader(body))
			require.NoError(t, err)
			out, err := io.ReadAll(zr)
			require.NoError(t, err)
			assert.Equal(t, tt.want, strings.TrimSpace(string(out)))
		})
	}
}

// A list that answers for another commit than the one asked is refused.
func TestBuildPathsWrongCommit(t *testing.T) {
	t.Parallel()
	browse := mocks.NewBrowseService(t)
	browse.EXPECT().ListPaths(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		domain.PathList{CommitID: objectID(t, strings.Repeat("b", 40))}, nil)
	srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "r")), Browse: browse})
	_, err := srv.buildPaths(context.Background(), repoName(t, "r"), pathsSHA)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

var _ ports.BrowseService = (*mocks.BrowseService)(nil)
