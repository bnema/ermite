// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

func rev(t *testing.T, s string) domain.Revision {
	t.Helper()
	r, err := domain.ParseRevision(s)
	require.NoError(t, err)
	return r
}

func tpath(t *testing.T, s string) domain.TreePath {
	t.Helper()
	p, err := domain.ParseTreePath(s)
	require.NoError(t, err)
	return p
}

// history is the repository of the read tests: three commits on main, a
// branch with a slash, a merge, lightweight and annotated tags.
type history struct {
	*fixtureRepo
	c1, c2, c3, side, merge string
}

func newHistory(t *testing.T, data string) *history {
	t.Helper()
	f := newFixtureRepo(t, data, "hist")
	h := &history{fixtureRepo: f}
	f.write("a.txt", "one\ntwo\nthree\n").
		writeBytes("bin", []byte{0, 1}).
		write("d/f", "hi\n").
		write("d/sub/g", "deep\n").
		write("we:ird*.txt", "x").
		write("README.md", "# hello\n").
		write("empty", "")
	h.c1 = f.commit("first\n\nbody of the first commit\n")

	f.mv("a.txt", "b.txt").write("b.txt", "one\ntwo\nthree\nfour\n").
		writeBytes("bin", []byte{0, 2}).
		chmod("d/f", 0o755).
		write("n.txt", "new\nnonl").
		remove("README.md")
	h.c2 = f.commit("second")

	f.remove("n.txt").symlink("b.txt", "n.txt")
	h.c3 = f.commit("third: type change")

	f.checkoutNew("feature/x")
	f.write("side.txt", "s\n")
	h.side = f.commit("side work")
	f.checkout("main")
	f.write("main.txt", "m\n")
	f.commit("main work")
	h.merge = f.merge("feature/x")

	f.git("tag", "v1", h.c1)
	f.git("tag", "-a", "-m", "release two", "v2", h.c2)
	f.git("tag", "-a", "-m", "tag of a tag", "v2-again", "v2")
	f.git("tag", "-a", "-m", "tag of a tree", "tree-tag", h.merge+"^{tree}")
	f.git("branch", "release/1.0", h.c2)
	f.git("branch", "rel/1", h.c1)
	f.git("tag", "rel/1/x", h.c3)
	return h
}

func TestListRefs(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	h := newHistory(t, data)

	refs, err := g.ListRefs(ctx, h.name)
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
	tagID := func(n string) string { return h.git("rev-parse", "refs/tags/"+n) }
	assert.Equal(t, []row{
		{"HEAD", "main", domain.RefKindHead, h.merge, h.merge},
		{"refs/heads/feature/x", "feature/x", domain.RefKindBranch, h.side, h.side},
		{"refs/heads/main", "main", domain.RefKindBranch, h.merge, h.merge},
		{"refs/heads/rel/1", "rel/1", domain.RefKindBranch, h.c1, h.c1},
		{"refs/heads/release/1.0", "release/1.0", domain.RefKindBranch, h.c2, h.c2},
		{"refs/tags/rel/1/x", "rel/1/x", domain.RefKindTag, h.c3, h.c3},
		{"refs/tags/tree-tag", "tree-tag", domain.RefKindTag, tagID("tree-tag"), ""},
		{"refs/tags/v1", "v1", domain.RefKindTag, h.c1, h.c1},
		{"refs/tags/v2", "v2", domain.RefKindTag, tagID("v2"), h.c2},
		{"refs/tags/v2-again", "v2-again", domain.RefKindTag, tagID("v2-again"), h.c2},
	}, got)
}

func TestListRefsEmptyAndMissing(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	f := newFixtureRepo(t, data, "empty")

	refs, err := g.ListRefs(ctx, f.name)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, "HEAD", refs[0].Name)
	assert.Equal(t, "main", refs[0].ShortName, "unborn default branch")
	assert.True(t, refs[0].ID.IsZero())
	assert.True(t, refs[0].CommitID.IsZero())

	_, err = g.ListRefs(ctx, name(t, "nope"))
	assert.ErrorIs(t, err, domain.ErrNotFound)

	// No commit to resolve in an empty repository.
	_, err = g.ResolveRevision(ctx, f.name, domain.Revision{})
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = g.GetTree(ctx, f.name, domain.Revision{}, domain.TreePath{}, false)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = g.ListCommits(ctx, f.name, ports.CommitQuery{Limit: 10})
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestDetachedHead(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	h := newHistory(t, data)
	h.git("update-ref", "--no-deref", "HEAD", h.c1)

	refs, err := g.ListRefs(ctx, h.name)
	require.NoError(t, err)
	assert.Equal(t, "HEAD", refs[0].Name)
	assert.Empty(t, refs[0].ShortName)
}

func TestResolveRevision(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	h := newHistory(t, data)

	tests := []struct {
		rev    string
		commit string
		ref    string
	}{
		{"", h.merge, ""},
		{"HEAD", h.merge, ""},
		{"main", h.merge, "refs/heads/main"},
		{"feature/x", h.side, "refs/heads/feature/x"},
		{"refs/heads/feature/x", h.side, "refs/heads/feature/x"},
		{"v1", h.c1, "refs/tags/v1"},
		{"v2", h.c2, "refs/tags/v2"},
		{"v2-again", h.c2, "refs/tags/v2-again"},
		{"refs/tags/v2", h.c2, "refs/tags/v2"},
		{h.c2, h.c2, ""},
		{h.c2[:7], h.c2, ""},
		{h.c2[:12], h.c2, ""},
	}
	for _, tt := range tests {
		t.Run(tt.rev, func(t *testing.T) {
			got, err := g.ResolveRevision(ctx, h.name, rev(t, tt.rev))
			require.NoError(t, err)
			assert.Equal(t, tt.commit, got.CommitID.String())
			assert.Equal(t, tt.ref, got.Ref)
		})
	}
	for _, bad := range []string{"nope", "tree-tag", "deadbeefdeadbeef", "HEAD~1", h.c2[:6], "refs/heads", h.git("rev-parse", h.merge+"^{tree}")} {
		t.Run("missing "+bad, func(t *testing.T) {
			r, err := domain.ParseRevision(bad)
			if err != nil {
				return // not a revision at all, rejected before the reader
			}
			_, err = g.ResolveRevision(ctx, h.name, r)
			assert.ErrorIs(t, err, domain.ErrNotFound)
		})
	}

	t.Run("a branch wins over a tag of the same name", func(t *testing.T) {
		h.git("branch", "both", h.c1)
		h.git("tag", "both", h.c2)
		got, err := g.ResolveRevision(ctx, h.name, rev(t, "both"))
		require.NoError(t, err)
		assert.Equal(t, h.c1, got.CommitID.String())
	})
}

func TestResolveRefPath(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	h := newHistory(t, data)

	tests := []struct {
		path string
		name string
		ref  string
		id   string
		rest string
	}{
		{"main", "main", "refs/heads/main", h.merge, ""},
		{"main/d/f", "main", "refs/heads/main", h.merge, "d/f"},
		{"feature/x", "feature/x", "refs/heads/feature/x", h.side, ""},
		{"feature/x/d/f", "feature/x", "refs/heads/feature/x", h.side, "d/f"},
		{"feature/x/", "feature/x", "refs/heads/feature/x", h.side, ""},
		{"release/1.0/a.txt", "release/1.0", "refs/heads/release/1.0", h.c2, "a.txt"},
		// The longest ref wins: the tag rel/1/x over the branch rel/1.
		{"rel/1/x/d/f", "rel/1/x", "refs/tags/rel/1/x", h.c3, "d/f"},
		{"rel/1/d/f", "rel/1", "refs/heads/rel/1", h.c1, "d/f"},
		{"v2/we:ird*.txt", "v2", "refs/tags/v2", h.c2, "we:ird*.txt"},
		{h.c1 + "/d", h.c1, "", h.c1, "d"},
		{h.c1[:8] + "/d/f", h.c1[:8], "", h.c1, "d/f"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got, err := g.ResolveRefPath(ctx, h.name, tt.path)
			require.NoError(t, err)
			assert.Equal(t, tt.name, got.Name)
			assert.Equal(t, tt.ref, got.Ref)
			assert.Equal(t, tt.id, got.CommitID.String())
			assert.Equal(t, tt.rest, got.Rest.String())
		})
	}
	for _, bad := range []string{"", "/", "nope", "nope/d/f", "a//b", "../x", "d/f"} {
		t.Run("missing "+bad, func(t *testing.T) {
			_, err := g.ResolveRefPath(ctx, h.name, bad)
			assert.ErrorIs(t, err, domain.ErrNotFound)
		})
	}
	_, err := g.ResolveRefPath(ctx, name(t, "nope"), "main")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestGetTree(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	h := newHistory(t, data)

	t.Run("root at HEAD", func(t *testing.T) {
		tree, err := g.GetTree(ctx, h.name, domain.Revision{}, domain.TreePath{}, false)
		require.NoError(t, err)
		assert.Equal(t, h.merge, tree.CommitID.String())
		assert.Equal(t, h.git("rev-parse", h.merge+"^{tree}"), tree.ID.String())
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
		tree, err := g.GetTree(ctx, h.name, rev(t, "v2"), tpath(t, "d"), false)
		require.NoError(t, err)
		require.Len(t, tree.Entries, 2)
		assert.Equal(t, "sub", tree.Entries[0].Name)
		assert.Equal(t, "d/sub", tree.Entries[0].Path)
		assert.Equal(t, domain.EntryExecutable, tree.Entries[1].Kind)
		assert.Equal(t, uint32(0o100755), tree.Entries[1].Mode)
		assert.Equal(t, "d/f", tree.Entries[1].Path)
		assert.Equal(t, h.c2, tree.CommitID.String())
	})

	t.Run("errors", func(t *testing.T) {
		_, err := g.GetTree(ctx, h.name, domain.Revision{}, tpath(t, "nope"), false)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = g.GetTree(ctx, h.name, domain.Revision{}, tpath(t, "d/nope/x"), false)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = g.GetTree(ctx, h.name, domain.Revision{}, tpath(t, "b.txt"), false)
		assert.ErrorIs(t, err, domain.ErrNotDirectory)
		_, err = g.GetTree(ctx, h.name, rev(t, "nope"), domain.TreePath{}, false)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("paths are literal", func(t *testing.T) {
		tree, err := g.GetTree(ctx, h.name, domain.Revision{}, tpath(t, "d"), true)
		require.NoError(t, err)
		require.Len(t, tree.Entries, 2)
		_, err = g.GetTree(ctx, h.name, domain.Revision{}, tpath(t, "*"), false)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
}

func TestGetTreeLastCommit(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	h := newHistory(t, data)

	tree, err := g.GetTree(ctx, h.name, rev(t, "main"), domain.TreePath{}, true)
	require.NoError(t, err)
	got := map[string]string{}
	for _, e := range tree.Entries {
		require.NotNil(t, e.LastCommit, e.Path)
		got[e.Path] = e.LastCommit.Summary
		// The oracle: git log -1 for the same path.
		assert.Equal(t, h.git("log", "-1", "--format=%H", h.merge, "--", e.Path), e.LastCommit.ID.String(), e.Path)
	}
	assert.Equal(t, "second", got["d"], "chmod of d/f is the last change of d")
	assert.Equal(t, "second", got["b.txt"])
	assert.Equal(t, "first", got["we:ird*.txt"])
	assert.Equal(t, "third: type change", got["n.txt"])
	assert.Equal(t, "side work", got["side.txt"])
	assert.Equal(t, "Ada Lovelace", tree.Entries[0].LastCommit.Author.Name)
}

func TestGetTreeLastCommitBound(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	f := newFixtureRepo(t, data, "wide")
	n := domain.MaxLastCommitEntries + 5
	for i := range n {
		f.write("f"+strings.Repeat("0", 3-len(itoa(i)))+itoa(i), "x\n")
	}
	f.commit("wide")

	tree, err := g.GetTree(ctx, f.name, domain.Revision{}, domain.TreePath{}, true)
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

	tree, err = g.GetTree(ctx, f.name, domain.Revision{}, domain.TreePath{}, false)
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

func TestListCommits(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	h := newHistory(t, data)

	oracle := strings.Split(h.git("log", "--format=%H", h.merge), "\n")
	require.Len(t, oracle, 6)

	t.Run("all", func(t *testing.T) {
		page, err := g.ListCommits(ctx, h.name, ports.CommitQuery{Limit: 100})
		require.NoError(t, err)
		assert.Equal(t, h.merge, page.Head.String())
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
			page, err := g.ListCommits(ctx, h.name, ports.CommitQuery{Skip: skip, Limit: 2})
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
		page, err := g.ListCommits(ctx, h.name, ports.CommitQuery{Path: tpath(t, "d/f"), Limit: 10})
		require.NoError(t, err)
		var ids []string
		for _, c := range page.Commits {
			ids = append(ids, c.ID.String())
		}
		assert.Equal(t, strings.Split(h.git("log", "--format=%H", h.merge, "--", "d/f"), "\n"), ids)
		assert.Len(t, ids, 2)

		page, err = g.ListCommits(ctx, h.name, ports.CommitQuery{Path: tpath(t, "we:ird*.txt"), Limit: 10})
		require.NoError(t, err)
		require.Len(t, page.Commits, 1, "paths are literal")
		page, err = g.ListCommits(ctx, h.name, ports.CommitQuery{Path: tpath(t, "*"), Limit: 10})
		require.NoError(t, err)
		assert.Empty(t, page.Commits)
	})

	t.Run("revision", func(t *testing.T) {
		page, err := g.ListCommits(ctx, h.name, ports.CommitQuery{Revision: rev(t, "v2"), Limit: 10})
		require.NoError(t, err)
		require.Len(t, page.Commits, 2)
		assert.Equal(t, h.c2, page.Head.String())
		assert.Equal(t, h.c2, page.Commits[0].ID.String())
	})

	t.Run("metadata", func(t *testing.T) {
		page, err := g.ListCommits(ctx, h.name, ports.CommitQuery{Revision: domain.RevisionOf(mustID(t, h.c1)), Limit: 10})
		require.NoError(t, err)
		require.Len(t, page.Commits, 1)
		c := page.Commits[0]
		assert.Empty(t, c.Parents)
		assert.Equal(t, "first", c.Summary)
		assert.Equal(t, "first\n\nbody of the first commit", c.Message)
		assert.Equal(t, domain.Signature{Name: "Ada Lovelace", Email: "ada@example.org", Time: time.Date(2026, 1, 1, 12, 1, 0, 0, time.UTC)}, c.Author)
		assert.Equal(t, "Grace Hopper", c.Committer.Name)
		assert.Equal(t, "grace@example.org", c.Committer.Email)

		page, err = g.ListCommits(ctx, h.name, ports.CommitQuery{Limit: 1})
		require.NoError(t, err)
		require.Len(t, page.Commits, 1)
		assert.Equal(t, []string{h.git("rev-parse", h.merge+"^1"), h.side}, idStrings(page.Commits[0].Parents))
	})

	t.Run("bad queries", func(t *testing.T) {
		for _, q := range []ports.CommitQuery{{Limit: 0}, {Limit: -1}, {Limit: ports.MaxCommitLimit + 1}, {Limit: 1, Skip: -1}} {
			_, err := g.ListCommits(ctx, h.name, q)
			assert.ErrorIs(t, err, domain.ErrInvalidPageSize, "%+v", q)
		}
		_, err := g.ListCommits(ctx, h.name, ports.CommitQuery{Revision: rev(t, "nope"), Limit: 1})
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = g.ListCommits(ctx, name(t, "nope"), ports.CommitQuery{Limit: 1})
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
}

func mustID(t *testing.T, s string) domain.ObjectID {
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

func TestOpenBlob(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	h := newHistory(t, data)

	read := func(t *testing.T, r string, p string) (domain.BlobInfo, string) {
		t.Helper()
		info, rc, err := g.OpenBlob(ctx, h.name, rev(t, r), tpath(t, p))
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
	assert.Equal(t, h.git("rev-parse", "main:b.txt"), info.ID.String())

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
		_, rc, err := g.OpenBlob(ctx, h.name, domain.Revision{}, tpath(t, p))
		assert.ErrorIs(t, err, want, p)
		assert.Nil(t, rc)
	}
	_, _, err := g.OpenBlob(ctx, h.name, rev(t, "nope"), tpath(t, "b.txt"))
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestOpenBlobMediaTypes(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	f := newFixtureRepo(t, data, "media")
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")
	f.writeBytes("logo.png", png).
		writeBytes("noext", png).
		write("page.html", "<!doctype html><html><body>hi</body></html>\n").
		write("style.css", "a { color: red }\n").
		write("data.json", `{"a":1}`+"\n").
		write("notes", "plain words\n").
		write("main.go", "package main\n").
		writeBytes("blob.dat", []byte{0xff, 0xfe, 0, 1, 2}).
		writeBytes("fake.txt", []byte{'a', 0, 'b'}).
		write("latin", "caf\xe9\n")
	f.commit("media")

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
		info, rc, err := g.OpenBlob(ctx, f.name, domain.Revision{}, tpath(t, p))
		require.NoError(t, err, p)
		require.NoError(t, rc.Close())
		assert.Equal(t, w.media, info.MediaType, p)
		assert.Equal(t, w.binary, info.Binary, p)
	}
}

func TestBinaryRuleLooksAtFirst8000Bytes(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	f := newFixtureRepo(t, data, "sniff")
	late := append([]byte(strings.Repeat("a", 8000)), 0) // NUL at offset 8000: not seen
	early := append([]byte(strings.Repeat("a", 7999)), 0)
	f.writeBytes("late", late).writeBytes("early", early)
	// Over the in-memory threshold, so that the streaming path decides too.
	f.writeBytes("biglate", append([]byte(strings.Repeat("a", 300<<10)), 0)).
		writeBytes("bigearly", append([]byte(strings.Repeat("a", 7999)), append([]byte{0}, make([]byte, 300<<10)...)...))
	f.commit("sniff")

	for p, binary := range map[string]bool{"late": false, "early": true, "biglate": false, "bigearly": true} {
		info, rc, err := g.OpenBlob(ctx, f.name, domain.Revision{}, tpath(t, p))
		require.NoError(t, err, p)
		require.NoError(t, rc.Close())
		assert.Equal(t, binary, info.Binary, p)
	}
}

func TestOpenBlobLargeThroughStream(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	f := newFixtureRepo(t, data, "large")
	content := randomBytes(1<<20+17, 7)
	f.writeBytes("big.bin", content).commit("big")

	info, rc, err := g.OpenBlob(ctx, f.name, domain.Revision{}, tpath(t, "big.bin"))
	require.NoError(t, err)
	assert.Equal(t, int64(len(content)), info.Size)
	assert.Equal(t, 1, g.BlobStreams())
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, content, got)
	require.NoError(t, rc.Close())
	require.NoError(t, rc.Close(), "Close is idempotent")
	assert.Equal(t, 0, g.BlobStreams())
}

func TestCancelledContext(t *testing.T) {
	g, data := newReader(t, nil)
	h := newHistory(t, data)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := g.ListRefs(ctx, h.name)
	assert.ErrorIs(t, err, context.Canceled)
	_, err = g.ResolveRevision(ctx, h.name, domain.Revision{})
	assert.ErrorIs(t, err, context.Canceled)
	_, err = g.GetCommit(ctx, h.name, domain.Revision{}, domain.DefaultDiffLimits())
	assert.ErrorIs(t, err, context.Canceled)
	_, err = g.ListCommits(ctx, h.name, ports.CommitQuery{Limit: 5})
	assert.ErrorIs(t, err, context.Canceled)

	// The adapter still works with a live context.
	_, err = g.ResolveRevision(context.Background(), h.name, domain.Revision{})
	assert.NoError(t, err)
}

func TestHostileRevisionAndPathStayLiteral(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	h := newHistory(t, data)

	// Revisions that look like options or git revision syntax are rejected by
	// the domain; ones that pass it are plain names for the reader.
	for _, s := range []string{"--output=/tmp/x", "-h", "main^", "main~1", "main:b.txt", "@{u}", "HEAD@{1}"} {
		_, err := domain.ParseRevision(s)
		assert.ErrorIs(t, err, domain.ErrInvalidRevision, s)
	}
	_, err := g.ResolveRevision(ctx, h.name, rev(t, "HEAD.x"))
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = g.GetTree(ctx, h.name, domain.Revision{}, tpath(t, ":(top)d"), false)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	var pe *domain.InvalidNameError
	assert.False(t, errors.As(err, &pe))
}
