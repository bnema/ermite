// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestScopes(t *testing.T) {
	t.Parallel()
	all := domain.AllScopes()
	assert.Len(t, all, 10)
	assert.Contains(t, all, domain.ScopeIssueWrite)
	got, err := domain.ParseScope("issue:write")
	require.NoError(t, err)
	assert.Equal(t, domain.ScopeIssueWrite, got)
	for _, s := range all {
		got, err := domain.ParseScope(s.String())
		require.NoError(t, err)
		assert.Equal(t, s, got)
	}
	for _, in := range []string{"", "repo", "repo:READ", "admin", "repo:read "} {
		_, err := domain.ParseScope(in)
		assert.ErrorIs(t, err, domain.ErrInvalidScope, in)
	}
}

func TestHashTokenSecret(t *testing.T) {
	t.Parallel()
	h := domain.HashTokenSecret("ermite_abc")
	assert.Equal(t, h, domain.HashTokenSecret("ermite_abc"))
	assert.NotEqual(t, h, domain.HashTokenSecret("ermite_abd"))
	assert.Len(t, h.String(), 64)
	// SHA-256("") known value.
	assert.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", domain.HashTokenSecret("").String())
}

func TestTokenExpired(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)
	assert.False(t, domain.Token{}.Expired(now), "no expiry never expires")
	assert.False(t, domain.Token{ExpiresAt: now.Add(time.Second)}.Expired(now))
	assert.True(t, domain.Token{ExpiresAt: now}.Expired(now), "expires at the instant")
	assert.True(t, domain.Token{ExpiresAt: now.Add(-time.Second)}.Expired(now))
}

func TestPrincipalScopes(t *testing.T) {
	t.Parallel()
	a, b := mustName(t, "a"), mustName(t, "b")
	tok := domain.Token{ID: "t1", Scopes: []domain.Scope{domain.ScopeRepoRead, domain.ScopeRepoWrite}, Repos: []domain.RepoName{a}}
	p := domain.TokenPrincipal(tok)

	assert.Equal(t, "token", p.Label())
	assert.Equal(t, "t1", p.TokenID())
	assert.False(t, p.IsRoot())
	assert.False(t, p.IsAnonymous())
	assert.True(t, p.HasScope(domain.ScopeRepoWrite))
	assert.False(t, p.HasScope(domain.ScopeTokenAdmin))
	assert.True(t, p.Allows(domain.ScopeRepoWrite, a))
	assert.False(t, p.Allows(domain.ScopeRepoWrite, b), "outside the repository list")
	assert.False(t, p.Allows(domain.ScopeRepoAdmin, a), "scope not held")

	// Without a list, every repository.
	all := domain.TokenPrincipal(domain.Token{Scopes: []domain.Scope{domain.ScopeRepoRead}})
	assert.True(t, all.Allows(domain.ScopeRepoRead, a))
	assert.True(t, all.Allows(domain.ScopeRepoRead, b))

	// The principal owns copies.
	tok.Scopes[0] = domain.ScopeTokenAdmin
	assert.True(t, p.HasScope(domain.ScopeRepoRead))
	p.Scopes()[0] = domain.ScopeTokenAdmin
	assert.True(t, p.HasScope(domain.ScopeRepoRead))

	assert.True(t, domain.Root().Allows(domain.ScopeTokenAdmin, a))
	assert.False(t, domain.Anonymous().HasScope(domain.ScopeRepoRead))
	assert.False(t, domain.Anonymous().Allows(domain.ScopeRepoRead, a))
	assert.Equal(t, "root", domain.Root().Label())
	assert.Equal(t, "anonymous", domain.Anonymous().Label())
}

func mustName(t *testing.T, s string) domain.RepoName {
	t.Helper()
	n, err := domain.ParseRepoName(s)
	require.NoError(t, err)
	return n
}

func TestOwnerPrincipal(t *testing.T) {
	t.Parallel()
	o := domain.Owner()
	assert.Equal(t, "owner", o.Label())
	assert.True(t, o.IsOwner())
	assert.False(t, domain.Root().IsOwner())
	assert.True(t, o.IsRoot(), "the owner has the rights of root")
	assert.True(t, o.HasScope(domain.ScopeMRMerge))
	assert.True(t, o.Allows(domain.ScopeRepoRead, mustName(t, "any")))
	assert.False(t, o.IsAnonymous())
	assert.Empty(t, o.TokenID())
}

func TestHasOwnerRights(t *testing.T) {
	t.Parallel()
	a := mustName(t, "a")
	all := domain.AllScopes()
	assert.True(t, domain.Root().HasOwnerRights())
	assert.True(t, domain.Owner().HasOwnerRights())
	assert.False(t, domain.Anonymous().HasOwnerRights())
	assert.True(t, domain.TokenPrincipal(domain.Token{Scopes: all}).HasOwnerRights())
	assert.False(t, domain.TokenPrincipal(domain.Token{Scopes: all[1:]}).HasOwnerRights(), "a scope short")
	assert.False(t, domain.TokenPrincipal(domain.Token{Scopes: all[:9]}).HasOwnerRights(), "without issue:write")
	assert.False(t, domain.TokenPrincipal(domain.Token{Scopes: all, Repos: []domain.RepoName{a}}).HasOwnerRights(), "limited to a repository")
}

func TestParseBranchPrefix(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"agent/", "agent/claude/", "a/b/c/", "feat-1/", strings.Repeat("a", 99) + "/"} {
		got, err := domain.ParseBranchPrefix(ok)
		require.NoError(t, err, ok)
		assert.Equal(t, ok, got)
	}
	for _, bad := range []string{
		"", "/", "agent", "agent/claude", "//", "agent//", "/agent/", "-x/", ".x/", "a/.b/", "a..b/",
		"a b/", "a~/", "a:/", "a?/", "a*/", "a[/", "a\\/", "a@{/", "x.lock/", "a\x00/", "a\n/", "\xff/",
		"refs/heads/agent/", "agent.//", strings.Repeat("a", 100) + "/",
	} {
		_, err := domain.ParseBranchPrefix(bad)
		assert.ErrorIs(t, err, domain.ErrInvalidToken, "%q", bad)
	}
}

func TestParseBranchPrefixes(t *testing.T) {
	t.Parallel()
	got, err := domain.ParseBranchPrefixes(nil)
	require.NoError(t, err)
	assert.Empty(t, got)

	got, err = domain.ParseBranchPrefixes([]string{"b/", "a/", "b/"})
	require.NoError(t, err)
	assert.Equal(t, []string{"b/", "a/"}, got, "duplicates removed, order kept")

	eight := []string{"a/", "b/", "c/", "d/", "e/", "f/", "g/", "h/"}
	got, err = domain.ParseBranchPrefixes(eight)
	require.NoError(t, err)
	assert.Len(t, got, 8)
	_, err = domain.ParseBranchPrefixes(append(eight, "i/"))
	assert.ErrorIs(t, err, domain.ErrInvalidToken)
	got, err = domain.ParseBranchPrefixes(append(eight, "a/"))
	require.NoError(t, err, "duplicates do not count")
	assert.Len(t, got, 8)

	_, err = domain.ParseBranchPrefixes([]string{"a/", "bad"})
	assert.ErrorIs(t, err, domain.ErrInvalidToken)
}

func TestAllowsRef(t *testing.T) {
	t.Parallel()
	limited := domain.TokenPrincipal(domain.Token{
		Scopes: []domain.Scope{domain.ScopeRepoWrite}, BranchPrefixes: []string{"agent/claude/", "bot/"},
	})
	for ref, want := range map[string]bool{
		"refs/heads/agent/claude/x":       true,
		"refs/heads/agent/claude/a/b":     true,
		"refs/heads/bot/y":                true,
		"refs/heads/agent/claude":         false,
		"refs/heads/agent/claude/":        false,
		"refs/heads/agent/claudex/y":      false,
		"refs/heads/main":                 false,
		"refs/heads/agent/other/x":        false,
		"refs/heads/other/agent/claude/x": false,
		"refs/tags/v1":                    false,
		"refs/tags/agent/claude/x":        false,
		"refs/notes/commits":              false,
		"refs/remotes/agent/claude/x":     false,
		"refs/changes/agent/claude/x":     false,
		"agent/claude/x":                  false,
		"heads/agent/claude/x":            false,
		"HEAD":                            false,
		"":                                false,
		"refs/heads/agent/claude/../main": false,
		"refs/heads/agent/claude/x.lock":  false,
		"refs/heads/agent/claude/a b":     false,
		"refs/heads//agent/claude/x":      false,
	} {
		assert.Equal(t, want, limited.AllowsRef(ref), "%q", ref)
	}

	// A corrupt empty prefix still limits the token and allows nothing.
	corrupt := domain.TokenPrincipal(domain.Token{Scopes: []domain.Scope{domain.ScopeRepoWrite}, BranchPrefixes: []string{""}})
	assert.False(t, corrupt.AllowsRef("refs/heads/main"))

	// Without prefixes every ref is allowed.
	unlimited := domain.TokenPrincipal(domain.Token{Scopes: []domain.Scope{domain.ScopeRepoWrite}})
	assert.True(t, unlimited.AllowsRef("refs/tags/v1"))
	assert.True(t, unlimited.AllowsRef("refs/heads/main"))
	assert.True(t, domain.Root().AllowsRef("refs/tags/v1"))
	assert.True(t, domain.Owner().AllowsRef("refs/heads/main"))
}

func TestBranchPrefixesOfPrincipal(t *testing.T) {
	t.Parallel()
	tok := domain.Token{Scopes: domain.AllScopes(), BranchPrefixes: []string{"a/"}}
	p := domain.TokenPrincipal(tok)
	assert.Equal(t, []string{"a/"}, p.BranchPrefixes())
	assert.False(t, p.HasOwnerRights(), "a token with every scope but limited branches is not the owner")
	assert.True(t, domain.TokenPrincipal(domain.Token{Scopes: domain.AllScopes()}).HasOwnerRights())

	// The principal owns copies.
	tok.BranchPrefixes[0] = "b/"
	assert.Equal(t, []string{"a/"}, p.BranchPrefixes())
	p.BranchPrefixes()[0] = "c/"
	assert.Equal(t, []string{"a/"}, p.BranchPrefixes())
	assert.Empty(t, domain.Root().BranchPrefixes())
}

func TestBranchLimitedAndCoversBranchPrefix(t *testing.T) {
	t.Parallel()
	free := domain.TokenPrincipal(domain.Token{Scopes: domain.AllScopes()})
	assert.False(t, free.IsBranchLimited())
	assert.False(t, free.CoversBranchPrefix("agent/x"), "no prefixes cover nothing: callers test IsBranchLimited first")
	assert.False(t, domain.Root().IsBranchLimited())
	assert.False(t, domain.Owner().IsBranchLimited())
	assert.False(t, domain.Anonymous().IsBranchLimited())

	limited := domain.TokenPrincipal(domain.Token{Scopes: domain.AllScopes(), BranchPrefixes: []string{"agent/claude/", "bot/"}})
	assert.True(t, limited.IsBranchLimited())
	for s, want := range map[string]bool{
		"agent/claude/x": true, "agent/claude/": true, "bot/y": true,
		"agent/claude": false, "agent/claudex/y": false, "main": false, "": false, "xbot/y": false,
	} {
		assert.Equal(t, want, limited.CoversBranchPrefix(s), "%q", s)
	}

	// A corrupt empty prefix still limits, and covers nothing.
	corrupt := domain.TokenPrincipal(domain.Token{Scopes: domain.AllScopes(), BranchPrefixes: []string{""}})
	assert.True(t, corrupt.IsBranchLimited())
	assert.False(t, corrupt.CoversBranchPrefix("main"))
	assert.False(t, corrupt.CoversBranchPrefix(""))
}
