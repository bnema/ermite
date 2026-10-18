// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

// dataOf is colours of n lines, about 8n+64 bytes.
func dataOf(n int) *hlData {
	d := &hlData{lineEnd: make([]uint32, n), runs: make([]hlRun, n)}
	for i := range n {
		d.runs[i] = hlRun{end: 3, class: 1}
		d.lineEnd[i] = uint32(i + 1)
	}
	return d
}

func withID(f domain.FileDiff, n int) domain.FileDiff {
	f.OldID, f.NewID = fakeBlob(n, 0), fakeBlob(n, 1)
	return f
}

// A file whose colours are in the cache is coloured with no lexing: with
// every lexing slot taken, a miss is plain and a hit is not.
func TestHLCacheHit(t *testing.T) {
	fd := withID(hunkOf("a.go", " package a", "+var x = 1 // c"), 1)
	c := newHLCache(1 << 20)

	first := c.files(context.Background(), []domain.FileDiff{fd})
	assert.Contains(t, rendered(t, first[0])[1], `class="kd"`)
	assert.Len(t, c.byKey, 1)

	for range cap(lexSlots) {
		lexSlots <- struct{}{}
	}
	defer func() {
		for range cap(lexSlots) {
			<-lexSlots
		}
	}()
	hit := c.files(context.Background(), []domain.FileDiff{fd})
	assert.Equal(t, first, hit, "same page from the cache")

	other := c.files(context.Background(), []domain.FileDiff{withID(fd, 2)})
	assert.NotContains(t, rendered(t, other[0])[1], `class=`, "another blob: a miss, and no slot to lex")
	assert.Len(t, c.byKey, 1, "a failed lexing is not kept")
}

// The cache holds colours, not text: the text of the page is the one of the
// diff being rendered, escaped as ever.
func TestHLCacheKeepsNoText(t *testing.T) {
	c := newHLCache(1 << 20)
	a := withID(hunkOf("a.go", "+x := \"<b>one</b>\""), 1)
	b := withID(hunkOf("a.go", "+x := \"<i>two</i>\""), 1) // same key, other text: cannot happen, but must not leak
	c.files(context.Background(), []domain.FileDiff{a})
	got := c.files(context.Background(), []domain.FileDiff{b})
	h := rendered(t, got[0])[0]
	assert.Contains(t, h, "&lt;i&gt;two&lt;/i&gt;")
	assert.NotContains(t, h, "one")
}

// The budget is in bytes: the least recently used entries go first, a hit
// makes an entry young, and an entry over a quarter of the budget is not kept.
func TestHLCacheEvictsByBytes(t *testing.T) {
	t.Parallel()
	one := entrySize("k0", dataOf(100))
	c := newHLCache(one*4 + one/2) // four entries fit, not five; one is under a quarter
	ctx := context.Background()
	add := func(k string) {
		c.load(ctx, k, func() *hlData { return dataOf(100) })
	}
	add("k0")
	add("k1")
	add("k2")
	add("k3")
	assert.Len(t, c.byKey, 4)
	c.load(ctx, "k0", func() *hlData { t.Fatal("k0 is cached"); return nil }) // k0 is the youngest now
	add("k4")                                                                 // pushes out k1, the oldest
	assert.Len(t, c.byKey, 4)
	assert.Contains(t, c.byKey, "k0")
	assert.NotContains(t, c.byKey, "k1")
	assert.LessOrEqual(t, c.size, c.budget)

	// the sum of the sizes of the entries is what is counted
	sum := 0
	for _, el := range c.byKey {
		sum += el.Value.(*hlEntry).size
	}
	assert.Equal(t, sum, c.size)

	// over a quarter of the budget: served, not kept, nothing pushed out
	big := 0
	c.load(ctx, "big", func() *hlData { big++; return dataOf(1000) })
	c.load(ctx, "big", func() *hlData { big++; return dataOf(1000) })
	assert.Equal(t, 2, big)
	assert.NotContains(t, c.byKey, "big")
	assert.Len(t, c.byKey, 4)

	// many entries: the size never goes over the budget
	for i := range 200 {
		add(fmt.Sprintf("n%d", i))
		require.LessOrEqual(t, c.size, c.budget)
	}
}

// A nil cache and an empty key keep nothing; a build that gives nothing is
// not kept and is tried again.
func TestHLCacheNotKept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := 0
	build := func() *hlData { n++; return nil }
	var nilCache *hlCache
	nilCache.load(ctx, "k", build)
	newHLCache(1<<20).load(ctx, "", build)
	c := newHLCache(1 << 20)
	c.load(ctx, "k", build)
	c.load(ctx, "k", build)
	assert.Equal(t, 4, n)
	assert.Empty(t, c.byKey)
	assert.Empty(t, c.flight)
}

// N concurrent first requests for one key lex once: the others wait for the
// first, and all get its data. Run with -race.
func TestHLCacheDedup(t *testing.T) {
	t.Parallel()
	c := newHLCache(1 << 20)
	const n = 32
	var builds atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	got := make([]*hlData, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = c.load(context.Background(), "k", func() *hlData {
				builds.Add(1)
				<-release // hold the build until every other caller has joined
				return dataOf(10)
			})
		}()
	}
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		f := c.flight["k"]
		return f != nil && f.waiters == n-1
	}, 5*time.Second, time.Millisecond)
	close(release)
	wg.Wait()

	assert.EqualValues(t, 1, builds.Load())
	for i := range n {
		require.NotNil(t, got[i])
		assert.Same(t, got[0], got[i], "one result for all")
	}
	assert.Empty(t, c.flight)
	assert.Len(t, c.byKey, 1)
}

// The same through the diff: concurrent pages of one commit lex its file once.
func TestHLCacheDedupFiles(t *testing.T) {
	c := newHLCache(1 << 20)
	var lines []string
	for range 300 {
		lines = append(lines, "+var v = 1 // 0123456789")
	}
	fd := withID(hunkOf("a.go", lines...), 7)
	const n = 16
	var wg sync.WaitGroup
	pages := make([][]DiffFile, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pages[i] = c.files(context.Background(), []domain.FileDiff{fd})
		}()
	}
	wg.Wait()
	for i := range n {
		assert.Equal(t, pages[0], pages[i])
		assert.Contains(t, rendered(t, pages[i][0])[0], `class="kd"`)
	}
	assert.Len(t, c.byKey, 1)
}

// A waiter that gives up is freed at once and shows the file plain; the
// build goes on and its result is kept for the next request.
func TestHLCacheWaiterCancelled(t *testing.T) {
	t.Parallel()
	c := newHLCache(1 << 20)
	release := make(chan struct{})
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.load(context.Background(), "k", func() *hlData { close(started); <-release; return dataOf(5) })
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.Nil(t, c.load(ctx, "k", func() *hlData { t.Error("joins the build"); return nil }))
	close(release)
	<-done
	assert.NotNil(t, c.load(context.Background(), "k", func() *hlData { t.Error("kept"); return nil }))
}

// A build that panics frees the callers that wait and is not kept.
func TestHLCacheBuildPanics(t *testing.T) {
	t.Parallel()
	c := newHLCache(1 << 20)
	started := make(chan struct{})
	release := make(chan struct{})
	res := make(chan *hlData, 1)
	go func() {
		defer func() { _ = recover() }()
		c.load(context.Background(), "k", func() *hlData { close(started); <-release; panic("lexer bug") })
	}()
	<-started
	go func() { res <- c.load(context.Background(), "k", func() *hlData { return dataOf(1) }) }()
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.flight["k"] != nil && c.flight["k"].waiters == 1
	}, 5*time.Second, time.Millisecond)
	close(release)
	select {
	case d := <-res:
		assert.Nil(t, d)
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter is stuck")
	}
	assert.Empty(t, c.flight)
	assert.Empty(t, c.byKey)
}

// The key tells apart what changes the colours: the blobs, the language, the
// shape of the hunks.
func TestDiffKey(t *testing.T) {
	t.Parallel()
	f := withID(hunkOf("a.go", " a", "+b"), 1)
	k := diffKey("Go", f)
	assert.NotEmpty(t, k)
	assert.Equal(t, k, diffKey("Go", f))
	assert.NotEqual(t, k, diffKey("C", f))
	assert.NotEqual(t, k, diffKey("Go", withID(f, 2)))
	assert.NotEqual(t, k, diffKey("Go", withID(hunkOf("a.go", " a", "+b", "+c"), 1)))
	assert.Empty(t, diffKey("Go", hunkOf("a.go", " a")), "no blob ids: not kept")
	assert.False(t, strings.Contains(k, "a.go"), "the path is not in the key: two paths with the same blobs share")
}

// The lines /context loads are cached under the blob of the file.
func TestHLCacheContext(t *testing.T) {
	c := newHLCache(1 << 20)
	lines := []string{"package a", "/* open", "still open", "closed */", "var x = 1"}
	blob := fakeBlob(3, 1).String()
	ctx := context.Background()
	first := c.highlightContext(ctx, blob, "a.go", lines, 1, 4)
	require.Len(t, first, 3)
	assert.Equal(t, []Seg{{Text: "still open", Class: "cm"}}, first[1])
	assert.Len(t, c.byKey, 1)

	for range cap(lexSlots) {
		lexSlots <- struct{}{}
	}
	defer func() {
		for range cap(lexSlots) {
			<-lexSlots
		}
	}()
	assert.Equal(t, first, c.highlightContext(ctx, blob, "a.go", lines, 1, 4), "from the cache")
	assert.Equal(t, first[1:], c.highlightContext(ctx, blob, "a.go", lines, 2, 4), "other window, same colours")
	assert.Nil(t, c.highlightContext(ctx, fakeBlob(4, 1).String(), "a.go", lines, 1, 4), "other blob: no slot, plain")
}
