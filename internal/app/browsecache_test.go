// SPDX-License-Identifier: AGPL-3.0-or-later

package app_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

func treePath(t *testing.T, s string) domain.TreePath {
	t.Helper()
	p, err := domain.ParseTreePath(s)
	require.NoError(t, err)
	return p
}

func treeWithLast(c string) domain.Tree {
	commit := domain.Commit{ID: oid(c), Parents: []domain.ObjectID{oid("0")}, Summary: "s"}
	return domain.Tree{
		CommitID: oid("a"),
		Entries:  []domain.TreeEntry{{Name: "f", Path: "d/f", LastCommit: &commit}},
	}
}

// TestGetTreeLastCommitCache covers which GetTree calls are served from the
// cache: only the ones with last commits at a full commit id.
func TestGetTreeLastCommitCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pub := domain.Repo{Name: name(t, "pub"), Visibility: domain.VisibilityPublic}
	full := domain.RevisionOf(oid("a"))
	branch, err := domain.ParseRevision("main")
	require.NoError(t, err)
	dir := treePath(t, "d")

	tests := []struct {
		name         string
		rev          domain.Revision
		includeLast  bool
		wantReaderOn int // reader calls for two identical requests
	}{
		{"full id with last commits is cached", full, true, 1},
		{"full id without last commits is not", full, false, 2},
		{"branch name is not", branch, true, 2},
		{"HEAD is not", domain.Revision{}, true, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newBrowseFixture(t)
			f.meta.EXPECT().Get(mock.Anything, pub.Name).Return(pub, nil)
			f.reader.EXPECT().GetTree(mock.Anything, pub.Name, tc.rev, dir, tc.includeLast).
				Return(treeWithLast("b"), nil).Times(tc.wantReaderOn)
			for range 2 {
				tree, err := f.browse.GetTree(ctx, domain.Anonymous(), pub.Name, tc.rev, dir, tc.includeLast)
				require.NoError(t, err)
				assert.Equal(t, "d/f", tree.Entries[0].Path)
			}
		})
	}
}

func TestGetTreeLastCommitCacheKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pubA := domain.Repo{Name: name(t, "a"), Visibility: domain.VisibilityPublic}
	pubB := domain.Repo{Name: name(t, "b"), Visibility: domain.VisibilityPublic}
	revA, revB := domain.RevisionOf(oid("a")), domain.RevisionOf(oid("b"))
	d1, d2 := treePath(t, "d1"), treePath(t, "d2")

	// Each distinct (repo, commit, path) is one reader call, then a hit.
	keys := []struct {
		repo domain.Repo
		rev  domain.Revision
		path domain.TreePath
	}{
		{pubA, revA, d1}, {pubB, revA, d1}, {pubA, revB, d1}, {pubA, revA, d2}, {pubA, revA, domain.TreePath{}},
	}
	f := newBrowseFixture(t)
	f.meta.EXPECT().Get(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, n domain.RepoName) (domain.Repo, error) {
			return domain.Repo{Name: n, Visibility: domain.VisibilityPublic}, nil
		})
	for _, k := range keys {
		f.reader.EXPECT().GetTree(mock.Anything, k.repo.Name, k.rev, k.path, true).Return(treeWithLast("b"), nil).Once()
	}
	for range 2 {
		for _, k := range keys {
			_, err := f.browse.GetTree(ctx, domain.Anonymous(), k.repo.Name, k.rev, k.path, true)
			require.NoError(t, err)
		}
	}
}

// TestGetTreeCacheKeepsAccessRule: a hit never skips the access check, and an
// error is not cached.
func TestGetTreeCacheKeepsAccessRule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	priv := domain.Repo{Name: name(t, "priv"), Visibility: domain.VisibilityPrivate}
	rev, dir := domain.RevisionOf(oid("a")), treePath(t, "d")

	f := newBrowseFixture(t)
	f.meta.EXPECT().Get(mock.Anything, priv.Name).Return(priv, nil)
	f.reader.EXPECT().GetTree(mock.Anything, priv.Name, rev, dir, true).Return(domain.Tree{}, domain.ErrNotDirectory).Once()
	f.reader.EXPECT().GetTree(mock.Anything, priv.Name, rev, dir, true).Return(treeWithLast("b"), nil).Once()

	_, err := f.browse.GetTree(ctx, domain.Root(), priv.Name, rev, dir, true)
	require.ErrorIs(t, err, domain.ErrNotDirectory) // not cached
	_, err = f.browse.GetTree(ctx, domain.Root(), priv.Name, rev, dir, true)
	require.NoError(t, err) // now cached
	_, err = f.browse.GetTree(ctx, domain.Anonymous(), priv.Name, rev, dir, true)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

// TestGetTreeCacheReturnsCopies: a caller that edits its tree does not alter
// what the next one gets.
func TestGetTreeCacheReturnsCopies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pub := domain.Repo{Name: name(t, "pub"), Visibility: domain.VisibilityPublic}
	rev, dir := domain.RevisionOf(oid("a")), treePath(t, "d")
	f := newBrowseFixture(t)
	f.meta.EXPECT().Get(mock.Anything, pub.Name).Return(pub, nil)
	f.reader.EXPECT().GetTree(mock.Anything, pub.Name, rev, dir, true).Return(treeWithLast("b"), nil).Once()

	for range 3 {
		tree, err := f.browse.GetTree(ctx, domain.Anonymous(), pub.Name, rev, dir, true)
		require.NoError(t, err)
		require.Len(t, tree.Entries, 1)
		assert.Equal(t, "s", tree.Entries[0].LastCommit.Summary)
		tree.Entries[0].LastCommit.Summary = "changed"
		tree.Entries[0].LastCommit.Parents[0] = oid("f")
		tree.Entries[0].Name = "changed"
	}
}

// TestHistoryCacheBounds: the entry bound and the byte bound evict the least
// recently used answer.
func TestHistoryCacheBounds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pub := domain.Repo{Name: name(t, "pub"), Visibility: domain.VisibilityPublic}
	paths := []string{"p0", "p1", "p2"}

	bigTree := treeWithLast("b")
	bigTree.Entries[0].Name = strings.Repeat("x", 4096)

	tests := []struct {
		name       string
		entries    int
		bytes      int64
		tree       domain.Tree
		wantCached []bool // after requesting p0, p1, p2 (and touching p0 before p2 when touch)
		touch      bool
	}{
		{"entry bound", 2, 1 << 20, treeWithLast("b"), []bool{false, true, true}, false},
		{"entry bound is LRU", 2, 1 << 20, treeWithLast("b"), []bool{true, false, true}, true},
		{"byte bound", 100, 6000, bigTree, []bool{false, false, true}, false},
		{"entry larger than the byte bound", 100, 1000, bigTree, []bool{false, false, false}, false},
		{"disabled by entries", 0, 1 << 20, treeWithLast("b"), []bool{false, false, false}, false},
		{"disabled by bytes", 10, 0, treeWithLast("b"), []bool{false, false, false}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newBrowseFixture(t)
			f.browse.WithHistoryCache(tc.entries, tc.bytes)
			f.meta.EXPECT().Get(mock.Anything, pub.Name).Return(pub, nil)
			rev := domain.RevisionOf(oid("a"))
			calls := map[string]int{}
			f.reader.EXPECT().GetTree(mock.Anything, pub.Name, rev, mock.Anything, true).
				RunAndReturn(func(_ context.Context, _ domain.RepoName, _ domain.Revision, p domain.TreePath, _ bool) (domain.Tree, error) {
					calls[p.String()]++
					return tc.tree, nil
				})
			get := func(p string) {
				_, err := f.browse.GetTree(ctx, domain.Anonymous(), pub.Name, rev, treePath(t, p), true)
				require.NoError(t, err)
			}
			get("p0")
			get("p1")
			if tc.touch {
				get("p0")
			}
			get("p2")
			before := map[string]int{"p0": calls["p0"], "p1": calls["p1"], "p2": calls["p2"]}
			// Probe from the newest to the oldest so probing one does not evict
			// the next one asked: asking a miss stores it.
			for i := len(paths) - 1; i >= 0; i-- {
				if !tc.wantCached[i] {
					continue
				}
				get(paths[i])
				assert.Equal(t, before[paths[i]], calls[paths[i]], "%s should be a hit", paths[i])
			}
			for i, p := range paths {
				if tc.wantCached[i] {
					continue
				}
				get(p)
				assert.Equal(t, before[p]+1, calls[p], "%s should be a miss", p)
				break // a miss stores the entry and may evict the others
			}
		})
	}
}

// TestListCommitsLastChangeCache: the latest commit of a path at a full
// commit id is cached; every other page shape is not.
func TestListCommitsLastChangeCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pub := domain.Repo{Name: name(t, "pub"), Visibility: domain.VisibilityPublic}
	full := domain.RevisionOf(oid("a"))
	main, err := domain.ParseRevision("main")
	require.NoError(t, err)
	file := treePath(t, "d/file.go")

	tests := []struct {
		name  string
		rev   domain.Revision
		path  domain.TreePath
		size  int
		calls int // reader calls for two identical requests
	}{
		{"last change at a full id", full, file, 1, 1},
		{"last change of the root at a full id", full, domain.TreePath{}, 1, 1},
		{"name", main, file, 1, 2},
		{"HEAD", domain.Revision{}, file, 1, 2},
		{"larger page", full, file, 2, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newBrowseFixture(t)
			f.meta.EXPECT().Get(mock.Anything, pub.Name).Return(pub, nil)
			q := ports.CommitQuery{Revision: tc.rev, Path: tc.path, Limit: tc.size}
			f.reader.EXPECT().ListCommits(mock.Anything, pub.Name, q).Return(ports.CommitPage{
				Head:    oid("a"),
				Commits: []domain.Commit{{ID: oid("c"), Parents: []domain.ObjectID{oid("d")}}},
			}, nil).Times(tc.calls)
			for range 2 {
				page, err := f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name,
					app.ListCommitsParams{Revision: tc.rev, Path: tc.path, PageSize: tc.size})
				require.NoError(t, err)
				require.Len(t, page.Commits, 1)
				page.Commits[0].Parents[0] = oid("f") // must not leak into the cache
			}
		})
	}

	t.Run("a following page is not cached", func(t *testing.T) {
		t.Parallel()
		f := newBrowseFixture(t)
		f.meta.EXPECT().Get(mock.Anything, pub.Name).Return(pub, nil)
		f.reader.EXPECT().ListCommits(mock.Anything, pub.Name, ports.CommitQuery{Revision: full, Path: file, Limit: 1}).
			Return(ports.CommitPage{Head: oid("a"), Commits: []domain.Commit{{ID: oid("c")}}, HasMore: true}, nil).Once()
		first, err := f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, app.ListCommitsParams{Revision: full, Path: file, PageSize: 1})
		require.NoError(t, err)
		require.NotEmpty(t, first.NextPageToken)
		f.reader.EXPECT().ListCommits(mock.Anything, pub.Name, ports.CommitQuery{Revision: domain.RevisionOf(oid("a")), Path: file, Skip: 1, Limit: 1}).
			Return(ports.CommitPage{Head: oid("a")}, nil).Twice()
		for range 2 {
			_, err = f.browse.ListCommits(ctx, domain.Anonymous(), pub.Name, app.ListCommitsParams{Path: file, PageSize: 1, PageToken: first.NextPageToken})
			require.NoError(t, err)
		}
	})
}

func TestHistoryCacheConcurrent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pub := domain.Repo{Name: name(t, "pub"), Visibility: domain.VisibilityPublic}
	f := newBrowseFixture(t)
	f.browse.WithHistoryCache(4, 1<<20)
	f.meta.EXPECT().Get(mock.Anything, pub.Name).Return(pub, nil)
	f.reader.EXPECT().GetTree(mock.Anything, pub.Name, mock.Anything, mock.Anything, true).Return(treeWithLast("b"), nil)
	paths := []string{"a", "b", "c", "d", "e", "f", "g"}
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 200 {
				p, err := domain.ParseTreePath(paths[(i+w)%len(paths)])
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := f.browse.GetTree(ctx, domain.Anonymous(), pub.Name, domain.RevisionOf(oid("a")), p, true); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
}
