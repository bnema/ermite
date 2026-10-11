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

func TestPathCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := newPathCache(100, 60)
	blob := func(n int) []byte { return make([]byte, n) }
	c.put("a", blob(40))
	c.put("b", blob(40))
	_, ok := c.get("a") // a is now the most recent
	require.True(t, ok)
	c.put("c", blob(40)) // 120 > 100: b, the least recent, goes
	_, ok = c.get("b")
	assert.False(t, ok, "b was evicted")
	_, okA := c.get("a")
	_, okC := c.get("c")
	assert.True(t, okA && okC)
	assert.Equal(t, 80, c.size)

	// Replacing an entry accounts for the old size.
	c.put("a", blob(10))
	assert.Equal(t, 50, c.size)

	// A big one pushes out as many as needed.
	c.put("d", blob(60))
	assert.LessOrEqual(t, c.size, 100)
	_, ok = c.get("d")
	assert.True(t, ok)
}

func TestPathCacheSkipsOversizedEntries(t *testing.T) {
	c := newPathCache(100, 60)
	c.put("big", make([]byte, 61))
	_, ok := c.get("big")
	assert.False(t, ok, "over the entry limit")
	c = newPathCache(50, 60)
	c.put("big", make([]byte, 51))
	_, ok = c.get("big")
	assert.False(t, ok, "over the budget")
	assert.Zero(t, c.size)
}

func TestPathCacheConcurrent(t *testing.T) {
	c := newPathCache(1000, 200)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				id := fmt.Sprint((g + i) % 17)
				c.put(id, make([]byte, 10+i%90))
				c.get(fmt.Sprint(i % 17))
			}
		}()
	}
	wg.Wait()
	assert.LessOrEqual(t, c.size, 1000)
	sum := 0
	for _, el := range c.byID {
		sum += len(el.Value.(*pathEntry).body)
	}
	assert.Equal(t, sum, c.size, "the size is the sum of the entries")
	assert.Equal(t, len(c.byID), c.lru.Len())
}

// Concurrent misses for one commit build once; a failure is shared and not
// stored; a waiter that gives up leaves the build running.
func TestPathCacheLoadBuildsOnce(t *testing.T) {
	c := newPathCache(1000, 200)
	var builds atomic.Int32
	release := make(chan struct{})
	build := func() ([]byte, error) {
		builds.Add(1)
		<-release
		return []byte("list"), nil
	}
	var wg sync.WaitGroup
	got := make([][]byte, 10)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := c.load(context.Background(), "x", build)
			assert.NoError(t, err)
			got[i] = b
		}()
	}
	// A waiter whose context ends does not stop the others.
	require.Eventually(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.building["x"] != nil }, time.Second, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.load(ctx, "x", build)
	assert.ErrorIs(t, err, context.Canceled)
	close(release)
	wg.Wait()
	assert.EqualValues(t, 1, builds.Load())
	for _, b := range got {
		assert.Equal(t, "list", string(b))
	}
	b, ok := c.get("x")
	assert.True(t, ok)
	assert.Equal(t, "list", string(b))

	boom := errors.New("boom")
	_, err = c.load(context.Background(), "y", func() ([]byte, error) { return nil, boom })
	assert.ErrorIs(t, err, boom)
	_, ok = c.get("y")
	assert.False(t, ok, "a failure is not cached")
	b, err = c.load(context.Background(), "y", func() ([]byte, error) { return []byte("ok"), nil })
	require.NoError(t, err)
	assert.Equal(t, "ok", string(b), "the next request builds again")
	assert.Empty(t, c.building)
}
