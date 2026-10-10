// SPDX-License-Identifier: AGPL-3.0-or-later

// Package httpserver is the HTTP entry of ermite: the router, the security
// headers, the request log and the graceful shutdown. Routes are added here
// as the other adapters arrive; for now it serves /-/health.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/bnema/zerowrap"
)

// ShutdownTimeout is how long Run waits for requests in flight after the
// context ends.
const ShutdownTimeout = 10 * time.Second

// Check reports whether a dependency works. A nil result means healthy.
type Check func(ctx context.Context) error

// Config configures a Server.
type Config struct {
	// Addr is the listen address, "host:port".
	Addr string
	// HealthChecks run on every GET /-/health; one failure answers 503.
	HealthChecks []Check
}

// Server is the HTTP server of ermite.
type Server struct {
	addr    string
	handler http.Handler
}

// New builds the server and its routes.
func New(cfg Config) *Server {
	mux := http.NewServeMux()
	// "GET" patterns also match HEAD; other methods get 405 with Allow, and
	// unknown paths get 404.
	mux.HandleFunc("GET /-/health", healthHandler(cfg.HealthChecks))
	return &Server{addr: cfg.Addr, handler: withRequestLog(withSecurityHeaders(mux))}
}

// Handler returns the root handler, for tests.
func (s *Server) Handler() http.Handler { return s.handler }

// Run listens on the configured address and serves until ctx ends, then
// shuts down gracefully within ShutdownTimeout. It returns nil after a clean
// shutdown. The logger of ctx is used for the server and its requests.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.addr, err)
	}
	return s.Serve(ctx, ln)
}

// Serve is Run on an existing listener, which it closes.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	log := zerowrap.FromCtx(ctx)
	srv := &http.Server{
		Handler: s.handler,
		// Requests keep the logger of ctx but outlive its cancellation, so
		// that a graceful shutdown lets them finish.
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No read or write timeout: Git clones and pushes can be long.
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info().Str("addr", ln.Addr().String()).Msg("listening")

	select {
	case err := <-errc:
		// Serve returns before Shutdown only on a failure.
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	log.Info().Dur("timeout", ShutdownTimeout).Msg("shutting down")
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	log.Info().Msg("http server stopped")
	return nil
}

// DirWritable returns a Check that creates and removes a file in dir.
func DirWritable(dir string) Check {
	return func(context.Context) error {
		f, err := os.CreateTemp(dir, ".health-*")
		if err != nil {
			return fmt.Errorf("data directory is not writable: %w", err)
		}
		name := f.Name()
		cerr := f.Close()
		rerr := os.Remove(name)
		if cerr != nil {
			return fmt.Errorf("data directory is not writable: %w", cerr)
		}
		if rerr != nil {
			return fmt.Errorf("data directory is not writable: %w", rerr)
		}
		return nil
	}
}

