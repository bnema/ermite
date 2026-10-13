// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"bnema.dev/ermite/internal/domain"
)

func TestIsRepoToken(t *testing.T) {
	t.Parallel()
	ok := domain.RepoTokenPrefix + strings.Repeat("aB3-_", 8) + "xyz"
	assert.True(t, domain.IsRepoToken(ok))
	for _, bad := range []string{
		"", domain.RepoTokenPrefix, ok + "x", ok[:len(ok)-1], strings.Replace(ok, "ermitegh_", "ermitejob_", 1),
		domain.RepoTokenPrefix + strings.Repeat("a", 42) + "=", domain.RepoTokenPrefix + strings.Repeat("a", 42) + " ",
		domain.TokenSecretPrefix + strings.Repeat("a", 43),
	} {
		assert.False(t, domain.IsRepoToken(bad), bad)
	}
	assert.False(t, domain.IsJobToken(ok), "the two kinds of secret are never mistaken")
}

func TestRepoTokenPrincipalReadsOneRepositoryAndNothingElse(t *testing.T) {
	t.Parallel()
	repo, _ := domain.ParseRepoName("proj")
	other, _ := domain.ParseRepoName("other")
	exp := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	p := domain.RepoTokenPrincipal(domain.CIRepoToken{ID: "ab12", Repo: repo, ExpiresAt: exp})

	assert.True(t, p.Allows(domain.ScopeRepoRead, repo))
	assert.False(t, p.Allows(domain.ScopeRepoRead, other))
	for _, s := range domain.AllScopes() {
		if s != domain.ScopeRepoRead {
			assert.False(t, p.Allows(s, repo), s)
		}
	}
	assert.False(t, p.IsRoot())
	assert.False(t, p.HasOwnerRights())
	assert.False(t, p.IsAnonymous())
	assert.Equal(t, exp, p.ExpiresAt())
	assert.True(t, domain.IsRepoTokenID(p.TokenID()))
	assert.False(t, domain.IsRepoTokenID("0123456789abcdef"), "a stored token id is 16 hex characters")

	tok := domain.CIRepoToken{ExpiresAt: exp}
	assert.False(t, tok.Expired(exp.Add(-time.Second)))
	assert.True(t, tok.Expired(exp), "the expiry instant is already over")
}
