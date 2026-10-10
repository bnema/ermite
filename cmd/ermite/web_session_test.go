// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
)

const (
	publicOrigin  = "http://example.test" // ERMITE_PUBLIC_URL of the harness
	sessionCookie = "__Host-ermite"
)

// page is the answer of a request to the web.
type page struct {
	status int
	hdr    http.Header
	body   string
}

func (p page) cookie(name string) *http.Cookie {
	for _, c := range (&http.Response{Header: p.hdr}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// browser is a bare HTTP client: it follows no redirect and keeps no cookie,
// so that the tests say exactly what the browser sends.
type browser struct {
	h *harness
}

func (b browser) do(method, path string, hdr map[string]string, body string) page {
	b.h.t.Helper()
	req, err := http.NewRequest(method, b.h.url(path), strings.NewReader(body))
	require.NoError(b.h.t, err)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	require.NoError(b.h.t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(b.h.t, err)
	return page{status: resp.StatusCode, hdr: resp.Header, body: string(out)}
}

func (b browser) get(path string, hdr map[string]string) page { return b.do("GET", path, hdr, "") }

// as is the header map of a browser holding the session token.
func as(token string) map[string]string {
	return map[string]string{"Cookie": sessionCookie + "=" + token}
}

func post(token string) map[string]string {
	h := as(token)
	h["Origin"] = publicOrigin
	return h
}

// loginPath turns the link the CLI printed into a path on the test server.
func loginPath(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(strings.TrimSpace(link))
	require.NoError(t, err)
	assert.Equal(t, publicOrigin, u.Scheme+"://"+u.Host, "the link is on the public URL")
	assert.True(t, strings.HasPrefix(u.Path, "/-/login/"), u.Path)
	return u.Path
}

// signIn runs `ermite web login` and confirms the link: it returns the token
// of the cookie the browser would hold.
func (c *cliHarness) signIn(t *testing.T) string {
	t.Helper()
	res := c.cli(t, adminToken, "web", "login").ok(t)
	p := browser{c.harness}.do("POST", loginPath(t, res.stdout), map[string]string{"Origin": publicOrigin}, "")
	require.Equal(t, http.StatusSeeOther, p.status, p.body)
	ck := p.cookie(sessionCookie)
	require.NotNil(t, ck)
	return ck.Value
}

// AC1, AC2, AC7 (the session part): the whole path of the owner, from the CLI
// link to the private page and back out.
func TestWebLoginEndToEnd(t *testing.T) {
	c := newCLIHarness(t)
	c.seed("secret", domain.VisibilityPrivate, "notes.txt")
	c.seed("pub", domain.VisibilityPublic, "a.txt")
	b := browser{c.harness}

	// Anonymous: the private repository is invisible; public caches as before.
	home := b.get("/", nil)
	assert.Contains(t, home.body, "pub")
	assert.NotContains(t, home.body, "secret")
	assert.Equal(t, "no-cache", home.hdr.Get("Cache-Control"))
	assert.Empty(t, home.hdr.Values("Vary"))
	assert.NotContains(t, home.body, "sign out")
	assert.Equal(t, 404, b.get("/secret", nil).status)

	// The CLI asks for a link and prints it; stderr says when it ends.
	res := c.cli(t, adminToken, "web", "login").ok(t)
	assert.Contains(t, res.stderr, "works once")
	path := loginPath(t, res.stdout)

	// GET shows the confirmation and never consumes the link.
	for range 3 {
		p := b.get(path, nil)
		assert.Equal(t, 200, p.status)
		assert.Equal(t, "no-store", p.hdr.Get("Cache-Control"))
		// Only the origin leaves the page; no-referrer would make the browser
		// send Origin: null on the sign-in POST.
		assert.Equal(t, "strict-origin", p.hdr.Get("Referrer-Policy"))
		assert.Contains(t, p.body, `method="post"`)
		assert.Contains(t, p.body, `action="`+path+`"`)
		assert.Nil(t, p.cookie(sessionCookie), "a GET sets no session")
		assertNoInline(t, p.body)
	}
	// A POST that is not from this site is refused and does not consume it.
	assert.Equal(t, 403, b.do("POST", path, nil, "").status, "no Origin")
	assert.Equal(t, 403, b.do("POST", path, map[string]string{"Origin": "https://evil.example"}, "").status, "foreign Origin")
	assert.Equal(t, 403, b.do("POST", path, map[string]string{"Origin": "null"}, "").status, "opaque Origin")
	assert.Equal(t, 200, b.get(path, nil).status, "still pending")

	// The POST consumes it and sets the cookie.
	login := b.do("POST", path, map[string]string{"Origin": publicOrigin}, "")
	require.Equal(t, http.StatusSeeOther, login.status, login.body)
	assert.Equal(t, "/", login.hdr.Get("Location"))
	assert.Equal(t, "no-store", login.hdr.Get("Cache-Control"))
	ck := login.cookie(sessionCookie)
	require.NotNil(t, ck)
	assert.True(t, ck.Secure && ck.HttpOnly)
	assert.Equal(t, http.SameSiteStrictMode, ck.SameSite)
	assert.Equal(t, "/", ck.Path)
	assert.Empty(t, ck.Domain, "no Domain: host only")
	assert.Equal(t, 12*60*60, ck.MaxAge)
	token := ck.Value
	claims := jwt.MapClaims{}
	_, _, err := jwt.NewParser().ParseUnverified(token, claims)
	require.NoError(t, err)
	assert.Equal(t, "owner", claims["sub"])
	assert.Equal(t, float64(0), claims["epoch"])
	exp, err := claims.GetExpirationTime()
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(12*time.Hour), exp.Time, time.Minute)
	assert.NotNil(t, claims["iat"])

	// Replayed and unknown links fail with the same page.
	for _, p := range []string{path, "/-/login/" + strings.Repeat("a", 43), "/-/login/short"} {
		g := b.get(p, nil)
		assert.Equal(t, 404, g.status, p)
		assert.Equal(t, "no-store", g.hdr.Get("Cache-Control"))
		assert.Contains(t, g.body, "login link not valid")
		r := b.do("POST", p, map[string]string{"Origin": publicOrigin}, "")
		assert.Equal(t, 404, r.status, p)
		assert.Equal(t, g.body, r.body, "no detail tells why")
		assert.Nil(t, r.cookie(sessionCookie))
	}

	// Signed in: private repositories show on every page, headers say private.
	for _, p := range []string{"/", "/secret", "/secret/tree", "/secret/blob/main/notes.txt", "/secret/commits/main", "/secret/refs", "/nope"} {
		got := b.get(p, as(token))
		if p == "/nope" {
			assert.Equal(t, 404, got.status)
		} else {
			assert.Equal(t, 200, got.status, p)
		}
		assert.Equal(t, "private, no-store", got.hdr.Get("Cache-Control"), p)
		assert.Contains(t, got.hdr.Values("Vary"), "Cookie", p)
		assertNoInline(t, got.body)
	}
	home = b.get("/", as(token))
	assert.Contains(t, home.body, "secret")
	assert.Contains(t, home.body, `<form class="st-owner" method="post" action="/-/logout">`)
	assert.Contains(t, b.get("/secret/blob/main/notes.txt", as(token)).body, "notes.txt")
	raw := b.get("/secret/raw/main/notes.txt", as(token))
	assert.Equal(t, 200, raw.status)
	assert.Equal(t, "private, no-store", raw.hdr.Get("Cache-Control"))
	assert.Equal(t, 404, b.get("/secret/raw/main/notes.txt", nil).status)
	// Static assets stay public.
	assert.Contains(t, b.get("/", as(token)).body, "/-/static/")

	// The live stream of a private repository is the owner's.
	assert.Equal(t, 404, b.get("/-/events?topic=repo:secret", nil).status)
	sctx, stop := context.WithCancel(c.ctx)
	defer stop()
	sreq, err := http.NewRequestWithContext(sctx, http.MethodGet, c.harness.url("/-/events?topic=repo:secret"), nil)
	require.NoError(t, err)
	sreq.Header.Set("Cookie", sessionCookie+"="+token)
	sres, err := http.DefaultClient.Do(sreq)
	require.NoError(t, err)
	assert.Equal(t, 200, sres.StatusCode)
	assert.Equal(t, "private, no-store, no-transform", sres.Header.Get("Cache-Control"))
	assert.Contains(t, sres.Header.Values("Vary"), "Cookie")
	stop()
	_ = sres.Body.Close()

	// The browser routes ignore Authorization: the admin token shows nothing.
	bearer := b.get("/", map[string]string{"Authorization": "Bearer " + adminToken})
	assert.NotContains(t, bearer.body, "secret")
	assert.Equal(t, "no-cache", bearer.hdr.Get("Cache-Control"))
	assert.Equal(t, 404, b.get("/secret", map[string]string{"Authorization": "Bearer " + adminToken}).status)

	// The API ignores the cookie: no bearer is anonymous, so nothing private
	// and nothing that needs a caller.
	api := c.api("", connect.WithInterceptors(cookieInterceptor(token)))
	list, err := api.repo.ListRepos(context.Background(), connect.NewRequest(&pb.ListReposRequest{}))
	require.NoError(t, err)
	require.Len(t, list.Msg.GetRepos(), 1)
	assert.Equal(t, "pub", list.Msg.GetRepos()[0].GetName())
	_, err = api.repo.GetRepo(context.Background(), connect.NewRequest(&pb.GetRepoRequest{Repo: "secret"}))
	assert.Equal(t, "not_found", codeName(err))
	_, err = api.token.ListTokens(context.Background(), connect.NewRequest(&pb.ListTokensRequest{}))
	assert.Equal(t, "unauthenticated", codeName(err))
	_, err = api.session.CreateLoginLink(context.Background(), connect.NewRequest(&pb.CreateLoginLinkRequest{}))
	assert.Equal(t, "unauthenticated", codeName(err))
	_, err = api.session.SignOutEverywhere(context.Background(), connect.NewRequest(&pb.SignOutEverywhereRequest{}))
	assert.Equal(t, "unauthenticated", codeName(err))

	// Logout: a POST from this site with the session; the cookie is deleted.
	assert.Equal(t, 403, b.do("POST", "/-/logout", as(token), "").status, "no Origin")
	assert.Equal(t, 403, b.do("POST", "/-/logout", map[string]string{"Origin": "https://evil.example", "Cookie": sessionCookie + "=" + token}, "").status)
	assert.Equal(t, 403, b.do("POST", "/-/logout", map[string]string{"Origin": publicOrigin}, "").status, "no session")
	out := b.do("POST", "/-/logout", post(token), "")
	assert.Equal(t, http.StatusSeeOther, out.status)
	assert.Equal(t, "/", out.hdr.Get("Location"))
	gone := out.cookie(sessionCookie)
	require.NotNil(t, gone)
	assert.Negative(t, gone.MaxAge)
	assert.Empty(t, gone.Value)
	assert.Equal(t, 404, b.get("/secret", nil).status)
	// A GET never signs out.
	g := b.get("/-/logout", as(token))
	assert.Equal(t, 404, g.status, "no page")
	assert.Nil(t, g.cookie(sessionCookie), "the session is untouched")
	assert.Equal(t, 200, b.get("/secret", as(token)).status, "the token itself lives until logout --all or expiry")

	// logout --all: the cookie is rejected and cleared; anonymous again.
	c.cli(t, adminToken, "web", "logout", "--all").ok(t)
	after := b.get("/secret", as(token))
	assert.Equal(t, 404, after.status)
	assert.Equal(t, "no-cache", after.hdr.Get("Cache-Control"))
	cleared := after.cookie(sessionCookie)
	require.NotNil(t, cleared, "a rejected cookie is cleared")
	assert.Negative(t, cleared.MaxAge)
	assert.NotContains(t, b.get("/", as(token)).body, "secret")
	assert.Equal(t, 403, b.do("POST", "/-/logout", post(token), "").status, "POST needs a valid session")

	// A new link gives a session of the new epoch.
	fresh := c.signIn(t)
	assert.Equal(t, 200, b.get("/secret", as(fresh)).status)
	assert.Equal(t, 404, b.get("/secret", as(token)).status)
}

// cookieInterceptor makes a Connect client send the session cookie, as a
// browser-made call would, to show the API pays no attention to it.
func cookieInterceptor(token string) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Cookie", sessionCookie+"="+token)
			return next(ctx, req)
		}
	})
}

// The cookie must be an unforgeable HS256 token for the key of the server.
func TestWebRejectsForgedSessions(t *testing.T) {
	c := newCLIHarness(t)
	c.seed("secret", domain.VisibilityPrivate, "notes.txt")
	b := browser{c.harness}
	good := c.signIn(t)
	require.Equal(t, 200, b.get("/secret", as(good)).status)

	now := time.Now()
	claims := jwt.MapClaims{"sub": "owner", "epoch": 0, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	none, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)
	wrongKey, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("not the key of the server........"))
	require.NoError(t, err)
	wrongAlg, err := jwt.NewWithClaims(jwt.SigningMethodHS512, claims).SignedString([]byte("not the key of the server........"))
	require.NoError(t, err)
	for name, tok := range map[string]string{
		"alg none": none, "wrong key": wrongKey, "wrong alg": wrongAlg, "garbage": "x.y.z", "truncated": good[:len(good)-2],
	} {
		p := b.get("/secret", as(tok))
		assert.Equal(t, 404, p.status, name)
		assert.Equal(t, "no-cache", p.hdr.Get("Cache-Control"), name)
		ck := p.cookie(sessionCookie)
		require.NotNil(t, ck, name)
		assert.Negative(t, ck.MaxAge, name)
		assert.Equal(t, 403, b.do("POST", "/-/logout", post(tok), "").status, name)
	}
}

// Failed logins count in the limiter of failed authentications, like a bad
// token on the API: the eleventh attempt of one client is refused, and a valid
// link of that client is too.
func TestWebLoginFailuresAreRateLimited(t *testing.T) {
	h := newHarness(t, harnessOpts{trustedProxies: "127.0.0.0/8"})
	c := &cliHarness{harness: h, url: h.srv.URL}
	b := browser{c.harness}
	from := func(ip string) map[string]string {
		return map[string]string{"Origin": publicOrigin, "X-Forwarded-For": ip}
	}
	bad := "/-/login/" + strings.Repeat("b", 43)
	for range 10 {
		assert.Equal(t, 404, b.do("POST", bad, from("203.0.113.9"), "").status)
	}
	blocked := b.do("POST", bad, from("203.0.113.9"), "")
	assert.Equal(t, http.StatusTooManyRequests, blocked.status)
	assert.NotEmpty(t, blocked.hdr.Get("Retry-After"))

	link := c.cli(t, adminToken, "web", "login").ok(t).stdout
	assert.Equal(t, http.StatusTooManyRequests, b.do("POST", loginPath(t, link), from("203.0.113.9"), "").status, "the budget is spent")
	assert.Equal(t, http.StatusSeeOther, b.do("POST", loginPath(t, link), from("203.0.113.10"), "").status, "another client is not affected")
	// The same budget as the API's: a bad token from this client is refused.
	_, err := c.api("bad-token", connect.WithInterceptors(forwardedFor("203.0.113.9"))).server.GetVersion(context.Background(), connect.NewRequest(&pb.GetVersionRequest{}))
	require.NoError(t, err, "GetVersion needs no authentication")
	_, err = c.api("bad-token", connect.WithInterceptors(forwardedFor("203.0.113.9"))).repo.ListRepos(context.Background(), connect.NewRequest(&pb.ListReposRequest{}))
	assert.Equal(t, "resource_exhausted", codeName(err))
}

func forwardedFor(ip string) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("X-Forwarded-For", ip)
			return next(ctx, req)
		}
	})
}

// Only the owner creates links: the admin token, or a token with every scope
// and no repository list.
func TestSessionServiceIsOwnerOnly(t *testing.T) {
	c := newCLIHarness(t)
	ctx := context.Background()
	c.seed("a", domain.VisibilityPublic)
	partial := c.token(domain.AllScopes()[:8])
	limited := c.token(domain.AllScopes(), "a")
	full := c.token(domain.AllScopes())

	for name, tc := range map[string]struct{ secret, code string }{
		"anonymous":     {"", "unauthenticated"},
		"bad token":     {"nope", "unauthenticated"},
		"missing scope": {partial, "permission_denied"},
		"repo list":     {limited, "permission_denied"},
		"every scope":   {full, "ok"},
		"admin token":   {adminToken, "ok"},
	} {
		a := c.api(tc.secret)
		_, err := a.session.CreateLoginLink(ctx, connect.NewRequest(&pb.CreateLoginLinkRequest{}))
		assert.Equal(t, tc.code, codeName(err), name)
		_, err = a.session.SignOutEverywhere(ctx, connect.NewRequest(&pb.SignOutEverywhereRequest{}))
		if tc.code == "unauthenticated" {
			assert.Equal(t, tc.code, codeName(err), name)
		} else {
			assert.Equal(t, tc.code, codeName(err), name)
		}
	}

	res := c.cli(t, partial, "web", "login")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "permission denied")
	assert.Empty(t, res.stdout)
	res = c.cli(t, adminToken, "web", "logout")
	require.Error(t, res.err, "--all is required")
	res = c.cli(t, adminToken, "web", "login", "--json").ok(t)
	assert.ElementsMatch(t, []string{"expires_at", "url"}, keys(t, res.stdout))
}

// Login links are bounded in number.
func TestLoginLinksAreBoundedOverTheAPI(t *testing.T) {
	c := newCLIHarness(t)
	a := c.api(adminToken)
	var code string
	for range 20 {
		_, err := a.session.CreateLoginLink(context.Background(), connect.NewRequest(&pb.CreateLoginLinkRequest{}))
		if code = codeName(err); code != "ok" {
			break
		}
	}
	assert.Equal(t, "resource_exhausted", code)
}

// A link, a session token and the signing key are secrets: no log line holds
// them, at the most verbose level.
func TestWebLoginSecretsStayOutOfTheLog(t *testing.T) {
	c := newCLIHarness(t)
	c.seed("secret", domain.VisibilityPrivate, "notes.txt")
	b := browser{c.harness}

	link := strings.TrimSpace(c.cli(t, adminToken, "web", "login").ok(t).stdout)
	nonce := link[strings.LastIndex(link, "/")+1:]
	require.Len(t, nonce, 43)
	get := b.get(loginPath(t, link), nil)
	require.Equal(t, 200, get.status)
	p := b.do("POST", loginPath(t, link), map[string]string{"Origin": publicOrigin}, "")
	token := p.cookie(sessionCookie).Value
	b.get("/secret", as(token))
	b.get("/-/login/"+strings.Repeat("z", 43), nil)
	b.do("POST", "/-/login/"+strings.Repeat("z", 43), map[string]string{"Origin": publicOrigin}, "")

	logs := c.logs.String()
	assert.NotContains(t, logs, nonce)
	assert.NotContains(t, logs, token)
	assert.NotContains(t, logs, strings.Repeat("z", 43))
	assert.Contains(t, logs, "login link created")
	assert.Contains(t, logs, "owner signed in on the web")
	key, err := c.forge.store.Sessions().Key(context.Background())
	require.NoError(t, err)
	assert.NotContains(t, logs, string(key))
}

// The owner principal is what the API sees from a signed-in page, and is
// labelled "owner".
func TestWebOwnerIsLabelledInLogs(t *testing.T) {
	c := newCLIHarness(t)
	c.seed("secret", domain.VisibilityPrivate, "notes.txt")
	token := c.signIn(t)
	browser{c.harness}.get("/secret", as(token))
	assert.Contains(t, c.logs.String(), `"principal":"owner"`)
}
