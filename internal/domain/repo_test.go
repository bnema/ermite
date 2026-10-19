// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestNormalizeTopics(t *testing.T) {
	t.Parallel()
	many := func(n int) []string {
		var out []string
		for i := range n {
			out = append(out, "t"+strings.Repeat("x", i))
		}
		return out
	}
	tests := []struct {
		name string
		in   []string
		want []string
		bad  bool
	}{
		{name: "nil", in: nil, want: nil},
		{name: "empty", in: []string{}, want: nil},
		{name: "lowercased", in: []string{"Go"}, want: []string{"go"}},
		{name: "sorted", in: []string{"zig", "c", "go"}, want: []string{"c", "go", "zig"}},
		{name: "deduplicated after lowercasing", in: []string{"Go", "go", "GO"}, want: []string{"go"}},
		{name: "digits and hyphens", in: []string{"3d-printing", "0"}, want: []string{"0", "3d-printing"}},
		{name: "50 characters", in: []string{strings.Repeat("a", 50)}, want: []string{strings.Repeat("a", 50)}},
		{name: "exactly 20", in: many(20), want: many(20)},
		{name: "duplicates do not count twice", in: append(many(20), "t", "tx"), want: many(20)},
		{name: "21", in: many(21), bad: true},
		{name: "51 characters", in: []string{strings.Repeat("a", 51)}, bad: true},
		{name: "empty topic", in: []string{""}, bad: true},
		{name: "leading hyphen", in: []string{"-go"}, bad: true},
		{name: "underscore", in: []string{"a_b"}, bad: true},
		{name: "space", in: []string{"a b"}, bad: true},
		{name: "dot", in: []string{"a.b"}, bad: true},
		{name: "non-ASCII", in: []string{"café"}, bad: true},
		{name: "one bad among good", in: []string{"go", "no!"}, bad: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := domain.NormalizeTopics(tt.in)
			if tt.bad {
				require.ErrorIs(t, err, domain.ErrInvalid)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRepoEffectiveSettings(t *testing.T) {
	custom := domain.RepoSettings{AllowedMergeModes: []domain.MergeMode{domain.MergeMode("squash")}, Issues: domain.IssuesPublic}
	for _, tc := range []struct {
		name           string
		settings, want domain.RepoSettings
	}{
		{"unset", domain.RepoSettings{}, domain.DefaultRepoSettings()},
		{"custom", custom, custom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := domain.Repo{Settings: tc.settings}
			assert.Equal(t, tc.want, repo.EffectiveSettings())
			assert.Equal(t, tc.settings, repo.Settings)
		})
	}
}

func TestRepoIssuePresentationRules(t *testing.T) {
	for _, tc := range []struct {
		mode             domain.IssuesMode
		enabled, receipt bool
	}{
		{"", false, false}, {domain.IssuesOff, false, false},
		{domain.IssuesOwner, true, false}, {domain.IssuesPublic, true, true},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			s := domain.RepoSettings{Issues: tc.mode}
			assert.Equal(t, tc.enabled, s.IssuesEnabled())
			assert.Equal(t, tc.receipt, s.ShowsIssueSubmissionReceipt())
			assert.Equal(t, tc.receipt, s.AcceptsAnonymousIssues())
		})
	}
}
