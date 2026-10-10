// SPDX-License-Identifier: AGPL-3.0-or-later

package client_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// redirectingServer answers every request with a redirect, like an http
// listener that forces https.
func redirectingServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.invalid"+r.URL.Path, http.StatusMovedPermanently)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
