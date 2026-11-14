// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"strings"
	"testing"
)

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

func TestReferrerDomainValidation(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "[::1]", "[::ffff:192.0.2.1]", "[fe80::1%25eth0]", "localhost", "bad_label.test", "-bad.test", "bad-.test", "bad..test", strings.Repeat("a", 64) + ".test", strings.Repeat("a.", 127) + "test"} {
		if got := ReferrerDomain("https://"+host+"/", "forge.test"); got != "" {
			t.Errorf("accepted %q: %q", host, got)
		}
	}
	if got := ReferrerDomain("https://example.com/"+strings.Repeat("x", 2048), ""); got != "" {
		t.Errorf("accepted oversized URL: %q", got)
	}
	for _, host := range []string{"xn--bcher-kva.example", strings.Repeat("a", 63) + ".test", strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)} {
		if got := ReferrerDomain("https://"+host, ""); got != host {
			t.Errorf("rejected valid host %q: %q", host, got)
		}
	}
}
