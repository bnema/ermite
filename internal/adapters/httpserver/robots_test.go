// SPDX-License-Identifier: AGPL-3.0-or-later

package httpserver_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/httpserver"
)

// webCatchAll stands for the web, mounted on the method-less "/": it answers
// every path the server has no more specific route for.
var webCatchAll = httpserver.Route{Pattern: "/", Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Test-Web", "1")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("<html>not found</html>"))
})}

func TestRobotsTxtDefault(t *testing.T) {
	h := httpserver.New(httpserver.Config{Routes: []httpserver.Route{webCatchAll}}).Handler()

	rec := do(t, h, "GET", "/robots.txt")
	require.Equal(t, http.StatusOK, rec.Code, "the robots route wins over the web's catch-all")
	assert.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Equal(t, "public, max-age=3600", rec.Header().Get("Cache-Control"))
	assert.Equal(t, httpserver.DefaultRobotsTxt, rec.Body.String())
	for k, v := range securityHeaders {
		assert.Equal(t, v, rec.Header().Get(k), k)
	}
}

func TestRobotsTxtHead(t *testing.T) {
	h := httpserver.New(httpserver.Config{Routes: []httpserver.Route{webCatchAll}}).Handler()
	rec := do(t, h, "HEAD", "/robots.txt")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Equal(t, "public, max-age=3600", rec.Header().Get("Cache-Control"))
	assert.Empty(t, rec.Body.String())
}

func TestRobotsTxtOverride(t *testing.T) {
	body := "User-agent: *\nDisallow: /\n"
	h := httpserver.New(httpserver.Config{Routes: []httpserver.Route{webCatchAll}, RobotsTxt: []byte(body)}).Handler()
	rec := do(t, h, "GET", "/robots.txt")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, body, rec.Body.String())
}

// Only GET and HEAD reach the robots handler; another method falls through
// to the web, as in production.
func TestRobotsTxtOtherMethodsGoToTheWeb(t *testing.T) {
	h := httpserver.New(httpserver.Config{Routes: []httpserver.Route{webCatchAll}}).Handler()
	rec := do(t, h, "POST", "/robots.txt")
	assert.Equal(t, "1", rec.Header().Get("X-Test-Web"))
	assert.NotEqual(t, httpserver.DefaultRobotsTxt, rec.Body.String())
}
