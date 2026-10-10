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
	"net/url"
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
}

// Server serves the pages. It is an http.Handler.
type Server struct {
	cfg      Config
	instance string
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
	s := &Server{cfg: cfg, instance: u.Host, assets: assets, md: newMarkdown()}
	s.mux = s.routes()
	return s, nil
}

// routes is the router of the web. Any path that is not a page is the 404
// page.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+StaticPrefix, s.assets.serve)
	mux.HandleFunc("GET /-/events", s.events)
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
	mux.HandleFunc("GET /{repo}/{rest...}", s.page(s.notFound))
	return mux
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
		log.Error().Err(err).Str("path", r.URL.Path).Msg("render page")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	// Public pages: the browser and any proxy revalidate each time.
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(buf.Bytes())
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
		log.Error().Err(err).Str("request_id", id).Str("path", r.URL.Path).Msg("web request failed")
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
func (s *Server) stateLayout(title, context string, crumbs []Crumb) Layout {
	l := s.base()
	l.Title = title + " · " + s.instance
	l.Crumbs = crumbs
	l.Sections = []Section{{Key: "h", Post: "ome", Href: "/"}}
	l.Status = Status{Mode: "state", Context: context, Hints: "g h home · ? help"}
	l.Cmd = []CmdItem{{Kind: "go", Label: "home", Href: "/"}}
	return l
}

func (s *Server) notFoundPage() templ.Component {
	l := s.stateLayout("404", "404 · not found", []Crumb{{Label: "-"}, {Label: "404"}})
	return State(l, StatePage{
		Code: "404", Title: "not found",
		Lines: []string{"no such page, ref or path.", "check the URL, or start from the repo list."},
		Back:  []Link{{Label: "home", Href: "/"}},
	})
}

func (s *Server) errorPage(id string) templ.Component {
	l := s.stateLayout("error", "500 · internal error", []Crumb{{Label: "-"}, {Label: "error"}})
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
