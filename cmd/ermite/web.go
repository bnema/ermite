// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"time"

	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/adapters/httpserver"
	"bnema.dev/ermite/internal/adapters/rpc"
	"bnema.dev/ermite/internal/adapters/web"
	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/clientlimit"
	"bnema.dev/ermite/internal/config"
	"bnema.dev/ermite/internal/domain"
)

// newWeb builds the web pages on the API of this process: the generated
// Connect clients are bound to the handlers of api, with no socket (ADR 0020).
//
// The owner's session cookie turns a request into the owner's (web.Server
// checks it with sessions); every other request is anonymous.
func newWeb(cfg config.Config, api *rpc.API, sessions *app.Sessions, limiter *clientlimit.Limiter) (http.Handler, error) {
	hc := api.InProcessClient()
	srv, err := web.New(web.Config{
		Repos:         ermiteconnect.NewRepoServiceClient(hc, rpc.InProcessURL),
		Git:           ermiteconnect.NewGitServiceClient(hc, rpc.InProcessURL),
		MergeRequests: ermiteconnect.NewMergeRequestServiceClient(hc, rpc.InProcessURL),
		Events:        ermiteconnect.NewEventServiceClient(hc, rpc.InProcessURL),
		PublicURL:     cfg.PublicURL,
		SourceURL:     cfg.SourceURL,
		Version:       serverVersion(),

		Sessions:       webSessions{sessions},
		Limiter:        limiter,
		TrustedProxies: cfg.TrustedProxies,
	})
	if err != nil {
		return nil, err
	}
	return asPrincipal(domain.Anonymous(), cfg.TrustedProxies, srv), nil
}

// webSessions adapts the session use cases to web.SessionAuth: a valid token
// gives the context of the owner, whose principal the API calls of the page
// carry (rpc.API.InProcessClient).
type webSessions struct{ *app.Sessions }

func (s webSessions) Login(ctx context.Context, nonce string) (string, time.Duration, error) {
	token, _, err := s.Sessions.Login(ctx, nonce)
	if errors.Is(err, domain.ErrUnauthenticated) {
		return "", 0, web.ErrLinkRejected
	}
	return token, app.SessionTTL, err
}

func (s webSessions) Verify(ctx context.Context, token string) (context.Context, bool, error) {
	p, err := s.Sessions.Verify(ctx, token)
	if errors.Is(err, domain.ErrUnauthenticated) {
		return ctx, false, nil
	}
	if err != nil {
		return ctx, false, err
	}
	return domain.ContextWithPrincipal(ctx, p), true, nil
}

// asPrincipal runs next with p as the caller of every request, attributed to
// the browser's address: the API calls the web makes carry both
// (rpc.API.InProcessClient), so the per-client limits of the API count each
// visitor. Browsers are anonymous; web.Server replaces the principal with the
// owner's when the request carries a valid session.
func asPrincipal(p domain.Principal, trusted []netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := domain.ContextWithPrincipal(r.Context(), p)
		ctx = rpc.ContextWithClient(ctx, clientlimit.ClientAddr(r, trusted))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// webRoutes mounts the web on the patterns it serves.
func webRoutes(h http.Handler) []httpserver.Route {
	routes := make([]httpserver.Route, 0, len(web.Patterns))
	for _, p := range web.Patterns {
		routes = append(routes, httpserver.Route{Pattern: p, Handler: h})
	}
	return routes
}
