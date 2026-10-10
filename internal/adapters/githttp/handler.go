// SPDX-License-Identifier: AGPL-3.0-or-later

// Package githttp serves Git over smart HTTP: it routes, authenticates and
// authorizes a request, then lets `git http-backend` answer it through
// net/http/cgi. It also hosts the Unix-socket server that the push hooks call
// back into.
package githttp

import (
	"context"
	"errors"
	"log"
	"math"
	"net/http"
	"net/http/cgi"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/clientlimit"
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

// uploadPackMaxBody bounds the body of git-upload-pack, which is only a list
// of wants and haves: git http-backend buffers it whole when its length is
// unknown, so it must stay small whatever Config.MaxBodyBytes says.
const uploadPackMaxBody int64 = 64 << 20

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
	Open(ctx context.Context, p domain.Principal, name domain.RepoName, create bool) error
}

// HookTokens hands out the per-request tokens the hooks of a push present
// back to ermite, and the socket they present them on.
type HookTokens interface {
	// Register binds a new token to the repository and principal of a push.
	// release revokes it and must be called when the request ends.
	Register(ctx context.Context, repo domain.RepoName, p domain.Principal) (token string, release func())
	// Socket is the path of the hook socket.
	Socket() string
}

// Config configures a Handler.
type Config struct {
	Auth    Authenticator
	Repos   Repos
	Pushes  Pushes
	Backend ports.GitBackend
	// Hooks provides hook tokens for pushes. It is required.
	Hooks HookTokens
	// TrustedProxies are the networks allowed to set X-Forwarded-For.
	TrustedProxies []netip.Prefix
	// Limiter counts failed authentications; nil builds a default one.
	Limiter *clientlimit.Limiter
	// MaxBodyBytes bounds the body of a push; 0 means DefaultMaxBodyBytes.
	// The body of a fetch is bounded by 64 MiB at most.
	MaxBodyBytes int64
}

// Handler serves the smart HTTP routes of Patterns.
type Handler struct {
	cfg     Config
	limiter *clientlimit.Limiter
	maxBody int64
}

// New builds a Handler.
func New(cfg Config) *Handler {
	h := &Handler{cfg: cfg, limiter: cfg.Limiter, maxBody: cfg.MaxBodyBytes}
	if h.limiter == nil {
		h.limiter = clientlimit.NewLimiter(nil)
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
		// Exactly one service value: a repeated parameter is ambiguous, and
		// what is handed to the backend is rebuilt from this one anyway.
		switch svcs := r.URL.Query()["service"]; {
		case len(svcs) != 1:
			return route{}, http.StatusNotFound
		case svcs[0] == serviceUpload, svcs[0] == serviceReceive:
			rt.service = svcs[0]
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
	var rt route
	var name domain.RepoName
	var principal domain.Principal
	defer func() {
		log := zerowrap.FromCtx(r.Context())
		log.Debug().
			Str("repo", name.String()).
			Str("service", rt.service).
			Str("principal", principal.Label()).
			Str("token_id", principal.TokenID()).
			Msg("git request")
	}()

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
	addr := clientlimit.ClientAddr(r, h.cfg.TrustedProxies)
	if blocked, wait := h.limiter.Blocked(ctx, addr); blocked {
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
		err = h.cfg.Pushes.Open(ctx, principal, name, rt.suffix == "info/refs")
	} else {
		_, err = h.cfg.Repos.Get(ctx, principal, name)
	}
	if err != nil {
		h.denied(ctx, w, principal, err)
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
			h.limiter.Fail(r.Context(), addr)
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

// gitProtocolRe is what the Git-Protocol header may carry: colon-separated
// key=value pairs such as "version=2". Git passes it on as GIT_PROTOCOL to
// its subprocesses, so anything else is dropped.
var gitProtocolRe = regexp.MustCompile(`^[a-z0-9._-]+=[a-z0-9._-]+(:[a-z0-9._-]+=[a-z0-9._-]+)*$`)

// maxGitProtocolLen bounds the Git-Protocol header.
const maxGitProtocolLen = 128

// inheritedEnv are the variables net/http/cgi copies from the ermite process
// on some platforms (osDefaultInheritEnv). Setting them empty keeps the
// environment of the process out of Git and its hooks.
var inheritedEnv = []string{
	"LD_LIBRARY_PATH=", "LD_LIBRARYN32_PATH=", "LD_LIBRARY64_PATH=",
	"LD_LIBRARY_PATH_32=", "LD_LIBRARY_PATH_64=", "DYLD_LIBRARY_PATH=", "SHLIB_PATH=",
}

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
	env := append(append([]string(nil), be.Env...), inheritedEnv...)
	if rt.suffix == serviceReceive {
		token, release := h.cfg.Hooks.Register(ctx, name, p)
		defer release()
		env = append(env, EnvHookSocket+"="+h.cfg.Hooks.Socket(), EnvHookToken+"="+token)
	}

	req, status := h.backendRequest(w, r, rt, name)
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

// bodyLimit is the largest request body accepted for the route.
func (h *Handler) bodyLimit(rt route) int64 {
	if rt.push() {
		return h.maxBody
	}
	return min(h.maxBody, uploadPackMaxBody)
}

// backendRequest builds the request handed to CGI. The path names the
// directory of the repository, the query is rebuilt from the validated
// service, and the headers are rebuilt from an allowlist: CGI turns every
// header into an environment variable of Git and of its hooks, so nothing
// else (credentials, cookies, forwarding headers, anything arbitrary) goes
// through. The body is streamed and cut off at the limit; a body of unknown
// length (chunked, which CGI refuses) is passed on with its length unknown.
// status is non-zero when the request must be refused.
func (h *Handler) backendRequest(w http.ResponseWriter, r *http.Request, rt route, name domain.RepoName) (req *http.Request, status int) {
	limit := h.bodyLimit(rt)
	if r.ContentLength > limit {
		return nil, http.StatusRequestEntityTooLarge
	}
	req = r.Clone(r.Context())
	req.URL.Path = "/" + name.String() + ".git/" + rt.suffix
	req.URL.RawPath = ""
	req.URL.RawQuery = ""
	if rt.suffix == "info/refs" {
		req.URL.RawQuery = "service=" + rt.service
	}

	req.Header = make(http.Header, 5)
	for _, k := range []string{"Content-Type", "Content-Encoding", "Accept", "User-Agent"} {
		if v := r.Header.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	if v := r.Header.Get("Git-Protocol"); v != "" && len(v) <= maxGitProtocolLen && gitProtocolRe.MatchString(v) {
		req.Header.Set("Git-Protocol", v)
	}

	req.Body = http.MaxBytesReader(w, r.Body, limit)
	if r.ContentLength < 0 || len(r.TransferEncoding) > 0 {
		req.ContentLength = -1
		req.TransferEncoding = nil
	}
	return req, 0
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
