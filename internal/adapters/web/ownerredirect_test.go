// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

const testOwner = "owner"

// ownerRedirect sends /<owner>/<rest> to /<rest> whenever no repository named
// like the owner is visible, and only for a clean repository segment.
func TestOwnerRedirect(t *testing.T) {
	t.Parallel()
	none := reposServing(t)
	tests := []struct {
		name   string
		method string
		target string
		repos  func(t *testing.T) *mocks.RepoService
		want   string // "" : not redirected
	}{
		{"a repository page", http.MethodGet, "/owner/demo", nil, "/demo"},
		{"a repository page with a slash", http.MethodGet, "/owner/demo/", nil, "/demo/"},
		{"a sub-route", http.MethodGet, "/owner/demo/commit/abc123", nil, "/demo/commit/abc123"},
		{"head", http.MethodHead, "/owner/demo", nil, "/demo"},
		{"post is not redirected", http.MethodPost, "/owner/demo", nil, ""},
		{"query is kept", http.MethodGet, "/owner/demo/commits/main?a=1&b=x%20y", nil, "/demo/commits/main?a=1&b=x%20y"},
		{"a repository named like a route", http.MethodGet, "/owner/actions", nil, "/actions"},
		{"a repository named like a route, with a path", http.MethodGet, "/owner/actions/actions/runs/1", nil, "/actions/actions/runs/1"},
		{"git smart HTTP", http.MethodGet, "/owner/repo.git/info/refs?service=git-upload-pack", nil, "/repo.git/info/refs?service=git-upload-pack"},
		{"the owner alone", http.MethodGet, "/owner", nil, ""},
		{"the owner with a slash", http.MethodGet, "/owner/", nil, ""},
		{"another first segment", http.MethodGet, "/other/demo", nil, ""},
		{"a prefix of the owner", http.MethodGet, "/owners/demo", nil, ""},
		{"a double slash host", http.MethodGet, "//evil.com", nil, ""},
		{"a double slash host after the owner", http.MethodGet, "/owner//evil.com", nil, ""},
		{"an encoded slash", http.MethodGet, "/owner/%2Fevil.com", nil, ""},
		{"an encoded slash after the repository", http.MethodGet, "/owner/demo%2F/x", nil, ""},
		{"an encoded backslash", http.MethodGet, "/owner/%5Cevil.com", nil, ""},
		{"a backslash", http.MethodGet, `/owner/\evil.com`, nil, ""},
		{"dot dot", http.MethodGet, "/owner/..", nil, ""},
		{"dot dot after the repository", http.MethodGet, "/owner/../etc", nil, ""},
		{"encoded dot dot", http.MethodGet, "/owner/%2e%2e/etc", nil, ""},
		{"a dot", http.MethodGet, "/owner/.", nil, ""},
		{"an uppercase name", http.MethodGet, "/owner/Demo", nil, ""},
		{"the /-/ routes", http.MethodGet, "/owner/-/events", nil, ""},
		{"a bare .git", http.MethodGet, "/owner/.git/info/refs", nil, ""},
		{"a repository really named like the owner", http.MethodGet, "/owner/demo", func(t *testing.T) *mocks.RepoService {
			return reposServing(t, publicRepo(t, testOwner))
		}, ""},
		{"a route of the repository named like the owner", http.MethodGet, "/owner/actions", func(t *testing.T) *mocks.RepoService {
			return reposServing(t, publicRepo(t, testOwner), publicRepo(t, "actions"))
		}, ""},
		{"a private repository named like the owner is a missing one", http.MethodGet, "/owner/demo", func(t *testing.T) *mocks.RepoService {
			m := mocks.NewRepoService(t)
			m.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything).Return(domain.Repo{}, domain.ErrNotFound)
			return m
		}, "/demo"},
		{"a failing lookup serves the request as it is", http.MethodGet, "/owner/demo", func(t *testing.T) *mocks.RepoService {
			return reposFailing(t, errors.New("store down"))
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repos := none
			if tt.repos != nil {
				repos = tt.repos(t)
			}
			srv := newTestServer(t, Config{Repos: repos, Browse: mocks.NewBrowseService(t), Owner: testOwner})
			req := httptest.NewRequest(tt.method, tt.target, nil)
			to, ok := srv.ownerRedirect(req)
			assert.Equal(t, tt.want != "", ok)
			assert.Equal(t, tt.want, to)
			if !ok {
				return
			}
			// Whatever the target is, it stays on this origin.
			require.True(t, to[0] == '/' && (len(to) == 1 || (to[1] != '/' && to[1] != '\\')), "target %q", to)
		})
	}
}

// Through ServeHTTP the redirect is a 308 with the Location, and the paths
// that are not redirected reach the router.
func TestOwnerRedirectServed(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, Config{Repos: reposServing(t), Browse: mocks.NewBrowseService(t), Owner: testOwner})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/owner/actions?x=1", nil))
	assert.Equal(t, http.StatusPermanentRedirect, rec.Code)
	assert.Equal(t, "/actions?x=1", rec.Header().Get("Location"))

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "//evil.com", nil))
	// The mux cleans the path itself and answers with a local redirect: the
	// owner redirect adds none, and nothing leads off this origin.
	assert.NotEqual(t, http.StatusPermanentRedirect, rec.Code)
	assert.Equal(t, "/evil.com", rec.Header().Get("Location"))
}

// Without an owner there is no redirect.
func TestOwnerRedirectOff(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, Config{Repos: reposServing(t), Browse: mocks.NewBrowseService(t)})
	_, ok := srv.ownerRedirect(httptest.NewRequest(http.MethodGet, "/owner/demo", nil))
	assert.False(t, ok)
}
