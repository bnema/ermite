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
type hlCache struct {
	mu     sync.Mutex
	budget int
	size   int
	lru    *list.List // of *hlEntry, most recently used first
	byKey  map[string]*list.Element
	flight map[string]*hlFlight
}

type hlEntry struct {
	key  string
	data *hlData
	size int
}

// hlFlight is a lexing in progress; done is closed when data is set.
type hlFlight struct {
	done    chan struct{}
	data    *hlData
	waiters int // callers that joined it, for the tests; guarded by hlCache.mu
}

func newHLCache(budget int) *hlCache {
	return &hlCache{
		budget: budget, lru: list.New(),
		byKey: map[string]*list.Element{}, flight: map[string]*hlFlight{},
	}
}

// entrySize is what an entry weighs against the budget: its data and its key.
func entrySize(key string, d *hlData) int { return d.size() + len(key) + 64 }

// load returns the colours under key: cached, or from build, which runs once
// however many callers ask at the same time, in the goroutine of the first.
// build returns nil for no colours. An empty key is not cached: build runs.
func (c *hlCache) load(ctx context.Context, key string, build func() *hlData) *hlData {
	if c == nil || key == "" {
		return build()
	}
	c.mu.Lock()
	if el, ok := c.byKey[key]; ok {
		c.lru.MoveToFront(el)
		d := el.Value.(*hlEntry).data
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
			return nil
		}
	}
	f := &hlFlight{done: make(chan struct{})}
	c.flight[key] = f
	c.mu.Unlock()

	// Whatever build does, the flight ends: the callers that wait are freed,
	// by a panic too (which then goes on up to the server's recovery).
	defer func() {
		c.mu.Lock()
		delete(c.flight, key)
		if f.data != nil {
			c.putLocked(key, f.data)
		}
		c.mu.Unlock()
		close(f.done)
	}()
	f.data = build()
	return f.data
}

// putLocked stores colours, evicting the least recently used ones to stay in
// budget. Colours over a quarter of the budget are not stored.
func (c *hlCache) putLocked(key string, d *hlData) {
	n := entrySize(key, d)
	if n > c.budget/4 {
		return
	}
	if el, ok := c.byKey[key]; ok {
		e := el.Value.(*hlEntry)
		c.size += n - e.size
		e.data, e.size = d, n
		c.lru.MoveToFront(el)
	} else {
		c.byKey[key] = c.lru.PushFront(&hlEntry{key: key, data: d, size: n})
		c.size += n
	}
	for c.size > c.budget {
		el := c.lru.Back()
		e := el.Value.(*hlEntry)
		c.lru.Remove(el)
		delete(c.byKey, e.key)
		c.size -= e.size
	}
}
