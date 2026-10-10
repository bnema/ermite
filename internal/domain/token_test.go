// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestScopes(t *testing.T) {
	all := domain.AllScopes()
	assert.Len(t, all, 9)
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
	h := domain.HashTokenSecret("ermite_abc")
	assert.Equal(t, h, domain.HashTokenSecret("ermite_abc"))
	assert.NotEqual(t, h, domain.HashTokenSecret("ermite_abd"))
	assert.Len(t, h.String(), 64)
	// SHA-256("") known value.
	assert.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", domain.HashTokenSecret("").String())
}

func TestTokenExpired(t *testing.T) {
	now := time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)
	assert.False(t, domain.Token{}.Expired(now), "no expiry never expires")
	assert.False(t, domain.Token{ExpiresAt: now.Add(time.Second)}.Expired(now))
	assert.True(t, domain.Token{ExpiresAt: now}.Expired(now), "expires at the instant")
	assert.True(t, domain.Token{ExpiresAt: now.Add(-time.Second)}.Expired(now))
}

func TestPrincipalScopes(t *testing.T) {
	a, b := mustName(t, "a"), mustName(t, "b")
	tok := domain.Token{ID: "t1", Scopes: []domain.Scope{domain.ScopeRepoRead, domain.ScopeRepoWrite}, Repos: []domain.RepoName{a}}
	p := domain.TokenPrincipal(tok)

	assert.Equal(t, domain.PrincipalToken, p.Kind())
	assert.Equal(t, "token", p.Kind().String())
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
	assert.Equal(t, "root", domain.Root().Kind().String())
	assert.Equal(t, "anonymous", domain.Anonymous().Kind().String())
}

func mustName(t *testing.T, s string) domain.RepoName {
	t.Helper()
	n, err := domain.ParseRepoName(s)
	require.NoError(t, err)
	return n
}
