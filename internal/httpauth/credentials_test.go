// SPDX-License-Identifier: AGPL-3.0-or-later

package httpauth

import (
	"encoding/base64"
	"net/http/httptest"
	"testing"
)

func TestCredentials(t *testing.T) {
	basic := func(s string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte(s)) }
	for _, tc := range []struct {
		name, header, secret string
		present              bool
	}{
		{"absent", "", "", false},
		{"arbitrary username", basic("anything:secret"), "secret", true},
		{"empty username", basic(":secret"), "secret", true},
		{"empty password", basic("user:"), "", true},
		{"malformed base64", "Basic !!!", "", true},
		{"missing colon", basic("user"), "", true},
		{"case insensitive basic", "bAsIc " + base64.StdEncoding.EncodeToString([]byte("u:secret")), "secret", true},
		{"case insensitive bearer", "bEaReR secret", "secret", true},
		{"case insensitive token", "tOkEn secret", "secret", true},
		{"extra whitespace", "Bearer   secret \t", "secret", true},
		{"unsupported", "Digest secret", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			secret, present := Credentials(r)
			if secret != tc.secret || present != tc.present {
				t.Fatalf("got (%q, %t), want (%q, %t)", secret, present, tc.secret, tc.present)
			}
		})
	}
}
