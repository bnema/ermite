// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func oidOf(t *testing.T, s string) domain.ObjectID {
	t.Helper()
	id, err := domain.ParseObjectID(s)
	require.NoError(t, err)
	return id
}

// mergeFixture is main (a, b) and feat (a, b, c, d) with a rename on feat.
type mergeFixture struct {
	f                *fixtureRepo
	a, b, c, d       domain.ObjectID
	mainTip, featTip domain.ObjectID
}

func newMergeFixture(t *testing.T, data string) mergeFixture {
	t.Helper()
	f := newFixtureRepo(t, data, "proj")
	f.write("a.txt", "one\ntwo\nthree\n")
	a := f.commit("add a")
	f.write("b.txt", "bee\n")
	b := f.commit("add b")
	f.git("update-ref", "refs/heads/main", b)
	f.checkoutNew("feat")
	f.write("a.txt", "one\n2\nthree\n")
	c := f.commit("change a")
	f.write("c.txt", "see\n")
	d := f.commit("add c")
	f.git("update-ref", "refs/heads/feat", d)
	return mergeFixture{
		f: f, a: oidOf(t, a), b: oidOf(t, b), c: oidOf(t, c), d: oidOf(t, d),
		mainTip: oidOf(t, b), featTip: oidOf(t, d),
	}
}

func TestBranchHeadAndHasCommit(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	m := newMergeFixture(t, data)
	t.Cleanup(func() { _ = g.Close(ctx) })

	got, err := g.BranchHead(ctx, m.f.name, "feat")
	require.NoError(t, err)
	assert.Equal(t, m.d, got)

	for _, branch := range []string{"nope", "", "-x", "a..b", "refs/heads/feat", "feat^{tree}"} {
		_, err = g.BranchHead(ctx, m.f.name, branch)
		assert.ErrorIs(t, err, domain.ErrNotFound, "%q", branch)
	}
	_, err = g.BranchHead(ctx, name(t, "missing"), "main")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	ok, err := g.HasCommit(ctx, m.f.name, m.c)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = g.HasCommit(ctx, m.f.name, oidOf(t, strings.Repeat("1", 40)))
	require.NoError(t, err)
	assert.False(t, ok)
	// A tree is not a commit.
	tree, err := g.CommitTree(ctx, m.f.name, m.d)
	require.NoError(t, err)
	ok, err = g.HasCommit(ctx, m.f.name, tree)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestMergeBaseAndAncestry(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	m := newMergeFixture(t, data)
	// A branch that diverged from a.
	m.f.git("checkout", "--quiet", "-b", "side", m.a.String())
	m.f.write("s.txt", "side\n")
	side := oidOf(t, m.f.commit("side"))
	m.f.git("update-ref", "refs/heads/side", side.String())
	// An unrelated root.
	m.f.git("checkout", "--quiet", "--orphan", "other")
	m.f.git("rm", "-rf", "--quiet", ".")
	m.f.write("o.txt", "other\n")
	other := oidOf(t, m.f.commit("other root"))

	base, err := g.MergeBase(ctx, m.f.name, m.mainTip, m.featTip)
	require.NoError(t, err)
	assert.Equal(t, m.b, base, "main is an ancestor of feat: it is the base")
	base, err = g.MergeBase(ctx, m.f.name, side, m.featTip)
	require.NoError(t, err)
	assert.Equal(t, m.a, base)
	_, err = g.MergeBase(ctx, m.f.name, other, m.featTip)
	assert.ErrorIs(t, err, domain.ErrNotFound, "no shared history")

	for _, tc := range []struct {
		anc, desc domain.ObjectID
		want      bool
	}{
		{m.mainTip, m.featTip, true},
		{m.featTip, m.mainTip, false},
		{m.featTip, m.featTip, true},
		{side, m.featTip, false},
		{other, m.featTip, false},
	} {
		got, err := g.IsAncestor(ctx, m.f.name, tc.anc, tc.desc)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "%s -> %s", tc.anc, tc.desc)
	}
	_, err = g.IsAncestor(ctx, m.f.name, oidOf(t, strings.Repeat("1", 40)), m.featTip)
	assert.Error(t, err, "an unknown commit is an error, not a plain no")
}

func TestListRange(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	m := newMergeFixture(t, data)

	commits, more, err := g.ListRange(ctx, m.f.name, m.mainTip, m.featTip, 10)
	require.NoError(t, err)
	assert.False(t, more)
	require.Len(t, commits, 2)
	assert.Equal(t, m.d, commits[0].ID, "newest first")
	assert.Equal(t, m.c, commits[1].ID)
	assert.Equal(t, "add c", commits[0].Summary)

	commits, more, err = g.ListRange(ctx, m.f.name, m.mainTip, m.featTip, 1)
	require.NoError(t, err)
	assert.True(t, more)
	require.Len(t, commits, 1)

	commits, more, err = g.ListRange(ctx, m.f.name, domain.ObjectID{}, m.featTip, 10)
	require.NoError(t, err)
	assert.False(t, more)
	assert.Len(t, commits, 4, "a zero exclude lists the whole history")

	commits, _, err = g.ListRange(ctx, m.f.name, m.featTip, m.mainTip, 10)
	require.NoError(t, err)
	assert.Empty(t, commits)

	_, _, err = g.ListRange(ctx, m.f.name, m.mainTip, m.featTip, 0)
	assert.ErrorIs(t, err, domain.ErrInvalidPageSize)
}

func TestDiffCommits(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	m := newMergeFixture(t, data)

	stats, diff, err := g.DiffCommits(ctx, m.f.name, m.mainTip, m.featTip, domain.DefaultDiffLimits())
	require.NoError(t, err)
	assert.Equal(t, domain.DiffStats{FilesChanged: 2, Additions: 2, Deletions: 1}, stats)
	require.Len(t, diff.Files, 2)
	assert.Equal(t, "a.txt", diff.Files[0].NewPath)
	assert.Equal(t, "c.txt", diff.Files[1].NewPath)
	assert.True(t, diff.HasLine("a.txt", domain.SideNew, 2))
	assert.True(t, diff.HasLine("a.txt", domain.SideOld, 2))
	assert.True(t, diff.HasLine("c.txt", domain.SideNew, 1))
	assert.False(t, diff.HasLine("c.txt", domain.SideOld, 1))

	// The diff between two commits of a branch.
	stats, _, err = g.DiffCommits(ctx, m.f.name, m.c, m.featTip, domain.DefaultDiffLimits())
	require.NoError(t, err)
	assert.Equal(t, domain.DiffStats{FilesChanged: 1, Additions: 1}, stats)
}

func TestWriteCommitAndUpdateBranch(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	m := newMergeFixture(t, data)

	tree, err := g.CommitTree(ctx, m.f.name, m.featTip)
	require.NoError(t, err)
	assert.Equal(t, m.f.git("rev-parse", m.featTip.String()+"^{tree}"), tree.String())

	obj := "tree " + tree.String() + "\nparent " + m.mainTip.String() + "\n" +
		"author A <a@x.org> 1 +0000\ncommitter C <c@x.org> 2 +0000\n\nsquashed\n"
	id, err := g.WriteCommit(ctx, m.f.name, []byte(obj))
	require.NoError(t, err)
	assert.Equal(t, "commit", m.f.git("cat-file", "-t", id.String()))
	assert.Equal(t, "squashed", m.f.git("log", "-1", "--format=%s", id.String()))

	_, err = g.WriteCommit(ctx, m.f.name, []byte("not a commit"))
	assert.Error(t, err, "git checks the object")

	// The old value is a precondition.
	err = g.UpdateBranch(ctx, m.f.name, "main", m.featTip, id)
	assert.ErrorIs(t, err, domain.ErrConflict, "main is not at feat")
	assert.Equal(t, m.mainTip.String(), m.f.git("rev-parse", "refs/heads/main"))

	require.NoError(t, g.UpdateBranch(ctx, m.f.name, "main", m.mainTip, id))
	assert.Equal(t, id.String(), m.f.git("rev-parse", "refs/heads/main"))

	err = g.UpdateBranch(ctx, m.f.name, "refs/heads/main", id, m.mainTip)
	assert.Error(t, err, "only short branch names")
	err = g.UpdateBranch(ctx, name(t, "missing"), "main", id, m.mainTip)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}
