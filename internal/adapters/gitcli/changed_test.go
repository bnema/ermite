// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestChangedPaths(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	m := newMergeFixture(t, data)
	t.Cleanup(func() { _ = g.Close(ctx) })

	// b..d changed a.txt and added c.txt.
	got, truncated, err := g.ChangedPaths(ctx, m.f.Name, m.b, m.d, 300)
	require.NoError(t, err)
	assert.False(t, truncated)
	assert.ElementsMatch(t, []string{"a.txt", "c.txt"}, got)

	// A zero base lists every file of the commit, as for a new repository.
	got, _, err = g.ChangedPaths(ctx, m.f.Name, domain.ObjectID{}, m.b, 300)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a.txt", "b.txt"}, got)

	// No change is an empty list, not an error.
	got, truncated, err = g.ChangedPaths(ctx, m.f.Name, m.d, m.d, 300)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.False(t, truncated)

	// A bound cuts the list and says so.
	got, truncated, err = g.ChangedPaths(ctx, m.f.Name, domain.ObjectID{}, m.d, 2)
	require.NoError(t, err)
	assert.Len(t, got, 2)
	assert.True(t, truncated)
	got, truncated, err = g.ChangedPaths(ctx, m.f.Name, domain.ObjectID{}, m.d, 3)
	require.NoError(t, err)
	assert.Len(t, got, 3)
	assert.False(t, truncated, "a list of exactly the bound is complete")

	_, _, err = g.ChangedPaths(ctx, m.f.Name, m.b, m.d, 0)
	assert.ErrorIs(t, err, domain.ErrInvalid)
	_, _, err = g.ChangedPaths(ctx, name(t, "missing"), m.b, m.d, 5)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, _, err = g.ChangedPaths(ctx, m.f.Name, m.b, oidOf(t, "1111111111111111111111111111111111111111"), 5)
	assert.Error(t, err, "an unknown commit is an error")
}

func TestChangedPathsKeepsAWholeRenameAndOddNames(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	m := newMergeFixture(t, data)
	t.Cleanup(func() { _ = g.Close(ctx) })

	m.f.Git("checkout", "-q", "feat")
	m.f.Write("dir with space/é.txt", "see\n")
	m.f.Git("rm", "-q", "c.txt")
	d2 := oidOf(t, m.f.Commit("rename"))
	got, _, err := g.ChangedPaths(ctx, m.f.Name, m.featTip, d2, 10)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"c.txt", "dir with space/é.txt"}, got, "both sides of a rename, verbatim")
}

func TestCommitsOffBranches(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	m := newMergeFixture(t, data)
	t.Cleanup(func() { _ = g.Close(ctx) })

	msgs := func(cs []domain.Commit) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Summary)
		}
		return out
	}

	// Every commit of the fixture is on main or feat.
	got, more, err := g.CommitsOffBranches(ctx, m.f.Name, m.d, 10)
	require.NoError(t, err)
	assert.Empty(t, got, "feat has its tip")
	assert.False(t, more)

	// A commit no branch has, on top of feat: listed alone, then with its
	// parent when that has no branch either (a branch deleted, a detached tip).
	m.f.Git("checkout", "--quiet", "--detach", m.d.String())
	m.f.Write("t1.txt", "one\n")
	t1 := oidOf(t, m.f.Commit("tagged one"))
	m.f.Write("t2.txt", "two\n")
	t2 := oidOf(t, m.f.Commit("tagged two"))
	m.f.Git("update-ref", "refs/tags/v1", t2.String())

	got, more, err = g.CommitsOffBranches(ctx, m.f.Name, t2, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"tagged two", "tagged one"}, msgs(got), "newest first, down to the commits a branch has")
	assert.False(t, more)
	got, _, err = g.CommitsOffBranches(ctx, m.f.Name, t1, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"tagged one"}, msgs(got))

	// A bound cuts the list and says so.
	got, more, err = g.CommitsOffBranches(ctx, m.f.Name, t2, 1)
	require.NoError(t, err)
	assert.Equal(t, []string{"tagged two"}, msgs(got))
	assert.True(t, more)

	// Another tag is no branch: t2 stays listed although v1 points to it, and
	// moving a branch onto t1 takes t1 out of the list.
	m.f.Git("update-ref", "refs/heads/rel", t1.String())
	got, _, err = g.CommitsOffBranches(ctx, m.f.Name, t2, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"tagged two"}, msgs(got))

	_, _, err = g.CommitsOffBranches(ctx, m.f.Name, domain.ObjectID{}, 10)
	assert.ErrorIs(t, err, domain.ErrInvalid)
	_, _, err = g.CommitsOffBranches(ctx, m.f.Name, t2, 0)
	assert.ErrorIs(t, err, domain.ErrInvalidPageSize)
	_, _, err = g.CommitsOffBranches(ctx, name(t, "missing"), t2, 5)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}
