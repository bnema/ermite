// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"container/list"
	"context"
	"runtime/debug"
	"sync"

	"github.com/bnema/zerowrap"
)

const (
	// pathCacheBytes is the budget of the cache of path lists.
	pathCacheBytes = 64 << 20
	// pathEntryMax is the largest list the cache keeps; a bigger one is built
	// again for each request that needs it.
	pathEntryMax = 16 << 20
)

// pathCache keeps the gzipped JSON path lists of commits, least recently used
// out first, within a budget of bytes. A commit id names its content, so an
// entry is right for every repository and every visitor; who may read it is
// decided before the cache is asked (Server.paths).
//
// Concurrent misses for one commit of one repository share a single build.
type pathCache struct {
	mu       sync.Mutex
	budget   int
	size     int
	max      int        // largest entry kept
	lru      *list.List // of *pathEntry, most recently used first
	byID     map[string]*list.Element
	building map[string]*pathBuild // by repository and commit
}

type pathEntry struct {
	id   string
	body []byte
}

// pathBuild is a build in progress; done is closed when body and err are set.
type pathBuild struct {
	done chan struct{}
	body []byte
	err  error
}

func newPathCache(budget, max int) *pathCache {
	return &pathCache{
		budget: budget, max: max, lru: list.New(),
		byID: map[string]*list.Element{}, building: map[string]*pathBuild{},
	}
}

// get returns the list of a commit and marks it as used.
func (c *pathCache) get(id string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getLocked(id)
}

func (c *pathCache) getLocked(id string) ([]byte, bool) {
	el, ok := c.byID[id]
	if !ok {
		return nil, false
	}
	c.lru.MoveToFront(el)
	return el.Value.(*pathEntry).body, true
}

// put stores a list, evicting the least recently used ones to stay in budget.
// A list over the entry limit or the budget is not stored.
func (c *pathCache) put(id string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putLocked(id, body)
}

func (c *pathCache) putLocked(id string, body []byte) {
	if len(body) > c.max || len(body) > c.budget {
		return
	}
	if el, ok := c.byID[id]; ok {
		c.size -= len(el.Value.(*pathEntry).body)
		el.Value.(*pathEntry).body = body
		c.size += len(body)
		c.lru.MoveToFront(el)
	} else {
		c.byID[id] = c.lru.PushFront(&pathEntry{id: id, body: body})
		c.size += len(body)
	}
	for c.size > c.budget {
		el := c.lru.Back()
		e := el.Value.(*pathEntry)
		c.lru.Remove(el)
		delete(c.byID, e.id)
		c.size -= len(e.body)
	}
}

// load returns the list of a commit: from the cache, or from build, which
// runs once however many callers of a repository ask at the same time. The
// build runs in its own goroutine, so no caller is held by it: each one,
// the one that started it included, waits for the build or for its own ctx.
//
// Builds in progress are keyed by repository and commit, though the cache is
// keyed by commit only: a failure of one repository (the commit is missing
// there, its access rule changed) must not be handed to the requests of
// another one that would have succeeded.
func (c *pathCache) load(ctx context.Context, repo, id string, build func() ([]byte, error)) ([]byte, error) {
	key := repo + "\x00" + id // a repository name has no NUL
	c.mu.Lock()
	if b, ok := c.getLocked(id); ok {
		c.mu.Unlock()
		return b, nil
	}
	f, ok := c.building[key]
	if !ok {
		f = &pathBuild{done: make(chan struct{})}
		c.building[key] = f
		go c.run(ctx, key, id, f, build)
	}
	c.mu.Unlock()
	select {
	case <-f.done:
		return f.body, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// run executes a build and publishes it. A panic of build is logged and
// handed to the waiters as errBuildPanicked: it cannot propagate, since
// nobody above this goroutine could recover it and it would end the process.
func (c *pathCache) run(ctx context.Context, key, id string, f *pathBuild, build func() ([]byte, error)) {
	defer func() {
		if r := recover(); r != nil {
			f.body, f.err = nil, errBuildPanicked
			zerowrap.Ctx(ctx).Error().Interface("panic", r).Str("stack", string(debug.Stack())).Msg("path list build panicked")
		}
		c.finish(key, id, f)
	}()
	f.body, f.err = build()
}

// finish publishes a build: stored when it worked, then released to the waiters.
func (c *pathCache) finish(key, id string, f *pathBuild) {
	c.mu.Lock()
	delete(c.building, key)
	if f.err == nil {
		c.putLocked(id, f.body)
	}
	c.mu.Unlock()
	close(f.done)
}
