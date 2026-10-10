// SPDX-License-Identifier: AGPL-3.0-or-later

// Package githttp serves Git over smart HTTP: it routes, authenticates and
// authorizes a request, then lets `git http-backend` answer it through
// net/http/cgi. It also hosts the Unix-socket server that the push hooks call
// back into.
package githttp

import (
	"context"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/cgi"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Patterns are the ServeMux patterns the handler serves. Everything else
// under /<name>/ (dumb HTTP, HEAD, objects/...) is a 404 from the mux.
var Patterns = []string{
	"GET /{repo}/info/refs",
	"POST /{repo}/git-upload-pack",
	"POST /{repo}/git-receive-pack",
}

// DefaultMaxBodyBytes bounds a request body (a push) when Config.MaxBodyBytes
// is zero.
const DefaultMaxBodyBytes int64 = 4 << 30

// Names of the environment variables that carry the hook callback settings
// from ermite, through git, to the hook process.
const (
	// EnvHookSocket is the path of the hook Unix socket.
	EnvHookSocket = "ERMITE_HOOK_SOCKET"
	// EnvHookToken is the per-request hook token.
	EnvHookToken = "ERMITE_HOOK_TOKEN"
)

const (
	serviceUpload  = "git-upload-pack"
	serviceReceive = "git-receive-pack"
)

// Authenticator turns the secret of a request into a principal. It returns
// domain.ErrUnauthenticated for a secret that is not valid. app.Auth fits.
type Authenticator interface {
	Authenticate(ctx context.Context, secret string) (domain.Principal, error)
}

// Repos is the read side of the repository use cases. A repository the
// principal may not read is domain.ErrNotFound. app.Repos fits.
type Repos interface {
	Get(ctx context.Context, p domain.Principal, name domain.RepoName) (domain.Repo, error)
}

// Pushes opens a repository for a push: it is domain.ErrUnauthenticated for an
// anonymous caller, domain.ErrNotFound for a repository the caller may not
// write and cannot see (or that is missing and may not be created),
// domain.ErrForbidden for one it can see but not write. With create, a missing
// repository the principal may write is created. app.Pushes fits.
type Pushes interface {
	Open(ctx context.Context, p domain.Principal, name domain.RepoName, create bool) (repo domain.Repo, created bool, err error)
}

// HookTokens hands out the per-request tokens the hooks of a push present
// back to ermite, and the socket they present them on.
type HookTokens interface {
	// Register binds a new token to the repository and principal of a push.
	// release revokes it and must be called when the request ends.
	Register(repo domain.RepoName, p domain.Principal) (token string, release func())
	// Socket is the path of the hook socket.
	Socket() string
}

// Config configures a Handler.
type Config struct {
	Auth    Authenticator
	Repos   Repos
	Pushes  Pushes
	Backend ports.GitBackend
	// Hooks provides hook tokens for pushes. Nil runs pushes without hook
	// environment (tests of the handler alone).
	Hooks HookTokens
	// TrustedProxies are the networks allowed to set X-Forwarded-For.
	TrustedProxies []netip.Prefix
	// Limiter counts failed authentications; nil builds a default one.
	Limiter *Limiter
	// TempDir holds request bodies that arrive chunked, which CGI cannot
	// stream. Empty uses the system temporary directory.
	TempDir string
	// MaxBodyBytes bounds a request body; 0 means DefaultMaxBodyBytes.
	MaxBodyBytes int64
}

// Handler serves the smart HTTP routes of Patterns.
type Handler struct {
	cfg     Config
	limiter *Limiter
	maxBody int64
}

// New builds a Handler.
func New(cfg Config) *Handler {
	h := &Handler{cfg: cfg, limiter: cfg.Limiter, maxBody: cfg.MaxBodyBytes}
	if h.limiter == nil {
		h.limiter = NewLimiter(nil)
	}
	if h.maxBody <= 0 {
		h.maxBody = DefaultMaxBodyBytes
	}
	return h
}

// route is a parsed Git request.
type route struct {
	repo    string // the path segment, with any .git suffix
	suffix  string // "info/refs", "git-upload-pack" or "git-receive-pack"
	service string // serviceUpload or serviceReceive
}

func (rt route) push() bool { return rt.service == serviceReceive }

// parseRoute recognizes /<repo>/info/refs?service=..., /<repo>/git-upload-pack
// and /<repo>/git-receive-pack. info/refs without a service is the dumb
// protocol, which ermite does not serve.
func parseRoute(r *http.Request) (route, int) {
	seg, rest, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if !ok || seg == "" {
		return route{}, http.StatusNotFound
	}
	rt := route{repo: seg, suffix: rest}
	switch rest {
	case "info/refs":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			return route{}, http.StatusMethodNotAllowed
		}
		switch svc := r.URL.Query().Get("service"); svc {
		case serviceUpload, serviceReceive:
			rt.service = svc
		default:
			return route{}, http.StatusNotFound
		}
	case serviceUpload, serviceReceive:
		if r.Method != http.MethodPost {
			return route{}, http.StatusMethodNotAllowed
		}
		rt.service = rest
	default:
		return route{}, http.StatusNotFound
	}
	return rt, 0
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &recorder{ResponseWriter: w}
	var rt route
	var name domain.RepoName
	var principal domain.Principal
	defer func() {
		log := zerowrap.FromCtx(r.Context())
		log.Debug().
			Str("repo", name.String()).
			Str("service", rt.service).
			Int("status", rec.code()).
			Dur("duration", time.Since(start)).
			Str("principal", principal.Label()).
			Str("token_id", principal.TokenID()).
			Msg("git request")
	}()
	w = rec

	rt, status := parseRoute(r)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	name, err := domain.ParseRepoPath(rt.repo)
	if err != nil {
		notFound(w)
		return
	}

	ctx := r.Context()
	log := zerowrap.FromCtx(ctx)
	addr := ClientAddr(r, h.cfg.TrustedProxies)
	if blocked, wait := h.limiter.Blocked(addr); blocked {
		log.Warn().Str("client", addr).Msg("too many failed authentications, request blocked")
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		http.Error(w, "too many failed authentications", http.StatusTooManyRequests)
		return
	}

	principal, err = h.authenticate(r, addr)
	if err != nil {
		if errors.Is(err, domain.ErrUnauthenticated) {
			challenge(w)
			return
		}
		log.Error().Err(err).Msg("authenticate git request")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if rt.push() {
		// Only the first request of a push may create the repository.
		_, _, err = h.cfg.Pushes.Open(ctx, principal, name, rt.suffix == "info/refs")
	} else {
		_, err = h.cfg.Repos.Get(ctx, principal, name)
	}
	if err != nil {
		h.denied(r.Context(), w, principal, err)
		return
	}

	h.serveBackend(w, r, rt, name, principal)
}

// authenticate reads the credentials of the request. No credentials is the
// anonymous principal; credentials that are not valid count as a failure of
// the client address and give domain.ErrUnauthenticated.
func (h *Handler) authenticate(r *http.Request, addr string) (domain.Principal, error) {
	secret, present := credentials(r)
	if !present {
		return domain.Anonymous(), nil
	}
	p, err := h.cfg.Auth.Authenticate(r.Context(), secret)
	if err != nil {
		if errors.Is(err, domain.ErrUnauthenticated) {
			h.limiter.Fail(addr)
			log := zerowrap.FromCtx(r.Context())
			log.Debug().Str("client", addr).Msg("invalid credentials")
		}
		return domain.Anonymous(), err
	}
	return p, nil
}

// credentials extracts the token from an Authorization header: the password of
// Basic (the username is ignored) or a Bearer token. present is true when the
// header exists, even if it carries nothing usable, which then fails
// authentication.
func credentials(r *http.Request) (secret string, present bool) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", false
	}
	if _, pass, ok := r.BasicAuth(); ok {
		return pass, true
	}
	if scheme, tok, ok := strings.Cut(auth, " "); ok && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(tok), true
	}
	return "", true
}

// denied answers an authorization failure. A caller without valid
// credentials is asked for them wherever the repository does not show itself,
// which keeps a private repository and a missing one indistinguishable.
func (h *Handler) denied(ctx context.Context, w http.ResponseWriter, p domain.Principal, err error) {
	switch {
	case errors.Is(err, domain.ErrUnauthenticated):
		challenge(w)
	case errors.Is(err, domain.ErrNotFound):
		if p.IsAnonymous() {
			challenge(w)
			return
		}
		notFound(w)
	case errors.Is(err, domain.ErrForbidden):
		http.Error(w, "forbidden", http.StatusForbidden)
	default:
		log := zerowrap.FromCtx(ctx)
		log.Error().Err(err).Msg("authorize git request")
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="ermite"`)
	http.Error(w, "authentication required", http.StatusUnauthorized)
}

func notFound(w http.ResponseWriter) { http.Error(w, "not found", http.StatusNotFound) }

// recorder remembers the status code of a response and keeps Flush working.
type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *recorder) code() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// Unwrap lets http.ResponseController reach Flush.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// sensitiveHeaders never reach the Git process: CGI would put them in its
// environment, which the hooks inherit.
var sensitiveHeaders = []string{"Authorization", "Proxy-Authorization", "Cookie"}

// serveBackend runs git http-backend for an authorized request.
func (h *Handler) serveBackend(w http.ResponseWriter, r *http.Request, rt route, name domain.RepoName, p domain.Principal) {
	ctx := r.Context()
	logger := zerowrap.FromCtx(ctx)

	be, err := h.cfg.Backend.HTTPBackend(ctx, rt.push())
	if err != nil {
		logger.Error().Err(err).Msg("prepare git http-backend")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	env := be.Env
	if rt.suffix == serviceReceive && h.cfg.Hooks != nil {
		token, release := h.cfg.Hooks.Register(name, p)
		defer release()
		env = append(env, EnvHookSocket+"="+h.cfg.Hooks.Socket(), EnvHookToken+"="+token)
	}

	req, cleanup, status := h.backendRequest(w, r, rt, name)
	defer cleanup()
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}

	(&cgi.Handler{
		Path:   be.Path,
		Dir:    be.Dir,
		Env:    env,
		Stderr: logWriter{ctx: ctx, repo: name.String()},
		Logger: log.New(logWriter{ctx: ctx, repo: name.String(), warn: true}, "", 0),
	}).ServeHTTP(w, req)
}

// backendRequest builds the request handed to CGI: the path names the
// directory of the repository, credentials are removed, and a body of unknown
// length (chunked, which CGI refuses) is spooled to a temporary file so that
// its length is known. status is non-zero when the request must be refused;
// cleanup is always safe to call.
func (h *Handler) backendRequest(w http.ResponseWriter, r *http.Request, rt route, name domain.RepoName) (req *http.Request, cleanup func(), status int) {
	cleanup = func() {}
	req = r.Clone(r.Context())
	req.URL.Path = "/" + name.String() + ".git/" + rt.suffix
	req.URL.RawPath = ""
	for _, k := range sensitiveHeaders {
		req.Header.Del(k)
	}
	if r.ContentLength > h.maxBody {
		return nil, cleanup, http.StatusRequestEntityTooLarge
	}
	if r.ContentLength >= 0 && len(r.TransferEncoding) == 0 {
		req.Body = http.MaxBytesReader(w, r.Body, h.maxBody)
		return req, cleanup, 0
	}

	f, err := os.CreateTemp(h.cfg.TempDir, "body-*")
	if err != nil {
		logger := zerowrap.FromCtx(r.Context())
		logger.Error().Err(err).Msg("spool request body")
		return nil, cleanup, http.StatusInternalServerError
	}
	cleanup = func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, h.maxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, cleanup, http.StatusRequestEntityTooLarge
		}
		return nil, cleanup, http.StatusBadRequest
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		logger := zerowrap.FromCtx(r.Context())
		logger.Error().Err(err).Msg("spool request body")
		return nil, cleanup, http.StatusInternalServerError
	}
	req.Body = f
	req.ContentLength = n
	req.TransferEncoding = nil
	return req, cleanup, 0
}

// logWriter sends what the Git process (or the CGI layer) writes to its
// standard error to the logger of the request.
type logWriter struct {
	ctx  context.Context
	repo string
	warn bool
}

func (l logWriter) Write(b []byte) (int, error) {
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		return len(b), nil
	}
	logger := zerowrap.FromCtx(l.ctx)
	if l.warn {
		logger.Warn().Str("repo", l.repo).Msg(msg)
	} else {
		logger.Debug().Str("repo", l.repo).Str("source", "git").Msg(msg)
	}
	return len(b), nil
}
