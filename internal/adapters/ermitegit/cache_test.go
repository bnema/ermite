// SPDX-License-Identifier: AGPL-3.0-or-later

package ermitegit_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

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

// The idle tests run in a synctest bubble: the clock is fake, so IdleTTL is the
// production one and the boundaries are exact. The sweeper ticks every
// IdleTTL/4 from the creation of the adapter and closes what has been idle for
// more than IdleTTL, so a repository last used at t is closed at the first tick
// after t+IdleTTL, which is t+IdleTTL+IdleTTL/4 here (no time passes while the
// fixtures are built: the bubble's clock only moves when everything sleeps).
const idleTTL = ermitegit.DefaultIdleTTL

// A repository that nobody reads for IdleTTL is closed, without a goroutine
// per repository.
func TestIdleRepositoriesAreClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		a, _, data := newAdapter(t, ermitegit.AllRoutes(ermitegit.ErmiteGit), func(c *ermitegit.Config) { c.IdleTTL = idleTTL })
		f := commitOf(t, data, "idle")
		_, err := a.ListRefs(ctx, f.Name)
		require.NoError(t, err)
		require.Equal(t, 1, a.OpenRepos())

		synctest.Sleep(idleTTL) // idle for exactly the TTL: not more
		assert.Equal(t, 1, a.OpenRepos(), "not idle for more than the TTL yet")
		synctest.Sleep(idleTTL/4 - time.Nanosecond)
		assert.Equal(t, 1, a.OpenRepos(), "the sweeper has not ticked yet")
		synctest.Sleep(time.Nanosecond)
		assert.Equal(t, 0, a.OpenRepos(), "closed by the first sweep after the TTL")

		// It opens again on the next read.
		_, err = a.ListRefs(ctx, f.Name)
		require.NoError(t, err)
		assert.Equal(t, 1, a.OpenRepos())
	})
}

// A read keeps the repository open: the TTL counts from the last use.
func TestReadsKeepARepositoryOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		a, _, data := newAdapter(t, ermitegit.AllRoutes(ermitegit.ErmiteGit), func(c *ermitegit.Config) { c.IdleTTL = idleTTL })
		f := commitOf(t, data, "busy")
		for range 10 {
			_, err := a.ListRefs(ctx, f.Name)
			require.NoError(t, err)
			synctest.Sleep(idleTTL / 2)
			require.Equal(t, 1, a.OpenRepos(), "used within the TTL")
		}
		synctest.Sleep(idleTTL)
		assert.Equal(t, 0, a.OpenRepos(), "closed once nobody reads it")
	})
}

// A Refresh after a write is not a read: a repository pushed to but never read
// still closes after IdleTTL.
func TestRefreshIsNotAUse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		a, _, data := newAdapter(t, ermitegit.AllRoutes(ermitegit.ErmiteGit), func(c *ermitegit.Config) { c.IdleTTL = idleTTL })
		f := commitOf(t, data, "pushed")
		_, err := a.ListRefs(ctx, f.Name)
		require.NoError(t, err)
		// A push every 30 seconds, until the sweep that follows the TTL.
		for elapsed := time.Duration(0); elapsed < idleTTL+idleTTL/4; elapsed += 30 * time.Second {
			require.Equal(t, 1, a.OpenRepos(), "open after %v", elapsed)
			a.Refresh(ctx, f.Name)
			synctest.Sleep(30 * time.Second)
		}
		assert.Equal(t, 0, a.OpenRepos(), "refreshes kept an unread repository open")
	})
}

// The idle close does not cut a blob stream the reader still holds: the library
// keeps the stream valid after Close.
func TestIdleCloseDoesNotCutAStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		a, _, data := newAdapter(t, ermitegit.AllRoutes(ermitegit.ErmiteGit), func(c *ermitegit.Config) { c.IdleTTL = idleTTL })
		f := readertest.NewFixture(t, data, "stream")
		content := readertest.RandomBytes(3<<20, 12)
		f.WriteBytes("big.bin", content)
		f.Commit("big")

		_, rc, err := a.OpenBlob(ctx, f.Name, domain.Revision{}, readertest.TreePath(t, "big.bin"))
		require.NoError(t, err)
		synctest.Sleep(idleTTL + idleTTL/4)
		require.Equal(t, 0, a.OpenRepos())
		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		require.NoError(t, rc.Close())
		assert.True(t, bytes.Equal(content, got))
	})
}

// A repository ermite-git could not read is served by git for UnsupportedRetry,
// then opened again: a repack, or the removal of a replace ref, can make it
// readable. The commit-graph of the history fixture is mapped only while the
// library has the repository open.
func TestUnsupportedRepositoryIsTriedAgainAfterTheRetryDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		a, _, data := newAdapter(t, ermitegit.AllRoutes(ermitegit.ErmiteGit), nil)
		h := readertest.NewHistory(t, data)
		h.Maintain()
		alternates := filepath.Join(h.Bare, "objects", "info", "alternates")
		require.NoError(t, os.WriteFile(alternates, []byte(filepath.Join(t.TempDir(), "objects")+"\n"), 0o644))
		read := func() {
			t.Helper()
			// ListRefs: git's answer is a one-shot command, where ListCommits
			// would start a long-lived cat-file process, whose pipes keep the
			// bubble from ever being idle.
			_, err := a.ListRefs(ctx, h.Name)
			require.NoError(t, err)
		}

		read()
		require.False(t, mapped(t, h.Bare), "unsupported: served by git")
		require.NoError(t, os.Remove(alternates)) // readable now

		synctest.Sleep(ermitegit.UnsupportedRetry)
		read()
		assert.False(t, mapped(t, h.Bare), "still served by git: the delay is not over")
		synctest.Sleep(time.Nanosecond)
		read()
		assert.True(t, mapped(t, h.Bare), "tried again once the delay is over")
	})
}

// mapped reports whether the process maps a file under dir (Linux).
func mapped(t *testing.T, dir string) bool {
	t.Helper()
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Skip("no /proc/self/maps")
	}
	return bytes.Contains(maps, []byte(dir))
}

// Close releases the mappings before it returns, not later from another
// goroutine.
func TestCloseReleasesMappingsSynchronously(t *testing.T) {
	ctx := context.Background()
	a, _, data := newAdapter(t, ermitegit.AllRoutes(ermitegit.ErmiteGit), nil)
	h := readertest.NewHistory(t, data)
	h.Maintain()
	_, err := a.ListCommits(ctx, h.Name, ports.CommitQuery{Limit: 5})
	require.NoError(t, err)
	require.True(t, mapped(t, h.Bare), "the commit-graph is mapped while the repository is open")
	a.Close()
	assert.False(t, mapped(t, h.Bare), "Close returned with the mappings still there")
}

// Forget (a deleted repository) also closes before it returns.
func TestForgetReleasesMappingsSynchronously(t *testing.T) {
	ctx := context.Background()
	a, _, data := newAdapter(t, ermitegit.AllRoutes(ermitegit.ErmiteGit), nil)
	h := readertest.NewHistory(t, data)
	h.Maintain()
	_, err := a.ListCommits(ctx, h.Name, ports.CommitQuery{Limit: 5})
	require.NoError(t, err)
	require.True(t, mapped(t, h.Bare))
	a.Forget(h.Bare)
	assert.False(t, mapped(t, h.Bare))
}

// Refresh, called after a write of ermite, makes the library notice at once
// what the throttled read path would not for an hour.
func TestRefreshAfterAWrite(t *testing.T) {
	var logs bytes.Buffer
	log := zerowrap.New(zerowrap.Config{Level: "info", Format: "json", Output: &logs})
	ctx := zerowrap.WithCtx(context.Background(), log)
	a, g, data := newAdapter(t, ermitegit.AllRoutes(ermitegit.ErmiteGit), func(c *ermitegit.Config) { c.RefreshEvery = time.Hour })
	h := readertest.NewHistory(t, data)

	a.Refresh(ctx, h.Name) // not open: nothing to do
	assert.Equal(t, 0, a.OpenRepos())
	_, err := a.ListRefs(ctx, h.Name)
	require.NoError(t, err)

	h.Git("replace", h.C2, h.C3) // the repository stops being readable by the library
	a.Refresh(ctx, h.Name)
	assert.Contains(t, logs.String(), "served by git", "Refresh noticed the replace ref")
	want, err := g.ListRefs(ctx, h.Name)
	require.NoError(t, err)
	got, err := a.ListRefs(ctx, h.Name)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	a.Close()
	a.Refresh(ctx, h.Name) // after Close: no-op
}
