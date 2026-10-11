// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"connectrpc.com/connect"
	"github.com/bnema/zerowrap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
)

// failing is a repository client that fails every call with err.
type failing struct {
	ermiteconnect.RepoServiceClient
	err error
}

func (f failing) ListRepos(context.Context, *connect.Request[pb.ListReposRequest]) (*connect.Response[pb.ListReposResponse], error) {
	return nil, f.err
}

func (f failing) GetRepo(context.Context, *connect.Request[pb.GetRepoRequest]) (*connect.Response[pb.GetRepoResponse], error) {
	return nil, f.err
}

func serve(t *testing.T, err error, path string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	srv, nerr := New(Config{
		Repos: failing{err: err}, Git: ermiteconnect.NewGitServiceClient(http.DefaultClient, "http://unused.invalid"),
		PublicURL: "https://forge.example", SourceURL: "https://forge.example/ermite", Version: "v1",
	})
	require.NoError(t, nerr)
	var logs bytes.Buffer
	ctx := zerowrap.WithCtx(context.Background(), zerowrap.New(zerowrap.Config{Level: "debug", Format: "json", Output: &logs}))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil))
	return rec, logs.String()
}

// Errors of the API: not found (and an invalid name) are the 404 page, any
// other is the 500 page with an id that the log carries.
func TestErrorPages(t *testing.T) {
	t.Parallel()
	rec, logs := serve(t, connect.NewError(connect.CodeNotFound, errors.New("repository not found")), "/gone")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not found")
	assert.Empty(t, logs, "a 404 is not an error")

	rec, _ = serve(t, connect.NewError(connect.CodeInvalidArgument, errors.New("bad name")), "/Bad")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotContains(t, rec.Body.String(), "bad name")

	secret := "dial tcp 10.0.0.5: kv exploded"
	rec, logs = serve(t, connect.NewError(connect.CodeInternal, errors.New(secret)), "/demo")
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	assert.Contains(t, rec.Body.String(), "internal error")
	assert.NotContains(t, rec.Body.String(), secret, "the cause stays in the log")
	m := regexp.MustCompile(`request id: ([0-9a-f]{8})`).FindStringSubmatch(rec.Body.String())
	require.Len(t, m, 2, "the page shows a request id")
	assert.Contains(t, logs, m[1], "the log carries the same id")
	assert.Contains(t, logs, "kv exploded")

	rec, _ = serve(t, connect.NewError(connect.CodeInternal, errors.New("x")), "/")
	assert.Equal(t, http.StatusInternalServerError, rec.Code, "the home page too")
}

func TestNewValidates(t *testing.T) {
	t.Parallel()
	_, err := New(Config{PublicURL: "https://forge.example"})
	assert.Error(t, err, "clients are required")
	git := ermiteconnect.NewGitServiceClient(http.DefaultClient, "http://x.invalid")
	_, err = New(Config{Repos: failing{}, Git: git, PublicURL: "not a url"})
	assert.Error(t, err)
}
