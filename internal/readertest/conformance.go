// SPDX-License-Identifier: AGPL-3.0-or-later

package readertest

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Maker builds a RepoReader serving the repositories below data/repos, and
// returns it with the data directory. The reader is closed when the test ends.
type Maker func(t *testing.T) (ports.RepoReader, string)

// Conformance runs the RepoReader contract against the readers mk builds:
// every adapter that serves repositories must pass it, whatever its route.
func Conformance(t *testing.T, mk Maker) {
	for _, tc := range []struct {
		name string
		fn   func(*testing.T, Maker)
	}{
		{"DiffChangedLineLimits", DiffChangedLineLimits},
		{"ListRefs", testListRefs},
		{"ListRefsEmptyAndMissing", testListRefsEmptyAndMissing},
		{"DetachedHead", testDetachedHead},
		{"ResolveRevision", testResolveRevision},
		{"ResolveRefPath", testResolveRefPath},
		{"GetTree", testGetTree},
		{"GetTreeLastCommit", testGetTreeLastCommit},
		{"GetTreeLastCommitBound", testGetTreeLastCommitBound},
		{"ListCommits", testListCommits},
		{"OpenBlob", testOpenBlob},
		{"OpenBlobMediaTypes", testOpenBlobMediaTypes},
		{"BinaryRuleLooksAtFirst8000Bytes", testBinaryRuleLooksAtFirst8000Bytes},
		{"OpenBlobLargeThroughStream", testOpenBlobLargeThroughStream},
		{"CancelledContext", testCancelledContext},
		{"HostileRevisionAndPathStayLiteral", testHostileRevisionAndPathStayLiteral},
		{"ResolveRevisionObjectIDBeatsRefs", testResolveRevisionObjectIDBeatsRefs},
		{"GetTreeIsBounded", testGetTreeIsBounded},
		{"GetTreeRefusesAGitlinkToATree", testGetTreeRefusesAGitlinkToATree},
		{"ListPaths", testListPaths},
		{"CountCommitsAndLatestTag", testCountCommitsAndLatestTag},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, mk) })
	}
}

// Rev parses a revision.
func Rev(t *testing.T, s string) domain.Revision {
	t.Helper()
	r, err := domain.ParseRevision(s)
	require.NoError(t, err)
	return r
}

// TreePath parses a tree path.
func TreePath(t *testing.T, s string) domain.TreePath {
	t.Helper()
	p, err := domain.ParseTreePath(s)
	require.NoError(t, err)
	return p
}

func testListRefs(t *testing.T, mk Maker) {
	ctx := context.Background()
	g, data := mk(t)
	h := NewHistory(t, data)

	refs, err := g.ListRefs(ctx, h.Name)
	require.NoError(t, err)

	type row struct {
		name  string
		short string
		kind  domain.RefKind
		id    string
		commi string
	}
	var got []row
	for _, r := range refs {
		got = append(got, row{r.Name, r.ShortName, r.Kind, r.ID.String(), r.CommitID.String()})
	}
	tagID := func(n string) string { return h.Git("rev-parse", "refs/tags/"+n) }
	assert.Equal(t, []row{
		{"HEAD", "main", domain.RefKindHead, h.Merge, h.Merge},
		{"refs/heads/feature/x", "feature/x", domain.RefKindBranch, h.Side, h.Side},
		{"refs/heads/main", "main", domain.RefKindBranch, h.Merge, h.Merge},
		{"refs/heads/rel/1", "rel/1", domain.RefKindBranch, h.C1, h.C1},
		{"refs/heads/release/1.0", "release/1.0", domain.RefKindBranch, h.C2, h.C2},
		{"refs/tags/rel/1/x", "rel/1/x", domain.RefKindTag, h.C3, h.C3},
		{"refs/tags/tree-tag", "tree-tag", domain.RefKindTag, tagID("tree-tag"), ""},
		{"refs/tags/v1", "v1", domain.RefKindTag, h.C1, h.C1},
		{"refs/tags/v2", "v2", domain.RefKindTag, tagID("v2"), h.C2},
		{"refs/tags/v2-again", "v2-again", domain.RefKindTag, tagID("v2-again"), h.C2},
	}, got)
}

func testCountCommitsAndLatestTag(t *testing.T, mk Maker) {
	ctx := context.Background()
	g, data := mk(t)
	f := NewFixture(t, data, "stats")

	_, ok, err := g.LatestTag(ctx, f.Name)
	require.NoError(t, err)
	assert.False(t, ok, "no tag yet")

	var ids []string
	for i := range 4 { // committed at 12:01, 12:02, 12:03, 12:04
		ids = append(ids, f.Commit("c"+itoa(i)))
	}
	head := MustID(t, ids[3])
	n, err := g.CountCommits(ctx, f.Name, head)
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	n, err = g.CountCommits(ctx, f.Name, MustID(t, ids[1]))
	require.NoError(t, err)
	assert.Equal(t, 2, n, "from another commit")
	_, err = g.CountCommits(ctx, f.Name, MustID(t, strings.Repeat("ab", 20)))
	assert.ErrorIs(t, err, domain.ErrNotFound)

	unix := func(ts []time.Time) []int64 {
		var out []int64
		for _, x := range ts {
			out = append(out, x.Unix())
		}
		slices.Sort(out)
		return out
	}
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	times, more, err := g.CommitTimes(ctx, f.Name, head, base.Add(150*time.Second))
	require.NoError(t, err)
	assert.False(t, more)
	assert.Equal(t, []int64{base.Add(3 * time.Minute).Unix(), base.Add(4 * time.Minute).Unix()}, unix(times), "12:03 and 12:04")
	_, _, err = g.CommitTimes(ctx, f.Name, MustID(t, strings.Repeat("ab", 20)), base)
	assert.ErrorIs(t, err, domain.ErrNotFound)

	// Dates out of order: a commit dated in the past on top of recent ones
	// does not hide them.
	recent := f.When
	f.When = recent.Add(-48 * time.Hour)
	old := f.Commit("backdated")
	f.When = recent
	times, _, err = g.CommitTimes(ctx, f.Name, MustID(t, old), base.Add(150*time.Second))
	require.NoError(t, err)
	assert.Len(t, times, 2, "the recent commits behind an old one count")

	// The latest tag is the one made last: an annotated tag by its own date,
	// a lightweight one by the date of its commit.
	f.Git("tag", "light-old", ids[0])
	f.When = f.When.Add(time.Hour)
	f.Git("tag", "-a", "-m", "new", "v2", ids[0])
	f.Git("tag", "light-new", ids[3])
	tag, ok, err := g.LatestTag(ctx, f.Name)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "v2", tag.Name)
	assert.True(t, tag.At.Equal(f.When), "%v", tag.At)
	assert.Equal(t, ids[0], tag.CommitID.String(), "an annotated tag is peeled to its commit")

	f.When = f.When.Add(time.Hour)
	f.Git("tag", "-a", "-m", "tree", "tree-tag", ids[0]+"^{tree}")
	tag, ok, err = g.LatestTag(ctx, f.Name)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "tree-tag", tag.Name)
	assert.True(t, tag.CommitID.IsZero(), "a tag of a tree has no commit")

	_, _, err = g.LatestTag(ctx, RepoName(t, "nope"))
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func testListRefsEmptyAndMissing(t *testing.T, mk Maker) {
	ctx := context.Background()
	g, data := mk(t)
	f := NewFixture(t, data, "empty")

	refs, err := g.ListRefs(ctx, f.Name)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, "HEAD", refs[0].Name)
	assert.Equal(t, "main", refs[0].ShortName, "unborn default branch")
	assert.True(t, refs[0].ID.IsZero())
	assert.True(t, refs[0].CommitID.IsZero())

	_, err = g.ListRefs(ctx, RepoName(t, "nope"))
	assert.ErrorIs(t, err, domain.ErrNotFound)

	// No commit to resolve in an empty repository.
	_, err = g.ResolveRevision(ctx, f.Name, domain.Revision{})
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = g.GetTree(ctx, f.Name, domain.Revision{}, domain.TreePath{}, false)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = g.ListCommits(ctx, f.Name, ports.CommitQuery{Limit: 10})
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func testDetachedHead(t *testing.T, mk Maker) {
	ctx := context.Background()
	g, data := mk(t)
	h := NewHistory(t, data)
	h.Git("update-ref", "--no-deref", "HEAD", h.C1)

	refs, err := g.ListRefs(ctx, h.Name)
	require.NoError(t, err)
	assert.Equal(t, "HEAD", refs[0].Name)
	assert.Empty(t, refs[0].ShortName)
}

func testResolveRevision(t *testing.T, mk Maker) {
	ctx := context.Background()
	g, data := mk(t)
	h := NewHistory(t, data)

	tests := []struct {
		rev    string
		commit string
		ref    string
	}{
		{"", h.Merge, ""},
		{"HEAD", h.Merge, ""},
		{"main", h.Merge, "refs/heads/main"},
		{"feature/x", h.Side, "refs/heads/feature/x"},
		{"refs/heads/feature/x", h.Side, "refs/heads/feature/x"},
		{"v1", h.C1, "refs/tags/v1"},
		{"v2", h.C2, "refs/tags/v2"},
		{"v2-again", h.C2, "refs/tags/v2-again"},
		{"refs/tags/v2", h.C2, "refs/tags/v2"},
		{h.C2, h.C2, ""},
		{h.C2[:7], h.C2, ""},
		{h.C2[:12], h.C2, ""},
	}
	for _, tt := range tests {
		t.Run(tt.rev, func(t *testing.T) {
			got, err := g.ResolveRevision(ctx, h.Name, Rev(t, tt.rev))
			require.NoError(t, err)
			assert.Equal(t, tt.commit, got.CommitID.String())
			assert.Equal(t, tt.ref, got.Ref)
		})
	}
	for _, bad := range []string{"nope", "tree-tag", "deadbeefdeadbeef", "HEAD~1", h.C2[:6], "refs/heads", h.Git("rev-parse", h.Merge+"^{tree}")} {
		t.Run("missing "+bad, func(t *testing.T) {
			r, err := domain.ParseRevision(bad)
			if err != nil {
				return // not a revision at all, rejected before the reader
			}
			_, err = g.ResolveRevision(ctx, h.Name, r)
			assert.ErrorIs(t, err, domain.ErrNotFound)
		})
	}

	t.Run("a branch wins over a tag of the same name", func(t *testing.T) {
		h.Git("branch", "both", h.C1)
		h.Git("tag", "both", h.C2)
		got, err := g.ResolveRevision(ctx, h.Name, Rev(t, "both"))
		require.NoError(t, err)
		assert.Equal(t, h.C1, got.CommitID.String())
	})
}

func testResolveRefPath(t *testing.T, mk Maker) {
	ctx := context.Background()
	g, data := mk(t)
	h := NewHistory(t, data)

	tests := []struct {
		path string
		name string
		ref  string
		id   string
		rest string
	}{
		{"main", "main", "refs/heads/main", h.Merge, ""},
		{"main/d/f", "main", "refs/heads/main", h.Merge, "d/f"},
		{"feature/x", "feature/x", "refs/heads/feature/x", h.Side, ""},
		{"feature/x/d/f", "feature/x", "refs/heads/feature/x", h.Side, "d/f"},
		{"feature/x/", "feature/x", "refs/heads/feature/x", h.Side, ""},
		{"release/1.0/a.txt", "release/1.0", "refs/heads/release/1.0", h.C2, "a.txt"},
		// The longest ref wins: the tag rel/1/x over the branch rel/1.
		{"rel/1/x/d/f", "rel/1/x", "refs/tags/rel/1/x", h.C3, "d/f"},
		{"rel/1/d/f", "rel/1", "refs/heads/rel/1", h.C1, "d/f"},
		{"v2/we:ird*.txt", "v2", "refs/tags/v2", h.C2, "we:ird*.txt"},
		{h.C1 + "/d", h.C1, "", h.C1, "d"},
		{h.C1[:8] + "/d/f", h.C1[:8], "", h.C1, "d/f"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got, err := g.ResolveRefPath(ctx, h.Name, tt.path)
			require.NoError(t, err)
			assert.Equal(t, tt.name, got.Name)
			assert.Equal(t, tt.ref, got.Ref)
			assert.Equal(t, tt.id, got.CommitID.String())
			assert.Equal(t, tt.rest, got.Rest.String())
		})
	}
	for _, bad := range []string{"", "/", "nope", "nope/d/f", "a//b", "../x", "d/f"} {
		t.Run("missing "+bad, func(t *testing.T) {
			_, err := g.ResolveRefPath(ctx, h.Name, bad)
			assert.ErrorIs(t, err, domain.ErrNotFound)
		})
	}
	_, err := g.ResolveRefPath(ctx, RepoName(t, "nope"), "main")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func testGetTree(t *testing.T, mk Maker) {
	ctx := context.Background()
	g, data := mk(t)
	h := NewHistory(t, data)

	t.Run("root at HEAD", func(t *testing.T) {
		tree, err := g.GetTree(ctx, h.Name, domain.Revision{}, domain.TreePath{}, false)
		require.NoError(t, err)
		assert.Equal(t, h.Merge, tree.CommitID.String())
		assert.Equal(t, h.Git("rev-parse", h.Merge+"^{tree}"), tree.ID.String())
		type row struct {
			name, path string
			kind       domain.EntryKind
			mode       uint32
			size       int64
		}
		var got []row
		for _, e := range tree.Entries {
			got = append(got, row{e.Name, e.Path, e.Kind, e.Mode, e.Size})
			assert.Nil(t, e.LastCommit)
		}
		assert.Equal(t, []row{
			{"d", "d", domain.EntryDirectory, 0o040000, 0},
			{"b.txt", "b.txt", domain.EntryFile, 0o100644, 19},
			{"bin", "bin", domain.EntryFile, 0o100644, 2},
			{"empty", "empty", domain.EntryFile, 0o100644, 0},
			{"main.txt", "main.txt", domain.EntryFile, 0o100644, 2},
			{"n.txt", "n.txt", domain.EntrySymlink, 0o120000, 5},
			{"side.txt", "side.txt", domain.EntryFile, 0o100644, 2},
			{"we:ird*.txt", "we:ird*.txt", domain.EntryFile, 0o100644, 1},
		}, got)
		assert.False(t, tree.LastCommitsOmitted)
	})

	t.Run("subdirectory with an executable", func(t *testing.T) {
		tree, err := g.GetTree(ctx, h.Name, Rev(t, "v2"), TreePath(t, "d"), false)
		require.NoError(t, err)
		require.Len(t, tree.Entries, 2)
		assert.Equal(t, "sub", tree.Entries[0].Name)
		assert.Equal(t, "d/sub", tree.Entries[0].Path)
		assert.Equal(t, domain.EntryExecutable, tree.Entries[1].Kind)
		assert.Equal(t, uint32(0o100755), tree.Entries[1].Mode)
		assert.Equal(t, "d/f", tree.Entries[1].Path)
		assert.Equal(t, h.C2, tree.CommitID.String())
	})

	t.Run("errors", func(t *testing.T) {
		_, err := g.GetTree(ctx, h.Name, domain.Revision{}, TreePath(t, "nope"), false)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = g.GetTree(ctx, h.Name, domain.Revision{}, TreePath(t, "d/nope/x"), false)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = g.GetTree(ctx, h.Name, domain.Revision{}, TreePath(t, "b.txt"), false)
		assert.ErrorIs(t, err, domain.ErrNotDirectory)
		_, err = g.GetTree(ctx, h.Name, Rev(t, "nope"), domain.TreePath{}, false)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("paths are literal", func(t *testing.T) {
		tree, err := g.GetTree(ctx, h.Name, domain.Revision{}, TreePath(t, "d"), true)
		require.NoError(t, err)
		require.Len(t, tree.Entries, 2)
		_, err = g.GetTree(ctx, h.Name, domain.Revision{}, TreePath(t, "*"), false)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
}

func testGetTreeLastCommit(t *testing.T, mk Maker) {
	ctx := context.Background()
	g, data := mk(t)
	h := NewHistory(t, data)

	tree, err := g.GetTree(ctx, h.Name, Rev(t, "main"), domain.TreePath{}, true)
	require.NoError(t, err)
	got := map[string]string{}
	for _, e := range tree.Entries {
		require.NotNil(t, e.LastCommit, e.Path)
		got[e.Path] = e.LastCommit.Summary
		// The oracle: git log -1 for the same path.
		assert.Equal(t, h.Git("log", "-1", "--format=%H", h.Merge, "--", e.Path), e.LastCommit.ID.String(), e.Path)
	}
	assert.Equal(t, "second", got["d"], "chmod of d/f is the last change of d")
	assert.Equal(t, "second", got["b.txt"])
	assert.Equal(t, "first", got["we:ird*.txt"])
	assert.Equal(t, "third: type change", got["n.txt"])
	assert.Equal(t, "side work", got["side.txt"])
	assert.Equal(t, "Ada Lovelace", tree.Entries[0].LastCommit.Author.Name)
}

func testGetTreeLastCommitBound(t *testing.T, mk Maker) {
	ctx := context.Background()
	g, data := mk(t)
	f := NewFixture(t, data, "wide")
	n := domain.MaxLastCommitEntries + 5
	for i := range n {
		f.Write("f"+strings.Repeat("0", 3-len(itoa(i)))+itoa(i), "x\n")
	}
	f.Commit("wide")

	tree, err := g.GetTree(ctx, f.Name, domain.Revision{}, domain.TreePath{}, true)
	require.NoError(t, err)
	require.Len(t, tree.Entries, n)
	assert.True(t, tree.LastCommitsOmitted)
	with := 0
	for _, e := range tree.Entries {
		if e.LastCommit != nil {
			with++
		}
	}
	assert.Equal(t, domain.MaxLastCommitEntries, with)
	assert.NotNil(t, tree.Entries[0].LastCommit)
	assert.Nil(t, tree.Entries[n-1].LastCommit)

	tree, err = g.GetTree(ctx, f.Name, domain.Revision{}, domain.TreePath{}, false)
	require.NoError(t, err)
	assert.False(t, tree.LastCommitsOmitted)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

func testListCommits(t *testing.T, mk Maker) {
	ctx := context.Background()
	g, data := mk(t)
	h := NewHistory(t, data)

	oracle := strings.Split(h.Git("log", "--format=%H", h.Merge), "\n")
	require.Len(t, oracle, 6)

	t.Run("all", func(t *testing.T) {
		page, err := g.ListCommits(ctx, h.Name, ports.CommitQuery{Limit: 100})
		require.NoError(t, err)
		assert.Equal(t, h.Merge, page.Head.String())
		assert.False(t, page.HasMore)
		var ids []string
		for _, c := range page.Commits {
			ids = append(ids, c.ID.String())
		}
		assert.Equal(t, oracle, ids)
	})

	t.Run("pages", func(t *testing.T) {
		var ids []string
		skip := 0
		for {
			page, err := g.ListCommits(ctx, h.Name, ports.CommitQuery{Skip: skip, Limit: 2})
			require.NoError(t, err)
			for _, c := range page.Commits {
				ids = append(ids, c.ID.String())
			}
			skip += len(page.Commits)
			if !page.HasMore {
				break
			}
			require.Len(t, page.Commits, 2)
		}
		assert.Equal(t, oracle, ids)
	})

	t.Run("path", func(t *testing.T) {
		page, err := g.ListCommits(ctx, h.Name, ports.CommitQuery{Path: TreePath(t, "d/f"), Limit: 10})
		require.NoError(t, err)
		var ids []string
		for _, c := range page.Commits {
			ids = append(ids, c.ID.String())
		}
		assert.Equal(t, strings.Split(h.Git("log", "--format=%H", h.Merge, "--", "d/f"), "\n"), ids)
		assert.Len(t, ids, 2)

		page, err = g.ListCommits(ctx, h.Name, ports.CommitQuery{Path: TreePath(t, "we:ird*.txt"), Limit: 10})
		require.NoError(t, err)
		require.Len(t, page.Commits, 1, "paths are literal")
		page, err = g.ListCommits(ctx, h.Name, ports.CommitQuery{Path: TreePath(t, "*"), Limit: 10})
		require.NoError(t, err)
		assert.Empty(t, page.Commits)
	})

	t.Run("revision", func(t *testing.T) {
		page, err := g.ListCommits(ctx, h.Name, ports.CommitQuery{Revision: Rev(t, "v2"), Limit: 10})
		require.NoError(t, err)
		require.Len(t, page.Commits, 2)
		assert.Equal(t, h.C2, page.Head.String())
		assert.Equal(t, h.C2, page.Commits[0].ID.String())
	})

	t.Run("metadata", func(t *testing.T) {
		page, err := g.ListCommits(ctx, h.Name, ports.CommitQuery{Revision: domain.RevisionOf(MustID(t, h.C1)), Limit: 10})
		require.NoError(t, err)
		require.Len(t, page.Commits, 1)
		c := page.Commits[0]
		assert.Empty(t, c.Parents)
		assert.Equal(t, "first", c.Summary)
		assert.Equal(t, "first\n\nbody of the first commit", c.Message)
		assert.Equal(t, domain.Signature{Name: "Ada Lovelace", Email: "ada@example.org", Time: time.Date(2026, 1, 1, 12, 1, 0, 0, time.UTC)}, c.Author)
		assert.Equal(t, "Grace Hopper", c.Committer.Name)
		assert.Equal(t, "grace@example.org", c.Committer.Email)

		page, err = g.ListCommits(ctx, h.Name, ports.CommitQuery{Limit: 1})
		require.NoError(t, err)
		require.Len(t, page.Commits, 1)
		assert.Equal(t, []string{h.Git("rev-parse", h.Merge+"^1"), h.Side}, idStrings(page.Commits[0].Parents))
	})

	t.Run("bad queries", func(t *testing.T) {
		for _, q := range []ports.CommitQuery{{Limit: 0}, {Limit: -1}, {Limit: ports.MaxCommitLimit + 1}, {Limit: 1, Skip: -1}} {
			_, err := g.ListCommits(ctx, h.Name, q)
			assert.ErrorIs(t, err, domain.ErrInvalidPageSize, "%+v", q)
		}
		_, err := g.ListCommits(ctx, h.Name, ports.CommitQuery{Revision: Rev(t, "nope"), Limit: 1})
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = g.ListCommits(ctx, RepoName(t, "nope"), ports.CommitQuery{Limit: 1})
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
}

// MustID parses an object id.
func MustID(t *testing.T, s string) domain.ObjectID {
	t.Helper()
	id, err := domain.ParseObjectID(s)
	require.NoError(t, err)
	return id
}

func idStrings(ids []domain.ObjectID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

func testOpenBlob(t *testing.T, mk Maker) {
	ctx := context.Background()
	g, data := mk(t)
	h := NewHistory(t, data)

	read := func(t *testing.T, r string, p string) (domain.BlobInfo, string) {
		t.Helper()
		info, rc, err := g.OpenBlob(ctx, h.Name, Rev(t, r), TreePath(t, p))
		require.NoError(t, err)
		body, err := io.ReadAll(rc)
		require.NoError(t, err)
		require.NoError(t, rc.Close())
		return info, string(body)
	}

	info, body := read(t, "main", "b.txt")
	assert.Equal(t, "one\ntwo\nthree\nfour\n", body)
	assert.Equal(t, int64(19), info.Size)
	assert.Equal(t, "b.txt", info.Path)
	assert.Equal(t, uint32(0o100644), info.Mode)
	assert.False(t, info.Binary)
	assert.Equal(t, "text/plain", info.MediaType)
	assert.Equal(t, h.Git("rev-parse", "main:b.txt"), info.ID.String())

	info, body = read(t, "main", "bin")
	assert.Equal(t, "\x00\x02", body)
	assert.True(t, info.Binary)
	assert.Equal(t, "application/octet-stream", info.MediaType)

	info, body = read(t, "main", "empty")
	assert.Empty(t, body)
	assert.Equal(t, int64(0), info.Size)
	assert.False(t, info.Binary)

	info, body = read(t, "main", "n.txt")
	assert.Equal(t, "b.txt", body, "a symlink's content is its target")
	assert.Equal(t, uint32(0o120000), info.Mode)

	info, _ = read(t, "main", "d/f")
	assert.Equal(t, uint32(0o100755), info.Mode)

	info, _ = read(t, "v1", "a.txt")
	assert.Equal(t, "text/plain", info.MediaType)

	for p, want := range map[string]error{"d": domain.ErrNotFile, "nope": domain.ErrNotFound, "d/nope": domain.ErrNotFound, "": domain.ErrNotFile} {
		_, rc, err := g.OpenBlob(ctx, h.Name, domain.Revision{}, TreePath(t, p))
		assert.ErrorIs(t, err, want, p)
		assert.Nil(t, rc)
	}
	_, _, err := g.OpenBlob(ctx, h.Name, Rev(t, "nope"), TreePath(t, "b.txt"))
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func testOpenBlobMediaTypes(t *testing.T, mk Maker) {
	ctx := context.Background()
	g, data := mk(t)
	f := NewFixture(t, data, "media")
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")
	f.WriteBytes("logo.png", png).
		WriteBytes("noext", png).
		Write("page.html", "<!doctype html><html><body>hi</body></html>\n").
		Write("style.css", "a { color: red }\n").
		Write("data.json", `{"a":1}`+"\n").
		Write("notes", "plain words\n").
		Write("main.go", "package main\n").
		WriteBytes("blob.dat", []byte{0xff, 0xfe, 0, 1, 2}).
		WriteBytes("fake.txt", []byte{'a', 0, 'b'}).
		Write("latin", "caf\xe9\n")
	f.Commit("media")

	want := map[string]struct {
		media  string
		binary bool
	}{
		"logo.png":  {"image/png", true},
		"noext":     {"image/png", true},
		"page.html": {"text/html", false},
		"style.css": {"text/css", false},
		"data.json": {"application/json", false},
		"notes":     {"text/plain", false},
		"main.go":   {"text/plain", false},
		"blob.dat":  {"application/octet-stream", true},
		"fake.txt":  {"application/octet-stream", true},
		"latin":     {"text/plain", false},
	}
	for p, w := range want {
		info, rc, err := g.OpenBlob(ctx, f.Name, domain.Revision{}, TreePath(t, p))
		require.NoError(t, err, p)
		require.NoError(t, rc.Close())
		assert.Equal(t, w.media, info.MediaType, p)
		assert.Equal(t, w.binary, info.Binary, p)
	}
}

func testBinaryRuleLooksAtFirst8000Bytes(t *testing.T, mk Maker) {
	ctx := context.Background()
	g, data := mk(t)
	f := NewFixture(t, data, "sniff")
	late := append([]byte(strings.Repeat("a", 8000)), 0) // NUL at offset 8000: not seen
	early := append([]byte(strings.Repeat("a", 7999)), 0)
	f.WriteBytes("late", late).WriteBytes("early", early)
	// Over the in-memory threshold, so that the streaming path decides too.
	f.WriteBytes("biglate", append([]byte(strings.Repeat("a", 300<<10)), 0)).
		WriteBytes("bigearly", append([]byte(strings.Repeat("a", 7999)), append([]byte{0}, make([]byte, 300<<10)...)...))
	f.Commit("sniff")

	for p, binary := range map[string]bool{"late": false, "early": true, "biglate": false, "bigearly": true} {
		info, rc, err := g.OpenBlob(ctx, f.Name, domain.Revision{}, TreePath(t, p))
		require.NoError(t, err, p)
		require.NoError(t, rc.Close())
		assert.Equal(t, binary, info.Binary, p)
	}
}

func testOpenBlobLargeThroughStream(t *testing.T, mk Maker) {
	ctx := context.Background()
	g, data := mk(t)
	f := NewFixture(t, data, "large")
	content := RandomBytes(1<<20+17, 7)
	f.WriteBytes("big.bin", content).Commit("big")

	info, rc, err := g.OpenBlob(ctx, f.Name, domain.Revision{}, TreePath(t, "big.bin"))
	require.NoError(t, err)
	assert.Equal(t, int64(len(content)), info.Size)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, content, got)
	require.NoError(t, rc.Close())
	require.NoError(t, rc.Close(), "Close is idempotent")
}

func testCancelledContext(t *testing.T, mk Maker) {
	g, data := mk(t)
	h := NewHistory(t, data)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := g.ListRefs(ctx, h.Name)
	assert.ErrorIs(t, err, context.Canceled)
	_, err = g.ResolveRevision(ctx, h.Name, domain.Revision{})
	assert.ErrorIs(t, err, context.Canceled)
	_, err = g.GetCommit(ctx, h.Name, domain.Revision{}, domain.DefaultDiffLimits())
	assert.ErrorIs(t, err, context.Canceled)
	_, err = g.ListCommits(ctx, h.Name, ports.CommitQuery{Limit: 5})
	assert.ErrorIs(t, err, context.Canceled)

	// The adapter still works with a live context.
	_, err = g.ResolveRevision(context.Background(), h.Name, domain.Revision{})
	assert.NoError(t, err)
}

func testHostileRevisionAndPathStayLiteral(t *testing.T, mk Maker) {
	ctx := context.Background()
	g, data := mk(t)
	h := NewHistory(t, data)

	// Revisions that look like options or git revision syntax are rejected by
	// the domain; ones that pass it are plain names for the reader.
	for _, s := range []string{"--output=/tmp/x", "-h", "main^", "main~1", "main:b.txt", "@{u}", "HEAD@{1}"} {
		_, err := domain.ParseRevision(s)
		assert.ErrorIs(t, err, domain.ErrInvalidRevision, s)
	}
	_, err := g.ResolveRevision(ctx, h.Name, Rev(t, "HEAD.x"))
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = g.GetTree(ctx, h.Name, domain.Revision{}, TreePath(t, ":(top)d"), false)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	var pe *domain.InvalidNameError
	assert.False(t, errors.As(err, &pe))
}

// A full object id is an object id before it is a ref: a branch or a tag
// named like a commit must not redirect a link to that commit.
func testResolveRevisionObjectIDBeatsRefs(t *testing.T, mk Maker) {
	ctx := context.Background()
	g, data := mk(t)
	h := NewHistory(t, data)

	// Git itself warns about these refs and prefers the object id.
	h.Git("-c", "advice.objectNameWarning=false", "branch", h.C1, h.C3)
	h.Git("-c", "advice.objectNameWarning=false", "tag", h.C2, h.C3)

	got, err := g.ResolveRevision(ctx, h.Name, Rev(t, h.C1))
	require.NoError(t, err)
	assert.Equal(t, h.C1, got.CommitID.String(), "the commit, not the branch of the same name")
	assert.Empty(t, got.Ref)

	got, err = g.ResolveRevision(ctx, h.Name, Rev(t, h.C2))
	require.NoError(t, err)
	assert.Equal(t, h.C2, got.CommitID.String(), "the commit, not the tag of the same name")

	// An abbreviation is still a name first, as before.
	h.Git("branch", "abc1234", h.C3)
	if !strings.HasPrefix(h.C1, "abc1234") {
		got, err = g.ResolveRevision(ctx, h.Name, Rev(t, "abc1234"))
		require.NoError(t, err)
		assert.Equal(t, h.C3, got.CommitID.String())
		assert.Equal(t, "refs/heads/abc1234", got.Ref)
	}

	// The ref is used when no object has that id.
	ghost := strings.Repeat("ab", 20)
	h.Git("-c", "advice.objectNameWarning=false", "branch", ghost, h.C3)
	got, err = g.ResolveRevision(ctx, h.Name, Rev(t, ghost))
	require.NoError(t, err)
	assert.Equal(t, h.C3, got.CommitID.String())
	assert.Equal(t, "refs/heads/"+ghost, got.Ref)
}

func testGetTreeIsBounded(t *testing.T, mk Maker) {
	ctx := context.Background()
	g, data := mk(t)
	f := NewFixture(t, data, "wide")
	n := domain.MaxTreeEntries + 7
	for i := range n {
		f.Write(fmt.Sprintf("d/f%05d", i), "x")
	}
	f.Commit("wide")

	tree, err := g.GetTree(ctx, f.Name, domain.Revision{}, TreePath(t, "d"), false)
	require.NoError(t, err)
	assert.True(t, tree.Truncated)
	require.Len(t, tree.Entries, domain.MaxTreeEntries)
	assert.Equal(t, "f00000", tree.Entries[0].Name, "the first entries in order")
	assert.Equal(t, fmt.Sprintf("f%05d", domain.MaxTreeEntries-1), tree.Entries[domain.MaxTreeEntries-1].Name)

	small, err := g.GetTree(ctx, f.Name, domain.Revision{}, domain.TreePath{}, false)
	require.NoError(t, err)
	assert.False(t, small.Truncated)
}

func testListPaths(t *testing.T, mk Maker) {
	ctx := context.Background()
	g, data := mk(t)
	f := buildPaths(t, data)

	// git ls-tree -r in order, minus the submodule and the path no address can
	// carry.
	var want []string
	for _, rec := range strings.Split(strings.TrimSuffix(GitOut(t, f.Bare, "ls-tree", "-r", "-z", "HEAD"), "\x00"), "\x00") {
		meta, p, _ := strings.Cut(rec, "\t")
		if !strings.Contains(meta, " commit ") && !strings.Contains(p, "\t") {
			want = append(want, p)
		}
	}
	require.Equal(t, []string{
		"a-b.txt", "a/b.txt", "a/b/c.txt", "d/e/f/deep.txt", "héllo wörld.txt", "link", "run.sh", "z.txt",
	}, want, "the fixture")

	got, err := g.ListPaths(ctx, f.Name, domain.Revision{}, domain.MaxPathEntries, domain.MaxPathBytes)
	require.NoError(t, err)
	assert.Equal(t, want, got.Paths)
	assert.False(t, got.Truncated)
	assert.Equal(t, GitOut(t, f.Bare, "rev-parse", "HEAD"), got.CommitID.String())
	assert.Equal(t, GitOut(t, f.Bare, "rev-parse", "HEAD^{tree}"), got.TreeID.String())

	t.Run("revisions", func(t *testing.T) {
		for _, s := range []string{"main", "v1", "refs/tags/v1", f.Git("rev-parse", "HEAD"), f.Git("rev-parse", "HEAD")[:10]} {
			list, err := g.ListPaths(ctx, f.Name, Rev(t, s), domain.MaxPathEntries, domain.MaxPathBytes)
			require.NoError(t, err, s)
			assert.Equal(t, want, list.Paths, s)
		}
		// The first commit has no submodule: the same files.
		first, err := g.ListPaths(ctx, f.Name, Rev(t, f.Git("rev-parse", "HEAD~1")), domain.MaxPathEntries, domain.MaxPathBytes)
		require.NoError(t, err)
		assert.Equal(t, want, first.Paths)
		assert.Equal(t, GitOut(t, f.Bare, "rev-parse", "HEAD~1^{tree}"), first.TreeID.String())
		assert.NotEqual(t, got.TreeID, first.TreeID)
	})
	t.Run("not found", func(t *testing.T) {
		for _, s := range []string{"nope", "refs/heads/nope", strings.Repeat("ab", 20), "deadbeef"} {
			_, err := g.ListPaths(ctx, f.Name, Rev(t, s), domain.MaxPathEntries, domain.MaxPathBytes)
			assert.ErrorIs(t, err, domain.ErrNotFound, s)
		}
		_, err := g.ListPaths(ctx, RepoName(t, "no-such-repo"), domain.Revision{}, domain.MaxPathEntries, domain.MaxPathBytes)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
	t.Run("empty repository", func(t *testing.T) {
		e := NewFixture(t, data, "empty-paths")
		_, err := g.ListPaths(ctx, e.Name, domain.Revision{}, domain.MaxPathEntries, domain.MaxPathBytes)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
	t.Run("bounds", func(t *testing.T) {
		list, err := g.ListPaths(ctx, f.Name, domain.Revision{}, 3, domain.MaxPathBytes)
		require.NoError(t, err)
		assert.Equal(t, want[:3], list.Paths)
		assert.True(t, list.Truncated)
		assert.Equal(t, got.CommitID, list.CommitID)
		assert.Equal(t, got.TreeID, list.TreeID)

		list, err = g.ListPaths(ctx, f.Name, domain.Revision{}, len(want), domain.MaxPathBytes)
		require.NoError(t, err)
		assert.Equal(t, want, list.Paths)
		assert.False(t, list.Truncated, "exactly maxPaths paths is not truncated")

		sum := len(want[0]) + len(want[1]) + len(want[2])
		list, err = g.ListPaths(ctx, f.Name, domain.Revision{}, domain.MaxPathEntries, sum-1)
		require.NoError(t, err)
		assert.Equal(t, want[:2], list.Paths)
		assert.True(t, list.Truncated)

		list, err = g.ListPaths(ctx, f.Name, domain.Revision{}, domain.MaxPathEntries, sum)
		require.NoError(t, err)
		assert.Equal(t, want[:3], list.Paths, "bytes up to maxBytes fit")
		assert.True(t, list.Truncated)

		total := 0
		for _, p := range want {
			total += len(p)
		}
		list, err = g.ListPaths(ctx, f.Name, domain.Revision{}, domain.MaxPathEntries, total)
		require.NoError(t, err)
		assert.Equal(t, want, list.Paths)
		assert.False(t, list.Truncated, "exactly maxBytes bytes is not truncated")
	})
	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := g.ListPaths(ctx, f.Name, domain.Revision{}, domain.MaxPathEntries, domain.MaxPathBytes)
		assert.ErrorIs(t, err, context.Canceled)
	})
	t.Run("a large tree", func(t *testing.T) {
		w := NewFixture(t, data, "paths-wide")
		for i := range 300 {
			w.Write(fmt.Sprintf("d%02d/e%d/f%03d.txt", i%17, i%5, i), "x")
		}
		w.Commit("wide")
		order := lines(GitOut(t, w.Bare, "ls-tree", "-r", "--name-only", "HEAD"))
		list, err := g.ListPaths(ctx, w.Name, domain.Revision{}, domain.MaxPathEntries, domain.MaxPathBytes)
		require.NoError(t, err)
		assert.Equal(t, order, list.Paths)
		list, err = g.ListPaths(ctx, w.Name, domain.Revision{}, 100, domain.MaxPathBytes)
		require.NoError(t, err)
		assert.Equal(t, order[:100], list.Paths)
		assert.True(t, list.Truncated)
	})
}

// A tree entry of mode 160000 (a submodule) names a commit of another
// repository. When its id is that of a tree this repository has, it still is no
// directory of it: both readers answer not found, and the same id as a real
// directory lists.
func testGetTreeRefusesAGitlinkToATree(t *testing.T, mk Maker) {
	ctx := context.Background()
	g, data := mk(t)
	f := NewFixture(t, data, "gitlink")
	f.Write("README", "r\n")
	f.Write("dir/f.txt", "f\n")
	f.Commit("dir")
	dirID := f.Git("rev-parse", "HEAD:dir")
	raw := func(entries ...[3]string) string {
		var b strings.Builder
		for _, e := range entries {
			id, err := hex.DecodeString(e[2])
			require.NoError(t, err)
			b.WriteString(e[0] + " " + e[1] + "\x00")
			b.Write(id)
		}
		return f.WriteObject("tree", b.String())
	}
	both := raw([3]string{"40000", "asdir", dirID}, [3]string{"160000", "sub", dirID}) // the same id, two modes
	commit := f.Git("commit-tree", both, "-m", "gitlink")
	f.Git("update-ref", "refs/heads/main", commit)

	tree, err := g.GetTree(ctx, f.Name, Rev(t, commit), TreePath(t, "asdir"), false)
	require.NoError(t, err)
	require.Len(t, tree.Entries, 1)
	assert.Equal(t, "f.txt", tree.Entries[0].Name)
	_, err = g.GetTree(ctx, f.Name, Rev(t, commit), TreePath(t, "sub"), false)
	assert.ErrorIs(t, err, domain.ErrNotFound, "a gitlink is no directory, whatever it names")
}
