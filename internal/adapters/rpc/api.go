// SPDX-License-Identifier: AGPL-3.0-or-later

// Package rpc serves the ermite API contract (bnema.dev/ermite-api) with
// ConnectRPC: handlers that map the messages of the contract to the use cases
// of app and back, the interceptor that authenticates callers, and the error
// mapping. The same handlers serve remote clients over HTTP and the web in
// process (see InProcessClient).
package rpc

import (
	"context"
	"net/http"
	"net/netip"

	"connectrpc.com/connect"
	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/clientlimit"
	"bnema.dev/ermite/internal/domain"
)

// MaxRequestBytes bounds the size of a request message. Responses are not
// bounded: a diff has its own limits and blobs are streamed.
const MaxRequestBytes = 1 << 20

// Features are the optional capabilities of the contract that this server
// implements, as GetVersion lists them.
var Features = []string{"repo.update", "events.resume"}

// Authenticator turns a token secret into a principal, or
// domain.ErrUnauthenticated. app.Auth fits.
type Authenticator interface {
	Authenticate(ctx context.Context, secret string) (domain.Principal, error)
}

// Config configures the API.
type Config struct {
	Auth    Authenticator
	Repos   *app.Repos
	Tokens  *app.Tokens
	Browse  *app.Browse
	Events  *app.Events
	Version string
	// TrustedProxies are the networks allowed to set X-Forwarded-For.
	TrustedProxies []netip.Prefix
	// Limiter counts failed authentications; nil builds a default one. Share
	// it with the other adapters that authenticate, so that a client has one
	// budget of failures.
	Limiter *clientlimit.Limiter
}

// API is the set of Connect handlers of the contract.
type API struct {
	cfg     Config
	handler http.Handler
}

// New builds the API.
func New(cfg Config) *API {
	if cfg.Limiter == nil {
		cfg.Limiter = clientlimit.NewLimiter(nil)
	}
	a := &API{cfg: cfg}

	opts := []connect.HandlerOption{
		connect.WithInterceptors(newInterceptor(cfg)),
		connect.WithReadMaxBytes(MaxRequestBytes),
		connect.WithRecover(func(ctx context.Context, spec connect.Spec, _ http.Header, panicValue any) error {
			log := zerowrap.FromCtx(ctx)
			log.Error().Interface("panic", panicValue).Str("procedure", spec.Procedure).Msg("api handler panicked")
			return connect.NewError(connect.CodeInternal, errInternal)
		}),
	}
	mux := http.NewServeMux()
	for _, mount := range []func() (string, http.Handler){
		func() (string, http.Handler) {
			return ermiteconnect.NewRepoServiceHandler(&repoService{cfg: cfg}, opts...)
		},
		func() (string, http.Handler) {
			return ermiteconnect.NewGitServiceHandler(&gitService{browse: cfg.Browse}, opts...)
		},
		func() (string, http.Handler) {
			return ermiteconnect.NewTokenServiceHandler(&tokenService{tokens: cfg.Tokens}, opts...)
		},
		func() (string, http.Handler) {
			return ermiteconnect.NewEventServiceHandler(&eventService{events: cfg.Events}, opts...)
		},
		func() (string, http.Handler) {
			return ermiteconnect.NewServerServiceHandler(&serverService{version: cfg.Version}, opts...)
		},
	} {
		path, h := mount()
		mux.Handle(path, h)
	}
	a.handler = withStreamStart(mux)
	return a
}

// Handler returns the handler of all services. Its paths are the procedure
// paths of the contract ("/ermite.RepoService/ListRepos"); httpserver mounts
// it under /-/api/ and strips the prefix.
func (a *API) Handler() http.Handler { return a.handler }

// caller returns the principal the interceptor (or the in-process caller)
// put in ctx; anonymous when there is none.
func caller(ctx context.Context) domain.Principal {
	p, _ := domain.PrincipalFromContext(ctx)
	return p
}

type startedKey struct{}

// withStreamStart gives the handlers a way to send the response headers
// before the first message of a stream (see startedFunc).
func withStreamStart(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := func() {
			w.WriteHeader(http.StatusOK)
			_ = http.NewResponseController(w).Flush()
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), startedKey{}, start)))
	})
}

// startedFunc returns the function that sends the response headers at once.
// A Connect client returns from a streaming call only when the headers have
// arrived; a stream that may stay silent for a long time (Watch) must send
// them first. Connect has set all the headers by the time a handler runs.
func startedFunc(ctx context.Context) func() {
	start, _ := ctx.Value(startedKey{}).(func())
	return start
}
