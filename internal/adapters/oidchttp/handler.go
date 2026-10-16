// SPDX-License-Identifier: AGPL-3.0-or-later

// Package oidchttp serves the OIDC provider of CI jobs under /-/oidc, the
// issuer (CI-4, D5):
//
//	GET /-/oidc/.well-known/openid-configuration   discovery, anonymous
//	GET /-/oidc/.well-known/jwks                   public keys, anonymous
//	GET /-/oidc/token?api-version=2.0[&audience=…] {"value":"<jwt>"}
//
// The token endpoint is the ACTIONS_ID_TOKEN_REQUEST_URL of a job, called by
// @actions/core's getIDToken with "Authorization: Bearer <the job's request
// token>". Everything is 404 when the server issues no ID tokens.
package oidchttp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/clientlimit"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Prefix is where the provider is mounted: the path of the issuer.
const Prefix = "/-/oidc"

// Patterns are the ServeMux patterns the handler serves: its whole prefix.
var Patterns = []string{Prefix + "/"}

// How long clients may cache the public documents. The JWKS is short: after a
// rotation the new key signs at once, and relying parties that do not refetch
// on an unknown kid see it within this delay.
const (
	discoveryMaxAge = "public, max-age=3600"
	jwksMaxAge      = "public, max-age=300"
)

// Config configures a Handler.
type Config struct {
	OIDC ports.OIDCService
	// TrustedProxies are the networks allowed to set X-Forwarded-For.
	TrustedProxies []netip.Prefix
	// Limiter counts failed authentications; nil builds a default one.
	Limiter ports.FailureLimiter
}

// Handler serves the provider.
type Handler struct {
	cfg     Config
	limiter ports.FailureLimiter
}

// New builds a Handler.
func New(cfg Config) *Handler {
	h := &Handler{cfg: cfg, limiter: cfg.Limiter}
	if h.limiter == nil {
		h.limiter = clientlimit.NewLimiter(nil)
	}
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	path := strings.TrimPrefix(r.URL.Path, Prefix)
	if !h.cfg.OIDC.OIDCEnabled() {
		w.Header().Set("Cache-Control", "no-store")
		jsonError(w, http.StatusNotFound, "Not Found")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		jsonError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	switch path {
	case "/.well-known/openid-configuration":
		h.public(w, r, discoveryMaxAge, h.cfg.OIDC.OIDCDiscovery)
	case "/.well-known/jwks":
		h.public(w, r, jwksMaxAge, h.cfg.OIDC.OIDCKeys)
	case "/token":
		h.token(w, r)
	default:
		w.Header().Set("Cache-Control", "no-store")
		jsonError(w, http.StatusNotFound, "Not Found")
	}
}

// public answers a document anyone may read and cache.
func (h *Handler) public(w http.ResponseWriter, r *http.Request, cache string, doc func() ([]byte, error)) {
	body, err := doc()
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", cache)
	writeJSON(w, r, body)
}

// token answers an ID token for the job whose request token is the bearer.
// api-version is accepted as GitHub's endpoint takes it, and not checked.
func (h *Handler) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()
	addr := clientlimit.ClientAddr(r, h.cfg.TrustedProxies)
	if blocked, _ := h.limiter.Blocked(ctx, addr); blocked {
		jsonError(w, http.StatusTooManyRequests, "Too many failed authentications")
		return
	}
	secret, ok := bearer(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ermite oidc"`)
		jsonError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	jwt, err := h.cfg.OIDC.IDToken(ctx, secret, r.URL.Query().Get("audience"))
	if err != nil {
		if errors.Is(err, domain.ErrUnauthenticated) {
			h.limiter.Fail(ctx, addr)
			w.Header().Set("WWW-Authenticate", `Bearer realm="ermite oidc"`)
			jsonError(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		h.fail(w, r, err)
		return
	}
	body, err := json.Marshal(map[string]string{"value": jwt})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, r, body)
}

// bearer extracts the token of "Authorization: Bearer <token>".
func bearer(r *http.Request) (string, bool) {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok = strings.TrimSpace(tok)
	return tok, tok != ""
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		jsonError(w, http.StatusNotFound, "Not Found")
	case domain.IsInvalid(err):
		jsonError(w, http.StatusBadRequest, err.Error())
	default:
		log := zerowrap.FromCtx(r.Context())
		log.Error().Err(err).Msg("oidc request failed")
		jsonError(w, http.StatusInternalServerError, "Server Error")
	}
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"message":`+strconv.Quote(msg)+`}`)
}

func writeJSON(w http.ResponseWriter, r *http.Request, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}
