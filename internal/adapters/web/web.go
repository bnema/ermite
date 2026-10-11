// SPDX-License-Identifier: AGPL-3.0-or-later

// Package web is the HTML front of ermite: the pages of the read-only
// showcase, rendered with templ from the data of the ermite API.
//
// The package is a client of the API like any other. It talks to the
// generated ermite-api Connect clients, which cmd binds in process to the
// handlers of the rpc adapter (ADR 0020), and it knows nothing of the use
// cases or the domain. Who the visitor is comes with the request context, set
// by the middleware cmd puts in front of the handler: anonymous in P5. The
// API then shows public repositories only; a private repository is
// indistinguishable from a missing one, here as everywhere.
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

	"connectrpc.com/connect"
	"github.com/a-h/templ"
	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
)

// Patterns are the ServeMux patterns the handler serves: everything. The
// router of the package answers the paths it does not know with its 404
// page, and the more specific patterns of the server (health, API, Git) win
// over this one.
var Patterns = []string{"/"}

// requestTimeout bounds the API calls that build one page.
const requestTimeout = 30 * time.Second

// Config configures a Server.
type Config struct {
	// Repos and Git are the API clients the pages are built from.
	Repos ermiteconnect.RepoServiceClient
	Git   ermiteconnect.GitServiceClient
	// MergeRequests is the client of the merge request pages (ADR 0017).
	// Without it there are no such pages, nor section for them.
	MergeRequests ermiteconnect.MergeRequestServiceClient
	// Events is the client of the event stream behind /-/events. Without
	// it the pages are not live and the route answers 404.
	Events ermiteconnect.EventServiceClient
	// Heartbeat is how often an idle event stream gets a comment line;
	// DefaultHeartbeat when zero.
	Heartbeat time.Duration
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
	Sessions SessionAuth
	// Limiter counts the failed logins; required with Sessions. Share it with
	// the API and Git, so that a client has one budget of failures.
	Limiter FailureLimiter
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
	mux      *http.ServeMux
}

// New builds the web handler.
func New(cfg Config) (*Server, error) {
	if cfg.Repos == nil || cfg.Git == nil {
		return nil, errors.New("web: the repository and Git API clients are required")
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
		origin: originOf(u),
	}
	s.mux = s.routes()
	return s, nil
}

// routes is the router of the web. Any path that is not a page is the 404
// page.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+StaticPrefix, s.assets.serve)
	mux.HandleFunc("GET /-/events", s.events)
	if s.cfg.Sessions != nil {
		mux.HandleFunc("GET "+loginPrefix+"{nonce}", s.loginConfirm)
		mux.HandleFunc("POST "+loginPrefix+"{nonce}", s.loginPost)
		mux.HandleFunc("POST /-/logout", s.logout)
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
	if s.cfg.MergeRequests != nil {
		mux.HandleFunc("GET /{repo}/merge-requests", s.page(s.mrList))
		mux.HandleFunc("GET /{repo}/merge-requests/{$}", s.page(s.mrList))
		mux.HandleFunc("GET /{repo}/merge-requests/{n}", s.page(s.mrShow))
		mux.HandleFunc("GET /{repo}/merge-requests/{n}/since", s.page(s.mrSince))
		mux.HandleFunc("GET /{repo}/compare/{rest...}", s.page(s.compare))
		// The writes: POST only, behind the gate (ServeHTTP).
		mux.HandleFunc("POST /{repo}/merge-requests", s.mrAction(s.mrCreate))
		mux.HandleFunc("POST /{repo}/merge-requests/{n}/comments", s.mrAction(s.mrComment))
		mux.HandleFunc("POST /{repo}/merge-requests/{n}/comments/{id}/resolve", s.mrAction(s.mrResolve))
		mux.HandleFunc("POST /{repo}/merge-requests/{n}/merge", s.mrAction(s.mrMerge))
		mux.HandleFunc("POST /{repo}/merge-requests/{n}/close", s.mrAction(s.mrClose))
		mux.HandleFunc("POST /{repo}/merge-requests/{n}/read", s.mrAction(s.mrRead))
	}
	mux.HandleFunc("GET /{repo}/{rest...}", s.page(s.notFound))
	return mux
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = s.withSession(w, r)
	if !s.gate(w, r) {
		return
	}
	s.mux.ServeHTTP(w, r)
}

// handler builds a page. It returns the component and its status, or an
// error, which becomes the 404 or 500 page.
type handler func(ctx context.Context, r *http.Request) (templ.Component, int, error)

// page adapts a handler: it bounds the API calls, renders the component into
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

// fail answers an error of the API: not found (and a name or revision the
// API refuses, which no page can show either) gives the 404 page, a visitor
// that left gives nothing, anything else gives the 500 page with an id to
// find the cause in the log.
func (s *Server) fail(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) {
	switch code := connect.CodeOf(err); {
	case errors.Is(err, errNotFound), code == connect.CodeNotFound, code == connect.CodeInvalidArgument:
		s.write(ctx, w, r, http.StatusNotFound, s.notFoundPage())
	case errors.Is(err, context.Canceled), code == connect.CodeCanceled:
		// The client is gone; there is nobody to answer.
	default:
		id := requestID()
		log := zerowrap.FromCtx(ctx)
		log.Error().Err(err).Str("request_id", id).Str("path", logPath(r.URL.Path)).Msg("web request failed")
		s.write(ctx, w, r, http.StatusInternalServerError, s.errorPage(id))
	}
}

// errNotFound is returned by handlers for a resource that does not exist
// although the API did not say so (a ref or a path that is not there).
var errNotFound = errors.New("not found")

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
		Instance:  s.instance,
		Version:   s.cfg.Version,
		SourceURL: s.cfg.SourceURL,
		Assets:    s.assets.urls,
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
