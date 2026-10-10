// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"net/http"

	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/adapters/httpserver"
	"bnema.dev/ermite/internal/adapters/rpc"
	"bnema.dev/ermite/internal/adapters/web"
	"bnema.dev/ermite/internal/config"
	"bnema.dev/ermite/internal/domain"
)

// newWeb builds the web pages on the API of this process: the generated
// Connect clients are bound to the handlers of api, with no socket (ADR 0020).
func newWeb(cfg config.Config, api *rpc.API) (http.Handler, error) {
	hc := api.InProcessClient()
	srv, err := web.New(web.Config{
		Repos:     ermiteconnect.NewRepoServiceClient(hc, rpc.InProcessURL),
		Git:       ermiteconnect.NewGitServiceClient(hc, rpc.InProcessURL),
		PublicURL: cfg.PublicURL,
		SourceURL: cfg.SourceURL,
		Version:   serverVersion(),
	})
	if err != nil {
		return nil, err
	}
	return asPrincipal(domain.Anonymous(), srv), nil
}

// asPrincipal runs next with p as the caller of every request: the API calls
// the web makes carry it (rpc.API.InProcessClient). Browsers are anonymous
// until the web login of P5b brings a session.
func asPrincipal(p domain.Principal, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(domain.ContextWithPrincipal(r.Context(), p)))
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
