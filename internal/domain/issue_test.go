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

func TestParseIssueTitle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, want, reason string // reason "" when valid
	}{
		{"plain", "Crash on start", "Crash on start", ""},
		{"max length in code points", strings.Repeat("é", 120), strings.Repeat("é", 120), ""},
		{"too long", strings.Repeat("a", 121), "", "longer than 120"},
		{"emoji count as one", strings.Repeat("🎉", 120), strings.Repeat("🎉", 120), ""},
		{"empty", "", "", "is empty"},
		{"blank", " \t ", "", "is empty"},
		{"only controls", "\x01\x02", "", "is empty"},
		{"newline", "a\nb", "", "one line"},
		{"carriage return", "a\rb", "", "one line"},
		{"nfc", "e\u0301", "\u00e9", ""},
		{"c0 stripped", "a\x00b\x1bc\tz", "abcz", ""},
		{"c1 stripped", "a\u0085b\u009fc", "abc", ""},
		{"del stripped", "a\x7fb", "ab", ""},
		{"bidi stripped", "a\u202ab\u202ec\u2066d\u2069e\u200ef\u200fg\u061ch", "abcdefgh", ""},
		{"not utf-8", "a\xffb", "", "UTF-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := domain.ParseIssueTitle(tt.in)
			if tt.reason == "" {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got.String())
				return
			}
			var te *domain.TextError
			require.True(t, errors.As(err, &te), "%v", err)
			assert.ErrorIs(t, err, domain.ErrInvalidText)
			assert.Contains(t, te.Reason, tt.reason)
		})
	}
}

func TestParseIssueBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, want, reason string
	}{
		{"empty allowed", "", "", ""},
		{"multiline and tabs kept", "a\n\tb\n", "a\n\tb\n", ""},
		{"crlf becomes lf", "a\r\nb", "a\nb", ""},
		{"max length", strings.Repeat("é", 10000), strings.Repeat("é", 10000), ""},
		{"too long", strings.Repeat("a", 10001), "", "longer than 10000"},
		{"nfc", "e\u0301\n", "\u00e9\n", ""},
		{"controls stripped", "a\x00\x08b\u0085c\u009fd\x7f", "abcd", ""},
		{"bidi stripped", "\u202etxt\u202c \u2067x\u2069", "txt x", ""},
		{"separators stripped", "a\u2028b\u2029c", "abc", ""},
		{"not utf-8", "\xc3\x28", "", "UTF-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := domain.ParseIssueBody(tt.in)
			if tt.reason == "" {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got.String())
				return
			}
			var te *domain.TextError
			require.True(t, errors.As(err, &te), "%v", err)
			assert.Contains(t, te.Reason, tt.reason)
		})
	}
}

func TestParseAuthorLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, want, reason string
	}{
		{"plain", "Ada", "Ada", ""},
		{"empty allowed", "", "", ""},
		{"max length", strings.Repeat("é", 40), strings.Repeat("é", 40), ""},
		{"too long", strings.Repeat("a", 41), "", "longer than 40"},
		{"newline", "a\nb", "", "one line"},
		{"nfc", "e\u0301", "\u00e9", ""},
		{"stripped", "A\u202ed\x00a", "Ada", ""},
		{"trimmed", "  Ada ", "Ada", ""},
		{"not utf-8", "\xff", "", "UTF-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := domain.ParseAuthorLabel(tt.in)
			if tt.reason == "" {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
				return
			}
			var te *domain.TextError
			require.True(t, errors.As(err, &te), "%v", err)
			assert.Contains(t, te.Reason, tt.reason)
		})
	}
}

func TestIssuesMode(t *testing.T) {
	t.Parallel()
	for _, m := range domain.IssuesModes() {
		got, err := domain.ParseIssuesMode(m.String())
		require.NoError(t, err)
		assert.Equal(t, m, got)
	}
	_, err := domain.ParseIssuesMode("all")
	assert.ErrorIs(t, err, domain.ErrInvalid)
	assert.Equal(t, domain.IssuesOff, domain.DefaultRepoSettings().Issues)
	assert.False(t, domain.DefaultRepoSettings().IsZero())
	assert.True(t, domain.RepoSettings{}.IsZero())
	assert.Equal(t, domain.IssuesOff, domain.IssuesMode("").OrDefault())
}

func TestIssueEventKinds(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "issue.opened", domain.IssueOpened{}.EventKind())
	assert.Equal(t, "issue.closed", domain.IssueClosed{}.EventKind())
	assert.Equal(t, "issue.reopened", domain.IssueReopened{}.EventKind())
	assert.Equal(t, "issue.commented", domain.IssueCommented{}.EventKind())
}

func TestIssueIsDone(t *testing.T) {
	t.Parallel()
	for _, state := range []domain.IssueState{"", "unknown", domain.IssueStateOpen, domain.IssueStateClosed, domain.IssueStateResolved} {
		t.Run(string(state), func(t *testing.T) {
			issue := domain.Issue{State: state}
			assert.Equal(t, state == domain.IssueStateClosed || state == domain.IssueStateResolved, issue.IsDone())
			assert.Equal(t, state == domain.IssueStateOpen, issue.IsOpen())
		})
	}
}

func TestIssueHistoryEventKinds(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "referenced", string(domain.IssueEventReferenced))
	assert.Equal(t, "closed", string(domain.IssueEventClosed))
	assert.Equal(t, "reopened", string(domain.IssueEventReopened))
	assert.Equal(t, "resolved", string(domain.IssueEventResolved))
}
