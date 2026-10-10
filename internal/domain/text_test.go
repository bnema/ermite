// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

// textCases are the rules shared by every text; multi says whether line breaks
// are allowed.
func TestParseMRTitle(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		reason string // "" when valid
	}{
		{"plain", "Add the login page", ""},
		{"unicode", "Ajoute la page d’accueil — ça marche 🎉", ""},
		{"max length in characters", strings.Repeat("é", domain.MaxTitleLen), ""},
		{"too long", strings.Repeat("a", domain.MaxTitleLen+1), "longer than 120"},
		{"empty", "", "is empty"},
		{"blank", "  \u00a0 ", "is empty"},
		{"line break", "two\nlines", "one line"},
		{"carriage return", "a\rb", "control character U+000D"},
		{"tab", "a\tb", "control character U+0009"},
		{"nul", "a\x00b", "control character U+0000"},
		{"escape", "\x1b[31mred", "control character U+001B"},
		{"delete", "a\x7fb", "control character U+007F"},
		{"c1 control", "a\u0085b", "control character U+0085"},
		{"invalid utf-8", "a\xffb", "not valid UTF-8"},
		{"bidi override", "evil \u202e txt.exe", "U+202E"},
		{"bidi embedding", "a\u202ab", "U+202A"},
		{"bidi isolate", "a\u2066b", "U+2066"},
		{"bidi pop isolate", "a\u2069b", "U+2069"},
		{"line separator", "a\u2028b", "U+2028"},
		{"paragraph separator", "a\u2029b", "U+2029"},
		{"left-to-right mark", "a\u200eb", "U+200E"},
		{"right-to-left mark", "a\u200fb", "U+200F"},
		{"arabic letter mark", "a\u061cb", "U+061C"},
		{"zero width joiner is plain text", "a\u200db", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.ParseMRTitle(tc.in)
			if tc.reason == "" {
				require.NoError(t, err)
				assert.Equal(t, tc.in, got.String())
				assert.False(t, got.IsZero())
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, domain.ErrInvalidText)
			assert.Contains(t, err.Error(), tc.reason)
			assert.True(t, got.IsZero())
		})
	}
}

func TestParsePlainText(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		reason string
	}{
		{"empty is valid", "", ""},
		{"multi-line", "one\ntwo\n\nthree", ""},
		{"trailing newline", "x\n", ""},
		{"max length", strings.Repeat("a\n", domain.MaxTextLen/2), ""},
		{"max length in characters not bytes", strings.Repeat("€", domain.MaxTextLen), ""},
		{"too long", strings.Repeat("a", domain.MaxTextLen+1), "longer than 8000"},
		{"carriage return", "a\r\nb", "control character U+000D"},
		{"tab", "a\tb", "control character U+0009"},
		{"escape sequence", "a\x1b]0;title\x07", "control character U+001B"},
		{"bell", "a\x07", "control character U+0007"},
		{"invalid utf-8", "ok\n\xc3\x28", "not valid UTF-8"},
		{"bidi override", "x\u202ey", "U+202E"},
		{"bidi left-to-right isolate", "x\u2066y", "U+2066"},
		{"bidi first strong isolate", "x\u2068y", "U+2068"},
		{"line separator", "x\u2028y", "U+2028"},
		{"paragraph separator", "x\u2029y", "U+2029"},
		{"left-to-right mark", "x\u200ey", "U+200E"},
		{"right-to-left mark", "x\u200fy", "U+200F"},
		{"arabic letter mark", "x\u061cy", "U+061C"},
		{"zero width joiner is plain text", "x\u200dy", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.ParsePlainText(tc.in)
			if tc.reason == "" {
				require.NoError(t, err)
				assert.Equal(t, tc.in, got.String())
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, domain.ErrInvalidText)
			assert.Contains(t, err.Error(), tc.reason)
			assert.Empty(t, got.String())
		})
	}
}

func TestPlainTextIsBlank(t *testing.T) {
	for in, blank := range map[string]bool{"": true, " \n\n ": true, "x": false, " x ": false} {
		got, err := domain.ParsePlainText(in)
		require.NoError(t, err)
		assert.Equal(t, blank, got.IsBlank(), "%q", in)
	}
}

func TestViolations(t *testing.T) {
	var v domain.Violations
	require.NoError(t, v.Err())

	_, err := domain.ParseMRTitle("a\nb")
	v.AddErr("title", err)
	v.Add("source_branch", "does not exist")
	v.AddErr("text", errors.New("boom"))

	err = v.Err()
	require.ErrorIs(t, err, domain.ErrInvalid)
	var fe *domain.InvalidFieldsError
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, []domain.FieldViolation{
		{Field: "title", Reason: "must be one line"},
		{Field: "source_branch", Reason: "does not exist"},
		{Field: "text", Reason: "boom"},
	}, fe.Fields)
	assert.Equal(t, "invalid argument: title must be one line; source_branch does not exist; text boom", err.Error())
}
