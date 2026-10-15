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

func TestPullRefNames(t *testing.T) {
	assert.Equal(t, "refs/pull/7/head", domain.PullHeadRef(7))
	assert.Equal(t, "refs/pull/7/merge", domain.PullMergeRef(7))
	for _, ok := range []string{"refs/pull/1/head", "refs/pull/12/merge"} {
		assert.True(t, domain.IsPullRef(ok), ok)
	}
	for _, no := range []string{"refs/pull/0/head", "refs/pull/01/head", "refs/pull/1/other", "refs/pull/1", "refs/pull/x/head", "refs/heads/pull/1/head", "refs/pull/1/head/x", "refs/pull/-1/merge"} {
		assert.False(t, domain.IsPullRef(no), no)
	}
	assert.True(t, domain.IsPullRefName("refs/pull/anything"))
	assert.False(t, domain.IsPullRefName("refs/pullx/1/head"))
}

func TestTestMergeCommit(t *testing.T) {
	tree := mustID(t, "1")
	base, head := mustID(t, "2"), mustID(t, "3")
	got := string(domain.TestMergeCommit(tree, base, head, domain.Identity{Name: "ermite", Email: "e@x"}, time.Unix(1700000000, 0)))
	assert.Equal(t, "tree "+tree.String()+"\nparent "+base.String()+"\nparent "+head.String()+
		"\nauthor ermite <e@x> 1700000000 +0000\ncommitter ermite <e@x> 1700000000 +0000\n\nMerge "+head.String()+" into "+base.String()+"\n", got)
}

func mustID(t *testing.T, c string) domain.ObjectID {
	t.Helper()
	id, err := domain.ParseObjectID(strings.Repeat(c, 40))
	require.NoError(t, err)
	return id
}

func TestJobPermits(t *testing.T) {
	repo, other := mustRepo(t, "proj"), mustRepo(t, "other")
	tok := func(perms map[string]string) domain.Principal {
		return domain.RepoTokenPrincipal(domain.CIRepoToken{ID: "a", Repo: repo, Permissions: perms})
	}
	read := map[string]string{domain.PermPullRequests: domain.PermissionRead}
	write := map[string]string{domain.PermPullRequests: domain.PermissionWrite}
	assert.True(t, tok(read).JobPermits(domain.PermPullRequests, domain.PermissionRead, repo))
	assert.False(t, tok(read).JobPermits(domain.PermPullRequests, domain.PermissionWrite, repo))
	assert.True(t, tok(write).JobPermits(domain.PermPullRequests, domain.PermissionRead, repo), "write includes read")
	assert.False(t, tok(read).JobPermits(domain.PermPullRequests, domain.PermissionRead, other), "another repository")
	assert.False(t, tok(nil).JobPermits(domain.PermPullRequests, domain.PermissionRead, repo), "the default has no pull-requests")
	assert.False(t, tok(map[string]string{domain.PermContents: domain.PermissionWrite}).JobPermits(domain.PermPullRequests, domain.PermissionRead, repo))
	assert.False(t, domain.Root().JobPermits(domain.PermPullRequests, domain.PermissionRead, repo), "only job tokens")
	assert.False(t, domain.Anonymous().JobPermits(domain.PermPullRequests, domain.PermissionRead, repo))
}

func mustRepo(t *testing.T, name string) domain.RepoName {
	t.Helper()
	r, err := domain.ParseRepoName(name)
	require.NoError(t, err)
	return r
}
