// SPDX-License-Identifier: AGPL-3.0-or-later

package httpserver

import (
	"net/http"
	"time"

	"github.com/bnema/zerowrap"
)

// contentSecurityPolicy allows scripts from the origin plus 'unsafe-eval'
// for htmx attribute expressions; inline scripts stay forbidden.
const contentSecurityPolicy = "default-src 'self'; script-src 'self' 'unsafe-eval'; frame-ancestors 'none'"

// withSecurityHeaders sets the security headers on every response, before
// the handler runs, so that errors and 404s carry them too.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// statusRecorder remembers the status code of a response.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach Flush and friends.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// withRequestLog logs method, path, status and duration at debug level. It
// never logs the query string or any header: they can carry credentials.
func withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		log := zerowrap.FromCtx(r.Context())
		log.Debug().
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", status).
			Dur("duration", time.Since(start)).
			Msg("request")
	})
}
