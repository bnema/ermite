// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/readertest"
)

func tp(t *testing.T, s string) domain.TreePath {
	t.Helper()
	p, err := domain.ParseTreePath(s)
	require.NoError(t, err)
	return p
}

func TestWriteTree(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	f := readertest.NewFixture(t, data, "proj")
	f.Write("a.txt", "one\n").Write("dir/b.txt", "bee\n").Write("run.sh", "#!/bin/sh\n").Write("-dash.txt", "d\n")
	f.Chmod("run.sh", 0o755)
	f.Symlink("a.txt", "link")
	base := oidOf(t, f.Commit("base"))

	refs := f.Git("for-each-ref")
	status := f.Git("status", "--porcelain")
	idx := f.Git("ls-files", "-s")

	t.Cleanup(func() { _ = g.Close(ctx) })

	// Expected tree: the same change committed by hand.
	got, err := g.WriteTree(ctx, f.Name, base, map[domain.TreePath][]byte{
		tp(t, "dir/b.txt"): []byte("new\n"),
		tp(t, "run.sh"):    []byte("#!/bin/sh\necho\n"),
		tp(t, "-dash.txt"): []byte("D\n"),
	})
	require.NoError(t, err)

	assert.Equal(t, refs, f.Git("for-each-ref"))
	assert.Equal(t, status, f.Git("status", "--porcelain"))
	assert.Equal(t, idx, f.Git("ls-files", "-s"))

	f.Write("dir/b.txt", "new\n").Write("run.sh", "#!/bin/sh\necho\n").Write("-dash.txt", "D\n")
	want := f.Git("rev-parse", oidOf(t, f.Commit("manual")).String()+"^{tree}")
	assert.Equal(t, want, got.String())
	assert.Equal(t, "100755", f.Git("ls-tree", got.String(), "--", "run.sh")[:6])

	// A tree id is also accepted as base.
	tree, err := g.CommitTree(ctx, f.Name, base)
	require.NoError(t, err)
	got2, err := g.WriteTree(ctx, f.Name, tree, map[domain.TreePath][]byte{tp(t, "a.txt"): []byte("one\n")})
	require.NoError(t, err)
	assert.Equal(t, tree, got2, "same content, same tree")

	for name, p := range map[string]string{"directory": "dir", "symlink": "link", "below file": "a.txt/x"} {
		_, err = g.WriteTree(ctx, f.Name, base, map[domain.TreePath][]byte{tp(t, p): []byte("x")})
		assert.ErrorIs(t, err, domain.ErrInvalid, name)
	}
	for name, p := range map[string]string{"below symlink": "link/x", "deep below file": "a.txt/x/y"} {
		_, err = g.WriteTree(ctx, f.Name, base, map[domain.TreePath][]byte{tp(t, p): []byte("x")})
		assert.ErrorIs(t, err, domain.ErrInvalid, name)
	}

	// New files: at the root, in an existing directory and in a new one.
	created, err := g.WriteTree(ctx, f.Name, base, map[domain.TreePath][]byte{
		tp(t, "go.sum"):       []byte("root\n"),
		tp(t, "dir/go.sum"):   []byte("sub\n"),
		tp(t, "new/x/go.sum"): []byte("deep\n"),
	})
	require.NoError(t, err)
	for p, c := range map[string]string{"go.sum": "root\n", "dir/go.sum": "sub\n", "new/x/go.sum": "deep\n"} {
		assert.Equal(t, "100644", f.Git("ls-tree", created.String(), "--", p)[:6], p)
		assert.Equal(t, strings.TrimSuffix(c, "\n"), f.Git("cat-file", "blob", created.String()+":"+p), p)
	}
	assert.Equal(t, "100755", f.Git("ls-tree", created.String(), "--", "run.sh")[:6])

	_, err = g.WriteTree(ctx, f.Name, base, map[domain.TreePath][]byte{{}: []byte("x")})
	assert.ErrorIs(t, err, domain.ErrInvalid, "root")
	_, err = g.WriteTree(ctx, name(t, "missing"), base, nil)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestUpdateBranchCreate(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	m := newMergeFixture(t, data)

	require.NoError(t, g.UpdateBranch(ctx, m.f.Name, "fresh", domain.ObjectID{}, m.a))
	assert.Equal(t, m.a.String(), m.f.Git("rev-parse", "refs/heads/fresh"))

	err := g.UpdateBranch(ctx, m.f.Name, "fresh", domain.ObjectID{}, m.b)
	assert.ErrorIs(t, err, domain.ErrConflict, "already exists")
	assert.Equal(t, m.a.String(), m.f.Git("rev-parse", "refs/heads/fresh"))
}
