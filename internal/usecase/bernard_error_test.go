// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestKeepDepsError(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"legacy", " dependency command failed \n", "dependency command failed"},
		{"multiline", "failed (exit 1)\ngo: requires go >= 1.26", "failed (exit 1)\ngo: requires go >= 1.26"},
		{"controls", " \x00failed\r\n\toutput\x7f\u0085 ", "failed\noutput"},
		{"invalid UTF8", "failed\xff", "failed"},
		{"lines", strings.Repeat("line\n", 20), strings.TrimSpace(strings.Repeat("line\n", 12))},
		{"bytes", strings.Repeat("a", 3000), strings.Repeat("a", maxDepsError)},
		{"rune boundary", strings.Repeat("€", 1000), strings.Repeat("€", maxDepsError/3)},
		{"empty", "\x00 \n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := keepDepsError(tc.in)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			if len(got) > maxDepsError || !utf8.ValidString(got) || strings.Count(got, "\n") >= 12 {
				t.Fatal("invalid bounds")
			}
		})
	}
}
