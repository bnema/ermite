// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/bnema/zerowrap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

// serve asks the web for path while every read of a repository fails with err,
// and returns the answer and what was logged.
func serve(t *testing.T, err error, path string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	srv := newTestServer(t, Config{Repos: reposFailing(t, err), Browse: mocks.NewBrowseService(t)})
	var logs bytes.Buffer
	ctx := zerowrap.WithCtx(context.Background(), zerowrap.New(zerowrap.Config{Level: "debug", Format: "json", Output: &logs}))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil))
	return rec, logs.String()
}

// Errors of the use cases: not found (and an invalid name) are the 404 page, a
// visitor that left gets nothing, any other is the 500 page with an id that
// the log carries.
func TestErrorPages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		path string
		want int
	}{
		{"not found", fmt.Errorf("repository: %w", domain.ErrNotFound), "/gone", http.StatusNotFound},
		{"invalid repository name", domain.ErrInvalidName, "/Bad", http.StatusNotFound},
		{"invalid argument", domain.ErrInvalid, "/demo", http.StatusNotFound},
		{"invalid page token", domain.ErrInvalidPageToken, "/demo", http.StatusNotFound},
		{"invalid revision", domain.ErrInvalidRevision, "/demo", http.StatusNotFound},
		{"invalid path", domain.ErrInvalidPath, "/demo", http.StatusNotFound},
		{"internal", errors.New("dial tcp 10.0.0.5: kv exploded"), "/demo", http.StatusInternalServerError},
		{"internal on the home page", errors.New("kv exploded on the list"), "/", http.StatusInternalServerError},
		{"forbidden is not a page of its own", domain.ErrForbidden, "/demo", http.StatusInternalServerError},
		{"resource exhausted", domain.ErrResourceExhausted, "/demo", http.StatusInternalServerError},
		{"deadline", context.DeadlineExceeded, "/demo", http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec, logs := serve(t, tt.err, tt.path)
			assert.Equal(t, tt.want, rec.Code)
			if tt.want == http.StatusNotFound {
				assert.Contains(t, rec.Body.String(), "not found")
				assert.NotContains(t, rec.Body.String(), tt.err.Error(), "the cause stays out of the page")
				assert.Empty(t, logs, "a 404 is not an error")
				return
			}
			assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
			assert.Contains(t, rec.Body.String(), "internal error")
			assert.NotContains(t, rec.Body.String(), tt.err.Error(), "the cause stays in the log")
			m := regexp.MustCompile(`request id: ([0-9a-f]{8})`).FindStringSubmatch(rec.Body.String())
			require.Len(t, m, 2, "the page shows a request id")
			assert.Contains(t, logs, m[1], "the log carries the same id")
			assert.Contains(t, logs, tt.err.Error())
		})
	}
}

// A visitor that left gets no page at all.
func TestCanceledRequestGetsNoPage(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, Config{Repos: reposFailing(t, context.Canceled), Browse: mocks.NewBrowseService(t)})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/demo", nil))
	assert.Equal(t, http.StatusOK, rec.Code, "nothing was written")
	assert.Empty(t, rec.Body.String())
}

// The caller of every use case call is the principal of the request: anonymous
// without a valid session, the owner with one. A private repository is as
// missing for the first as one that does not exist (the 404 page, never a 403).
func TestUseCasesGetThePrincipalOfTheRequest(t *testing.T) {
	t.Parallel()
	const token = "tok"
	tests := []struct {
		name   string
		cookie string
		want   domain.Principal
	}{
		{"no cookie is anonymous", "", domain.Anonymous()},
		{"an invalid session is anonymous", "bad", domain.Anonymous()},
		{"a valid session is the owner", token, domain.Owner()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repos := mocks.NewRepoService(t)
			// The use case answers with the principal it was given: only the
			// owner sees the private repository.
			repos.EXPECT().Get(mock.Anything, tt.want, repoName(t, "secret")).RunAndReturn(
				func(_ context.Context, p domain.Principal, name domain.RepoName) (domain.Repo, error) {
					if !p.IsRoot() {
						return domain.Repo{}, domain.ErrNotFound
					}
					return domain.Repo{Name: name, Visibility: domain.VisibilityPrivate}, nil
				}).Once()
			if tt.want.IsOwner() {
				repos.EXPECT().View(mock.Anything, mock.Anything).Return(publicRepo(t, "secret"), nil).Once()
				repos.EXPECT().ListPage(mock.Anything, tt.want, mock.Anything, mock.Anything).Return(ports.RepoPage{}, nil).Once()
			}
			browse := mocks.NewBrowseService(t)
			if tt.want.IsOwner() {
				browse.EXPECT().ListRefs(mock.Anything, tt.want, repoName(t, "secret"), mock.Anything).Return(ports.RefPage{}, nil).Once()
			}
			srv := newTestServer(t, Config{Repos: repos, Browse: browse, Sessions: newSessions(t, token), Limiter: &countingLimiter{}})

			req := httptest.NewRequest(http.MethodGet, "/secret/refs", nil)
			if tt.cookie != "" {
				req.AddCookie(&http.Cookie{Name: CookieName, Value: tt.cookie})
			}
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if tt.want.IsOwner() {
				assert.Equal(t, http.StatusOK, rec.Code)
				return
			}
			assert.Equal(t, http.StatusNotFound, rec.Code, "a private repository is a missing one")
			assert.Contains(t, rec.Body.String(), "not found")
		})
	}
}

// A request that arrives with a principal in its context is anonymous anyway:
// only a valid session makes the owner.
func TestRequestContextCannotSetThePrincipal(t *testing.T) {
	t.Parallel()
	repos := mocks.NewRepoService(t)
	repos.EXPECT().Get(mock.Anything, domain.Anonymous(), repoName(t, "secret")).Return(domain.Repo{}, domain.ErrNotFound).Once()
	srv := newTestServer(t, Config{Repos: repos, Browse: mocks.NewBrowseService(t)})

	ctx := domain.ContextWithPrincipal(context.Background(), domain.Root())
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/secret", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestNewValidates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  Config
	}{
		{"use cases are required", Config{PublicURL: testPublicURL}},
		{"browse is required", Config{Repos: mocks.NewRepoService(t), PublicURL: testPublicURL}},
		{"repos are required", Config{Browse: mocks.NewBrowseService(t), PublicURL: testPublicURL}},
		{"the public URL must be one", Config{Repos: mocks.NewRepoService(t), Browse: mocks.NewBrowseService(t), PublicURL: "not a url"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tt.cfg)
			assert.Error(t, err)
		})
	}
}
