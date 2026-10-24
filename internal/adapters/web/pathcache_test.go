// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// store puts a list in the cache the way a finished build does.
func (c *pathCache) store(id string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putLocked(id, body)
}

// has says whether a list is cached, marking it as used.
func (c *pathCache) has(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.getLocked(id)
	return ok
}

// entry is the budget an entry of a one-letter id and an n-byte body takes.
func entry(n int) int { return 1 + n + entryOverhead }

func TestPathCacheEvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	budget := 4 * entry(100)
	c := newPathCache(budget, 1)
	blob := func(n int) []byte { return make([]byte, n) }
	c.store("a", blob(100))
	c.store("b", blob(100))
	c.store("e", blob(100))
	require.True(t, c.has("a")) // a is now the most recent
	c.store("c", blob(100))     // four fit
	c.store("d", blob(10))      // one more: b, the least recent, goes
	assert.False(t, c.has("b"), "b was evicted")
	assert.True(t, c.has("a") && c.has("c") && c.has("d") && c.has("e"))
	assert.Equal(t, 3*entry(100)+entry(10), c.size)

	// Replacing an entry accounts for the old size.
	c.store("a", blob(10))
	assert.Equal(t, 2*entry(100)+2*entry(10), c.size)

	// A big one pushes out as many as needed.
	c.store("f", blob(100))
	c.store("g", blob(100))
	assert.LessOrEqual(t, c.size, budget)
	assert.True(t, c.has("g"))
}

// Tiny bodies (a cached "no license") count their id and overhead too, so
// their number stays bounded by the budget.
func TestPathCacheBoundsTinyEntries(t *testing.T) {
	t.Parallel()
	c := newPathCache(1<<12, 1)
	for i := range 10_000 {
		c.store(fmt.Sprintf("repo\x00%040d", i), []byte("null"))
	}
	assert.LessOrEqual(t, c.size, 1<<12)
	assert.Less(t, len(c.byID), (1<<12)/entryOverhead+1)
	assert.Equal(t, len(c.byID), c.lru.Len())
}

// A list over a quarter of the budget is not kept, since it would push out the
// others; one at the limit is.
func TestPathCacheSkipsOversizedEntries(t *testing.T) {
	t.Parallel()
	c := newPathCache(1000, 1)
	var builds atomic.Int32
	big := func(n int) func() ([]byte, error) {
		return func() ([]byte, error) { builds.Add(1); return make([]byte, n), nil }
	}
	for range 2 {
		b, err := c.load(context.Background(), "r", "big", big(251))
		require.NoError(t, err)
		assert.Len(t, b, 251, "the caller still gets the list")
	}
	assert.EqualValues(t, 2, builds.Load(), "not cached, so built again")
	assert.False(t, c.has("big"))
	assert.Zero(t, c.size)

	for range 2 {
		_, err := c.load(context.Background(), "r", "fits", big(250))
		require.NoError(t, err)
	}
	assert.EqualValues(t, 3, builds.Load(), "at a quarter: kept")
	assert.Equal(t, len("fits")+250+entryOverhead, c.size)
}

func TestPathCacheConcurrent(t *testing.T) {
	t.Parallel()
	c := newPathCache(1000, 1)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				c.store(fmt.Sprint((g+i)%17), make([]byte, 10+i%90))
				c.has(fmt.Sprint(i % 17))
			}
		}()
	}
	wg.Wait()
	assert.LessOrEqual(t, c.size, 1000)
	sum := 0
	for _, el := range c.byID {
		sum += el.Value.(*pathEntry).cost()
	}
	assert.Equal(t, sum, c.size, "the size is the sum of the entries")
	assert.Equal(t, len(c.byID), c.lru.Len())
}

// waiters is the number of callers waiting for the build of a key, -1 when
// none is in flight.
func waiters(c *pathCache, repo, id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f := c.building[repo+"\x00"+id]; f != nil {
		return f.waiters
	}
	return -1
}

func building(c *pathCache, n int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.building) == n
}

// Concurrent misses for one commit build once; a failure is shared and not
// stored; a waiter that gives up leaves the build running.
func TestPathCacheLoadBuildsOnce(t *testing.T) {
	t.Parallel()
	c := newPathCache(1000, 4)
	var builds atomic.Int32
	release := make(chan struct{})
	build := func() ([]byte, error) {
		builds.Add(1)
		<-release
		return []byte("list"), nil
	}
	const callers = 10
	var wg sync.WaitGroup
	got := make([][]byte, callers)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := c.load(context.Background(), "r", "x", build)
			assert.NoError(t, err)
			got[i] = b
		}()
	}
	// Every caller waits for the one build before it is released: none can
	// find the list cached, so one build serving all ten shows they joined it.
	require.Eventually(t, func() bool { return waiters(c, "r", "x") == callers }, 5*time.Second, time.Millisecond)
	// A waiter whose context ends does not stop the others.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.load(ctx, "r", "x", build)
	assert.ErrorIs(t, err, context.Canceled)
	close(release)
	wg.Wait()
	assert.EqualValues(t, 1, builds.Load())
	for _, b := range got {
		assert.Equal(t, "list", string(b))
	}
	assert.True(t, c.has("x"))

	boom := errors.New("boom")
	_, err = c.load(context.Background(), "r", "y", func() ([]byte, error) { return nil, boom })
	assert.ErrorIs(t, err, boom)
	assert.False(t, c.has("y"), "a failure is not cached")
	b, err := c.load(context.Background(), "r", "y", func() ([]byte, error) { return []byte("ok"), nil })
	require.NoError(t, err)
	assert.Equal(t, "ok", string(b), "the next request builds again")
	assert.Empty(t, c.building)
}

// The builds of all commits share the slots: with one slot, a load of another
// commit waits for the running build, and leaves with its ctx if it is
// cancelled first. The slot is the build's, not the caller's.
func TestPathCacheLoadBoundsBuilds(t *testing.T) {
	t.Parallel()
	c := newPathCache(1000, 1)
	started, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		_, err := c.load(context.Background(), "r", "a", func() ([]byte, error) { close(started); <-release; return []byte("a"), nil })
		first <- err
	}()
	<-started

	// A second commit waits for the slot...
	var secondBuilt atomic.Bool
	second := make(chan error, 1)
	go func() {
		_, err := c.load(context.Background(), "r", "b", func() ([]byte, error) { secondBuilt.Store(true); return []byte("b"), nil })
		second <- err
	}()
	time.Sleep(30 * time.Millisecond)
	assert.False(t, secondBuilt.Load(), "no slot, no build")
	select {
	case err := <-second:
		t.Fatalf("the second load did not wait for a slot (err: %v)", err)
	default:
	}

	// ...or leaves with its ctx.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.load(ctx, "r", "c", func() ([]byte, error) { t.Error("built without a slot"); return nil, nil })
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	// A caller of the running build is not held by the slots.
	_, err = c.load(ctx, "r", "a", nil)
	assert.ErrorIs(t, err, context.DeadlineExceeded, "it joins the build and leaves with its ctx")

	close(release)
	require.NoError(t, <-first)
	require.NoError(t, <-second)
	assert.True(t, secondBuilt.Load())
	assert.True(t, c.has("a") && c.has("b"))
	assert.Empty(t, c.building)
	assert.Empty(t, c.slots, "every slot is back")
}

// The slot of a build is kept after its callers leave, and given back when it
// ends, whether it fails or panics.
func TestPathCacheSlotOutlivesCallers(t *testing.T) {
	t.Parallel()
	c := newPathCache(1000, 1)
	started, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.load(ctx, "r", "a", func() ([]byte, error) { close(started); <-release; panic("boom") })
		done <- err
	}()
	<-started
	cancel()
	assert.ErrorIs(t, <-done, context.Canceled)
	assert.Len(t, c.slots, 1, "the build still holds its slot")
	close(release)
	require.Eventually(t, func() bool { return len(c.slots) == 0 && building(c, 0) }, time.Second, time.Millisecond)
	b, err := c.load(context.Background(), "r", "b", func() ([]byte, error) { return []byte("ok"), nil })
	require.NoError(t, err)
	assert.Equal(t, "ok", string(b))
}

// A build that panics is handed to its waiters as an error, leaves nothing
// in flight or cached, and does not take the process down.
func TestPathCacheLoadPanic(t *testing.T) {
	t.Parallel()
	c := newPathCache(1000, 4)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	build := func() ([]byte, error) {
		once.Do(func() { close(started) })
		<-release
		panic("boom")
	}
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := c.load(context.Background(), "r", "x", build)
			errs <- err
		}()
	}
	<-started
	require.Eventually(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return len(c.building) == 1 }, time.Second, time.Millisecond)
	close(release)
	for range 2 {
		assert.ErrorIs(t, <-errs, errBuildPanicked)
	}
	assert.Empty(t, c.building)
	assert.False(t, c.has("x"), "a panic is not cached")

	b, err := c.load(context.Background(), "r", "x", func() ([]byte, error) { return []byte("ok"), nil })
	require.NoError(t, err)
	assert.Equal(t, "ok", string(b), "the next request builds again")
}

// The caller that starts a build is released with its context too, and the
// build goes on for the others.
func TestPathCacheLoadStarterLeaves(t *testing.T) {
	t.Parallel()
	c := newPathCache(1000, 4)
	release := make(chan struct{})
	build := func() ([]byte, error) { <-release; return []byte("list"), nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.load(ctx, "r", "x", build); done <- err }()
	require.Eventually(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return len(c.building) == 1 }, time.Second, time.Millisecond)
	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("the starter is held by its build")
	}
	close(release)
	require.Eventually(t, func() bool { return c.has("x") }, time.Second, time.Millisecond)
	assert.Empty(t, c.building)
}

// A failure belongs to the repository that built it: another repository
// asking for the same commit builds its own.
func TestPathCacheLoadFailureStaysInRepo(t *testing.T) {
	t.Parallel()
	c := newPathCache(1000, 4)
	started, release := make(chan struct{}), make(chan struct{})
	boom := errors.New("boom")
	failing := make(chan error, 1)
	go func() {
		_, err := c.load(context.Background(), "a", "x", func() ([]byte, error) { close(started); <-release; return nil, boom })
		failing <- err
	}()
	<-started
	b, err := c.load(context.Background(), "b", "x", func() ([]byte, error) { return []byte("list"), nil })
	require.NoError(t, err)
	assert.Equal(t, "list", string(b))
	close(release)
	assert.ErrorIs(t, <-failing, boom)
}
