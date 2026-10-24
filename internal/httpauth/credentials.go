// SPDX-License-Identifier: AGPL-3.0-or-later

// Package httpauth decodes HTTP token credentials for inbound adapters.
package httpauth

import (
	"net/http"
	"strings"
)

// Credentials returns the Basic password or Bearer/token value. A malformed
// nonempty header is present with an empty secret, so authentication fails closed.
func Credentials(r *http.Request) (secret string, present bool) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", false
	}
	if _, pass, ok := r.BasicAuth(); ok {
		return pass, true
	}
	if scheme, tok, ok := strings.Cut(auth, " "); ok && (strings.EqualFold(scheme, "Bearer") || strings.EqualFold(scheme, "token")) {
		return strings.TrimSpace(tok), true
	}
	return "", true
}
