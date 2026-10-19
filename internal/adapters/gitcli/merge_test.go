// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/readertest"
)

func oidOf(t *testing.T, s string) domain.ObjectID {
	t.Helper()
	id, err := domain.ParseObjectID(s)
	require.NoError(t, err)
	return id
}

// mergeFixture is main (a, b) and feat (a, b, c, d) with a rename on feat.
type mergeFixture struct {
	f                *readertest.Fixture
	a, b, c, d       domain.ObjectID
	mainTip, featTip domain.ObjectID
}

func newMergeFixture(t *testing.T, data string) mergeFixture {
	t.Helper()
	f := readertest.NewFixture(t, data, "proj")
	f.Write("a.txt", "one\ntwo\nthree\n")
	a := f.Commit("add a")
	f.Write("b.txt", "bee\n")
	b := f.Commit("add b")
	f.Git("update-ref", "refs/heads/main", b)
	f.CheckoutNew("feat")
	f.Write("a.txt", "one\n2\nthree\n")
	c := f.Commit("change a")
	f.Write("c.txt", "see\n")
	d := f.Commit("add c")
	f.Git("update-ref", "refs/heads/feat", d)
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

	got, err := g.BranchHead(ctx, m.f.Name, "feat")
	require.NoError(t, err)
	assert.Equal(t, m.d, got)

	for _, branch := range []string{"nope", "", "-x", "a..b", "refs/heads/feat", "feat^{tree}"} {
		_, err = g.BranchHead(ctx, m.f.Name, branch)
		assert.ErrorIs(t, err, domain.ErrNotFound, "%q", branch)
	}
	_, err = g.BranchHead(ctx, name(t, "missing"), "main")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	ok, err := g.HasCommit(ctx, m.f.Name, m.c)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = g.HasCommit(ctx, m.f.Name, oidOf(t, strings.Repeat("1", 40)))
	require.NoError(t, err)
	assert.False(t, ok)
	// A tree is not a commit.
	tree, err := g.CommitTree(ctx, m.f.Name, m.d)
	require.NoError(t, err)
	ok, err = g.HasCommit(ctx, m.f.Name, tree)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestMergeBaseAndAncestry(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	m := newMergeFixture(t, data)
	// A branch that diverged from a.
	m.f.Git("checkout", "--quiet", "-b", "side", m.a.String())
	m.f.Write("s.txt", "side\n")
	side := oidOf(t, m.f.Commit("side"))
	m.f.Git("update-ref", "refs/heads/side", side.String())
	// An unrelated root.
	m.f.Git("checkout", "--quiet", "--orphan", "other")
	m.f.Git("rm", "-rf", "--quiet", ".")
	m.f.Write("o.txt", "other\n")
	other := oidOf(t, m.f.Commit("other root"))

	base, err := g.MergeBase(ctx, m.f.Name, m.mainTip, m.featTip)
	require.NoError(t, err)
	assert.Equal(t, m.b, base, "main is an ancestor of feat: it is the base")
	base, err = g.MergeBase(ctx, m.f.Name, side, m.featTip)
	require.NoError(t, err)
	assert.Equal(t, m.a, base)
	_, err = g.MergeBase(ctx, m.f.Name, other, m.featTip)
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
		got, err := g.IsAncestor(ctx, m.f.Name, tc.anc, tc.desc)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "%s -> %s", tc.anc, tc.desc)
	}
	_, err = g.IsAncestor(ctx, m.f.Name, oidOf(t, strings.Repeat("1", 40)), m.featTip)
	assert.Error(t, err, "an unknown commit is an error, not a plain no")
}

func TestListRange(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	m := newMergeFixture(t, data)

	commits, more, err := g.ListRange(ctx, m.f.Name, m.mainTip, m.featTip, 10)
	require.NoError(t, err)
	assert.False(t, more)
	require.Len(t, commits, 2)
	assert.Equal(t, m.d, commits[0].ID, "newest first")
	assert.Equal(t, m.c, commits[1].ID)
	assert.Equal(t, "add c", commits[0].Summary)

	commits, more, err = g.ListRange(ctx, m.f.Name, m.mainTip, m.featTip, 1)
	require.NoError(t, err)
	assert.True(t, more)
	require.Len(t, commits, 1)

	commits, more, err = g.ListRange(ctx, m.f.Name, domain.ObjectID{}, m.featTip, 10)
	require.NoError(t, err)
	assert.False(t, more)
	assert.Len(t, commits, 4, "a zero exclude lists the whole history")

	commits, _, err = g.ListRange(ctx, m.f.Name, m.featTip, m.mainTip, 10)
	require.NoError(t, err)
	assert.Empty(t, commits)

	_, _, err = g.ListRange(ctx, m.f.Name, m.mainTip, m.featTip, 0)
	assert.ErrorIs(t, err, domain.ErrInvalidPageSize)
}

func TestDiffCommits(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	m := newMergeFixture(t, data)

	stats, diff, err := g.DiffCommits(ctx, m.f.Name, m.mainTip, m.featTip, domain.DefaultDiffLimits())
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
	stats, _, err = g.DiffCommits(ctx, m.f.Name, m.c, m.featTip, domain.DefaultDiffLimits())
	require.NoError(t, err)
	assert.Equal(t, domain.DiffStats{FilesChanged: 1, Additions: 1}, stats)
}

func TestWriteCommitAndUpdateBranch(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	m := newMergeFixture(t, data)

	tree, err := g.CommitTree(ctx, m.f.Name, m.featTip)
	require.NoError(t, err)
	assert.Equal(t, m.f.Git("rev-parse", m.featTip.String()+"^{tree}"), tree.String())

	obj := "tree " + tree.String() + "\nparent " + m.mainTip.String() + "\n" +
		"author A <a@x.org> 1 +0000\ncommitter C <c@x.org> 2 +0000\n\nsquashed\n"
	id, err := g.WriteCommit(ctx, m.f.Name, []byte(obj))
	require.NoError(t, err)
	assert.Equal(t, "commit", m.f.Git("cat-file", "-t", id.String()))
	assert.Equal(t, "squashed", m.f.Git("log", "-1", "--format=%s", id.String()))

	_, err = g.WriteCommit(ctx, m.f.Name, []byte("not a commit"))
	assert.Error(t, err, "git checks the object")

	// The old value is a precondition.
	err = g.UpdateBranch(ctx, m.f.Name, "main", m.featTip, id)
	assert.ErrorIs(t, err, domain.ErrConflict, "main is not at feat")
	assert.Equal(t, m.mainTip.String(), m.f.Git("rev-parse", "refs/heads/main"))

	require.NoError(t, g.UpdateBranch(ctx, m.f.Name, "main", m.mainTip, id))
	assert.Equal(t, id.String(), m.f.Git("rev-parse", "refs/heads/main"))

	err = g.UpdateBranch(ctx, m.f.Name, "refs/heads/main", id, m.mainTip)
	assert.Error(t, err, "only short branch names")
	err = g.UpdateBranch(ctx, name(t, "missing"), "main", id, m.mainTip)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestDeleteBranch(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	m := newMergeFixture(t, data)
	m.f.Git("pack-refs", "--all") // the ref is in packed-refs too

	// The old value is a precondition.
	err := g.DeleteBranch(ctx, m.f.Name, "feat", m.mainTip)
	assert.ErrorIs(t, err, domain.ErrConflict, "feat is not at main")
	assert.Equal(t, m.featTip.String(), m.f.Git("rev-parse", "refs/heads/feat"))

	require.NoError(t, g.DeleteBranch(ctx, m.f.Name, "feat", m.featTip))
	_, err = g.BranchHead(ctx, m.f.Name, "feat")
	assert.ErrorIs(t, err, domain.ErrNotFound)
	assert.Empty(t, m.f.Git("for-each-ref", "refs/heads/feat"), "gone from packed-refs too")

	err = g.DeleteBranch(ctx, m.f.Name, "feat", m.featTip)
	assert.ErrorIs(t, err, domain.ErrConflict, "already gone")
	err = g.DeleteBranch(ctx, m.f.Name, "refs/heads/main", m.mainTip)
	assert.Error(t, err, "only short branch names")
	assert.Equal(t, m.mainTip.String(), m.f.Git("rev-parse", "refs/heads/main"))
	err = g.DeleteBranch(ctx, name(t, "missing"), "main", m.mainTip)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestMergeTree(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	m := newMergeFixture(t, data)
	t.Cleanup(func() { _ = g.Close(ctx) })

	// Clean: main is an ancestor of feat.
	res, err := g.MergeTree(ctx, m.f.Name, m.mainTip, m.featTip)
	require.NoError(t, err)
	assert.True(t, res.Clean)
	assert.False(t, res.Tree.IsZero())
	assert.Empty(t, res.ConflictPaths)

	// Conflict: both sides change a.txt and c.txt differently.
	m.f.Git("checkout", "--quiet", "-b", "clash", m.mainTip.String())
	m.f.Write("a.txt", "one\nTWO\nthree\n")
	m.f.Write("c.txt", "other\n")
	clash := oidOf(t, m.f.Commit("clash"))
	res, err = g.MergeTree(ctx, m.f.Name, clash, m.featTip)
	require.NoError(t, err)
	assert.False(t, res.Clean)
	assert.True(t, res.Tree.IsZero())
	assert.ElementsMatch(t, []string{"a.txt", "c.txt"}, res.ConflictPaths)
	assert.False(t, res.Truncated)

	// No common history: not clean, no paths.
	m.f.Git("checkout", "--quiet", "--orphan", "other")
	m.f.Git("rm", "-rf", "--quiet", ".")
	m.f.Write("o.txt", "other\n")
	other := oidOf(t, m.f.Commit("other root"))
	res, err = g.MergeTree(ctx, m.f.Name, other, m.featTip)
	require.NoError(t, err)
	assert.False(t, res.Clean)
	assert.Empty(t, res.ConflictPaths)

	// More conflicts than the cap.
	m.f.Git("checkout", "--quiet", "-b", "wide-a", m.a.String())
	for i := 0; i < domain.MaxConflictPaths+5; i++ {
		m.f.Write(fmt.Sprintf("f%02d.txt", i), "base\n")
	}
	base := oidOf(t, m.f.Commit("many"))
	m.f.Git("checkout", "--quiet", "-b", "wide-b", base.String())
	for i := 0; i < domain.MaxConflictPaths+5; i++ {
		m.f.Write(fmt.Sprintf("f%02d.txt", i), "b\n")
	}
	wb := oidOf(t, m.f.Commit("b side"))
	m.f.Git("checkout", "--quiet", "wide-a")
	for i := 0; i < domain.MaxConflictPaths+5; i++ {
		m.f.Write(fmt.Sprintf("f%02d.txt", i), "a\n")
	}
	wa := oidOf(t, m.f.Commit("a side"))
	res, err = g.MergeTree(ctx, m.f.Name, wa, wb)
	require.NoError(t, err)
	assert.False(t, res.Clean)
	assert.Len(t, res.ConflictPaths, domain.MaxConflictPaths)
	assert.True(t, res.Truncated)
}

func TestCountBehind(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	m := newMergeFixture(t, data)
	t.Cleanup(func() { _ = g.Close(ctx) })

	// main has b that a-based head lacks.
	n, capped, err := g.CountBehind(ctx, m.f.Name, m.a, m.mainTip, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.False(t, capped)

	n, capped, err = g.CountBehind(ctx, m.f.Name, m.featTip, m.mainTip, 10)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.False(t, capped)

	// feat is 3 commits ahead of a: counting target=feat from head=a with cap 2.
	n, capped, err = g.CountBehind(ctx, m.f.Name, m.a, m.featTip, 2)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.True(t, capped)
	n, capped, err = g.CountBehind(ctx, m.f.Name, m.a, m.featTip, 3)
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.False(t, capped)
}
