// SPDX-License-Identifier: AGPL-3.0-or-later

package githttp_test

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/githttp"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

type fakeAuth struct {
	mu   sync.Mutex
	seen []string
}

func (f *fakeAuth) Authenticate(_ context.Context, secret string) (domain.Principal, error) {
	f.mu.Lock()
	f.seen = append(f.seen, secret)
	f.mu.Unlock()
	switch secret {
	case "root-secret":
		return domain.Root(), nil
	case "reader-secret":
		return domain.TokenPrincipal(domain.Token{ID: "rd", Scopes: []domain.Scope{domain.ScopeRepoRead}}), nil
	}
	return domain.Principal{}, domain.ErrUnauthenticated
}

type fakeRepos struct{}

func (fakeRepos) Get(_ context.Context, p domain.Principal, n domain.RepoName) (domain.Repo, error) {
	switch n.String() {
	case "pub":
		return domain.Repo{Name: n, Visibility: domain.VisibilityPublic}, nil
	case "priv":
		if p.Allows(domain.ScopeRepoRead, n) {
			return domain.Repo{Name: n, Visibility: domain.VisibilityPrivate}, nil
		}
	}
	return domain.Repo{}, domain.ErrNotFound
}

type fakePushes struct {
	mu    sync.Mutex
	calls []bool // the create flag of each Open
	err   error
}

func (f *fakePushes) Open(_ context.Context, p domain.Principal, n domain.RepoName, create bool) (domain.Repo, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, create)
	if p.IsAnonymous() {
		return domain.Repo{}, false, domain.ErrUnauthenticated
	}
	return domain.Repo{Name: n}, false, f.err
}

type fakeTokens struct {
	mu       sync.Mutex
	released int
	repo     domain.RepoName
}

func (f *fakeTokens) Register(r domain.RepoName, _ domain.Principal) (string, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repo = r
	return "hook-token", func() { f.mu.Lock(); f.released++; f.mu.Unlock() }
}
func (f *fakeTokens) Socket() string { return "/run/hook.sock" }

// cgiScript is a stand-in for git-http-backend: it answers with the
// environment and the body it received.
func cgiScript(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "backend")
	body := "#!/bin/sh\nprintf 'Content-Type: text/plain\\r\\n\\r\\n'\nenv | sort\nprintf '\\n--body--\\n'\ncat\n"
	require.NoError(t, os.WriteFile(p, []byte(body), 0o755))
	return p
}

type rig struct {
	h       *githttp.Handler
	srv     *httptest.Server
	pushes  *fakePushes
	backend *mocks.GitBackend
	hooks   *fakeTokens
	auth    *fakeAuth
	tmp     string

	pushFlags  []bool
	backendErr error
}

func newRig(t *testing.T, mut func(*githttp.Config)) *rig {
	t.Helper()
	r := &rig{pushes: &fakePushes{}, backend: mocks.NewGitBackend(t), hooks: &fakeTokens{}, auth: &fakeAuth{}, tmp: t.TempDir()}
	cfg := githttp.Config{
		Auth: r.auth, Repos: fakeRepos{}, Pushes: r.pushes, Backend: r.backend, Hooks: r.hooks,
		TempDir: r.tmp,
	}
	script := cgiScript(t)
	r.backend.EXPECT().HTTPBackend(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, push bool) (ports.HTTPBackend, error) {
			r.pushFlags = append(r.pushFlags, push)
			if r.backendErr != nil {
				return ports.HTTPBackend{}, r.backendErr
			}
			return ports.HTTPBackend{Path: script, Dir: filepath.Dir(script), Env: []string{"PATH=/usr/bin:/bin", "GIT_PROJECT_ROOT=/x"}}, nil
		}).Maybe()
	if mut != nil {
		mut(&cfg)
	}
	r.h = githttp.New(cfg)
	mux := http.NewServeMux()
	for _, p := range githttp.Patterns {
		mux.Handle(p, r.h)
	}
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	return r
}

func (r *rig) do(t *testing.T, method, path, user, pass string, body io.Reader, hdr map[string]string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, r.srv.URL+path, body)
	require.NoError(t, err)
	if pass != "" {
		req.SetBasicAuth(user, pass)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(b), resp.Header
}

func envOf(body string) map[string]string {
	head, _, _ := strings.Cut(body, "\n--body--\n")
	m := map[string]string{}
	for _, l := range strings.Split(head, "\n") {
		k, v, ok := strings.Cut(l, "=")
		if ok {
			m[k] = v
		}
	}
	return m
}

func TestHandlerRoutesAndStatuses(t *testing.T) {
	r := newRig(t, nil)
	tests := []struct {
		name   string
		method string
		path   string
		pass   string
		want   int
		auth   bool // expects a WWW-Authenticate challenge
	}{
		{"public, anonymous", "GET", "/pub.git/info/refs?service=git-upload-pack", "", 200, false},
		{"public without .git", "GET", "/pub/info/refs?service=git-upload-pack", "", 200, false},
		{"private, anonymous", "GET", "/priv.git/info/refs?service=git-upload-pack", "", 401, true},
		{"missing, anonymous", "GET", "/none.git/info/refs?service=git-upload-pack", "", 401, true},
		{"private, reader", "GET", "/priv.git/info/refs?service=git-upload-pack", "reader-secret", 200, false},
		{"private, wrong token", "GET", "/priv.git/info/refs?service=git-upload-pack", "bad", 401, true},
		{"missing, valid token", "GET", "/none.git/info/refs?service=git-upload-pack", "root-secret", 404, false},
		{"invalid name", "GET", "/Bad.git/info/refs?service=git-upload-pack", "root-secret", 404, false},
		{"name ending in .git.git", "GET", "/x.git.git/info/refs?service=git-upload-pack", "root-secret", 404, false},
		{"no service", "GET", "/pub.git/info/refs", "", 404, false},
		{"unknown service", "GET", "/pub.git/info/refs?service=git-evil", "", 404, false},
		{"HEAD file", "GET", "/pub.git/HEAD", "", 404, false},
		{"objects", "GET", "/pub.git/objects/info/packs", "", 404, false},
		{"upload-pack wrong method", "GET", "/pub.git/git-upload-pack", "", 405, false},
		{"info/refs wrong method", "POST", "/pub.git/info/refs?service=git-upload-pack", "", 405, false},
		{"receive-pack, anonymous", "GET", "/pub.git/info/refs?service=git-receive-pack", "", 401, true},
		{"receive-pack, root", "GET", "/pub.git/info/refs?service=git-receive-pack", "root-secret", 200, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := ""
			if tt.pass != "" {
				user = "whatever" // the username is ignored
			}
			code, _, hdr := r.do(t, tt.method, tt.path, user, tt.pass, nil, nil)
			assert.Equal(t, tt.want, code)
			if tt.auth {
				assert.Equal(t, `Basic realm="ermite"`, hdr.Get("WWW-Authenticate"))
			} else {
				assert.Empty(t, hdr.Get("WWW-Authenticate"))
			}
		})
	}
}

func TestHandlerPushAuthorizationAnswers(t *testing.T) {
	r := newRig(t, nil)
	get := func() int {
		code, _, _ := r.do(t, "GET", "/p.git/info/refs?service=git-receive-pack", "u", "root-secret", nil, nil)
		return code
	}
	assert.Equal(t, 200, get())
	r.pushes.err = domain.ErrForbidden
	assert.Equal(t, 403, get())
	r.pushes.err = domain.ErrNotFound
	assert.Equal(t, 404, get())
	r.pushes.err = domain.ErrUnauthenticated
	assert.Equal(t, 401, get())
	r.pushes.err = errBoom{}
	assert.Equal(t, 500, get())

	// Only the first request of a push may create.
	r.pushes.err = nil
	r.pushes.calls = nil
	r.do(t, "GET", "/p.git/info/refs?service=git-receive-pack", "u", "root-secret", nil, nil)
	r.do(t, "POST", "/p.git/git-receive-pack", "u", "root-secret", strings.NewReader("0000"), map[string]string{"Content-Type": "application/x-git-receive-pack-request"})
	assert.Equal(t, []bool{true, false}, r.pushes.calls)
	// Reads never reach the push use case.
	r.pushes.calls = nil
	r.do(t, "GET", "/pub.git/info/refs?service=git-upload-pack", "", "", nil, nil)
	assert.Empty(t, r.pushes.calls)
}

type errBoom struct{}

func (errBoom) Error() string { return "boom with /internal/detail" }

func TestHandlerBackendEnvironment(t *testing.T) {
	r := newRig(t, nil)
	hdr := map[string]string{
		"Cookie":              "session=secret",
		"Proxy-Authorization": "Basic proxy-should-not-leak",
		"Git-Protocol":        "version=2",
		"Content-Type":        "application/x-git-receive-pack-request",
		"X-Forwarded-For":     "1.2.3.4",
	}

	// A read: no hook environment.
	code, body, _ := r.do(t, "GET", "/pub.git/info/refs?service=git-upload-pack", "u", "root-secret", nil, hdr)
	require.Equal(t, 200, code)
	e := envOf(body)
	assert.Equal(t, "/pub.git/info/refs", e["PATH_INFO"])
	assert.Equal(t, "service=git-upload-pack", e["QUERY_STRING"])
	assert.Equal(t, "version=2", e["HTTP_GIT_PROTOCOL"])
	assert.Equal(t, "/x", e["GIT_PROJECT_ROOT"], "the backend environment is used")
	assert.NotContains(t, e, "ERMITE_HOOK_TOKEN")
	assert.NotContains(t, body, "should-not-leak")
	assert.NotContains(t, body, "root-secret")
	assert.NotContains(t, body, base64.StdEncoding.EncodeToString([]byte("u:root-secret")), "the Authorization header is not passed on")
	assert.NotContains(t, body, "session=secret")
	assert.NotContains(t, e, "HTTP_AUTHORIZATION")
	assert.NotContains(t, e, "HTTP_PROXY_AUTHORIZATION")
	assert.NotContains(t, e, "HTTP_COOKIE")
	assert.Equal(t, []bool{false}, r.pushFlags)

	// A receive-pack POST gets the hook environment and releases the token.
	code, _, _ = r.do(t, "POST", "/Pub.git/git-receive-pack", "u", "root-secret", strings.NewReader("PACKDATA"), hdr)
	assert.Equal(t, 404, code, "invalid name")
	code, body, _ = r.do(t, "POST", "/pub.git/git-receive-pack", "u", "root-secret", strings.NewReader("PACKDATA"), hdr)
	require.Equal(t, 200, code)
	e = envOf(body)
	assert.Equal(t, "hook-token", e["ERMITE_HOOK_TOKEN"])
	assert.Equal(t, "/run/hook.sock", e["ERMITE_HOOK_SOCKET"])
	assert.Equal(t, "/pub.git/git-receive-pack", e["PATH_INFO"])
	assert.Equal(t, "8", e["CONTENT_LENGTH"])
	assert.True(t, strings.HasSuffix(body, "\n--body--\nPACKDATA"), "the body reaches the backend")
	assert.NotContains(t, body, "root-secret")
	assert.Equal(t, 1, r.hooks.released, "the hook token is revoked when the request ends")
	assert.Equal(t, "pub", r.hooks.repo.String())

	// The info/refs advertisement of a push does not register a hook token.
	r.do(t, "GET", "/pub.git/info/refs?service=git-receive-pack", "u", "root-secret", nil, nil)
	assert.Equal(t, 1, r.hooks.released)
}

func TestHandlerNormalizesRepositoryPath(t *testing.T) {
	r := newRig(t, nil)
	_, body, _ := r.do(t, "GET", "/pub/info/refs?service=git-upload-pack", "", "", nil, nil)
	assert.Equal(t, "/pub.git/info/refs", envOf(body)["PATH_INFO"], "the backend always sees <name>.git")
}

func TestHandlerChunkedBodyIsSpooled(t *testing.T) {
	r := newRig(t, nil)
	payload := strings.Repeat("x", 300_000)
	// An io.Reader of unknown length makes the client send chunked.
	body := io.MultiReader(strings.NewReader(payload))
	code, out, _ := r.do(t, "POST", "/pub.git/git-upload-pack", "", "", body, map[string]string{"Content-Type": "application/x-git-upload-pack-request"})
	require.Equal(t, 200, code)
	e := envOf(out)
	assert.Equal(t, "300000", e["CONTENT_LENGTH"], "the length is known to the backend")
	_, got, _ := strings.Cut(out, "\n--body--\n")
	assert.Equal(t, payload, got)
	entries, err := os.ReadDir(r.tmp)
	require.NoError(t, err)
	assert.Empty(t, entries, "spool files are removed")
}

func TestHandlerBodyLimit(t *testing.T) {
	r := newRig(t, func(c *githttp.Config) { c.MaxBodyBytes = 1000 })
	big := strings.Repeat("x", 5000)
	code, _, _ := r.do(t, "POST", "/pub.git/git-upload-pack", "", "", strings.NewReader(big), nil)
	assert.Equal(t, http.StatusRequestEntityTooLarge, code, "declared length over the limit")
	code, _, _ = r.do(t, "POST", "/pub.git/git-upload-pack", "", "", io.MultiReader(strings.NewReader(big)), nil)
	assert.Equal(t, http.StatusRequestEntityTooLarge, code, "chunked body over the limit")
	code, _, _ = r.do(t, "POST", "/pub.git/git-upload-pack", "", "", strings.NewReader("ok"), nil)
	assert.Equal(t, 200, code)
	entries, err := os.ReadDir(r.tmp)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestHandlerBackendFailure(t *testing.T) {
	r := newRig(t, nil)
	r.backendErr = io.ErrUnexpectedEOF
	code, body, _ := r.do(t, "GET", "/pub.git/info/refs?service=git-upload-pack", "", "", nil, nil)
	assert.Equal(t, 500, code)
	assert.NotContains(t, body, "EOF", "no internal detail in the response")
}

func TestHandlerLimiter(t *testing.T) {
	r := newRig(t, func(c *githttp.Config) { c.Limiter = githttp.NewLimiter(nil) })
	for i := range githttp.MaxFailures {
		code, _, _ := r.do(t, "GET", "/pub.git/info/refs?service=git-upload-pack", "u", "wrong", nil, nil)
		require.Equal(t, 401, code, i)
	}
	code, _, hdr := r.do(t, "GET", "/pub.git/info/refs?service=git-upload-pack", "u", "root-secret", nil, nil)
	assert.Equal(t, 429, code)
	assert.NotEmpty(t, hdr.Get("Retry-After"))
	// A blocked request does not even reach the authenticator.
	r.auth.mu.Lock()
	n := len(r.auth.seen)
	r.auth.mu.Unlock()
	r.do(t, "GET", "/pub.git/info/refs?service=git-upload-pack", "u", "root-secret", nil, nil)
	r.auth.mu.Lock()
	assert.Equal(t, n, len(r.auth.seen))
	r.auth.mu.Unlock()
}

func TestHandlerLimiterTrustedProxyViaConfig(t *testing.T) {
	r := newRig(t, func(c *githttp.Config) {
		c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	})
	for range githttp.MaxFailures {
		r.do(t, "GET", "/pub.git/info/refs?service=git-upload-pack", "u", "wrong", nil, map[string]string{"X-Forwarded-For": "198.51.100.1"})
	}
	code, _, _ := r.do(t, "GET", "/pub.git/info/refs?service=git-upload-pack", "", "", nil, map[string]string{"X-Forwarded-For": "198.51.100.1"})
	assert.Equal(t, 429, code)
	code, _, _ = r.do(t, "GET", "/pub.git/info/refs?service=git-upload-pack", "", "", nil, map[string]string{"X-Forwarded-For": "198.51.100.2"})
	assert.Equal(t, 200, code)
}

func TestHandlerBearerAndMalformedAuthorization(t *testing.T) {
	r := newRig(t, nil)
	path := "/priv.git/info/refs?service=git-upload-pack"
	code, _, _ := r.do(t, "GET", path, "", "", nil, map[string]string{"Authorization": "Bearer reader-secret"})
	assert.Equal(t, 200, code)
	code, _, _ = r.do(t, "GET", path, "", "", nil, map[string]string{"Authorization": "bearer reader-secret"})
	assert.Equal(t, 200, code, "the scheme is case-insensitive")
	for _, h := range []string{"Basic !!!notbase64", "Digest abc", "Bearer", "garbage"} {
		code, _, _ := r.do(t, "GET", path, "", "", nil, map[string]string{"Authorization": h})
		assert.Equal(t, 401, code, h)
	}
	// An empty password is a failed attempt, not anonymous access.
	req, _ := http.NewRequest("GET", r.srv.URL+"/pub.git/info/refs?service=git-upload-pack", nil)
	req.SetBasicAuth("u", "")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, 401, resp.StatusCode)
}
