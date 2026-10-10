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
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
)

// fakeSessions accepts one token.
type fakeSessions struct {
	valid string
	err   error
	used  []string
}

func (f *fakeSessions) LoginLinkValid(n string) bool { return n == strings.Repeat("a", 43) }

func (f *fakeSessions) Login(_ context.Context, n string) (string, time.Duration, error) {
	f.used = append(f.used, n)
	if n != strings.Repeat("a", 43) {
		return "", 0, ErrLinkRejected
	}
	return f.valid, time.Hour, nil
}

func (f *fakeSessions) Verify(ctx context.Context, tok string) (context.Context, bool, error) {
	return ctx, f.err == nil && tok == f.valid, f.err
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
	assert.True(t, validNonce(strings.Repeat("aZ09-_", 7)+"a"))
	for _, n := range []string{"", "short", strings.Repeat("a", 44), strings.Repeat("a", 42) + "=", strings.Repeat("a", 42) + "/", strings.Repeat("é", 22)} {
		assert.False(t, validNonce(n), n)
	}
}

// The gate: a POST needs the Origin of the public URL, whatever Host says, and
// a session unless it is the login.
func TestGate(t *testing.T) {
	srv, lim := newSessionServer(t, &fakeSessions{valid: "tok"}, "https://forge.example")
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
	assert.Equal(t, 404, do("/-/login/x", "https://forge.example", ""))
	assert.Equal(t, 2, lim.fails, "a malformed link counts too")
}

// A session check that cannot be made fails closed (anonymous) and keeps the
// cookie; a check that says no clears it.
func TestWithSessionFailsClosed(t *testing.T) {
	broken := &fakeSessions{valid: "tok", err: errors.New("kv down")}
	srv, _ := newSessionServer(t, broken, "https://forge.example")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "tok"})
	rec := httptest.NewRecorder()
	r := srv.withSession(rec, req)
	assert.False(t, SignedIn(r.Context()))
	assert.Empty(t, rec.Header().Values("Set-Cookie"))

	srv, _ = newSessionServer(t, &fakeSessions{valid: "tok"}, "https://forge.example")
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

// Without sessions configured there is no login route and no cookie is read.
func TestNoSessionsNoRoutes(t *testing.T) {
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
		PublicURL: "https://forge.example", Sessions: &fakeSessions{},
	})
	assert.Error(t, err, "sessions need a limiter")
}

func TestLogPathHidesLinks(t *testing.T) {
	assert.Equal(t, "/-/login/…", logPath("/-/login/"+strings.Repeat("a", 43)))
	assert.Equal(t, "/demo/tree", logPath("/demo/tree"))
}
