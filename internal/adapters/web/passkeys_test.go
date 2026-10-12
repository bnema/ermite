// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

const forgeOrigin = "https://forge.example"

// blockingLimiter is a FailureLimiter that can be told to block.
type blockingLimiter struct {
	blocked bool
	fails   int
}

func (l *blockingLimiter) Blocked(context.Context, string) (bool, time.Duration) {
	if l.blocked {
		return true, 90 * time.Second
	}
	return false, 0
}
func (l *blockingLimiter) Fail(context.Context, string) { l.fails++ }

type passkeyFixture struct {
	srv      *Server
	sessions *mocks.WebSessionService
	passkeys *mocks.WebPasskeyService
	lim      *blockingLimiter
}

func newPasskeyServer(t *testing.T) *passkeyFixture {
	t.Helper()
	f := &passkeyFixture{
		sessions: newSessions(t, "tok"),
		passkeys: mocks.NewWebPasskeyService(t),
		lim:      &blockingLimiter{},
	}
	f.srv = newTestServer(t, Config{
		Repos: reposFailing(t, errors.New("x")), Browse: mocks.NewBrowseService(t),
		PublicURL: forgeOrigin, Sessions: f.sessions, Passkeys: f.passkeys, Limiter: f.lim,
	})
	return f
}

type req struct {
	method, target string
	origin         string // "" sends none
	cookie         string
	contentType    string
	body           string
}

func (f *passkeyFixture) do(t *testing.T, r req) *httptest.ResponseRecorder {
	t.Helper()
	hr := httptest.NewRequest(r.method, r.target, strings.NewReader(r.body))
	hr.Host = "forge.example"
	if r.origin != "" {
		hr.Header.Set("Origin", r.origin)
	}
	if r.cookie != "" {
		hr.AddCookie(&http.Cookie{Name: CookieName, Value: r.cookie})
	}
	if r.contentType != "" {
		hr.Header.Set("Content-Type", r.contentType)
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, hr)
	return rec
}

func postJSON(target, cookie, body string) req {
	return req{method: "POST", target: target, origin: forgeOrigin, cookie: cookie, contentType: "application/json", body: body}
}

const finishBody = `{"ceremony":"c1","name":"Pixel 9","credential":{"id":"x","type":"public-key"}}`

var (
	beginLogin     = "/-/passkeys/login/begin"
	finishLogin    = "/-/passkeys/login/finish"
	beginRegister  = "/-/passkeys/register/begin"
	finishRegister = "/-/passkeys/register/finish"
)

// Every new POST needs the Origin of the public URL.
func TestPasskeyPostsNeedTheOrigin(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t) // the mocks fail the test on any call
	removeTarget := "/-/passkeys/" + base64.RawURLEncoding.EncodeToString([]byte("a")) + "/remove"
	for _, target := range []string{beginLogin, finishLogin, beginRegister, finishRegister, removeTarget} {
		for _, cookie := range []string{"", "tok"} {
			for _, origin := range []string{"", "null", "https://evil.example", "http://forge.example", "https://forge.example.evil.example"} {
				rec := f.do(t, req{method: "POST", target: target, origin: origin, cookie: cookie, contentType: "application/json", body: finishBody})
				assert.Equal(t, http.StatusForbidden, rec.Code, "%s origin=%q cookie=%q", target, origin, cookie)
				assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
			}
		}
	}
}

// An anonymous browser may post to the two login endpoints only.
func TestPasskeyAnonymousPostsAreExact(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	id := base64.RawURLEncoding.EncodeToString([]byte("a"))
	for _, target := range []string{
		beginRegister, finishRegister, "/-/passkeys/" + id + "/remove",
		"/-/passkeys", "/-/passkeys/", "/-/passkeys/login", "/-/passkeys/login/begin/", "/-/passkeys/login/begin/x",
		"/-/passkeys/login/finish/x", "/-/passkeys%2Flogin%2Fbegin", "/-%2Fpasskeys/login/begin",
		"/-/passkeys/login%2Ffinish", "/-/passkeys/login/./begin", "/-/passkeys/x/../login/begin",
		"/-/passkeys/%6Cogin/begin", "//-/passkeys/login/begin", "/-/passkeys/login/begin%2f",
	} {
		rec := f.do(t, postJSON(target, "", finishBody))
		assert.Equal(t, http.StatusForbidden, rec.Code, target)
	}
	// A GET is never a login.
	rec := f.do(t, req{method: "GET", target: beginLogin, origin: forgeOrigin})
	assert.NotEqual(t, http.StatusOK, rec.Code)
}

func TestPasskeyLoginBegin(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	f.passkeys.EXPECT().BeginLogin(mock.Anything, mock.Anything).Return("cer1", []byte(`{"publicKey":{"challenge":"abc"}}`), nil).Once()

	rec := f.do(t, req{method: "POST", target: beginLogin, origin: forgeOrigin})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.JSONEq(t, `{"ceremony":"cer1","options":{"publicKey":{"challenge":"abc"}}}`, rec.Body.String())
	assert.Empty(t, rec.Header().Values("Set-Cookie"))
}

func TestPasskeyLoginBeginFailure(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	f.passkeys.EXPECT().BeginLogin(mock.Anything, mock.Anything).Return("", nil, errors.New("secret detail")).Once()
	rec := f.do(t, req{method: "POST", target: beginLogin, origin: forgeOrigin})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "secret detail")
	assert.Contains(t, rec.Body.String(), "request id")
}

func TestPasskeyLoginBlocked(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t) // no use case call is expected
	f.lim.blocked = true
	for _, r := range []req{
		{method: "POST", target: beginLogin, origin: forgeOrigin},
		postJSON(finishLogin, "", finishBody),
	} {
		rec := f.do(t, r)
		assert.Equal(t, http.StatusTooManyRequests, rec.Code, r.target)
		assert.JSONEq(t, `{"error":"too many attempts"}`, rec.Body.String())
		assert.Equal(t, "91", rec.Header().Get("Retry-After"))
		assert.Empty(t, rec.Header().Values("Set-Cookie"))
	}
	assert.Zero(t, f.lim.fails, "a blocked client is not counted again")
}

func TestPasskeyLoginFinishSetsTheSession(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	f.passkeys.EXPECT().FinishLogin(mock.Anything, "c1", mock.MatchedBy(func(b []byte) bool {
		return string(b) == `{"id":"x","type":"public-key"}`
	})).Return("tok", 12*time.Hour, nil).Once()

	rec := f.do(t, postJSON(finishLogin, "", finishBody))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Zero(t, f.lim.fails)

	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	c := cookies[0]
	assert.Equal(t, CookieName, c.Name)
	assert.Equal(t, "tok", c.Value)
	assert.True(t, c.Secure)
	assert.True(t, c.HttpOnly)
	assert.Equal(t, http.SameSiteStrictMode, c.SameSite)
	assert.Equal(t, "/", c.Path)
	assert.Equal(t, 12*3600, c.MaxAge)
	assert.Empty(t, c.Domain)
	set := rec.Header().Get("Set-Cookie")
	for _, attr := range []string{"Secure", "HttpOnly", "SameSite=Strict", "Path=/"} {
		assert.Contains(t, set, attr)
	}
}

// A stale cookie that withSession cleared is replaced, not doubled.
func TestPasskeyLoginFinishReplacesAStaleCookie(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	f.passkeys.EXPECT().FinishLogin(mock.Anything, mock.Anything, mock.Anything).Return("tok", time.Hour, nil).Once()
	rec := f.do(t, postJSON(finishLogin, "stale", finishBody))
	// A stale cookie is not a session: the login endpoint still lets it through.
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, rec.Result().Cookies(), 1)
	assert.Equal(t, "tok", rec.Result().Cookies()[0].Value)
}

func TestPasskeyLoginFinishRefusal(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	f.passkeys.EXPECT().FinishLogin(mock.Anything, mock.Anything, mock.Anything).Return("", time.Duration(0), ports.ErrLinkRejected).Twice()

	for range 2 {
		rec := f.do(t, postJSON(finishLogin, "", finishBody))
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.JSONEq(t, `{"error":"passkey not accepted"}`, rec.Body.String())
		assert.Empty(t, rec.Header().Values("Set-Cookie"))
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	}
	assert.Equal(t, 2, f.lim.fails, "every refusal counts")
}

func TestPasskeyLoginFinishInternalError(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	f.passkeys.EXPECT().FinishLogin(mock.Anything, mock.Anything, mock.Anything).Return("", time.Duration(0), errors.New("kv down")).Once()
	rec := f.do(t, postJSON(finishLogin, "", finishBody))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "kv down")
	assert.Empty(t, rec.Header().Values("Set-Cookie"))
	assert.Zero(t, f.lim.fails, "a server failure is not the client's")
}

func TestPasskeyFinishNeedsJSON(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t) // no use case call is expected
	for _, target := range []string{finishLogin, finishRegister} {
		for _, ct := range []string{"application/x-www-form-urlencoded", "text/plain", "multipart/form-data; boundary=x", "", "application/jsonx", "json"} {
			cookie := ""
			if target == finishRegister {
				cookie = "tok"
			}
			rec := f.do(t, req{method: "POST", target: target, origin: forgeOrigin, cookie: cookie, contentType: ct, body: finishBody})
			assert.Equal(t, http.StatusUnsupportedMediaType, rec.Code, "%s %q", target, ct)
		}
	}
	assert.Zero(t, f.lim.fails)
}

func TestPasskeyFinishAcceptsJSONWithCharset(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	f.passkeys.EXPECT().FinishLogin(mock.Anything, mock.Anything, mock.Anything).Return("tok", time.Hour, nil).Once()
	rec := f.do(t, req{method: "POST", target: finishLogin, origin: forgeOrigin, contentType: "application/json; charset=utf-8", body: finishBody})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestPasskeyFinishBodyLimit(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	big := `{"ceremony":"c1","credential":"` + strings.Repeat("a", 64<<10) + `"}`
	for _, target := range []string{finishLogin, finishRegister} {
		rec := f.do(t, postJSON(target, "tok", big))
		assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, target)
	}
	// Right at the limit is read (and then refused as malformed, not as too large).
	exact := `{"ceremony":"` + strings.Repeat("a", 64<<10-len(`{"ceremony":""}`)) + `"}`
	require.Len(t, exact, 64<<10)
	rec := f.do(t, postJSON(finishLogin, "", exact))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Zero(t, f.lim.fails, "a malformed body is not a failed authentication")
}

func TestPasskeyFinishMalformed(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	for _, body := range []string{``, `not json`, `{}`, `{"ceremony":"c1"}`, `{"credential":{}}`, `{"ceremony":"","credential":{}}`, `[]`} {
		for _, target := range []string{finishLogin, finishRegister} {
			rec := f.do(t, postJSON(target, "tok", body))
			assert.Equal(t, http.StatusBadRequest, rec.Code, "%s %q", target, body)
		}
	}
}

// The registration endpoints are the owner's.
func TestPasskeyRegistration(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	f.passkeys.EXPECT().BeginRegistration(mock.Anything, domain.Owner(), "").Return("cer9", []byte(`{"publicKey":{}}`), nil).Once()
	rec := f.do(t, postJSON(beginRegister, "tok", `{}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"ceremony":"cer9","options":{"publicKey":{}}}`, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	f.passkeys.EXPECT().FinishRegistration(mock.Anything, domain.Owner(), "c1", "Pixel 9", mock.MatchedBy(func(b []byte) bool {
		return string(b) == `{"id":"x","type":"public-key"}`
	})).Return(nil).Once()
	rec = f.do(t, postJSON(finishRegister, "tok", finishBody))
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Empty(t, rec.Header().Values("Set-Cookie"), "registering does not touch the session")
}

func TestPasskeyRegistrationErrors(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err    error
		status int
		body   string
	}{
		"duplicate":    {domain.ErrAlreadyExists, 409, "already registered"},
		"limit":        {domain.ErrResourceExhausted, 429, ""},
		"bad name":     {&domain.TextError{Reason: "contains the control character U+0009"}, 400, "control character"},
		"refused":      {domain.ErrUnauthenticated, 400, "passkey not accepted"},
		"not owner":    {domain.ErrForbidden, 403, ""},
		"server error": {errors.New("kv down"), 500, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newPasskeyServer(t)
			f.passkeys.EXPECT().FinishRegistration(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(tc.err).Once()
			rec := f.do(t, postJSON(finishRegister, "tok", finishBody))
			assert.Equal(t, tc.status, rec.Code)
			assert.Contains(t, rec.Body.String(), `"error"`)
			assert.Contains(t, rec.Body.String(), tc.body)
			assert.NotContains(t, rec.Body.String(), "kv down")
			assert.Zero(t, f.lim.fails, "the owner's errors are not login failures")
		})
	}

	f := newPasskeyServer(t)
	f.passkeys.EXPECT().BeginRegistration(mock.Anything, mock.Anything, mock.Anything).Return("", nil, domain.ErrResourceExhausted).Once()
	rec := f.do(t, postJSON(beginRegister, "tok", `{}`))
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
}

func TestPasskeysPage(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	now := time.Now()
	list := []domain.Passkey{
		{ID: []byte{0xfb, 0xff, 'a'}, Name: "Pixel <9>", CreatedAt: now.Add(-72 * time.Hour), LastUsedAt: now.Add(-time.Hour), BackupState: true},
		{ID: []byte("hw"), Name: "yubikey", CreatedAt: now.Add(-time.Hour)},
	}
	f.passkeys.EXPECT().List(mock.Anything, domain.Owner()).Return(list, nil).Once()

	rec := f.do(t, req{method: "GET", target: "/-/passkeys", cookie: "tok"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
	body := rec.Body.String()
	assert.Contains(t, body, "Pixel &lt;9&gt;", "names are escaped")
	assert.NotContains(t, body, "Pixel <9>")
	assert.Contains(t, body, "yubikey")
	assert.Contains(t, body, "3d ago")
	assert.Contains(t, body, "never")
	assert.Contains(t, body, `action="/-/passkeys/`+base64.RawURLEncoding.EncodeToString(list[0].ID)+`/remove"`)
	assert.Contains(t, body, `data-passkey="register"`)
	// TS-004: the name field and the add button are one form, so Enter submits.
	form := body[strings.Index(body, "<form class=\"passkey-add\""):]
	form = form[:strings.Index(form, "</form>")]
	assert.Contains(t, form, "data-passkey-form")
	assert.Contains(t, form, `data-passkey-name`)
	assert.Contains(t, form, `type="submit"`)
	assert.Contains(t, form, `data-passkey="register"`)
	assert.NotContains(t, form, "action=", "the script handles the submit; the form never posts by itself")
	assert.Contains(t, body, `data-begin="/-/passkeys/register/begin"`)
	assert.Contains(t, body, `data-finish="/-/passkeys/register/finish"`)
	assert.Contains(t, body, "data-passkey-status")
	assert.Contains(t, body, `aria-live="polite"`)
}

func TestPasskeysPageFull(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	list := make([]domain.Passkey, domain.MaxPasskeys)
	for i := range list {
		list[i] = domain.Passkey{ID: []byte{byte(i + 1)}, Name: "k", CreatedAt: time.Now()}
	}
	f.passkeys.EXPECT().List(mock.Anything, mock.Anything).Return(list, nil).Once()
	rec := f.do(t, req{method: "GET", target: "/-/passkeys", cookie: "tok"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), `data-passkey="register"`)
	assert.Contains(t, rec.Body.String(), "Remove one to add another")
}

// Anybody but the owner gets the 404 page, and the list is not read.
func TestPasskeysPageIsTheOwners(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	for _, cookie := range []string{"", "stale"} {
		rec := f.do(t, req{method: "GET", target: "/-/passkeys", cookie: cookie})
		assert.Equal(t, http.StatusNotFound, rec.Code, cookie)
		assert.NotContains(t, rec.Body.String(), "Passkeys")
		assert.NotContains(t, rec.Body.String(), "data-passkey")
	}
}

func TestPasskeyRemove(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	id := []byte{0xfb, 0xff, 'a'}
	target := "/-/passkeys/" + base64.RawURLEncoding.EncodeToString(id) + "/remove"
	f.passkeys.EXPECT().Remove(mock.Anything, domain.Owner(), id).Return(nil).Once()

	rec := f.do(t, req{method: "POST", target: target, origin: forgeOrigin, cookie: "tok", contentType: "application/x-www-form-urlencoded"})
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/-/login", rec.Header().Get("Location"), "the epoch bump signed this browser out too")
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

func TestPasskeyRemoveErrors(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	// Ids that are not unpadded base64url never reach the use case (the mock
	// fails the test on a call).
	for _, id := range []string{"not%20base64%21", "%2B%2F8B", "YQ%3D%3D", "%C3%A9"} {
		rec := f.do(t, req{method: "POST", target: "/-/passkeys/" + id + "/remove", origin: forgeOrigin, cookie: "tok"})
		assert.Equal(t, http.StatusNotFound, rec.Code, id)
	}

	f.passkeys.EXPECT().Remove(mock.Anything, mock.Anything, []byte("zz")).Return(domain.ErrNotFound).Once()
	rec := f.do(t, req{method: "POST", target: "/-/passkeys/" + base64.RawURLEncoding.EncodeToString([]byte("zz")) + "/remove", origin: forgeOrigin, cookie: "tok"})
	assert.Equal(t, http.StatusNotFound, rec.Code)

	f.passkeys.EXPECT().Remove(mock.Anything, mock.Anything, []byte("yy")).Return(errors.New("kv down")).Once()
	rec = f.do(t, req{method: "POST", target: "/-/passkeys/" + base64.RawURLEncoding.EncodeToString([]byte("yy")) + "/remove", origin: forgeOrigin, cookie: "tok"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "kv down")
}

func TestPasskeyLoginPage(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	rec := f.do(t, req{method: "GET", target: "/-/login"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "strict-origin", rec.Header().Get("Referrer-Policy"))
	body := rec.Body.String()
	assert.Contains(t, body, "<title>sign in")
	assert.Contains(t, body, "Sign in with a passkey")
	assert.Contains(t, body, `data-passkey="login"`)
	assert.Contains(t, body, `data-begin="/-/passkeys/login/begin"`)
	assert.Contains(t, body, `data-finish="/-/passkeys/login/finish"`)
	assert.Contains(t, body, "ermite web login")
	assert.NotContains(t, body, "<script>", "no inline script")

	// A signed-in visitor goes home.
	rec = f.do(t, req{method: "GET", target: "/-/login", cookie: "tok"})
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/", rec.Header().Get("Location"))
}

// The header links: "Sign in" for a visitor, "Passkeys" for the owner.
func TestPasskeyHeaderLinks(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	rec := f.do(t, req{method: "GET", target: "/-/login"})
	assert.Contains(t, rec.Body.String(), `href="/-/login"`)
	assert.NotContains(t, rec.Body.String(), `href="/-/passkeys"`)
	assert.Contains(t, rec.Body.String(), "passkey.", "the script is loaded")

	f.passkeys.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil).Once()
	rec = f.do(t, req{method: "GET", target: "/-/passkeys", cookie: "tok"})
	assert.Contains(t, rec.Body.String(), `href="/-/passkeys"`)
	assert.Contains(t, rec.Body.String(), `action="/-/logout"`)
	assert.NotContains(t, rec.Body.String(), `>Sign in</a>`)
}

// Without the passkey service there is no route, no link and no script
// behavior to reach: the routes answer like any unknown path.
func TestPasskeyRoutesAbsentWithoutTheService(t *testing.T) {
	t.Parallel()
	lim := &blockingLimiter{}
	srv := newTestServer(t, Config{
		Repos: reposFailing(t, errors.New("x")), Browse: mocks.NewBrowseService(t),
		PublicURL: forgeOrigin, Sessions: newSessions(t, "tok"), Limiter: lim,
	})
	do := func(method, target, cookie string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(finishBody))
		r.Header.Set("Origin", forgeOrigin)
		r.Header.Set("Content-Type", "application/json")
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: CookieName, Value: cookie})
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, r)
		return rec
	}
	for _, target := range []string{"/-/login", "/-/passkeys"} {
		assert.Equal(t, http.StatusNotFound, do("GET", target, "").Code, target)
		assert.Equal(t, http.StatusNotFound, do("GET", target, "tok").Code, target)
	}
	// A POST to a path without a route gets what any unknown path gets for a
	// POST (the catch-all page route is GET only).
	unknown := do("POST", "/-/nothing-here", "tok").Code
	assert.Equal(t, http.StatusMethodNotAllowed, unknown)
	for _, target := range []string{beginLogin, finishLogin} {
		assert.Equal(t, http.StatusForbidden, do("POST", target, "").Code, target+": the gate still refuses an anonymous POST")
		assert.Equal(t, unknown, do("POST", target, "tok").Code, target)
	}
	for _, target := range []string{beginRegister, finishRegister, "/-/passkeys/AQ/remove"} {
		assert.Equal(t, unknown, do("POST", target, "tok").Code, target)
	}
	// And no link in the header.
	rec := do("GET", "/", "")
	assert.NotContains(t, rec.Body.String(), `href="/-/login"`)
	rec = do("GET", "/", "tok")
	assert.NotContains(t, rec.Body.String(), `href="/-/passkeys"`)
}

// Passkeys need sessions: with the service alone nothing is served.
func TestPasskeyRoutesNeedSessions(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, Config{
		Repos: reposFailing(t, errors.New("x")), Browse: mocks.NewBrowseService(t),
		PublicURL: forgeOrigin, Passkeys: mocks.NewWebPasskeyService(t),
	})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/-/login", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	r := httptest.NewRequest("POST", beginLogin, nil)
	r.Header.Set("Origin", forgeOrigin)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// The existing login links keep working next to the passkey routes.
func TestPasskeyRoutesLeaveLoginLinksAlone(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	rec := f.do(t, req{method: "GET", target: "/-/login/" + nonceA})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "sign in as the owner")
	rec = f.do(t, req{method: "POST", target: "/-/login/" + nonceA, origin: forgeOrigin})
	assert.Equal(t, http.StatusSeeOther, rec.Code)
}

func TestIsPasskeyLoginPath(t *testing.T) {
	t.Parallel()
	assert.True(t, isPasskeyLoginPath("/-/passkeys/login/begin"))
	assert.True(t, isPasskeyLoginPath("/-/passkeys/login/finish"))
	for _, p := range []string{"", "/-/passkeys/login", "/-/passkeys/login/begin/", "/-/passkeys/register/begin", "/-/passkeys/Login/begin", "/x/-/passkeys/login/begin"} {
		assert.False(t, isPasskeyLoginPath(p), p)
	}
}

// GO-005: a blocked client gets 429 whatever it sends, and a malformed finish
// is a 4xx that does not count as a failed authentication.
func TestPasskeyLoginFinishBlockedBeforeBodyChecks(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	f.lim.blocked = true
	for _, r := range []req{
		{method: "POST", target: finishLogin, origin: forgeOrigin, contentType: "text/plain", body: "x"},
		postJSON(finishLogin, "", `not json`),
		postJSON(finishLogin, "", strings.Repeat("a", 64<<10+1)),
	} {
		rec := f.do(t, r)
		assert.Equal(t, http.StatusTooManyRequests, rec.Code, "%q", r.contentType)
	}
	assert.Zero(t, f.lim.fails)

	f.lim.blocked = false
	for _, r := range []req{
		{method: "POST", target: finishLogin, origin: forgeOrigin, contentType: "text/plain", body: "x"},
		postJSON(finishLogin, "", `not json`),
		postJSON(finishLogin, "", `{}`),
	} {
		rec := f.do(t, r)
		assert.GreaterOrEqual(t, rec.Code, 400)
		assert.Less(t, rec.Code, 500)
	}
	assert.Zero(t, f.lim.fails, "a malformed request is not a failed sign-in")
}

// The client key the web hands to the use case is the one the limiter uses.
func TestPasskeyLoginBeginPassesTheClientAddress(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	f.passkeys.EXPECT().BeginLogin(mock.Anything, "192.0.2.77").Return("c", []byte(`{}`), nil).Once()
	r := httptest.NewRequest("POST", beginLogin, nil)
	r.RemoteAddr = "192.0.2.77:51000"
	r.Header.Set("Origin", forgeOrigin)
	r.Header.Set("X-Forwarded-For", "203.0.113.5") // no trusted proxy: ignored
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, r)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TS-001: the name travels with register/begin and is checked there.
func TestPasskeyRegisterBeginChecksTheName(t *testing.T) {
	t.Parallel()
	f := newPasskeyServer(t)
	f.passkeys.EXPECT().BeginRegistration(mock.Anything, domain.Owner(), "Pixel 9").Return("cer", []byte(`{"publicKey":{}}`), nil).Once()
	rec := f.do(t, postJSON(beginRegister, "tok", `{"name":"Pixel 9"}`))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	f.passkeys.EXPECT().BeginRegistration(mock.Anything, domain.Owner(), "bad\u0000name").
		Return("", nil, &domain.TextError{Reason: "contains the control character U+0000"}).Once()
	rec = f.do(t, postJSON(beginRegister, "tok", `{"name":"bad\u0000name"}`))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "control character")

	// No body at all is no name; a body that is not JSON, or not the right type, is refused.
	f.passkeys.EXPECT().BeginRegistration(mock.Anything, domain.Owner(), "").Return("cer", []byte(`{}`), nil).Once()
	assert.Equal(t, http.StatusOK, f.do(t, postJSON(beginRegister, "tok", ``)).Code)
	assert.Equal(t, http.StatusBadRequest, f.do(t, postJSON(beginRegister, "tok", `nope`)).Code)
	assert.Equal(t, http.StatusUnsupportedMediaType, f.do(t, req{method: "POST", target: beginRegister, origin: forgeOrigin, cookie: "tok"}).Code)
}
