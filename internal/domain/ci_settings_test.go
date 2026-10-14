// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestParseSettingName(t *testing.T) {
	t.Parallel()
	ok := map[string]string{"a": "A", "My_Secret1": "MY_SECRET1", "_x": "_X", "X": "X", strings.Repeat("a", 255): strings.Repeat("A", 255)}
	for in, want := range ok {
		got, err := domain.ParseSettingName(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, "stored upper-cased")
	}
	for _, bad := range []string{"", "1abc", "a-b", "a b", "a.b", "é", "GITHUB_TOKEN", "github_x", "Github_", strings.Repeat("a", 256), "a\n"} {
		_, err := domain.ParseSettingName(bad)
		require.Error(t, err, bad)
		assert.True(t, domain.IsInvalid(err), bad)
	}
	// "GITHUB" alone, and names that merely contain the prefix, are allowed.
	for _, fine := range []string{"GITHUB", "MY_GITHUB_X", "GITHUBX"} {
		_, err := domain.ParseSettingName(fine)
		assert.NoError(t, err, fine)
	}
}

func TestSettingValueSize(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 49152, domain.MaxSettingValueBytes, "48 KB as docs.github.com says it, taken as 48 KiB")
	assert.NoError(t, domain.CheckSecretValue("x"))
	assert.NoError(t, domain.CheckSecretValue(strings.Repeat("x", 49152)))
	assert.Error(t, domain.CheckSecretValue(strings.Repeat("x", 49153)))
	assert.Error(t, domain.CheckSecretValue(""), "an empty secret is refused")
	assert.NoError(t, domain.CheckVariableValue(""), "an empty variable is allowed")
	assert.NoError(t, domain.CheckVariableValue(strings.Repeat("x", 49152)))
	assert.Error(t, domain.CheckVariableValue(strings.Repeat("x", 49153)))
}

func TestParseEnvironmentName(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"production", "Staging 2", "prod/eu", "é", strings.Repeat("é", 255)} {
		_, err := domain.ParseEnvironmentName(ok)
		assert.NoError(t, err, ok)
	}
	for _, bad := range []string{"", "a\nb", "a\x00", "a\u202eb", strings.Repeat("é", 256), "\xff", " lead", "trail ", "\tx", "x\u00a0", " "} {
		_, err := domain.ParseEnvironmentName(bad)
		require.Error(t, err, bad)
		assert.True(t, domain.IsInvalid(err), bad)
	}
}

func TestSettingsScopeKeysAreDistinctAndFoldEnvironmentCase(t *testing.T) {
	t.Parallel()
	repo, _ := domain.ParseRepoName("proj")
	assert.Equal(t, "i", domain.InstanceScope().Key())
	assert.Equal(t, "r/proj", domain.RepoScope(repo).Key())
	assert.Equal(t, domain.EnvironmentScope(repo, "Prod").Key(), domain.EnvironmentScope(repo, "prod").Key(), "environments are unique without regard to case")
	assert.NotContains(t, domain.EnvironmentScope(repo, "a/b").Key()[len("e/proj/"):], "/", "no separator inside the environment part")
	assert.NotEqual(t, domain.EnvironmentScope(repo, "a/b").Key(), domain.EnvironmentScope(repo, "a%2Fb").Key())
	assert.False(t, domain.SettingsScope{}.Valid())
	assert.False(t, domain.EnvironmentScope(repo, "").Valid())
	assert.True(t, domain.EnvironmentScope(repo, "x").Valid())
}

func TestSecretAADBindsScopeAndName(t *testing.T) {
	t.Parallel()
	repo, _ := domain.ParseRepoName("proj")
	a := domain.SecretAAD(domain.RepoScope(repo), "TOKEN")
	assert.NotEqual(t, a, domain.SecretAAD(domain.RepoScope(repo), "OTHER"))
	assert.NotEqual(t, a, domain.SecretAAD(domain.InstanceScope(), "TOKEN"))
	assert.NotEqual(t, a, domain.SecretAAD(domain.EnvironmentScope(repo, "prod"), "TOKEN"))
}
