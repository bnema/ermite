// SPDX-License-Identifier: AGPL-3.0-or-later

package httpserver

import (
	"net/http"

	"github.com/bnema/zerowrap"
)

// healthHandler answers 200 "ok" when every check passes and 503 otherwise.
// The failure is logged, never sent: the body stays the same for any cause.
func healthHandler(checks []Check) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		for _, check := range checks {
			if err := check(r.Context()); err != nil {
				log := zerowrap.FromCtx(r.Context())
				log.Warn().Err(err).Msg("health check failed")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("unavailable\n"))
				return
			}
		}
		_, _ = w.Write([]byte("ok\n"))
	}
}
