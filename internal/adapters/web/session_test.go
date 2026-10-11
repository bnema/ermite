// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/adapters/web/mocks"
)

// nonceA is the one valid login link of the mocks.
var nonceA = strings.Repeat("a", 43)

// newVerifier is a SessionAuth that accepts the session token valid, checked
// by verify when it is not nil, and that does not know any login link. A
// call that is not expected fails the test.
func newVerifier(t *testing.T, valid string, verify func(token string) (bool, error)) *mocks.SessionAuth {
	t.Helper()
	if verify == nil {
		verify = func(token string) (bool, error) { return token == valid, nil }
	}
	m := mocks.NewSessionAuth(t)
	m.EXPECT().Verify(mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, token string) (context.Context, bool, error) {
			ok, err := verify(token)
			return ctx, ok, err
		}).Maybe()
	return m
}

// expectLogin makes nonceA the one link of m, answering with the token valid.
func expectLogin(m *mocks.SessionAuth, valid string) {
	m.EXPECT().LoginLinkValid(mock.Anything).RunAndReturn(func(n string) bool { return n == nonceA }).Maybe()
	m.EXPECT().Login(mock.Anything, nonceA).Return(valid, time.Hour, nil).Maybe()
	m.EXPECT().Login(mock.Anything, mock.Anything).Return("", time.Duration(0), ErrLinkRejected).Maybe()
}

// newSessions is a SessionAuth with one session token and one login link.
func newSessions(t *testing.T, valid string) *mocks.SessionAuth {
	t.Helper()
	m := newVerifier(t, valid, nil)
	expectLogin(m, valid)
	return m
}

// newBrokenSessions is a SessionAuth whose check cannot be made.
func newBrokenSessions(t *testing.T, valid string) *mocks.SessionAuth {
	t.Helper()
	m := newVerifier(t, valid, func(string) (bool, error) { return false, errors.New("kv down") })
	expectLogin(m, valid)
	return m
}

type countingLimiter struct{ fails int }

func (*countingLimiter) Blocked(context.Context, string) (bool, time.Duration) { return false, 0 }
func (l *countingLimiter) Fail(context.Context, string)                        { l.fails++ }

func newSessionServer(t *testing.T, sess SessionAuth, publicURL string) (*Server, *countingLimiter) {
	t.Helper()
	lim := &countingLimiter{}
	srv, err := New(Config{
		Repos: failing{err: errors.New("x")}, Git: ermiteconnect.NewGitServiceClient(http.DefaultClient, "http://unused.invalid"),
		PublicURL: publicURL, SourceURL: "https://forge.example/ermite", Version: "v1",
		Sessions: sess, Limiter: lim,
	})
	require.NoError(t, err)
	return srv, lim
}

func TestOriginOf(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"https://forge.example":         "https://forge.example",
		"https://Forge.Example/":        "https://forge.example",
		"https://forge.example:443":     "https://forge.example",
		"http://forge.example:80":       "http://forge.example",
		"http://forge.example:8080":     "http://forge.example:8080",
		"https://forge.example:8443":    "https://forge.example:8443",
		"http://127.0.0.1:3000":         "http://127.0.0.1:3000",
		"http://[::1]:3000":             "http://[::1]:3000",
		"https://forge.example/sub/dir": "https://forge.example",
	} {
		u, err := url.Parse(in)
		require.NoError(t, err)
		assert.Equal(t, want, originOf(u), in)
	}
}

func TestValidNonce(t *testing.T) {
	t.Parallel()
	assert.True(t, validNonce(strings.Repeat("aZ09-_", 7)+"a"))
	for _, n := range []string{"", "short", strings.Repeat("a", 44), strings.Repeat("a", 42) + "=", strings.Repeat("a", 42) + "/", strings.Repeat("é", 22)} {
		assert.False(t, validNonce(n), n)
	}
}

// The gate: a POST needs the Origin of the public URL, whatever Host says, and
// a session unless it is the login.
func TestGate(t *testing.T) {
	t.Parallel()
	srv, lim := newSessionServer(t, newSessions(t, "tok"), "https://forge.example")
	do := func(path, origin, cookie string) int {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Host = "forge.example"
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: CookieName, Value: cookie})
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}
	link := "/-/login/" + strings.Repeat("a", 43)

	assert.Equal(t, 403, do("/-/logout", "", "tok"), "no Origin")
	assert.Equal(t, 403, do("/-/logout", "https://evil.example", "tok"))
	assert.Equal(t, 403, do("/-/logout", "https://forge.example.evil.example", "tok"))
	assert.Equal(t, 403, do("/-/logout", "http://forge.example", "tok"), "other scheme")
	assert.Equal(t, 403, do("/-/logout", "null", "tok"))
	assert.Equal(t, 403, do("/-/logout", "https://forge.example", ""), "no session")
	assert.Equal(t, 403, do("/-/logout", "https://forge.example", "stale"), "bad session")
	assert.Equal(t, 303, do("/-/logout", "https://forge.example", "tok"))
	assert.Equal(t, 403, do("/anything", "https://forge.example", ""), "every POST is gated")

	assert.Equal(t, 403, do(link, "", ""), "login without Origin")
	assert.Equal(t, 403, do(link, "https://evil.example", ""))
	assert.Equal(t, 303, do(link, "https://forge.example", ""), "login needs the link, not a session")
	assert.Equal(t, 404, do("/-/login/"+strings.Repeat("b", 43), "https://forge.example", ""))
	assert.Equal(t, 1, lim.fails, "one failed login counted")
	assert.Equal(t, 403, do("/-/login/x", "https://forge.example", ""), "a malformed link is not a login")
}

// GO-001: the exemption of the login POST is decided on the escaped path, the
// one the router matches, and on exactly loginPrefix + nonce. An encoded slash
// must not turn another route into a "login".
func TestGateLoginExemptionIsExact(t *testing.T) {
	t.Parallel()
	sess := newVerifier(t, "tok", nil) // no Login is expected until the last request
	lim := &countingLimiter{}
	srv, err := New(Config{
		Repos: failing{err: errors.New("x")}, Git: ermiteconnect.NewGitServiceClient(http.DefaultClient, "http://unused.invalid"),
		MergeRequests: ermiteconnect.NewMergeRequestServiceClient(http.DefaultClient, "http://unused.invalid"),
		PublicURL:     "https://forge.example", Sessions: sess, Limiter: lim,
	})
	require.NoError(t, err)
	post := func(target, cookie string) int {
		req := httptest.NewRequest(http.MethodPost, target, nil)
		req.Header.Set("Origin", "https://forge.example")
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: CookieName, Value: cookie})
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}
	nonce := nonceA
	for _, target := range []string{
		"/-%2Flogin%2Fx/merge-requests/1/close",
		"/-%2flogin%2fx/merge-requests/1/close",
		"/-%2Flogin%2F" + nonce + "/merge-requests/1/close",
		"/-/login%2F" + nonce,
		"/-/login/" + nonce + "/merge-requests/1/close",
		"/-/login/" + nonce + "/",
		"/-/login/" + nonce + "%2Fx",
		"/-/login/" + nonce + "%2F..%2Fx",
		"/-/login/%2e%2e/x/merge-requests/1/close",
		"/-/login/x/../../x/merge-requests/1/close",
		"/-/login/",
		"/-/login/%61" + nonce[1:],
		"/-/login//" + nonce,
		"/%2D/login/" + nonce,
	} {
		assert.Equal(t, http.StatusForbidden, post(target, ""), target)
	}
	// No Login was expected so far: the mock fails the test on any call.
	sess.AssertNotCalled(t, "Login", mock.Anything, mock.Anything)

	sess.EXPECT().Login(mock.Anything, nonce).Return("tok", time.Hour, nil).Once()
	assert.Equal(t, http.StatusSeeOther, post("/-/login/"+nonce, ""), "a valid link still logs in")
}

// A session check that cannot be made fails closed (anonymous) and keeps the
// cookie; a check that says no clears it.
func TestWithSessionFailsClosed(t *testing.T) {
	t.Parallel()
	broken := newBrokenSessions(t, "tok")
	srv, _ := newSessionServer(t, broken, "https://forge.example")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "tok"})
	rec := httptest.NewRecorder()
	r := srv.withSession(rec, req)
	assert.False(t, SignedIn(r.Context()))
	assert.Empty(t, rec.Header().Values("Set-Cookie"))

	srv, _ = newSessionServer(t, newSessions(t, "tok"), "https://forge.example")
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "bad"})
	rec = httptest.NewRecorder()
	r = srv.withSession(rec, req)
	assert.False(t, SignedIn(r.Context()))
	assert.Contains(t, rec.Header().Get("Set-Cookie"), "Max-Age=0")

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "tok"})
	assert.True(t, SignedIn(srv.withSession(httptest.NewRecorder(), req).Context()))
}

// GO-006: the answer to a request with an invalid cookie is not cached and
// varies on Cookie, and nothing later in the request undoes it: the page
// writer, the raw file handler and the event stream keep it.
func TestInvalidCookieAnswerIsNotCached(t *testing.T) {
	t.Parallel()
	sess := newSessions(t, "tok")
	srv, _ := newSessionServer(t, sess, "https://forge.example")

	// withSession alone.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "bad"})
	rec := httptest.NewRecorder()
	srv.withSession(rec, req)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, []string{"Cookie"}, rec.Header().Values("Vary"))
	assert.Contains(t, rec.Header().Get("Set-Cookie"), "Max-Age=0")

	// A check that cannot be made keeps the cookie and the headers as they are.
	broken, _ := newSessionServer(t, newBrokenSessions(t, "tok"), "https://forge.example")
	rec = httptest.NewRecorder()
	broken.withSession(rec, req)
	assert.Empty(t, rec.Header().Values("Cache-Control"))
	assert.Empty(t, rec.Header().Values("Vary"))

	// No cookie, a valid cookie: untouched by withSession.
	rec = httptest.NewRecorder()
	srv.withSession(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Empty(t, rec.Header().Values("Cache-Control"))
	assert.Empty(t, rec.Header().Values("Vary"))

	// The page writer, a 404 and a 500 included, does not replace it.
	for _, target := range []string{"/", "/nope/refs", "/x/tree/main"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.AddCookie(&http.Cookie{Name: CookieName, Value: "bad"})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"), target)
		assert.Equal(t, []string{"Cookie", "Accept-Encoding"}, rec.Header().Values("Vary"), target)
		assert.Contains(t, rec.Header().Get("Set-Cookie"), "Max-Age=0", target)
	}

	// The shared helper keeps no-store for a visitor, and replaces it for the
	// owner (whose answer is private anyway).
	h := http.Header{"Cache-Control": {"no-store"}, "Vary": {"Accept, cookie"}}
	cacheHeaders(context.Background(), h, "no-cache", sessionCache)
	assert.Equal(t, []string{"no-store"}, h.Values("Cache-Control"))
	assert.Equal(t, []string{"Accept, cookie"}, h.Values("Vary"), "no second Vary: Cookie")
	owner := context.WithValue(context.Background(), signedInKey{}, struct{}{})
	cacheHeaders(owner, h, "no-cache", sessionCache)
	assert.Equal(t, []string{sessionCache}, h.Values("Cache-Control"))

	// Static assets are not touched by sessions at all, cookie or not.
	req = httptest.NewRequest(http.MethodGet, StaticPrefix+"nope", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "bad"})
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	assert.Empty(t, rec.Header().Values("Set-Cookie"))
	assert.Empty(t, rec.Header().Values("Vary"))
}

// Without sessions configured there is no login route and no cookie is read.
func TestNoSessionsNoRoutes(t *testing.T) {
	t.Parallel()
	srv, err := New(Config{
		Repos: failing{err: errors.New("x")}, Git: ermiteconnect.NewGitServiceClient(http.DefaultClient, "http://unused.invalid"),
		PublicURL: "https://forge.example", SourceURL: "https://forge.example/ermite", Version: "v1",
	})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/-/login/"+strings.Repeat("a", 43), nil))
	assert.Equal(t, 404, rec.Code)

	_, err = New(Config{
		Repos: failing{}, Git: ermiteconnect.NewGitServiceClient(http.DefaultClient, "http://unused.invalid"),
		PublicURL: "https://forge.example", Sessions: mocks.NewSessionAuth(t),
	})
	assert.Error(t, err, "sessions need a limiter")
}

func TestLogPathHidesLinks(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "/-/login/…", logPath("/-/login/"+strings.Repeat("a", 43)))
	assert.Equal(t, "/demo/tree", logPath("/demo/tree"))
	for _, p := range []string{"//-/login/secret", "/x/../-/login/secret", "/-/login/secret/more", "/-/./login/secret"} {
		assert.Equal(t, "/-/login/…", logPath(p), p)
	}
}
