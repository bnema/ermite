// SPDX-License-Identifier: AGPL-3.0-or-later

// Package githubapi serves the subset of GitHub's REST API that the actions
// running on ermite call under github.api_url (/-/github/api, ADR 0025 CI-2):
//
//	GET /repos/{owner}/{repo}
//	GET /repos/{owner}/{repo}/hash-algorithm
//	GET /repos/{owner}/{repo}/tarball[/{ref}]
//	GET /repos/{owner}/{repo}/pulls/{number}/files   (dorny/paths-filter on pull_request)
//
// Everything else under the prefix is GitHub's 404 JSON, so that an action
// calling an endpoint of a feature ermite lacks fails as it does on a GitHub
// instance without it, never silently succeeds. The auth is the forge's: a
// token (stored, or the repository token of a job) as the password of Basic with
// any user, or as "Bearer" or "token" credentials; no credentials is anonymous,
// which reads public repositories.
package githubapi

import (
	"cmp"
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/clientlimit"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/httpauth"
	"bnema.dev/ermite/internal/ports"
)

// Prefix is where the API is mounted.
const Prefix = "/-/github/api"

// Patterns are the ServeMux patterns the handler serves: its whole prefix.
var Patterns = []string{Prefix + "/"}

// notFoundBody is GitHub's body for an unknown endpoint or repository.
const notFoundBody = `{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`

// retryAfter is the Retry-After, in seconds, of an answer that asks the client to
// come back (429 and 503).
const retryAfter = "5"

// Bounds on the archives written at once.
const (
	// DefaultMaxArchives is the archives written at once on the server.
	DefaultMaxArchives = 16
	// DefaultMaxArchivesPerClient is the archives written at once for one client
	// (see clientlimit.Key). A runner host runs several jobs at once, each
	// downloading its actions, and all come from one address; eight is half of
	// the server's slots, so one host cannot take them all. Beyond it the answer
	// is 429 with Retry-After: the runner waits and tries again.
	DefaultMaxArchivesPerClient = 8
	// DefaultWriteIdleTimeout is how long a write of the archive may wait for the
	// client. The deadline moves forward with every chunk: an archive is bounded
	// by its size, not by its duration, but a client that stops reading is cut.
	DefaultWriteIdleTimeout = 2 * time.Minute
)

// Config configures a Handler.
type Config struct {
	Auth ports.AuthService
	API  ports.GitHubAPIService
	// Releases and ReleaseDownload enable the release REST routes. Nil services
	// leave their routes unavailable with GitHub's 404 JSON.
	Releases        ports.GitHubReleaseAPI
	ReleaseDownload ports.ReleaseService
	// TrustedProxies are the networks allowed to set X-Forwarded-For.
	TrustedProxies []netip.Prefix
	// Limiter counts failed authentications; nil builds a default one.
	Limiter ports.FailureLimiter
	// MaxArchives bounds the archives written at once (each runs a git
	// process); zero means DefaultMaxArchives. MaxArchivesPerClient bounds them
	// per client; zero means DefaultMaxArchivesPerClient. WriteIdleTimeout is
	// the idle write deadline; zero means DefaultWriteIdleTimeout.
	MaxArchives          int
	MaxArchivesPerClient int
	WriteIdleTimeout     time.Duration
}

// Handler serves the REST subset.
type Handler struct {
	cfg      Config
	limiter  ports.FailureLimiter
	archives chan struct{}
	perCli   int
	idle     time.Duration

	mu      sync.Mutex     // guards clients
	clients map[string]int // archives being written, per clientlimit.Key
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
	h.perCli = cmp.Or(cfg.MaxArchivesPerClient, DefaultMaxArchivesPerClient)
	h.idle = cmp.Or(cfg.WriteIdleTimeout, DefaultWriteIdleTimeout)
	h.clients = make(map[string]int)
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	rest := strings.TrimPrefix(r.URL.Path, Prefix)
	kind, owner, repo, ref, ok := route(rest)
	var number int64
	if kind == "pullfiles" {
		number, _ = strconv.ParseInt(ref, 10, 64)
	}
	if !ok {
		notFound(w)
		return
	}
	if strings.HasPrefix(kind, "release") && h.cfg.Releases == nil {
		notFound(w)
		return
	}
	allowed := releaseMethods(kind)
	if !methodAllowed(r.Method, allowed) {
		w.Header().Set("Allow", allowed)
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

	if strings.HasPrefix(kind, "release") {
		h.releaseRequest(w, r, p, kind, owner, repo, ref, clientlimit.Key(addr))
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
		h.tarball(w, r, p, owner, repo, ref, clientlimit.Key(addr))
	case "pullfiles":
		h.pullFiles(w, r, p, owner, repo, number)
	}
}

// route recognises the endpoints. ref is "" for /tarball.
func route(rest string) (kind, owner, repo, ref string, ok bool) {
	// go-github's enterprise client (used by GoReleaser) appends api/v3
	// to a custom API base. Keep the canonical URLs, accepting that alias too.
	if strings.HasPrefix(rest, "/api/v3/") {
		rest = strings.TrimPrefix(rest, "/api/v3")
	}
	if kind, owner, repo, ref, ok = releaseRoute(rest); ok {
		return
	}
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
	case len(parts) == 6 && parts[3] == "pulls" && parts[5] == "files" && isNumber(parts[4]):
		// ref carries the number of the pull request.
		return "pullfiles", owner, repo, parts[4], true
	case len(parts) >= 4 && parts[3] == "tarball":
		// The ref may hold slashes (feature/x): everything after tarball/.
		return "tarball", owner, repo, strings.Join(parts[4:], "/"), true
	}
	return "", "", "", "", false
}

// isNumber reports whether s is a positive decimal number.
func isNumber(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == s
}

// pullFiles answers the files of a pull request, a page at a time as GitHub
// does: page and per_page in the query (30 and 100 at most), the other pages
// in the Link header. A parameter that is not a positive number is the default,
// as GitHub treats it.
func (h *Handler) pullFiles(w http.ResponseWriter, r *http.Request, p domain.Principal, owner, repo string, number int64) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	perPage, _ := strconv.Atoi(q.Get("per_page"))
	body, link, err := h.cfg.API.ListPullFiles(r.Context(), p, owner, repo, number, page, perPage)
	if err != nil {
		if errors.Is(err, domain.ErrForbidden) {
			jsonError(w, http.StatusForbidden, "Resource not accessible by integration")
			return
		}
		h.fail(r.Context(), w, err)
		return
	}
	if link != "" {
		w.Header().Set("Link", link)
	}
	writeJSON(w, r, body)
}

// tarball answers an archive request. A HEAD resolves the ref and sends the
// headers of the archive, and runs no git. A GET streams the archive; it is not
// a 302 to a codeload: ermite serves the bytes itself, which the runner follows
// without a second credential.
func (h *Handler) tarball(w http.ResponseWriter, r *http.Request, p domain.Principal, owner, repo, ref, client string) {
	ctx := r.Context()
	if r.Method == http.MethodHead {
		name, err := h.cfg.API.TarballName(ctx, p, owner, repo, ref)
		if err != nil {
			h.fail(ctx, w, err)
			return
		}
		setArchiveHeaders(w, name)
		w.WriteHeader(http.StatusOK)
		return
	}
	release, ok := h.acquire(w, client)
	if !ok {
		return
	}
	defer release()
	rc := http.NewResponseController(w)
	started := false
	err := h.cfg.API.Tarball(ctx, p, owner, repo, ref, func(name string) io.Writer {
		started = true
		setArchiveHeaders(w, name)
		return &idleWriter{w: w, rc: rc, idle: h.idle}
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

// acquire takes an archive slot for the client, or answers 429 (the client
// already has its share) or 503 (the server writes all it can) and reports
// false. It never waits: an archive can take minutes.
func (h *Handler) acquire(w http.ResponseWriter, client string) (release func(), ok bool) {
	h.mu.Lock()
	if h.clients[client] >= h.perCli {
		h.mu.Unlock()
		w.Header().Set("Retry-After", retryAfter)
		jsonError(w, http.StatusTooManyRequests, "Too many archives in progress")
		return nil, false
	}
	h.clients[client]++
	h.mu.Unlock()
	releaseClient := func() {
		h.mu.Lock()
		if h.clients[client]--; h.clients[client] <= 0 {
			delete(h.clients, client)
		}
		h.mu.Unlock()
	}
	select {
	case h.archives <- struct{}{}:
		return func() { <-h.archives; releaseClient() }, true
	default:
		releaseClient()
		w.Header().Set("Retry-After", retryAfter)
		jsonError(w, http.StatusServiceUnavailable, "Server busy")
		return nil, false
	}
}

// idleWriter moves the write deadline of the connection forward before each
// chunk, so that only a client that stops reading is cut. Where the response
// writer cannot set a deadline, the writes are not bounded here.
type idleWriter struct {
	w    io.Writer
	rc   *http.ResponseController
	idle time.Duration
}

func (i *idleWriter) Write(b []byte) (int, error) {
	_ = i.rc.SetWriteDeadline(time.Now().Add(i.idle))
	return i.w.Write(b)
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
func credentials(r *http.Request) (string, bool) { return httpauth.Credentials(r) }

// fail answers an error of a use case. A repository the caller cannot read is
// as not found as one that does not exist.
func (h *Handler) fail(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound), domain.IsInvalid(err):
		notFound(w)
	case errors.Is(err, domain.ErrResourceExhausted):
		w.Header().Set("Retry-After", retryAfter)
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
