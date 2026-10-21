// SPDX-License-Identifier: AGPL-3.0-or-later

package client_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/client"
)

func TestScopeWireNames(t *testing.T) {
	want := map[ermite.Scope]string{
		ermite.Scope_SCOPE_MR_FORCE_MERGE:  "mr:force-merge",
		ermite.Scope_SCOPE_MR_REVIEW:       "mr:review",
		ermite.Scope_SCOPE_REPO_READ:       "repo:read",
		ermite.Scope_SCOPE_REPO_WRITE:      "repo:write",
		ermite.Scope_SCOPE_REPO_ADMIN:      "repo:admin",
		ermite.Scope_SCOPE_MR_WRITE:        "mr:write",
		ermite.Scope_SCOPE_MR_MERGE:        "mr:merge",
		ermite.Scope_SCOPE_CI_WRITE:        "ci:write",
		ermite.Scope_SCOPE_SECRETS_WRITE:   "secrets:write",
		ermite.Scope_SCOPE_RUNNER_REGISTER: "runner:register",
		ermite.Scope_SCOPE_TOKEN_ADMIN:     "token:admin",
		ermite.Scope_SCOPE_ISSUE_WRITE:     "issue:write",
	}
	for s, name := range want {
		assert.Equal(t, name, client.ScopeName(s))
		got, err := client.ParseScope(strings.ToUpper(name))
		require.NoError(t, err)
		assert.Equal(t, s, got)
	}
	assert.Len(t, client.AllScopes(), len(want), "the cli preset covers every scope of the contract")
}

func TestPresetExpansion(t *testing.T) {
	cli, err := client.PresetScopes("cli")
	require.NoError(t, err)
	assert.Equal(t, client.AllScopes(), cli)
	assert.NotContains(t, cli, ermite.Scope_SCOPE_UNSPECIFIED)

	agent, err := client.PresetScopes("agent")
	require.NoError(t, err)
	assert.NotContains(t, agent, ermite.Scope_SCOPE_ISSUE_WRITE, "agents get issue:write only when asked")
	assert.Equal(t, []ermite.Scope{ermite.Scope_SCOPE_REPO_WRITE, ermite.Scope_SCOPE_MR_WRITE}, agent)

	ci, err := client.PresetScopes("CI")
	require.NoError(t, err)
	assert.Equal(t, []ermite.Scope{ermite.Scope_SCOPE_REPO_READ}, ci)

	_, err = client.PresetScopes("root")
	require.ErrorContains(t, err, "unknown preset")
}

func TestMergeScopes(t *testing.T) {
	got, err := client.MergeScopes("ci", []string{"repo:write", "repo:read"})
	require.NoError(t, err)
	assert.Equal(t, []ermite.Scope{ermite.Scope_SCOPE_REPO_READ, ermite.Scope_SCOPE_REPO_WRITE}, got, "preset and scopes add up, sorted, no duplicates")

	got, err = client.MergeScopes("", []string{"token:admin"})
	require.NoError(t, err)
	assert.Equal(t, []ermite.Scope{ermite.Scope_SCOPE_TOKEN_ADMIN}, got)

	_, err = client.MergeScopes("", nil)
	require.ErrorContains(t, err, "--preset")
	_, err = client.MergeScopes("", []string{"repo:everything"})
	require.ErrorContains(t, err, "unknown scope")
}

func TestParseExpiry(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Time
	}{
		{"30d", now.Add(30 * 24 * time.Hour)},
		{"2w", now.Add(14 * 24 * time.Hour)},
		{"12h", now.Add(12 * time.Hour)},
		{"1h30m", now.Add(90 * time.Minute)},
		{"2027-01-31", time.Date(2027, 1, 31, 23, 59, 59, 0, time.UTC)},
		{"2027-01-31T10:00:00+02:00", time.Date(2027, 1, 31, 8, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		got, err := client.ParseExpiry(tt.in, now)
		require.NoError(t, err, tt.in)
		assert.True(t, tt.want.Equal(got), "%s: %s != %s", tt.in, tt.want, got)
	}
	for _, in := range []string{"", "soon", "0d", "-3d", "d", "1.5d", "2026-10-09", "-1h", "99999999d"} {
		_, err := client.ParseExpiry(in, now)
		require.Error(t, err, in)
		assert.Contains(t, err.Error(), "--expires")
	}
}
