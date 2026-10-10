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
	h := newHistory(t, data)

	assert.Empty(t, g.CatFilePIDs(), "nothing runs before the first read")
	_, err := g.ResolveRevision(ctx, h.name, domain.Revision{})
	require.NoError(t, err)
	pids := g.CatFilePIDs()
	require.Len(t, pids, 1)
	require.True(t, alive(pids[0]))

	for range 5 {
		_, err = g.GetTree(ctx, h.name, domain.Revision{}, domain.TreePath{}, false)
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
	_, err = g.ResolveRevision(ctx, h.name, domain.Revision{})
	require.NoError(t, err)
	next := g.CatFilePIDs()
	require.Len(t, next, 1)
	assert.NotEqual(t, pids, next)
}

func TestCatFileIdleTimerStopsAnUnusedProcess(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, func(c *gitcli.Config) { c.CatFileIdle = 20 * time.Millisecond })
	h := newHistory(t, data)
	_, err := g.ResolveRevision(ctx, h.name, domain.Revision{})
	require.NoError(t, err)
	pids := g.CatFilePIDs()
	require.Eventually(t, func() bool { return len(g.CatFilePIDs()) == 0 }, 5*time.Second, 5*time.Millisecond)
	requireGone(t, pids...)
}

func TestCloseStopsEveryProcess(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil) // default idle: 60 s, so only Close can stop them
	h := newHistory(t, data)
	f2 := newFixtureRepo(t, data, "second")
	f2.write("x", "x").commit("x")
	big := newFixtureRepo(t, data, "bigone")
	big.writeBytes("big", randomBytes(600<<10, 1)).commit("big")

	_, err := g.ResolveRevision(ctx, h.name, domain.Revision{})
	require.NoError(t, err)
	_, err = g.ResolveRevision(ctx, f2.name, domain.Revision{})
	require.NoError(t, err)
	_, rc, err := g.OpenBlob(ctx, big.name, domain.Revision{}, tpath(t, "big"))
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

	_, err = g.ResolveRevision(ctx, h.name, domain.Revision{})
	assert.ErrorIs(t, err, gitcli.ErrClosed)
	_, _, err = g.OpenBlob(ctx, big.name, domain.Revision{}, tpath(t, "big"))
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
		f := newFixtureRepo(t, data, fmt.Sprintf("r%d", i))
		f.write("x", "x").commit("x")
		names = append(names, f.name)
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
	h := newHistory(t, data)

	_, err := g.ResolveRevision(ctx, h.name, domain.Revision{})
	require.NoError(t, err)
	old := g.CatFilePIDs()
	require.Len(t, old, 1)
	require.NoError(t, syscall.Kill(old[0], syscall.SIGKILL))
	requireGone(t, old[0])

	// The request that finds the dead process succeeds on a fresh one.
	got, err := g.ResolveRevision(ctx, h.name, domain.Revision{})
	require.NoError(t, err)
	assert.Equal(t, h.merge, got.CommitID.String())
	now := g.CatFilePIDs()
	require.Len(t, now, 1)
	assert.NotEqual(t, old, now)
}

func TestConcurrentReadsAreSerializedPerProcess(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	h := newHistory(t, data)

	var wg sync.WaitGroup
	errs := make(chan error, 24*10)
	for i := range 24 {
		wg.Go(func() {
			for j := range 10 {
				switch (i + j) % 4 {
				case 0:
					res, err := g.ResolveRevision(ctx, h.name, rev(t, "feature/x"))
					if err == nil && res.CommitID.String() != h.side {
						err = fmt.Errorf("resolved %s", res.CommitID)
					}
					errs <- err
				case 1:
					info, rc, err := g.OpenBlob(ctx, h.name, domain.Revision{}, tpath(t, "b.txt"))
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
					tree, err := g.GetTree(ctx, h.name, rev(t, "v2"), tpath(t, "d"), false)
					if err == nil && len(tree.Entries) != 2 {
						err = fmt.Errorf("tree of %d entries", len(tree.Entries))
					}
					errs <- err
				default:
					_, err := g.ResolveRefPath(ctx, h.name, "feature/x/d/f")
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
	assert.Len(t, g.CatFilePIDs(), 1, "one process for one repository")
}

func TestCancelledRequestDoesNotPoisonTheProcess(t *testing.T) {
	g, data := newReader(t, nil)
	h := newHistory(t, data)

	// Many requests, some with a context that dies at a random point. Whatever
	// state a killed process was in, later requests must get right answers.
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			for j := range 20 {
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration((i*7+j*3)%5)*time.Millisecond)
				res, err := g.ResolveRevision(ctx, h.name, rev(t, "v2"))
				cancel()
				if err == nil {
					assert.Equal(t, h.c2, res.CommitID.String())
				} else {
					assert.True(t, errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled), "%v", err)
				}
			}
		})
	}
	wg.Wait()
	res, err := g.ResolveRevision(context.Background(), h.name, rev(t, "v2"))
	require.NoError(t, err)
	assert.Equal(t, h.c2, res.CommitID.String())
}

func TestBlobStreamStopsWhenContextEndsOrReaderCloses(t *testing.T) {
	g, data := newReader(t, nil)
	f := newFixtureRepo(t, data, "bigone")
	f.writeBytes("big", randomBytes(2<<20, 3)).commit("big")

	ctx, cancel := context.WithCancel(context.Background())
	_, rc, err := g.OpenBlob(ctx, f.name, domain.Revision{}, tpath(t, "big"))
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
	_, rc, err = g.OpenBlob(context.Background(), f.name, domain.Revision{}, tpath(t, "big"))
	require.NoError(t, err)
	pids = g.BlobStreamPIDs()
	require.Len(t, pids, 1)
	require.NoError(t, rc.Close())
	requireGone(t, pids...)
	assert.Equal(t, 0, g.BlobStreams())

	// A context that is already cancelled starts nothing.
	cctx, cancel2 := context.WithCancel(context.Background())
	cancel2()
	_, _, err = g.OpenBlob(cctx, f.name, domain.Revision{}, tpath(t, "big"))
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
	f := newFixtureRepo(t, data, "huge")
	const size = 50 << 20
	content := randomBytes(size, 42)
	want := sha256.Sum256(content)
	f.writeBytes("huge.bin", content).commit("huge")
	content = nil //nolint:ineffassign // let the 50 MiB go before measuring

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	info, rc, err := g.OpenBlob(ctx, f.name, domain.Revision{}, tpath(t, "huge.bin"))
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
		<-errs // their outcome is the version check's, not the subject
	}
	require.Eventually(t, func() bool { _, taken := g.ExecSlots(); return taken == 0 }, 5*time.Second, time.Millisecond)
}

// Open blob streams are bounded; a refused stream leaves nothing behind and a
// closed one frees its slot.
func TestBlobStreamsAreBounded(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, func(c *gitcli.Config) { c.MaxBlobStreams = 2 })
	f := newFixtureRepo(t, data, "bigone")
	f.writeBytes("big", randomBytes(600<<10, 5)).commit("big")
	open := func() (io.ReadCloser, error) {
		_, rc, err := g.OpenBlob(ctx, f.name, domain.Revision{}, tpath(t, "big"))
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
