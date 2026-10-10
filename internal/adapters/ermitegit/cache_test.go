// SPDX-License-Identifier: AGPL-3.0-or-later

package ermitegit_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bnema/zerowrap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/ermitegit"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/readertest"
)

func commitOf(t *testing.T, data, repo string) *readertest.Fixture {
	t.Helper()
	f := readertest.NewFixture(t, data, repo)
	f.Write("a.txt", repo+"\n")
	f.Commit("commit of " + repo)
	return f
}

func TestCacheIsBoundedAndEvictsLeastRecentlyUsed(t *testing.T) {
	ctx := context.Background()
	a, _, data := newAdapter(t, ermitegit.AllRoutes(ermitegit.ErmiteGit), func(c *ermitegit.Config) { c.MaxRepos = 3 })
	var fx []*readertest.Fixture
	for i := range 6 {
		fx = append(fx, commitOf(t, data, fmt.Sprintf("r%d", i)))
	}
	for _, f := range fx {
		res, err := a.ResolveRevision(ctx, f.Name, domain.Revision{})
		require.NoError(t, err)
		require.Equal(t, f.Git("rev-parse", "HEAD"), res.CommitID.String())
		assert.LessOrEqual(t, a.OpenRepos(), 3)
	}
	assert.Equal(t, 3, a.OpenRepos())
	// An evicted repository is opened again on the next read.
	res, err := a.ResolveRevision(ctx, fx[0].Name, domain.Revision{})
	require.NoError(t, err)
	assert.Equal(t, fx[0].Git("rev-parse", "HEAD"), res.CommitID.String())
	assert.Equal(t, 3, a.OpenRepos())
}

// An evicted repository stays usable by the reads that hold it: a blob being
// streamed is not cut when the cache closes its repository.
func TestEvictionDoesNotCutAStream(t *testing.T) {
	ctx := context.Background()
	a, _, data := newAdapter(t, ermitegit.AllRoutes(ermitegit.ErmiteGit), func(c *ermitegit.Config) { c.MaxRepos = 1 })
	f := readertest.NewFixture(t, data, "big")
	content := readertest.RandomBytes(3<<20, 11)
	f.WriteBytes("big.bin", content)
	f.Commit("big")
	other := commitOf(t, data, "other")

	_, rc, err := a.OpenBlob(ctx, f.Name, domain.Revision{}, readertest.TreePath(t, "big.bin"))
	require.NoError(t, err)
	head := make([]byte, 1000)
	_, err = rc.Read(head)
	require.NoError(t, err)

	_, err = a.ResolveRevision(ctx, other.Name, domain.Revision{}) // evicts "big"
	require.NoError(t, err)
	assert.Equal(t, 1, a.OpenRepos())

	rest := new(bytes.Buffer)
	_, err = rest.ReadFrom(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.True(t, bytes.Equal(content[1000:], rest.Bytes()))
}

func TestConcurrentReadsEvictionAndPushes(t *testing.T) {
	ctx := context.Background()
	a, _, data := newAdapter(t, ermitegit.AllRoutes(ermitegit.ErmiteGit), func(c *ermitegit.Config) { c.MaxRepos = 2; c.RefreshEvery = -1 })
	var fx []*readertest.Fixture
	for i := range 5 {
		fx = append(fx, commitOf(t, data, fmt.Sprintf("c%d", i)))
	}
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 40 {
				f := fx[(w+i)%len(fx)]
				if _, err := a.ListRefs(ctx, f.Name); err != nil {
					t.Error(err)
					return
				}
				page, err := a.ListCommits(ctx, f.Name, ports.CommitQuery{Limit: 5})
				if err != nil || len(page.Commits) != 1 {
					t.Errorf("%v %v", err, page)
					return
				}
				if _, err := a.GetTree(ctx, f.Name, domain.Revision{}, domain.TreePath{}, true); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	a.Close()
	_, err := a.ListRefs(ctx, fx[0].Name)
	assert.ErrorIs(t, err, ermitegit.ErrClosed)
}

// A repository ermite-git rejects is served by git for every method, and the
// reason is logged once at info.
func TestUnsupportedRepositoryFallsBackToGit(t *testing.T) {
	var logs bytes.Buffer
	log := zerowrap.New(zerowrap.Config{Level: "info", Format: "json", Output: &logs})
	ctx := zerowrap.WithCtx(context.Background(), log)
	a, g, data := newAdapter(t, ermitegit.AllRoutes(ermitegit.ErmiteGit), nil)

	h := readertest.NewHistory(t, data)
	// An alternates file makes the repository unsupported for the library.
	other := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(h.Bare, "objects", "info", "alternates"), []byte(filepath.Join(other, "objects")+"\n"), 0o644))

	for range 3 {
		want, err := g.ListRefs(ctx, h.Name)
		require.NoError(t, err)
		got, err := a.ListRefs(ctx, h.Name)
		require.NoError(t, err)
		assert.Equal(t, want, got)
		page, err := a.ListCommits(ctx, h.Name, ports.CommitQuery{Limit: 100})
		require.NoError(t, err)
		assert.Len(t, page.Commits, 6)
		tree, err := a.GetTree(ctx, h.Name, domain.Revision{}, domain.TreePath{}, true)
		require.NoError(t, err)
		assert.NotEmpty(t, tree.Entries)
	}
	assert.Equal(t, 1, bytes.Count(logs.Bytes(), []byte("served by git")), "logged once: %s", logs.String())
	assert.Contains(t, logs.String(), `"level":"info"`)
}

// A repository that becomes unsupported while it is open (a replace ref is
// pushed) is handed to git from then on.
func TestRepositoryBecomingUnsupportedFallsBack(t *testing.T) {
	ctx := context.Background()
	a, g, data := newAdapter(t, ermitegit.AllRoutes(ermitegit.ErmiteGit), func(c *ermitegit.Config) { c.RefreshEvery = -1 })
	h := readertest.NewHistory(t, data)
	_, err := a.ListRefs(ctx, h.Name)
	require.NoError(t, err)

	h.Git("replace", h.C2, h.C3)
	want, err := g.ListRefs(ctx, h.Name)
	require.NoError(t, err)
	got, err := a.ListRefs(ctx, h.Name)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// Errors that are not ErrUnsupported are returned, not hidden by a fallback.
func TestOtherOpenErrorsAreReturned(t *testing.T) {
	ctx := context.Background()
	a, _, data := newAdapter(t, ermitegit.AllRoutes(ermitegit.ErmiteGit), nil)
	f := commitOf(t, data, "broken")
	// A corrupt packed-refs is neither unsupported nor missing.
	require.NoError(t, os.WriteFile(filepath.Join(f.Bare, "packed-refs"), []byte("not a packed-refs file\n"), 0o644))
	_, err := a.ListRefs(ctx, f.Name)
	require.Error(t, err)
	assert.NotErrorIs(t, err, domain.ErrNotFound)

	_, err = a.ListRefs(ctx, readertest.RepoName(t, "ghost"))
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestCancelledContext(t *testing.T) {
	a, _, data := newAdapter(t, ermitegit.AllRoutes(ermitegit.ErmiteGit), nil)
	f := commitOf(t, data, "ctx")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.ListRefs(ctx, f.Name)
	assert.ErrorIs(t, err, context.Canceled)
	_, err = a.ListCommits(ctx, f.Name, ports.CommitQuery{Limit: 5})
	assert.ErrorIs(t, err, context.Canceled)
	_, _, err = a.OpenBlob(ctx, f.Name, domain.Revision{}, readertest.TreePath(t, "a.txt"))
	assert.ErrorIs(t, err, context.Canceled)
}

// A commit in an encoding other than UTF-8 is converted by git, not by the
// library: the adapter hands it to the CLI reader even when ermite-git is
// routed, and the answer is the one of git.
func TestEncodedCommitIsServedByGit(t *testing.T) {
	ctx := context.Background()
	a, g, data := newAdapter(t, ermitegit.AllRoutes(ermitegit.ErmiteGit), nil)
	f := readertest.NewFixture(t, data, "enc")
	f.Write("a.txt", "a\n")
	f.Git("add", "-A")
	f.Git("-c", "i18n.commitEncoding=ISO-8859-1", "commit", "--quiet", "-m", "caf\xe9")
	want, err := g.ListCommits(ctx, f.Name, ports.CommitQuery{Limit: 5})
	require.NoError(t, err)
	got, err := a.ListCommits(ctx, f.Name, ports.CommitQuery{Limit: 5})
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, "café", got.Commits[0].Message)
}
