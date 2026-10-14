// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"bnema.dev/ermite/internal/domain"
)

func tokenOf(t *testing.T, tok domain.Token) domain.Principal {
	t.Helper()
	return domain.TokenPrincipal(tok)
}

func TestTrustedActor(t *testing.T) {
	t.Parallel()
	repo, _ := domain.ParseRepoName("proj")
	all := domain.AllScopes()
	cases := []struct {
		name string
		p    domain.Principal
		want bool
	}{
		{"owner session", domain.Owner(), true},
		{"root token", domain.Root(), true},
		{"unlimited token with every scope", tokenOf(t, domain.Token{ID: "a", Scopes: all}), true},
		{"branch-limited token with every scope", tokenOf(t, domain.Token{ID: "b", Scopes: all, BranchPrefixes: []string{"agent/x/"}}), false},
		{"token limited to a repository", tokenOf(t, domain.Token{ID: "c", Scopes: all, Repos: []domain.RepoName{repo}}), false},
		{"token without every scope", tokenOf(t, domain.Token{ID: "d", Scopes: []domain.Scope{domain.ScopeRepoWrite}}), false},
		{"job token", domain.RepoTokenPrincipal(domain.CIRepoToken{ID: "e", Repo: repo}), false},
		{"anonymous", domain.Anonymous(), false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, domain.TrustedActor(tc.p), tc.name)
	}
}

func TestRerunTrustedNeverRaises(t *testing.T) {
	t.Parallel()
	agent := tokenOf(t, domain.Token{ID: "b", Scopes: domain.AllScopes(), BranchPrefixes: []string{"agent/"}})
	assert.True(t, domain.RerunTrusted(true, domain.Owner()), "owner re-runs an owner's run")
	assert.False(t, domain.RerunTrusted(true, agent), "an agent re-running an owner's run lowers it")
	assert.False(t, domain.RerunTrusted(false, domain.Owner()), "the owner re-running an agent's run does not unlock it")
	assert.False(t, domain.RerunTrusted(false, domain.Root()), "nor does root")
}
