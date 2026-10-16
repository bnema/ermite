// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"bnema.dev/ermite/internal/domain"
)

func TestIsOIDCRequestToken(t *testing.T) {
	t.Parallel()
	ok := domain.OIDCRequestTokenPrefix + strings.Repeat("aB3-_", 8) + "xyz"
	assert.True(t, domain.IsOIDCRequestToken(ok))
	assert.False(t, domain.IsRepoToken(ok), "not a repository token")
	for _, bad := range []string{"", domain.OIDCRequestTokenPrefix, ok + "x", strings.Replace(ok, "ermiteoidc_", "ermitegh_", 1)} {
		assert.False(t, domain.IsOIDCRequestToken(bad), bad)
	}
}

// Only a trusted run with id-token: write gets ID tokens: GitHub's rule, plus
// ermite's trust rule (an untrusted run's writes are lowered, and id-token has
// no read).
func TestIDTokenAllowed(t *testing.T) {
	t.Parallel()
	write := map[string]string{"id-token": "write", "contents": "read"}
	assert.True(t, domain.IDTokenAllowed(write, true))
	assert.False(t, domain.IDTokenAllowed(write, false), "untrusted")
	assert.False(t, domain.IDTokenAllowed(map[string]string{"id-token": "read"}, true), "read is not a level of id-token")
	assert.False(t, domain.IDTokenAllowed(nil, true), "the default has no id-token")
	assert.False(t, domain.IDTokenAllowed(map[string]string{"contents": "write"}, true))
	all := domain.ResolvePermissions(&domain.PermissionSpec{All: domain.PermissionsWriteAll}, nil)
	assert.True(t, domain.IDTokenAllowed(all, true), "write-all")
	readAll := domain.ResolvePermissions(&domain.PermissionSpec{All: domain.PermissionsReadAll}, nil)
	assert.False(t, domain.IDTokenAllowed(readAll, true), "read-all")
}

func TestOIDCSubject(t *testing.T) {
	t.Parallel()
	tests := []struct{ event, env, ref, want string }{
		{"push", "", "refs/heads/main", "repo:o/r:ref:refs/heads/main"},
		{"push", "", "refs/tags/v1", "repo:o/r:ref:refs/tags/v1"},
		{"workflow_dispatch", "", "refs/heads/dev", "repo:o/r:ref:refs/heads/dev"},
		{"pull_request", "", "refs/pull/1/merge", "repo:o/r:pull_request"},
		{"pull_request_target", "", "refs/heads/main", "repo:o/r:pull_request"},
		{"pull_request", "prod", "refs/pull/1/merge", "repo:o/r:environment:prod"},
		{"push", "Prod", "refs/heads/main", "repo:o/r:environment:Prod"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, domain.OIDCSubject("o/r", tt.event, tt.env, tt.ref), tt)
	}
}

func TestOIDCKeyPublished(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	assert.True(t, domain.OIDCKey{}.Published(at), "an active key")
	k := domain.OIDCKey{RetiredAt: at}
	assert.True(t, k.Published(at.Add(24*time.Hour-time.Second)))
	assert.False(t, k.Published(at.Add(24*time.Hour)))
	assert.Equal(t, "oidc/key/abc", domain.OIDCKeyPath("abc"))
}

// An ID token request token is nobody outside the token endpoint.
func TestOIDCRequestTokenHasNoRights(t *testing.T) {
	t.Parallel()
	p := domain.RepoTokenPrincipal(domain.CIRepoToken{ID: "x", OIDC: true, Permissions: map[string]string{"contents": "write"}})
	assert.True(t, p.IsAnonymous())
	assert.Empty(t, p.Scopes())
}
