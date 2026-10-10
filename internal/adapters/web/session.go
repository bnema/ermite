// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/clientlimit"
)

// The owner's session on the web (ADR 0018).
//
// A browser signs in by opening the one-time link `ermite web login` printed:
// GET /-/login/<nonce> shows a confirmation and consumes nothing, so link
// previews and prefetchers cannot use it; POST /-/login/<nonce> consumes the
// link and sets the cookie. The cookie holds a signed token; every request
// that carries a valid one runs as the owner, every other as anonymous.
//
// State changes are POST only and pass one gate (ServeHTTP): an Origin equal to
// the origin of the public URL, and a valid session, except the login POST,
// which needs the link instead. The routes read the cookie and never the
// Authorization header; the API does the opposite.

// CookieName is the session cookie. The __Host- prefix makes browsers accept
// it only with Secure, Path=/ and no Domain, and refuse it from a sibling
// subdomain.
const CookieName = "__Host-ermite"

// loginPrefix is the path of login links, up to the nonce.
const loginPrefix = "/-/login/"

// ErrLinkRejected is what SessionAuth.Login returns for a link that is
// unknown, expired or already used.
var ErrLinkRejected = errors.New("login link rejected")

// SessionAuth is the session side of the use cases, as the web needs it.
type SessionAuth interface {
	// LoginLinkValid reports whether the link is pending, without using it.
	LoginLinkValid(nonce string) bool
	// Login uses the link and returns the session token and how long it
	// lasts, or ErrLinkRejected.
	Login(ctx context.Context, nonce string) (token string, lifetime time.Duration, err error)
	// Verify checks a session token. When it is valid it returns the context
	// to serve the request with, which carries the owner as the caller of the
	// API; when it is not, ok is false and err nil. An error means the check
	// could not be made (the request then runs as anonymous).
	Verify(ctx context.Context, token string) (owner context.Context, ok bool, err error)
}

// FailureLimiter counts failed logins per client, with the failed
// authentications of the other routes. *clientlimit.Limiter fits.
type FailureLimiter interface {
	Blocked(ctx context.Context, addr string) (bool, time.Duration)
	Fail(ctx context.Context, addr string)
}

type signedInKey struct{}

// SignedIn reports whether the request carries a valid owner session. The
// page is then private: it was rendered for the owner and must not be cached
// by anyone else. Handlers of POST routes can rely on it: the gate refused
// the request otherwise.
func SignedIn(ctx context.Context) bool {
	_, ok := ctx.Value(signedInKey{}).(struct{})
	return ok
}

// sessionCache is the Cache-Control of a page rendered for the owner.
const sessionCache = "private, no-store"

// cacheHeaders sets Cache-Control: public for a visitor, and for the owner
// the private variant with Vary: Cookie.
func cacheHeaders(ctx context.Context, h http.Header, public, private string) {
	if SignedIn(ctx) {
		h.Set("Cache-Control", private)
		h.Add("Vary", "Cookie")
		return
	}
	h.Set("Cache-Control", public)
}

// logPath is the path of a request as the log may show it: a login link is
// a secret. The decoded path is given, so the check is wide on purpose: a
// path that only reaches the link after cleaning ("/x/../-/login/…",
// "//-/login/…") or that holds the prefix further in is cut too.
func logPath(p string) string {
	if strings.HasPrefix(path.Clean(p), loginPrefix) || strings.Contains(p, loginPrefix) {
		return loginPrefix + "…"
	}
	return p
}

// withSession resolves the session of the request: the cookie, when valid,
// makes the request the owner's. A cookie that is not valid is cleared. A
// request without a cookie is untouched.
func (s *Server) withSession(w http.ResponseWriter, r *http.Request) *http.Request {
	if s.cfg.Sessions == nil || strings.HasPrefix(r.URL.EscapedPath(), StaticPrefix) {
		return r
	}
	c, err := r.Cookie(CookieName)
	if err != nil {
		return r
	}
	ctx := r.Context()
	owner, ok, err := s.cfg.Sessions.Verify(ctx, c.Value)
	switch {
	case err != nil:
		// Fail closed, and keep the cookie: the failure may be passing.
		log := zerowrap.FromCtx(ctx)
		log.Error().Err(err).Msg("check web session")
		return r
	case !ok:
		http.SetCookie(w, s.cookie("", -1))
		return r
	}
	return r.WithContext(context.WithValue(owner, signedInKey{}, struct{}{}))
}

// sessionStillValid checks again the cookie a long-lived request started
// with. It fails closed: a check that cannot be made ends the session of the
// stream.
func (s *Server) sessionStillValid(ctx context.Context, r *http.Request) bool {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return false
	}
	_, ok, err := s.cfg.Sessions.Verify(ctx, c.Value)
	return err == nil && ok
}

// cookie builds the session cookie. maxAge < 0 deletes it.
func (s *Server) cookie(token string, maxAge int) *http.Cookie {
	// SameSite=Strict, as ADR 0018 says: the cookie is not sent when another
	// site links to a page (that first page shows as signed out, the next
	// navigation on the site is signed in), and not on any cross-site request,
	// on top of the Origin check of the POST gate.
	return &http.Cookie{
		Name: CookieName, Value: token, Path: "/", MaxAge: maxAge,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	}
}

// gate refuses a state change that does not come from a page of this site
// with a session. It answers 403 and returns false. GET and HEAD pass: they
// never change anything.
func (s *Server) gate(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	// The Origin is compared with the configured public URL, never with the
	// Host header, which the client chooses. A missing header, "null" and any
	// other origin are refused.
	if r.Header.Get("Origin") != s.origin {
		denyForbidden(w)
		return false
	}
	if !SignedIn(r.Context()) && (r.Method != http.MethodPost || !isLoginPath(r.URL)) {
		denyForbidden(w)
		return false
	}
	return true
}

// isLoginPath tells whether u is exactly a login link: loginPrefix, a nonce of
// the right shape and nothing after it. It looks at the escaped path, which is
// what the router matches: the decoded path would let "/-%2Flogin%2Fx/..." pass
// for a link while the router sends it to another route.
func isLoginPath(u *url.URL) bool {
	nonce, ok := strings.CutPrefix(u.EscapedPath(), loginPrefix)
	return ok && validNonce(nonce)
}

func denyForbidden(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "forbidden", http.StatusForbidden)
}

// validNonce tells whether s has the shape of a login link secret, so that
// anything else is refused before it reaches the store.
func validNonce(s string) bool {
	if len(s) != nonceLen {
		return false
	}
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// nonceLen is the length of the secret of a login link: 32 bytes in unpadded
// base64url.
const nonceLen = 43

// secretPage writes a page of the login flow: never cached, and its address,
// which holds the link, is never sent along: strict-origin sends only the
// origin. no-referrer would also turn the Origin of the sign-in POST into
// "null" in browsers, which the POST gate refuses.
func (s *Server) secretPage(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "strict-origin")
	s.write(r.Context(), w, r, status, c)
}

// loginConfirm is GET /-/login/<nonce>: it shows the confirmation and does not
// use the link.
func (s *Server) loginConfirm(w http.ResponseWriter, r *http.Request) {
	nonce := r.PathValue("nonce")
	if !validNonce(nonce) || !s.cfg.Sessions.LoginLinkValid(nonce) {
		s.secretPage(w, r, http.StatusNotFound, s.loginRejectedPage())
		return
	}
	l := s.stateLayout("sign in", "sign in · confirm", []Crumb{{Label: "-"}, {Label: "sign in"}})
	s.secretPage(w, r, http.StatusOK, Login(l, LoginPage{
		Title:  "sign in as the owner",
		Lines:  []string{"this browser will be signed in for 12 hours.", "only confirm if you ran `ermite web login` just now."},
		Action: loginPrefix + nonce,
		Button: "[sign in]",
	}))
}

// loginPost is POST /-/login/<nonce>: it uses the link, sets the cookie and
// goes home. A link that does not work answers the same page whatever the
// reason, and counts as a failed authentication of the client.
func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	addr := clientlimit.ClientAddr(r, s.cfg.TrustedProxies)
	if blocked, wait := s.cfg.Limiter.Blocked(ctx, addr); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		s.secretPage(w, r, http.StatusTooManyRequests, s.stateOnly("sign in", "429", "too many attempts",
			"too many failed sign-in attempts from this address.", "try again later."))
		return
	}
	nonce := r.PathValue("nonce")
	var token string
	var lifetime time.Duration
	err := ErrLinkRejected
	if validNonce(nonce) {
		token, lifetime, err = s.cfg.Sessions.Login(ctx, nonce)
	}
	switch {
	case errors.Is(err, ErrLinkRejected):
		s.cfg.Limiter.Fail(ctx, addr)
		s.secretPage(w, r, http.StatusNotFound, s.loginRejectedPage())
		return
	case err != nil:
		id := requestID()
		log := zerowrap.FromCtx(ctx)
		log.Error().Err(err).Str("request_id", id).Msg("web login failed")
		s.secretPage(w, r, http.StatusInternalServerError, s.errorPage(id))
		return
	}
	h := w.Header()
	h.Del("Set-Cookie") // a stale cookie cleared by withSession
	http.SetCookie(w, s.cookie(token, int(lifetime.Seconds())))
	h.Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// logout is POST /-/logout: it ends the session of this browser. The token
// itself stays valid until it expires, as a stateless token must; `ermite web
// logout --all` ends them all.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	w.Header().Del("Set-Cookie")
	http.SetCookie(w, s.cookie("", -1))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// stateOnly is a state page with a title and two lines, for the login flow.
func (s *Server) stateOnly(crumb, code, title string, lines ...string) templ.Component {
	l := s.stateLayout(title, code+" · "+title, []Crumb{{Label: "-"}, {Label: crumb}})
	return State(l, StatePage{Code: code, Title: title, Lines: lines, Back: []Link{{Label: "home", Href: "/"}}})
}

// loginRejectedPage is the answer for a link that is unknown, expired or used.
func (s *Server) loginRejectedPage() templ.Component {
	return s.stateOnly("sign in", "404", "login link not valid",
		"this link has expired, was already used, or never existed.", "run `ermite web login` for a new one.")
}
