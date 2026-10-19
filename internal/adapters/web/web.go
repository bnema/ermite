// SPDX-License-Identifier: AGPL-3.0-or-later

// Package web is the HTML front of ermite: the pages of the read-only
// showcase, rendered with templ from the data of the use cases.
//
// Like every other inbound adapter, the package reads and writes through the
// use case interfaces of package ports (ADR 0024), never through the API. The
// caller of every use case call is passed explicitly: the principal of the
// request, which is anonymous unless the request carries a valid owner session
// (ServeHTTP sets it in the context, principal reads it). The use cases then
// show public repositories only to an anonymous caller; a private repository
// is indistinguishable from a missing one (domain.ErrNotFound), here as
// everywhere.
//
// Adding a page: a handler on Server, a route in routes, a templ page in
// pages.templ and its view model in view.go.
package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Patterns are the ServeMux patterns the handler serves: everything. The
// router of the package answers the paths it does not know with its 404
// page, and the more specific patterns of the server (health, API, Git) win
// over this one.
var Patterns = []string{"/"}

// requestTimeout bounds the use case calls that build one page.
const requestTimeout = 30 * time.Second

// Config configures a Server.
type Config struct {
	// Repos and Browse are the use cases the pages are built from.
	Repos  ports.RepoService
	Browse ports.BrowseService
	// MergeRequests serves the merge request pages (ADR 0017). Without it
	// there are no such pages, nor section for them. Leave it unset rather
	// than assigning a typed nil pointer.
	MergeRequests ports.MergeRequestService
	// Issues serves the issue pages (ADR 0031). Without it there are no such
	// pages, nor tab for them. Leave it unset rather than assigning a typed
	// nil pointer.
	Issues ports.IssueService
	// IssueNumbers tells whether a number names an issue or a merge request,
	// so that the page of the wrong kind redirects to the right one. Without
	// it there is no such redirect. Leave it unset rather than assigning a
	// typed nil pointer.
	IssueNumbers ports.IssueNumberResolver
	// Inbox serves the owner's inbox page (/-/inbox) and the button that
	// leads to it. Without it there is neither. Leave it unset rather than
	// assigning a typed nil pointer.
	Inbox ports.InboxService
	// Runs serves the CI pages (/<repo>/actions) and the checks shown next to
	// commits and merge requests. Without it there are no such pages. Leave it
	// unset rather than assigning a typed nil pointer.
	Runs ports.RunService
	// Artifacts serves the artifacts of the runs: a section of the run page,
	// their download and their deletion. It needs Runs. Without it there is
	// neither. Leave it unset rather than assigning a typed nil pointer.
	Artifacts ports.ArtifactService
	// Events is the source of the event stream behind /-/events. Without
	// it the pages are not live and the route answers 404. Leave it unset
	// rather than assigning a typed nil pointer.
	Events ports.EventService
	// Heartbeat is how often an idle event stream gets a comment line;
	// DefaultHeartbeat when zero.
	Heartbeat time.Duration
	// Owner is the owner login the GitHub-shaped URLs of CI show
	// (<server>/<owner>/<repo>/...). A GET or HEAD of /<owner>/<rest> is
	// redirected to /<rest> unless a repository named like the owner is
	// visible to the caller. Empty: no redirect.
	Owner string
	// PublicURL is the base URL of the instance, without a trailing slash:
	// the clone URLs are built on it, and its host is the instance name.
	PublicURL string
	// SourceURL is the link to the source of the running version (AGPL §13).
	SourceURL string
	// Version is the version of the running server.
	Version string
	// Now is the clock of relative dates; time.Now when nil.
	Now func() time.Time
	// Sessions signs the owner in (ADR 0018). Without it there are no login
	// routes and every visitor is anonymous.
	Sessions ports.WebSessionService
	// Passkeys signs the owner in with a passkey and manages the passkeys
	// (ADR 0026). Without it, or without Sessions, there are no passkey
	// routes and no link to them. Leave it unset rather than assigning a typed
	// nil pointer.
	Passkeys ports.WebPasskeyService
	// Limiter counts the failed logins; required with Sessions. Share it with
	// the API and Git, so that a client has one budget of failures.
	Limiter ports.FailureLimiter
	// TrustedProxies are the networks allowed to set X-Forwarded-For, to find
	// the address of the client that fails a login.
	TrustedProxies []netip.Prefix
}

// Server serves the pages. It is an http.Handler.
type Server struct {
	cfg      Config
	instance string
	origin   string // scheme://host of the public URL, what a POST must come from
	assets   *assetSet
	md       *markdown
	// pathCache holds the path lists the file finder fetches.
	pathCache *pathCache
	// hl holds the colours of highlighted diffs.
	hl  *hlCache
	mux *http.ServeMux
	// ownerRepo is the owner login as a repository name, zero when it is not
	// a valid one (then no repository can be named like the owner).
	ownerRepo domain.RepoName
	// refsFan shares the fragments of the refs page between the watchers of
	// a push; entries are few and short-lived.
	refsFan fanout
	// pulses holds the activity curves of the home rows.
	pulses pulseCache
}

// New builds the web handler.
func New(cfg Config) (*Server, error) {
	if cfg.Repos == nil || cfg.Browse == nil {
		return nil, errors.New("web: the repository and browse use cases are required")
	}
	u, err := url.Parse(cfg.PublicURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("web: public URL %q is not a valid base URL", cfg.PublicURL)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	assets, err := loadAssets()
	if err != nil {
		return nil, fmt.Errorf("web: load assets: %w", err)
	}
	if cfg.Sessions != nil && cfg.Limiter == nil {
		return nil, errors.New("web: sessions need a failure limiter")
	}
	s := &Server{
		cfg: cfg, instance: u.Host, assets: assets, md: newMarkdown(),
		pathCache: newPathCache(pathCacheBytes, pathBuilds),
		hl:        newHLCache(hlCacheBytes),
		origin:    originOf(u),
	}
	s.ownerRepo, _ = domain.ParseRepoName(cfg.Owner)
	s.mux = s.routes()
	return s, nil
}

// routes is the router of the web. Any path that is not a page is the 404
// page.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+StaticPrefix, s.assets.serve)
	mux.HandleFunc("GET /-/events", s.events)
	mux.HandleFunc("GET "+ServiceWorkerPath, s.assets.sw.serve)
	if s.cfg.Sessions != nil {
		mux.HandleFunc("GET "+loginPrefix+"{nonce}", s.loginConfirm)
		mux.HandleFunc("POST "+loginPrefix+"{nonce}", s.loginPost)
		mux.HandleFunc("POST /-/logout", s.logout)
	}
	if s.passkeysOn() {
		s.passkeyRoutes(mux)
	}
	if s.cfg.Inbox != nil {
		mux.HandleFunc("GET "+inboxPath, s.page(s.inbox))
	}
	mux.HandleFunc("GET /{$}", s.page(s.home))
	mux.HandleFunc("GET /{repo}", s.page(s.summary))
	mux.HandleFunc("GET /{repo}/{$}", s.page(s.summary))
	mux.HandleFunc("GET /{repo}/refs", s.page(s.refs))
	mux.HandleFunc("GET /{repo}/commits", s.page(s.commits))
	mux.HandleFunc("GET /{repo}/commits/{rest...}", s.page(s.commits))
	mux.HandleFunc("GET /{repo}/commit/{sha}", s.page(s.commit))
	mux.HandleFunc("GET /{repo}/tree", s.page(s.tree))
	mux.HandleFunc("GET /{repo}/tree/{rest...}", s.page(s.tree))
	mux.HandleFunc("GET /{repo}/blob/{rest...}", s.page(s.blob))
	mux.HandleFunc("GET /{repo}/raw/{rest...}", s.raw)
	mux.HandleFunc("GET /{repo}/paths/{sha}", s.paths)
	mux.HandleFunc("GET /{repo}/context/{sha}", s.page(s.diffContext))
	if s.cfg.MergeRequests != nil {
		mux.HandleFunc("GET /{repo}/merge-requests", s.page(s.mrList))
		mux.HandleFunc("GET /{repo}/merge-requests/{$}", s.page(s.mrList))
		mux.HandleFunc("GET /{repo}/merge-requests/{n}", s.pageOrRedirect(s.mrShow, s.mrToIssue))
		mux.HandleFunc("GET /{repo}/merge-requests/{n}/since", s.page(s.mrSince))
		mux.HandleFunc("GET /{repo}/merge-requests/{n}/files", s.page(s.mrFiles))
		mux.HandleFunc("GET /{repo}/compare/{rest...}", s.page(s.compare))
		// The writes: POST only, behind the gate (ServeHTTP).
		mux.HandleFunc("POST /{repo}/merge-requests", s.mrAction(s.mrCreate))
		mux.HandleFunc("POST /{repo}/merge-requests/{n}/comments", s.mrAction(s.mrComment))
		mux.HandleFunc("POST /{repo}/merge-requests/{n}/comments/{id}/resolve", s.mrAction(s.mrResolve))
		mux.HandleFunc("POST /{repo}/merge-requests/{n}/merge", s.mrAction(s.mrMerge))
		mux.HandleFunc("POST /{repo}/merge-requests/{n}/close", s.mrAction(s.mrClose))
		mux.HandleFunc("POST /{repo}/merge-requests/{n}/ready", s.mrAction(s.mrReady))
		mux.HandleFunc("POST /{repo}/merge-requests/{n}/read", s.mrAction(s.mrRead))
	}
	if s.cfg.Issues != nil {
		mux.HandleFunc("GET /{repo}/issues", s.page(s.issueList))
		mux.HandleFunc("GET /{repo}/issues/{$}", s.page(s.issueList))
		mux.HandleFunc("GET /{repo}/issues/new", s.page(s.issueNew))
		mux.HandleFunc("GET /{repo}/issues/{n}", s.pageOrRedirect(s.issueShow, s.issueToMR))
		// The writes: POST only, behind the gate (ServeHTTP).
		mux.HandleFunc("POST /{repo}/issues", s.issueAction(s.issueCreate))
		mux.HandleFunc("POST /{repo}/issues/{n}/comments", s.issueAction(s.issueComment))
		mux.HandleFunc("POST /{repo}/issues/{n}/close", s.issueAction(s.issueClose))
		mux.HandleFunc("POST /{repo}/issues/{n}/reopen", s.issueAction(s.issueReopen))
	}
	if s.cfg.Runs != nil {
		mux.HandleFunc("GET /{repo}/actions", s.page(s.actions))
		mux.HandleFunc("GET /{repo}/actions/{$}", s.page(s.actions))
		mux.HandleFunc("GET /{repo}/actions/runs/{id}", s.page(s.run))
		mux.HandleFunc("GET /{repo}/actions/runs/{id}/job/{job}", s.page(s.job))
		mux.HandleFunc("GET /{repo}/actions/runs/{id}/job/{job}/raw", s.rawLog)
		mux.HandleFunc("GET /{repo}/actions/runs/{id}/job/{job}/stream", s.logStream)
		mux.HandleFunc("POST /{repo}/actions/runs/{id}/cancel", s.ciAction(s.runCancel))
		mux.HandleFunc("POST /{repo}/actions/runs/{id}/rerun", s.ciAction(s.runRerun(false)))
		mux.HandleFunc("POST /{repo}/actions/runs/{id}/rerun-failed", s.ciAction(s.runRerun(true)))
		if s.cfg.Artifacts != nil {
			mux.HandleFunc("GET /{repo}/actions/runs/{id}/artifacts/{artifact}", s.artifactDownload)
			mux.HandleFunc("POST /{repo}/actions/runs/{id}/artifacts/{artifact}/delete", s.ciAction(s.artifactDelete))
		}
	}
	mux.HandleFunc(catchAll, s.page(s.notFound))
	return mux
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Whatever the context carried, the caller is anonymous until a valid
	// session says otherwise (withSession).
	r = r.WithContext(domain.ContextWithPrincipal(r.Context(), domain.Anonymous()))
	r = s.withSession(w, r)
	if !s.gate(w, r) {
		return
	}
	if to, ok := s.ownerRedirect(r); ok {
		http.Redirect(w, r, to, http.StatusPermanentRedirect)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// catchAll is the pattern of the paths that are no page.
const catchAll = "GET /{repo}/{rest...}"

// ownerRedirect maps /<owner>/<repo>[.git][/rest] to /<repo>[.git][/rest]: the
// URLs that the github context of CI shows (html_url, commit and compare
// URLs, clone_url) have the owner of the instance in front, like GitHub's.
//
// The rule is by repository, not by route: when no repository named like the
// owner is visible to the caller, every GET or HEAD of /<owner>/<rest> is
// redirected, so /<owner>/actions reaches the repository named "actions". A
// repository named like the owner keeps its own pages. A private one is seen
// as missing by an anonymous caller (the use case answers ErrNotFound), so the
// redirect is the same as for a name nobody has and tells nothing.
//
// The target is built from a segment that parses as a repository name, which
// has no slash, backslash, percent sign or dot-dot: it can only be a path on
// this origin, never a "//host" one.
func (s *Server) ownerRedirect(r *http.Request) (string, bool) {
	if s.cfg.Owner == "" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return "", false
	}
	first, rest, ok := strings.Cut(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	if !ok || !domain.OwnerMatches(s.cfg.Owner, first) || rest == "" {
		return "", false
	}
	repo, _, _ := strings.Cut(rest, "/")
	if _, err := domain.ParseRepoPath(repo); err != nil {
		return "", false
	}
	if s.ownerNamesARepo(r) {
		return "", false
	}
	to := "/" + rest
	if r.URL.RawQuery != "" {
		to += "?" + r.URL.RawQuery
	}
	return to, true
}

// ownerNamesARepo tells whether the caller can see a repository named like
// the owner. A lookup that fails otherwise counts as yes: the request is then
// served as it was before, without a redirect.
func (s *Server) ownerNamesARepo(r *http.Request) bool {
	if s.ownerRepo.IsZero() {
		return false // the owner login is not a repository name
	}
	ctx := r.Context()
	_, err := s.cfg.Repos.Get(ctx, principal(ctx), s.ownerRepo)
	switch {
	case err == nil:
		return true
	case errors.Is(err, domain.ErrNotFound):
		return false
	}
	log := zerowrap.FromCtx(ctx)
	log.Debug().Err(err).Msg("owner redirect: the lookup of the repository named like the owner failed")
	return true
}

// handler builds a page. It returns the component and its status, or an
// error, which becomes the 404 or 500 page.
type handler func(ctx context.Context, r *http.Request) (templ.Component, int, error)

// page adapts a handler: it bounds the use case calls, renders the component into
// a buffer (so that a failure never leaves half a page), and maps errors to
// the state pages.
func (s *Server) page(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		c, status, err := h(ctx, r)
		if err != nil {
			s.fail(ctx, w, r, err)
			return
		}
		s.write(ctx, w, r, status, c)
	}
}

// write renders c with the given status.
func (s *Server) write(ctx context.Context, w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	var buf bytes.Buffer
	if err := c.Render(ctx, &buf); err != nil {
		// The error page itself failed: a bare answer is all that is left.
		log := zerowrap.FromCtx(ctx)
		log.Error().Err(err).Str("path", logPath(r.URL.Path)).Msg("render page")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	// Links on this origin are prefetched on hover, so a click shows the page
	// at once. Every GET page is safe to load (nothing is marked read on GET).
	h.Set("Speculation-Rules", `"`+s.assets.urls.Rules+`"`)
	// Public pages: the browser and any proxy revalidate each time. A page
	// rendered for the owner is private and never stored; a handler that set
	// its own Cache-Control keeps it.
	if h.Get("Cache-Control") == "" {
		cacheHeaders(ctx, h, "no-cache", sessionCache)
	}
	body := buf.Bytes()
	// Whether the body is sent gzipped follows from its size and the request
	// alone, so that the tag is known before the compression is spent.
	gz := len(body) >= minCompress && acceptsGzip(r.Header) &&
		!noTransform(h) && h.Get("Content-Encoding") == ""
	addVary(h, "Accept-Encoding")
	readOnly := r.Method == http.MethodGet || r.Method == http.MethodHead
	// A page that may be stored gets a validator: the browser then
	// revalidates with If-None-Match and a page that did not change costs a
	// 304. A no-store page (the owner's) is never revalidated.
	if readOnly && status == http.StatusOK && h.Get("ETag") == "" && !hasCacheDirective(h, noStore) {
		tag := pageETag(body, SignedIn(ctx), gz)
		h.Set("ETag", tag)
		if etagMatches(strings.Join(r.Header.Values("If-None-Match"), ","), tag) {
			h.Del("Content-Type")
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	if gz {
		if z, err := gzipPage(body); err == nil && len(z) < len(body) {
			body = z
			h.Set("Content-Encoding", "gzip")
		}
	}
	h.Set("Content-Length", itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// fail answers an error of a use case: not found (and a name or revision the
// use case refuses, which no page can show either) gives the 404 page, a
// visitor that left gives nothing, anything else gives the 500 page with an id
// to find the cause in the log.
func (s *Server) fail(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound), domain.IsInvalid(err):
		s.write(ctx, w, r, http.StatusNotFound, s.notFoundPage())
	case errors.Is(err, context.Canceled):
		// The client is gone; there is nobody to answer.
	default:
		id := requestID()
		log := zerowrap.FromCtx(ctx)
		log.Error().Err(err).Str("request_id", id).Str("path", logPath(r.URL.Path)).Msg("web request failed")
		s.write(ctx, w, r, http.StatusInternalServerError, s.errorPage(id))
	}
}

// errNotFound is returned by handlers for a resource that does not exist
// although the use case did not say so (a ref or a path that is not there).
var errNotFound = domain.ErrNotFound

// requestID is a short random id, shown on the 500 page and logged.
func requestID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// notFound is the handler of the paths without a page.
func (s *Server) notFound(context.Context, *http.Request) (templ.Component, int, error) {
	return nil, 0, errNotFound
}

// base is the Layout every page starts from.
func (s *Server) base() Layout {
	return Layout{
		Owner:     s.cfg.Owner,
		Version:   s.cfg.Version,
		SourceURL: s.cfg.SourceURL,
		Assets:    s.assets.urls,
		Passkeys:  s.passkeysOn(),
	}
}

// stateLayout is the layout of a state page: only the way home.
func (s *Server) stateLayout(title string, crumbs []Crumb) Layout {
	l := s.base()
	l.Title = title + " · " + s.instance
	l.Crumbs = crumbs
	l.Cmd = []CmdItem{{Kind: "go", Label: "home", Href: "/"}}
	return l
}

func (s *Server) notFoundPage() templ.Component {
	l := s.stateLayout("404", []Crumb{{Label: "not found", Current: true}})
	return State(l, StatePage{
		Code: "404", Title: "not found",
		Lines: []string{"no such page, ref or path.", "check the URL, or start from the repo list."},
		Back:  []Link{{Label: "home", Href: "/"}},
	})
}

func (s *Server) errorPage(id string) templ.Component {
	l := s.stateLayout("error", []Crumb{{Label: "error", Current: true}})
	return State(l, StatePage{
		Code: "500", Title: "internal error",
		Lines: []string{
			"the server failed to answer this request.",
			"request id: " + id,
			"retry in a moment; if it persists, the owner has the log.",
		},
		Back: []Link{{Label: "home", Href: "/"}},
	})
}

// originOf is the origin of a URL as a browser writes it in the Origin
// header: scheme and host in lower case, the port only when it is not the
// default of the scheme.
func originOf(u *url.URL) string {
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	defaultPort := (scheme == "http" && port == "80") || (scheme == "https" && port == "443")
	if port != "" && !defaultPort {
		host += ":" + port
	}
	return scheme + "://" + host
}
