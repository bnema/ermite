// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"container/list"
	"context"
	"sync"
)

// hlCacheBytes is the budget of the cache of highlighted diffs: a file of 36
// KB of code weighs about 50 KB there, so it holds a few hundred of them.
const hlCacheBytes = 8 << 20

// codeCacheBytes is the separate budget for highlighted blob HTML.
const codeCacheBytes = 32 << 20

// hlCache keeps the colours of the files of diffs (hlData), least recently
// used out first, within a budget of bytes. A key names the blobs the colours
// come from (diffKey), so an entry is right for every visitor and every
// repository that has the blobs, forever; who may read the diff is decided
// before the cache is asked, and what is cached is runs of classes, never the
// text.
//
// Concurrent misses for one key share a single lexing: the first caller
// lexes, the others wait for it (or for their own ctx), as the figures of the
// home page do (usecase statsCache). A lexing that gave nothing (no free
// lexing slot, out of time or text, a failed lexer) is not kept: the next
// request tries again, and the callers that waited show the file plain.
//
// An entry over a quarter of the budget is not kept (one big file would push
// out most of the others).
//
// A nil *hlCache is a valid one that keeps nothing.
type hlCache struct{ *lruCache[*hlData] }

func newHLCache(budget int) *hlCache {
	return &hlCache{newLRUCache(budget,
		func(d *hlData) int { return d.size() },
		func(d *hlData) bool { return d != nil })}
}

func (c *hlCache) cache() *lruCache[*hlData] {
	if c == nil {
		return nil
	}
	return c.lruCache
}

// lruCache keeps valid values within a byte budget, sharing concurrent builds.
// A nil cache keeps nothing. Entries over a quarter of the budget are not kept.
type lruCache[V any] struct {
	mu           sync.Mutex
	budget, size int
	weigh        func(V) int
	valid        func(V) bool
	lru          *list.List // of *lruEntry[V], most recently used first
	byKey        map[string]*list.Element
	flight       map[string]*lruFlight[V]
}

type lruEntry[V any] struct {
	key  string
	data V
	size int
}

// lruFlight is a build in progress; done is closed when data is set.
type lruFlight[V any] struct {
	done    chan struct{}
	data    V
	waiters int // callers that joined it, for the tests; guarded by lruCache.mu
}

func newLRUCache[V any](budget int, weigh func(V) int, valid func(V) bool) *lruCache[V] {
	return &lruCache[V]{
		budget: budget, weigh: weigh, valid: valid, lru: list.New(),
		byKey: map[string]*list.Element{}, flight: map[string]*lruFlight[V]{},
	}
}

// entrySize is what an entry weighs against the budget: its data and its key.
func entrySize(key string, d *hlData) int { return d.size() + len(key) + 64 }

// load returns the value under key: cached, or from build, which runs once
// however many callers ask at the same time, in the goroutine of the first.
// Invalid values are not kept. An empty key is not cached: build runs.
func (c *lruCache[V]) load(ctx context.Context, key string, build func() V) V {
	if c == nil || key == "" {
		return build()
	}
	c.mu.Lock()
	if el, ok := c.byKey[key]; ok {
		c.lru.MoveToFront(el)
		d := el.Value.(*lruEntry[V]).data
		c.mu.Unlock()
		return d
	}
	if f, ok := c.flight[key]; ok {
		f.waiters++
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.data
		case <-ctx.Done():
			var zero V
			return zero
		}
	}
	f := &lruFlight[V]{done: make(chan struct{})}
	c.flight[key] = f
	c.mu.Unlock()

	// Whatever build does, the flight ends: the callers that wait are freed,
	// by a panic too (which then goes on up to the server's recovery).
	defer func() {
		c.mu.Lock()
		delete(c.flight, key)
		if c.valid(f.data) {
			c.putLocked(key, f.data)
		}
		c.mu.Unlock()
		close(f.done)
	}()
	f.data = build()
	return f.data
}

// putLocked stores a value, evicting the least recently used ones to stay in
// budget. Values over a quarter of the budget are not stored.
func (c *lruCache[V]) putLocked(key string, d V) {
	n := c.weigh(d) + len(key) + 64
	if n > c.budget/4 {
		return
	}
	if el, ok := c.byKey[key]; ok {
		e := el.Value.(*lruEntry[V])
		c.size += n - e.size
		e.data, e.size = d, n
		c.lru.MoveToFront(el)
	} else {
		c.byKey[key] = c.lru.PushFront(&lruEntry[V]{key: key, data: d, size: n})
		c.size += n
	}
	for c.size > c.budget {
		el := c.lru.Back()
		e := el.Value.(*lruEntry[V])
		c.lru.Remove(el)
		delete(c.byKey, e.key)
		c.size -= e.size
	}
}
