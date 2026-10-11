// SPDX-License-Identifier: AGPL-3.0-or-later

package httpserver

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/bnema/zerowrap"
)

// healthCacheTTL is how long the result of the checks is reused. The endpoint
// is unauthenticated and the checks touch the disk and the store: a burst of
// requests must not turn into a burst of work.
const healthCacheTTL = 5 * time.Second

// healthCheckTimeout bounds one run of the checks.
const healthCheckTimeout = 5 * time.Second

// healthHandler answers 200 "ok" when every check passes and 503 otherwise.
// The failure is logged, never sent: the body stays the same for any cause.
// The result, failure included, is kept for healthCacheTTL; concurrent
// requests wait for the run in progress rather than starting their own.
func healthHandler(checks []Check) http.HandlerFunc {
	var (
		mu      sync.Mutex
		until   time.Time // the cached result is valid before this instant
		healthy bool
	)
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if !time.Now().Before(until) {
			// The run belongs to every waiting request, not to this one: a
			// client that goes away must not cache a failure.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), healthCheckTimeout)
			healthy = runChecks(ctx, checks)
			cancel()
			until = time.Now().Add(healthCacheTTL)
		}
		ok := healthy
		mu.Unlock()

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("unavailable\n"))
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	}
}

// runChecks runs the checks in order and stops at the first failure, which it
// logs.
func runChecks(ctx context.Context, checks []Check) bool {
	for _, check := range checks {
		if err := check(ctx); err != nil {
			log := zerowrap.FromCtx(ctx)
			log.Warn().Err(err).Msg("health check failed")
			return false
		}
	}
	return true
}
