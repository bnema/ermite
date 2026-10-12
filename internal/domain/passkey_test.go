// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestParsePasskeyName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		in     string
		reason string // "" when valid
	}{
		{"plain", "Pixel 9", ""},
		{"unicode", "clé de ma mère 🔑", ""},
		{"one byte", "a", ""},
		{"max bytes", strings.Repeat("a", domain.MaxPasskeyNameLen), ""},
		{"too long", strings.Repeat("a", domain.MaxPasskeyNameLen+1), "longer than 64"},
		{"too long in bytes, short in characters", strings.Repeat("é", 33), "longer than 64 bytes"},
		{"empty", "", "is empty"},
		{"blank", "  \u00a0 ", "is empty"},
		{"line break", "two\nlines", "one line"},
		{"tab", "a\tb", "control character U+0009"},
		{"nul", "a\x00b", "control character U+0000"},
		{"escape", "\x1b[31mred", "control character U+001B"},
		{"bidi override", "evil \u202e txt.exe", "U+202E"},
		{"bidi isolate", "a\u2066b", "U+2066"},
		{"line separator", "a\u2028b", "U+2028"},
		{"invalid utf-8", "a\xffb", "not valid UTF-8"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := domain.ParsePasskeyName(tc.in)
			if tc.reason == "" {
				require.NoError(t, err)
				assert.Equal(t, tc.in, got.String())
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, domain.ErrInvalidText)
			assert.True(t, domain.IsInvalid(err))
			assert.Contains(t, err.Error(), tc.reason)
		})
	}
}

func TestDefaultPasskeyNameIsValid(t *testing.T) {
	t.Parallel()
	_, err := domain.ParsePasskeyName(domain.DefaultPasskeyName)
	require.NoError(t, err)
}
