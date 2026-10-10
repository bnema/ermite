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

func TestParseRepoName(t *testing.T) {
	tests := []struct {
		in         string
		valid      bool
		suggestion string
	}{
		{in: "ermite", valid: true},
		{in: "a", valid: true},
		{in: "my-repo_1.0", valid: true},
		{in: "x.y", valid: true},
		{in: "0", valid: true},
		{in: strings.Repeat("a", 100), valid: true},
		{in: strings.Repeat("a", 101)},
		{in: ""},
		{in: "Ermite", suggestion: "ermite"},
		{in: "My-Repo", suggestion: "my-repo"},
		{in: "UPPER.git"}, // lowercase would still end in .git: no suggestion
		{in: ".hidden"},
		{in: "trailing."},
		{in: "a..b"},
		{in: "tools.git"},
		{in: "-"},
		{in: "robots.txt"},
		{in: "favicon.ico"},
		{in: "-dash-ok", valid: true},
		{in: "has space"},
		{in: "a/b"},
		{in: "caf\u00e9"},
		{in: "a\x00b"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			n, err := domain.ParseRepoName(tt.in)
			if tt.valid {
				require.NoError(t, err)
				assert.Equal(t, tt.in, n.String())
				assert.False(t, n.IsZero())
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, domain.ErrInvalidName)
			assert.True(t, n.IsZero())
			var ine *domain.InvalidNameError
			require.True(t, errors.As(err, &ine))
			assert.Equal(t, tt.suggestion, ine.Suggestion)
			if tt.suggestion != "" {
				assert.Contains(t, err.Error(), tt.suggestion)
			}
		})
	}
}

func TestParseRepoPath(t *testing.T) {
	tests := []struct {
		in    string
		want  string
		valid bool
	}{
		{"tools", "tools", true},
		{"tools.git", "tools", true},
		{"tools.git.git", "", false}, // only one suffix is stripped
		{".git", "", false},
		{"Tools.git", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			n, err := domain.ParseRepoPath(tt.in)
			if !tt.valid {
				assert.ErrorIs(t, err, domain.ErrInvalidName)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, n.String())
		})
	}
}

func TestParseVisibility(t *testing.T) {
	for in, want := range map[string]domain.Visibility{"public": domain.VisibilityPublic, "private": domain.VisibilityPrivate} {
		got, err := domain.ParseVisibility(in)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	for _, in := range []string{"", "Public", "secret"} {
		_, err := domain.ParseVisibility(in)
		assert.ErrorIs(t, err, domain.ErrInvalidVisibility, in)
	}
}

func TestPrincipal(t *testing.T) {
	var zero domain.Principal
	assert.True(t, zero.IsAnonymous())
	assert.False(t, zero.IsRoot())
	assert.True(t, domain.Root().IsRoot())
	assert.Equal(t, "anonymous", domain.Anonymous().Label())
	assert.False(t, domain.Root().IsToken())
}
