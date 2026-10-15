// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"bnema.dev/ermite/internal/domain"
)

func spec(all string, kv ...string) *domain.PermissionSpec {
	s := &domain.PermissionSpec{All: all, Levels: map[string]string{}}
	for i := 0; i+1 < len(kv); i += 2 {
		s.Levels[kv[i]] = kv[i+1]
	}
	return s
}

func TestResolvePermissions(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name          string
		workflow, job *domain.PermissionSpec
		want          map[string]string // nil: nothing asked, the default applies
	}{
		{"nothing anywhere", nil, nil, nil},
		{"workflow only", spec("", "contents", "write"), nil, map[string]string{"metadata": "read", "contents": "write"}},
		{"job replaces the workflow, no merge", spec("", "contents", "write", "issues", "write"), spec("", "pull-requests", "read"),
			map[string]string{"metadata": "read", "pull-requests": "read"}},
		{"job alone", nil, spec("", "contents", "write"), map[string]string{"metadata": "read", "contents": "write"}},
		{"empty mapping is all none", spec("", "contents", "write"), spec(""), map[string]string{"metadata": "read"}},
		{"workflow {} with no job", spec(""), nil, map[string]string{"metadata": "read"}},
		{"none removes a key", spec("", "contents", "none", "packages", "read"), nil, map[string]string{"metadata": "read", "packages": "read"}},
		{"read-all", spec("read-all"), nil, map[string]string{
			"metadata": "read", "actions": "read", "artifact-metadata": "read", "attestations": "read", "checks": "read", "code-quality": "read",
			"contents": "read", "deployments": "read", "discussions": "read", "drives": "read", "issues": "read", "models": "read",
			"packages": "read", "pages": "read", "pull-requests": "read", "repository-projects": "read", "security-events": "read",
			"statuses": "read", "vulnerability-alerts": "read"}},
		{"write-all", nil, spec("write-all"), map[string]string{
			"metadata": "read", "actions": "write", "artifact-metadata": "write", "attestations": "write", "checks": "write", "code-quality": "write",
			"contents": "write", "copilot-requests": "write", "deployments": "write", "discussions": "write", "drives": "write", "id-token": "write",
			"issues": "write", "models": "read", "packages": "write", "pages": "write", "pull-requests": "write", "repository-projects": "write",
			"security-events": "write", "statuses": "write", "vulnerability-alerts": "read"}},
		{"job read-all over workflow write-all", spec("write-all"), spec("read-all"), nil}, // checked below by key
		{"a level a key cannot have is none", spec("", "models", "write", "id-token", "read", "contents", "read"), nil,
			map[string]string{"metadata": "read", "contents": "read"}},
		{"unknown keys and levels are none", spec("", "bogus", "write", "contents", "admin"), nil, map[string]string{"metadata": "read"}},
		{"id-token write", spec("", "id-token", "write"), nil, map[string]string{"metadata": "read", "id-token": "write"}},
	} {
		got := domain.ResolvePermissions(tt.workflow, tt.job)
		if tt.name == "job read-all over workflow write-all" {
			assert.Equal(t, "read", got["contents"], tt.name)
			assert.Equal(t, "read", got["issues"], tt.name)
			assert.Equal(t, "read", got["models"], tt.name)
			assert.NotContains(t, got, "id-token", tt.name)
			continue
		}
		assert.Equal(t, tt.want, got, tt.name)
	}
}

func TestTokenPermissionsTrust(t *testing.T) {
	t.Parallel()
	def := map[string]string{"contents": "read", "metadata": "read", "packages": "read"}
	assert.Equal(t, def, domain.TokenPermissions(nil, true), "absent: the restricted default")
	assert.Equal(t, def, domain.TokenPermissions(nil, false))
	assert.Equal(t, def, domain.TokenPermissions(map[string]string{}, true), "empty is absent")

	asked := map[string]string{"metadata": "read", "contents": "write", "issues": "write", "id-token": "write", "packages": "read", "models": "read"}
	assert.Equal(t, asked, domain.TokenPermissions(asked, true), "trusted: as asked")
	assert.Equal(t, map[string]string{"metadata": "read", "contents": "read", "issues": "read", "packages": "read", "models": "read"},
		domain.TokenPermissions(asked, false), "untrusted: every write is read, id-token (no read) is none")

	in := map[string]string{"contents": "write"}
	_ = domain.TokenPermissions(in, false)
	assert.Equal(t, map[string]string{"contents": "write"}, in, "the input is not changed")
	assert.Equal(t, "read", domain.TokenPermissions(map[string]string{"metadata": "none"}, true)["metadata"], "metadata is always read")
}

func TestRepoTokenScopes(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		perms map[string]string
		want  []domain.Scope
	}{
		{"absent is the default", nil, []domain.Scope{domain.ScopeRepoRead}},
		{"contents read", map[string]string{"contents": "read"}, []domain.Scope{domain.ScopeRepoRead}},
		{"contents write", map[string]string{"contents": "write"}, []domain.Scope{domain.ScopeRepoRead, domain.ScopeRepoWrite}},
		{"contents none", map[string]string{"metadata": "read", "packages": "read"}, nil},
		{"pull-requests write only", map[string]string{"pull-requests": "write"}, []domain.Scope{domain.ScopeMRWrite}},
		{"pull-requests read", map[string]string{"contents": "read", "pull-requests": "read"}, []domain.Scope{domain.ScopeRepoRead}},
		{"both", map[string]string{"contents": "write", "pull-requests": "write"}, []domain.Scope{domain.ScopeRepoRead, domain.ScopeRepoWrite, domain.ScopeMRWrite}},
		{"write-all adds no other scope", domain.ResolvePermissions(spec("write-all"), nil), []domain.Scope{domain.ScopeRepoRead, domain.ScopeRepoWrite, domain.ScopeMRWrite}},
	} {
		assert.Equal(t, tt.want, domain.RepoTokenScopes(tt.perms), tt.name)
	}
}

func TestRepoTokenPrincipalFollowsItsPermissions(t *testing.T) {
	t.Parallel()
	repo, _ := domain.ParseRepoName("proj")
	other, _ := domain.ParseRepoName("other")
	exp := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	tok := func(perms map[string]string) domain.Principal {
		return domain.RepoTokenPrincipal(domain.CIRepoToken{ID: "ab12", Repo: repo, ExpiresAt: exp, Permissions: perms})
	}

	w := tok(map[string]string{"contents": "write", "pull-requests": "write", "actions": "write", "issues": "write", "id-token": "write"})
	assert.True(t, w.Allows(domain.ScopeRepoRead, repo))
	assert.True(t, w.Allows(domain.ScopeRepoWrite, repo))
	assert.True(t, w.Allows(domain.ScopeMRWrite, repo))
	assert.False(t, w.Allows(domain.ScopeRepoWrite, other), "one repository only")
	for _, s := range []domain.Scope{domain.ScopeRepoAdmin, domain.ScopeMRMerge, domain.ScopeCIWrite, domain.ScopeSecretsWrite, domain.ScopeRunnerRegister, domain.ScopeTokenAdmin} {
		assert.False(t, w.HasScope(s), "never "+string(s))
	}
	assert.False(t, w.HasOwnerRights())
	assert.False(t, w.IsBranchLimited())
	assert.True(t, w.ViaJobToken())

	none := tok(map[string]string{"metadata": "read"})
	assert.False(t, none.Allows(domain.ScopeRepoRead, repo), "contents: none reads nothing")
	assert.True(t, none.ViaJobToken())

	assert.True(t, tok(nil).Allows(domain.ScopeRepoRead, repo))
	assert.False(t, tok(nil).Allows(domain.ScopeRepoWrite, repo))

	arch := domain.RepoTokenPrincipal(domain.CIRepoToken{ID: "cd", Repo: repo, Archive: true, Sha: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ExpiresAt: exp})
	assert.True(t, arch.ViaJobToken())

	assert.False(t, domain.Root().ViaJobToken())
	assert.False(t, domain.Owner().ViaJobToken())
	assert.False(t, domain.Anonymous().ViaJobToken())
	assert.False(t, domain.TokenPrincipal(domain.Token{ID: "0123456789abcdef"}).ViaJobToken())
}

func TestPermissionViolations(t *testing.T) {
	t.Parallel()
	asked, allowed := domain.PermissionViolations(
		map[string]string{"contents": "write", "issues": "read", "packages": "read", "pages": "none"},
		map[string]string{"contents": "read", "packages": "write"})
	assert.Equal(t, []string{"contents: write", "issues: read"}, asked)
	assert.Equal(t, []string{"contents: read", "issues: none"}, allowed)
	asked, _ = domain.PermissionViolations(map[string]string{"contents": "read"}, map[string]string{"contents": "write"})
	assert.Empty(t, asked, "lowering is allowed")
}
