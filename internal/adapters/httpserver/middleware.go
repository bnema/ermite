// SPDX-License-Identifier: AGPL-3.0-or-later

package httpserver

import (
	"net/http"
	"strings"
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

// Flush lets streaming handlers (Connect server streams require an
// http.Flusher) push their messages out as they are written.
func (s *statusRecorder) Flush() {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach Flush and friends.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// loginPathPrefix is where the web serves one-time login links: the rest of
// the path is a secret.
const loginPathPrefix = "/-/login/"

// loggedPath is the path of a request as the log may show it.
func loggedPath(p string) string {
	if strings.HasPrefix(p, loginPathPrefix) {
		return loginPathPrefix + "…"
	}
	return p
}

// withRequestLog logs method, path, status and duration at debug level. It
// never logs the query string or any header: they can carry credentials. The
// secret of a login link is cut from the path.
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
			Str("path", loggedPath(r.URL.Path)).
			Int("status", status).
			Dur("duration", time.Since(start)).
			Msg("request")
	})
}
