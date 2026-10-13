// SPDX-License-Identifier: AGPL-3.0-or-later

// Package githubapi serves the subset of GitHub's REST API that the actions
// running on ermite call under github.api_url (/-/github/api, ADR 0025 CI-2):
//
//	GET /repos/{owner}/{repo}
//	GET /repos/{owner}/{repo}/hash-algorithm
//	GET /repos/{owner}/{repo}/tarball[/{ref}]
//
// Everything else under the prefix is GitHub's 404 JSON, so that an action
// calling an endpoint of a feature ermite lacks fails as it does on a GitHub
// instance without it, never silently succeeds. The auth is the forge's: a
// token (stored, or the repository token of a job) as the password of Basic with
// any user, or as "Bearer" or "token" credentials; no credentials is anonymous,
// which reads public repositories.
package githubapi

import (
	"context"
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

// Prefix is where the API is mounted.
const Prefix = "/-/github/api"

// Patterns are the ServeMux patterns the handler serves: its whole prefix.
var Patterns = []string{Prefix + "/"}

// notFoundBody is GitHub's body for an unknown endpoint or repository.
const notFoundBody = `{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`

// DefaultMaxArchives bounds the archives written at once.
const DefaultMaxArchives = 8

// Config configures a Handler.
type Config struct {
	Auth ports.AuthService
	API  ports.GitHubAPIService
	// TrustedProxies are the networks allowed to set X-Forwarded-For.
	TrustedProxies []netip.Prefix
	// Limiter counts failed authentications; nil builds a default one.
	Limiter ports.FailureLimiter
	// MaxArchives bounds the archives written at once (each runs a git
	// process); zero means DefaultMaxArchives.
	MaxArchives int
}

// Handler serves the REST subset.
type Handler struct {
	cfg      Config
	limiter  ports.FailureLimiter
	archives chan struct{}
}

// New builds a Handler.
func New(cfg Config) *Handler {
	h := &Handler{cfg: cfg, limiter: cfg.Limiter}
	if h.limiter == nil {
		h.limiter = clientlimit.NewLimiter(nil)
	}
	n := cfg.MaxArchives
	if n <= 0 {
		n = DefaultMaxArchives
	}
	h.archives = make(chan struct{}, n)
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	rest := strings.TrimPrefix(r.URL.Path, Prefix)
	kind, owner, repo, ref, ok := route(rest)
	if !ok {
		notFound(w)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		jsonError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}

	ctx := r.Context()
	addr := clientlimit.ClientAddr(r, h.cfg.TrustedProxies)
	if blocked, _ := h.limiter.Blocked(ctx, addr); blocked {
		jsonError(w, http.StatusTooManyRequests, "API rate limit exceeded")
		return
	}
	p, err := h.authenticate(r, addr)
	if err != nil {
		if errors.Is(err, domain.ErrUnauthenticated) {
			jsonError(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		log := zerowrap.FromCtx(ctx)
		log.Error().Err(err).Msg("authenticate github api request")
		jsonError(w, http.StatusInternalServerError, "Server Error")
		return
	}

	switch kind {
	case "repo":
		body, err := h.cfg.API.GetRepo(ctx, p, owner, repo)
		if err != nil {
			h.fail(ctx, w, err)
			return
		}
		writeJSON(w, r, body)
	case "hash":
		alg, err := h.cfg.API.HashAlgorithm(ctx, p, owner, repo)
		if err != nil {
			h.fail(ctx, w, err)
			return
		}
		writeJSON(w, r, []byte(`{"hash_algorithm":`+strconv.Quote(alg)+`}`))
	case "tarball":
		h.tarball(w, r, p, owner, repo, ref)
	}
}

// route recognises the three endpoints. ref is "" for /tarball.
func route(rest string) (kind, owner, repo, ref string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	if len(parts) < 3 || parts[0] != "repos" || parts[1] == "" || parts[2] == "" {
		return "", "", "", "", false
	}
	owner, repo = parts[1], parts[2]
	switch {
	case len(parts) == 3:
		return "repo", owner, repo, "", true
	case len(parts) == 4 && parts[3] == "hash-algorithm":
		return "hash", owner, repo, "", true
	case len(parts) >= 4 && parts[3] == "tarball":
		// The ref may hold slashes (feature/x): everything after tarball/.
		return "tarball", owner, repo, strings.Join(parts[4:], "/"), true
	}
	return "", "", "", "", false
}

// tarball streams the archive. It is not a 302 to a codeload: ermite serves the
// bytes itself, which the runner follows without a second credential.
func (h *Handler) tarball(w http.ResponseWriter, r *http.Request, p domain.Principal, owner, repo, ref string) {
	ctx := r.Context()
	select {
	case h.archives <- struct{}{}:
		defer func() { <-h.archives }()
	default:
		w.Header().Set("Retry-After", "5")
		jsonError(w, http.StatusServiceUnavailable, "Server busy")
		return
	}
	if r.Method == http.MethodHead {
		// Resolve without writing: a HEAD is answered with the headers only.
		err := h.cfg.API.Tarball(ctx, p, owner, repo, ref, func(name string) io.Writer {
			setArchiveHeaders(w, name)
			w.WriteHeader(http.StatusOK)
			return io.Discard
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			h.fail(ctx, w, err)
		}
		return
	}
	started := false
	err := h.cfg.API.Tarball(ctx, p, owner, repo, ref, func(name string) io.Writer {
		started = true
		setArchiveHeaders(w, name)
		return w
	})
	switch {
	case err == nil:
	case started:
		// The status is out: the client sees a truncated archive. Log it.
		log := zerowrap.FromCtx(ctx)
		log.Warn().Err(err).Msg("github api archive cut short")
	default:
		h.fail(ctx, w, err)
	}
}

func setArchiveHeaders(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "application/x-gzip")
	w.Header().Set("Content-Disposition", "attachment; filename="+name)
}

// authenticate reads the credentials of the request. None is the anonymous
// principal; credentials that are not valid count as a failure of the client
// address.
func (h *Handler) authenticate(r *http.Request, addr string) (domain.Principal, error) {
	secret, present := credentials(r)
	if !present {
		return domain.Anonymous(), nil
	}
	p, err := h.cfg.Auth.Authenticate(r.Context(), secret)
	if err != nil {
		if errors.Is(err, domain.ErrUnauthenticated) {
			h.limiter.Fail(r.Context(), addr)
		}
		return domain.Anonymous(), err
	}
	return p, nil
}

// credentials extracts the token of an Authorization header: the password of
// Basic (the user is ignored, checkout sends x-access-token), or the value of
// "Bearer" or "token".
func credentials(r *http.Request) (secret string, present bool) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", false
	}
	if _, pass, ok := r.BasicAuth(); ok {
		return pass, true
	}
	if scheme, tok, ok := strings.Cut(auth, " "); ok && (strings.EqualFold(scheme, "Bearer") || strings.EqualFold(scheme, "token")) {
		return strings.TrimSpace(tok), true
	}
	return "", true
}

// fail answers an error of a use case. A repository the caller cannot read is
// as not found as one that does not exist.
func (h *Handler) fail(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound), domain.IsInvalid(err):
		notFound(w)
	case errors.Is(err, domain.ErrResourceExhausted):
		w.Header().Set("Retry-After", "5")
		jsonError(w, http.StatusServiceUnavailable, "Server busy")
	case errors.Is(err, context.Canceled):
	default:
		log := zerowrap.FromCtx(ctx)
		log.Error().Err(err).Msg("github api request failed")
		jsonError(w, http.StatusInternalServerError, "Server Error")
	}
}

func notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, notFoundBody)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"message":`+strconv.Quote(msg)+`,"documentation_url":"https://docs.github.com/rest"}`)
}

func writeJSON(w http.ResponseWriter, r *http.Request, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}
