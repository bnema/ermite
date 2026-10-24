// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/gitcli"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/readertest"
)

// alive reports whether a process still exists. The adapter collects its
// children, so a stopped one is gone and not a zombie.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func requireGone(t *testing.T, pids ...int) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, p := range pids {
			if alive(p) {
				return false
			}
		}
		return true
	}, 5*time.Second, 10*time.Millisecond, "processes %v are still running", pids)
}

func TestCatFileProcessIsReusedAndStopsWhenIdle(t *testing.T) {
	ctx := context.Background()
	// The default idle timeout (60 s) never fires during the test: the hooks
	// do what the timers would, deterministically.
	g, data := newReader(t, nil)
	h := readertest.NewHistory(t, data)

	assert.Empty(t, g.CatFilePIDs(), "nothing runs before the first read")
	_, err := g.ResolveRevision(ctx, h.Name, domain.Revision{})
	require.NoError(t, err)
	pids := g.CatFilePIDs()
	require.Len(t, pids, 1)
	require.True(t, alive(pids[0]))

	for range 5 {
		_, err = g.GetTree(ctx, h.Name, domain.Revision{}, domain.TreePath{}, false)
		require.NoError(t, err)
	}
	assert.Equal(t, pids, g.CatFilePIDs(), "one long-lived process serves every request")

	// A timer armed before the last use is stale: use keeps the process alive.
	g.FireStaleIdleTimers()
	assert.Equal(t, pids, g.CatFilePIDs())

	// The timer of the last use stops it once nobody used it since.
	g.FireIdleTimers()
	require.Empty(t, g.CatFilePIDs())
	requireGone(t, pids...)

	// The next request starts a new one.
	_, err = g.ResolveRevision(ctx, h.Name, domain.Revision{})
	require.NoError(t, err)
	next := g.CatFilePIDs()
	require.Len(t, next, 1)
	assert.NotEqual(t, pids, next)
}

func TestCatFileIdleTimerStopsAnUnusedProcess(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, func(c *gitcli.Config) { c.CatFileIdle = 20 * time.Millisecond })
	h := readertest.NewHistory(t, data)
	_, err := g.ResolveRevision(ctx, h.Name, domain.Revision{})
	require.NoError(t, err)
	pids := g.CatFilePIDs()
	require.Eventually(t, func() bool { return len(g.CatFilePIDs()) == 0 }, 5*time.Second, 5*time.Millisecond)
	requireGone(t, pids...)
}

func TestCloseStopsEveryProcess(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil) // default idle: 60 s, so only Close can stop them
	h := readertest.NewHistory(t, data)
	f2 := readertest.NewFixture(t, data, "second")
	f2.Write("x", "x").Commit("x")
	big := readertest.NewFixture(t, data, "bigone")
	big.WriteBytes("big", readertest.RandomBytes(600<<10, 1)).Commit("big")

	_, err := g.ResolveRevision(ctx, h.Name, domain.Revision{})
	require.NoError(t, err)
	_, err = g.ResolveRevision(ctx, f2.Name, domain.Revision{})
	require.NoError(t, err)
	_, rc, err := g.OpenBlob(ctx, big.Name, domain.Revision{}, readertest.TreePath(t, "big"))
	require.NoError(t, err)
	_ = rc // left open on purpose: Close must stop its process too

	pids := append(g.CatFilePIDs(), g.BlobStreamPIDs()...)
	require.GreaterOrEqual(t, len(pids), 4, "two cat-file processes, the big repo's one and the blob stream")
	for _, p := range pids {
		require.True(t, alive(p))
	}

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	require.NoError(t, g.Close(cctx))
	requireGone(t, pids...)
	assert.Empty(t, g.CatFilePIDs())

	_, err = g.ResolveRevision(ctx, h.Name, domain.Revision{})
	assert.ErrorIs(t, err, gitcli.ErrClosed)
	_, _, err = g.OpenBlob(ctx, big.Name, domain.Revision{}, readertest.TreePath(t, "big"))
	assert.ErrorIs(t, err, gitcli.ErrClosed)
	assert.NoError(t, g.Close(ctx), "Close is idempotent")
	_, err = io.Copy(io.Discard, rc)
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF, "a stream cut by Close fails instead of looking complete")
	assert.NoError(t, rc.Close())
	assert.Equal(t, 0, g.BlobStreams())
}

func TestPoolIsBounded(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, func(c *gitcli.Config) { c.MaxCatFiles = 2 })
	var names []domain.RepoName
	for i := range 6 {
		f := readertest.NewFixture(t, data, fmt.Sprintf("r%d", i))
		f.Write("x", "x").Commit("x")
		names = append(names, f.Name)
	}
	var seen []int
	for _, n := range names {
		_, err := g.ResolveRevision(ctx, n, domain.Revision{})
		require.NoError(t, err)
		pids := g.CatFilePIDs()
		assert.LessOrEqual(t, len(pids), 2)
		seen = append(seen, pids...)
	}
	// The evicted processes were stopped, not leaked.
	live := map[int]bool{}
	for _, p := range g.CatFilePIDs() {
		live[p] = true
	}
	for _, p := range seen {
		if !live[p] {
			requireGone(t, p)
		}
	}
}

func TestPoolRestartsAfterProcessDeath(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	h := readertest.NewHistory(t, data)

	_, err := g.ResolveRevision(ctx, h.Name, domain.Revision{})
	require.NoError(t, err)
	old := g.CatFilePIDs()
	require.Len(t, old, 1)
	require.NoError(t, syscall.Kill(old[0], syscall.SIGKILL))
	requireGone(t, old[0])

	// The request that finds the dead process succeeds on a fresh one.
	got, err := g.ResolveRevision(ctx, h.Name, domain.Revision{})
	require.NoError(t, err)
	assert.Equal(t, h.Merge, got.CommitID.String())
	now := g.CatFilePIDs()
	require.Len(t, now, 1)
	assert.NotEqual(t, old, now)
}

func TestConcurrentReadsAreCorrectAndBounded(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	h := readertest.NewHistory(t, data)

	var wg sync.WaitGroup
	errs := make(chan error, 24*10)
	for i := range 24 {
		wg.Go(func() {
			for j := range 10 {
				switch (i + j) % 4 {
				case 0:
					res, err := g.ResolveRevision(ctx, h.Name, readertest.Rev(t, "feature/x"))
					if err == nil && res.CommitID.String() != h.Side {
						err = fmt.Errorf("resolved %s", res.CommitID)
					}
					errs <- err
				case 1:
					info, rc, err := g.OpenBlob(ctx, h.Name, domain.Revision{}, readertest.TreePath(t, "b.txt"))
					if err == nil {
						var body []byte
						body, err = io.ReadAll(rc)
						_ = rc.Close()
						if err == nil && (info.Size != 19 || string(body) != "one\ntwo\nthree\nfour\n") {
							err = fmt.Errorf("blob %d %q", info.Size, body)
						}
					}
					errs <- err
				case 2:
					tree, err := g.GetTree(ctx, h.Name, readertest.Rev(t, "v2"), readertest.TreePath(t, "d"), false)
					if err == nil && len(tree.Entries) != 2 {
						err = fmt.Errorf("tree of %d entries", len(tree.Entries))
					}
					errs <- err
				default:
					_, err := g.ResolveRefPath(ctx, h.Name, "feature/x/d/f")
					errs <- err
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.NotEmpty(t, g.CatFilePIDs())
	assert.LessOrEqual(t, len(g.CatFilePIDs()), 4, "a repository holds a bounded number of processes")
}

func TestCancelledRequestDoesNotPoisonTheProcess(t *testing.T) {
	g, data := newReader(t, nil)
	h := readertest.NewHistory(t, data)

	// Many requests, some with a context that dies at a random point. Whatever
	// state a killed process was in, later requests must get right answers.
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			for j := range 20 {
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration((i*7+j*3)%5)*time.Millisecond)
				res, err := g.ResolveRevision(ctx, h.Name, readertest.Rev(t, "v2"))
				cancel()
				if err == nil {
					assert.Equal(t, h.C2, res.CommitID.String())
				} else {
					assert.True(t, errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled), "%v", err)
				}
			}
		})
	}
	wg.Wait()
	res, err := g.ResolveRevision(context.Background(), h.Name, readertest.Rev(t, "v2"))
	require.NoError(t, err)
	assert.Equal(t, h.C2, res.CommitID.String())
}

func TestBlobStreamStopsWhenContextEndsOrReaderCloses(t *testing.T) {
	g, data := newReader(t, nil)
	f := readertest.NewFixture(t, data, "bigone")
	f.WriteBytes("big", readertest.RandomBytes(2<<20, 3)).Commit("big")

	ctx, cancel := context.WithCancel(context.Background())
	_, rc, err := g.OpenBlob(ctx, f.Name, domain.Revision{}, readertest.TreePath(t, "big"))
	require.NoError(t, err)
	pids := g.BlobStreamPIDs()
	require.Len(t, pids, 1)
	_, err = io.ReadFull(rc, make([]byte, 1000))
	require.NoError(t, err)

	cancel() // the client went away mid-stream
	requireGone(t, pids...)
	_, err = io.Copy(io.Discard, rc)
	assert.Error(t, err, "a cut stream does not look complete")
	require.NoError(t, rc.Close())
	assert.Equal(t, 0, g.BlobStreams())

	// Closing early stops the process as well.
	_, rc, err = g.OpenBlob(context.Background(), f.Name, domain.Revision{}, readertest.TreePath(t, "big"))
	require.NoError(t, err)
	pids = g.BlobStreamPIDs()
	require.Len(t, pids, 1)
	require.NoError(t, rc.Close())
	requireGone(t, pids...)
	assert.Equal(t, 0, g.BlobStreams())

	// A context that is already cancelled starts nothing.
	cctx, cancel2 := context.WithCancel(context.Background())
	cancel2()
	_, _, err = g.OpenBlob(cctx, f.Name, domain.Revision{}, readertest.TreePath(t, "big"))
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 0, g.BlobStreams())
}

// TestOpenBlobStreams50MiB is AC3: a 50 MiB file is streamed with memory that
// does not depend on its size. The test reads it in 64 KiB chunks, as the
// GetBlob RPC does, and bounds both what was allocated while reading and the
// peak of the heap.
func TestOpenBlobStreams50MiB(t *testing.T) {
	if testing.Short() {
		t.Skip("50 MiB fixture")
	}
	ctx := context.Background()
	g, data := newReader(t, nil)
	f := readertest.NewFixture(t, data, "huge")
	const size = 50 << 20
	content := readertest.RandomBytes(size, 42)
	want := sha256.Sum256(content)
	f.WriteBytes("huge.bin", content).Commit("huge")
	content = nil //nolint:ineffassign // let the 50 MiB go before measuring

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	info, rc, err := g.OpenBlob(ctx, f.Name, domain.Revision{}, readertest.TreePath(t, "huge.bin"))
	require.NoError(t, err)
	assert.Equal(t, int64(size), info.Size)
	assert.True(t, info.Binary)
	assert.Equal(t, "application/octet-stream", info.MediaType)

	h := sha256.New()
	buf := make([]byte, 64<<10)
	var total int64
	var peak uint64
	for i := 0; ; i++ {
		n, err := rc.Read(buf)
		total += int64(n)
		h.Write(buf[:n])
		if i%64 == 0 {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			peak = max(peak, m.HeapAlloc)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
	}
	require.NoError(t, rc.Close())
	runtime.ReadMemStats(&after)

	assert.Equal(t, int64(size), total)
	assert.Equal(t, want[:], h.Sum(nil), "content is intact")

	allocated := after.TotalAlloc - before.TotalAlloc
	assert.Less(t, allocated, uint64(8<<20), "bytes allocated while streaming 50 MiB: %d", allocated)
	growth := int64(peak) - int64(before.HeapAlloc)
	assert.Less(t, growth, int64(8<<20), "heap growth while streaming 50 MiB: %d", growth)
	t.Logf("streamed %d MiB: allocated %d KiB, peak heap growth %d KiB", size>>20, allocated>>10, growth>>10)
}

// The one-shot git commands share a bounded number of slots: a burst of
// requests waits for a slot, and gives up with its context.
func TestExecSemaphoreBoundsOneShotCommands(t *testing.T) {
	ctx := context.Background()
	gate := t.TempDir() + "/gate"
	// A git that blocks until the gate file exists.
	script := writeScript(t, `while [ ! -e `+gate+` ]; do sleep 0.01; done; echo "git version 2.50.0"`)
	g := gitcli.New(gitcli.Config{Bin: script, DataDir: t.TempDir(), Path: os.Getenv("PATH"), MaxExec: 2})
	size, taken := g.ExecSlots()
	require.Equal(t, 2, size)
	require.Zero(t, taken)

	errs := make(chan error, 2)
	for range 2 {
		go func() { _, err := g.CheckVersion(ctx); errs <- err }()
	}
	require.Eventually(t, func() bool { _, taken := g.ExecSlots(); return taken == 2 }, 5*time.Second, time.Millisecond)

	// A third waits and gives up with its deadline: it never started git.
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err := g.CheckVersion(short)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	require.NoError(t, os.WriteFile(gate, nil, 0o600))
	for range 2 {
		assert.NoError(t, <-errs, "a check that got a slot finishes once the gate opens")
	}
	require.Eventually(t, func() bool { _, taken := g.ExecSlots(); return taken == 0 }, 5*time.Second, time.Millisecond)
}

// Open blob streams are bounded; a refused stream leaves nothing behind and a
// closed one frees its slot.
func TestBlobStreamsAreBounded(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, func(c *gitcli.Config) { c.MaxBlobStreams = 2 })
	f := readertest.NewFixture(t, data, "bigone")
	f.WriteBytes("big", readertest.RandomBytes(600<<10, 5)).Commit("big")
	open := func() (io.ReadCloser, error) {
		_, rc, err := g.OpenBlob(ctx, f.Name, domain.Revision{}, readertest.TreePath(t, "big"))
		return rc, err
	}

	a, err := open()
	require.NoError(t, err)
	b, err := open()
	require.NoError(t, err)
	_, err = open()
	assert.ErrorIs(t, err, domain.ErrResourceExhausted)
	assert.Equal(t, 2, g.BlobStreams())

	require.NoError(t, a.Close())
	c, err := open()
	require.NoError(t, err, "a closed stream frees its slot")
	require.NoError(t, b.Close())
	require.NoError(t, c.Close())
	assert.Zero(t, g.BlobStreams())
}

// hold runs HoldCatFile in the background and returns once the process is
// held. The returned func releases it and waits for the request to end.
func hold(t *testing.T, g *gitcli.Git, repo domain.RepoName) (release func()) {
	t.Helper()
	held, free, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- g.HoldCatFile(context.Background(), repo, func() { close(held); <-free })
	}()
	select {
	case <-held:
	case err := <-done:
		require.NoError(t, err)
		t.Fatal("the request ended before it held a process")
	}
	var once sync.Once
	release = func() {
		once.Do(func() { close(free); require.NoError(t, <-done) })
	}
	t.Cleanup(release)
	return release
}

// A request in flight does not make the others of the repository wait: they
// get a process of their own, started only because of the contention.
func TestConcurrentReadsOnOneRepoDoNotSerialize(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	h := readertest.NewHistory(t, data)

	_, err := g.ResolveRevision(ctx, h.Name, domain.Revision{})
	require.NoError(t, err)
	require.Len(t, g.CatFilePIDs(), 1, "no contention, one process")

	release := hold(t, g, h.Name)
	// The slow request holds the only process; this one must not wait for it
	// (it would block forever: the holder is released after).
	res, err := g.ResolveRevision(ctx, h.Name, domain.Revision{})
	require.NoError(t, err)
	assert.Equal(t, h.Merge, res.CommitID.String())
	pids := g.CatFilePIDs()
	assert.Len(t, pids, 2, "the contention started a second process")
	assert.Zero(t, g.CatFileWaiting())

	release()
	// Both stay for reuse until they idle out.
	assert.Equal(t, pids, g.CatFilePIDs())
	g.FireIdleTimers()
	assert.Empty(t, g.CatFilePIDs())
	requireGone(t, pids...)
}

func TestCatFileProcessBounds(t *testing.T) {
	tests := []struct {
		name string
		max  int // Config.MaxCatFiles
		// holds are the repositories, by index, whose process a request holds.
		holds []int
		// extra is the repository of one more request, which must wait.
		extra       int
		wantProcs   int
		wantWaiting int
	}{
		{name: "per repository cap", max: 16, holds: []int{0, 0, 0, 0}, extra: 0, wantProcs: 4, wantWaiting: 1},
		{name: "global cap, repository with a process waits", max: 3, holds: []int{0, 0, 1}, extra: 1, wantProcs: 3, wantWaiting: 1},
		{name: "global cap lower than the per repository cap", max: 2, holds: []int{0, 0}, extra: 0, wantProcs: 2, wantWaiting: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, data := newReader(t, func(c *gitcli.Config) { c.MaxCatFiles = tt.max })
			var names []domain.RepoName
			for i := range 2 {
				f := readertest.NewFixture(t, data, fmt.Sprintf("r%d", i))
				f.Write("x", "x").Commit("x")
				names = append(names, f.Name)
			}
			var releases []func()
			for _, i := range tt.holds {
				releases = append(releases, hold(t, g, names[i]))
			}
			assert.Len(t, g.CatFilePIDs(), tt.wantProcs)

			done := make(chan error, 1)
			go func() {
				_, err := g.ResolveRevision(context.Background(), names[tt.extra], domain.Revision{})
				done <- err
			}()
			require.Eventually(t, func() bool { return g.CatFileWaiting() == tt.wantWaiting }, 5*time.Second, time.Millisecond)
			assert.Len(t, g.CatFilePIDs(), tt.wantProcs, "a waiting request starts nothing")
			select {
			case err := <-done:
				t.Fatalf("the request did not wait: %v", err)
			default:
			}

			// A process released to a waiter serves it, without a new process.
			for k, i := range tt.holds {
				if i == tt.extra {
					releases[k]()
					break
				}
			}
			require.NoError(t, <-done)
			assert.LessOrEqual(t, len(g.CatFilePIDs()), tt.wantProcs)
			assert.Zero(t, g.CatFileWaiting())
		})
	}
}

// A waiting request gives up with its context and leaves nothing queued.
func TestWaitingForAProcessEndsWithTheContext(t *testing.T) {
	g, data := newReader(t, func(c *gitcli.Config) { c.MaxCatFiles = 1 })
	h := readertest.NewHistory(t, data)
	release := hold(t, g, h.Name)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := g.ResolveRevision(ctx, h.Name, domain.Revision{}); done <- err }()
	require.Eventually(t, func() bool { return g.CatFileWaiting() == 1 }, 5*time.Second, time.Millisecond)
	cancel()
	assert.ErrorIs(t, <-done, context.Canceled)
	assert.Zero(t, g.CatFileWaiting())
	assert.Len(t, g.CatFilePIDs(), 1)

	release()
	_, err := g.ResolveRevision(context.Background(), h.Name, domain.Revision{})
	require.NoError(t, err, "the process survived the abandoned wait")
}

// The bound counts processes, not repositories: idle processes of several
// repositories are evicted to make room.
func TestPoolBoundCountsProcessesAcrossRepositories(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, func(c *gitcli.Config) { c.MaxCatFiles = 3 })
	var names []domain.RepoName
	for i := range 4 {
		f := readertest.NewFixture(t, data, fmt.Sprintf("r%d", i))
		f.Write("x", "x").Commit("x")
		names = append(names, f.Name)
	}
	// Repository 0 grows to three processes.
	releases := []func(){hold(t, g, names[0]), hold(t, g, names[0]), hold(t, g, names[0])}
	require.Len(t, g.CatFilePIDs(), 3)
	for _, r := range releases {
		r()
	}
	before := g.CatFilePIDs()

	for _, n := range names[1:] {
		_, err := g.ResolveRevision(ctx, n, domain.Revision{})
		require.NoError(t, err)
		assert.LessOrEqual(t, len(g.CatFilePIDs()), 3)
	}
	live := map[int]bool{}
	for _, p := range g.CatFilePIDs() {
		live[p] = true
	}
	for _, p := range before {
		if !live[p] {
			requireGone(t, p)
		}
	}
}
