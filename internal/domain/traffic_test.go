// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import "testing"

func TestIsBotUserAgent(t *testing.T) {
	for _, ua := range []string{"", "  ", "GoogleBOT", "crawler", "spider", "Slurp", "facebookexternalhit", "GPTBot", "ClaudeBot", "CCBot", "Bytespider", "Ahrefs", "Semrush", "PetalBot", "Yandex", "bingpreview", "HeadlessChrome"} {
		if !IsBotUserAgent(ua) {
			t.Errorf("IsBotUserAgent(%q) = false", ua)
		}
	}
	for _, ua := range []string{"Mozilla/5.0 Firefox/130.0", "curl/8.0", "Wget/1.21", "git/2.47"} {
		if IsBotUserAgent(ua) {
			t.Errorf("IsBotUserAgent(%q) = true", ua)
		}
	}
}

func TestReferrerDomain(t *testing.T) {
	for _, tt := range []struct{ ref, self, want string }{
		{"https://WWW.Example.COM/path?q=private#fragment", "forge.test", "example.com"},
		{"https://example.com:8443/path", "forge.test", "example.com"},
		{"https://www.forge.test/path", "FORGE.test:443", ""},
		{"https://forge.test./path", "www.forge.test", ""},
		{"https://other.test/forge.test", "forge.test", "other.test"},
		{"https://[2001:db8::1]:443/path", "[2001:db8::1]:8080", ""},
		{"", "forge.test", ""},
		{"/relative", "forge.test", ""},
		{"//example.com/path", "forge.test", ""},
		{"ftp://example.com/path", "forge.test", ""},
		{"https://%zz/path", "forge.test", ""},
		{"https:///path", "forge.test", ""},
	} {
		t.Run(tt.ref, func(t *testing.T) {
			if got := ReferrerDomain(tt.ref, tt.self); got != tt.want {
				t.Errorf("ReferrerDomain(%q, %q) = %q, want %q", tt.ref, tt.self, got, tt.want)
			}
		})
	}
}
