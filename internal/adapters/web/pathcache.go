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
	// pathBuilds is how many path lists are built at the same time, for all
	// commits and repositories: a build walks a whole tree and holds the
	// list in memory.
	pathBuilds = 4
)

// pathCache keeps the gzipped JSON path lists of commits, least recently used
// out first, within a budget of bytes. A commit id names its content, so an
// entry is right for every repository and every visitor; who may read it is
// decided before the cache is asked (Server.paths).
//
// Concurrent misses for one commit of one repository share a single build,
// and only a few builds run at a time.
//
// A list over a quarter of the budget is not kept (one big list would push
// out most of the others): it is built again for each request that needs it.
type pathCache struct {
	mu       sync.Mutex
	budget   int
	size     int
	lru      *list.List // of *pathEntry, most recently used first
	byID     map[string]*list.Element
	building map[string]*pathBuild // by repository and commit
	slots    chan struct{}         // one token per build running
}

type pathEntry struct {
	id   string
	body []byte
}

// pathBuild is a build in progress; done is closed when body and err are set.
type pathBuild struct {
	waiters int // callers that joined it, for the tests; guarded by pathCache.mu
	done    chan struct{}
	body    []byte
	err     error
}

func newPathCache(budget, builds int) *pathCache {
	return &pathCache{
		budget: budget, lru: list.New(),
		byID: map[string]*list.Element{}, building: map[string]*pathBuild{},
		slots: make(chan struct{}, builds),
	}
}

func (c *pathCache) getLocked(id string) ([]byte, bool) {
	el, ok := c.byID[id]
	if !ok {
		return nil, false
	}
	c.lru.MoveToFront(el)
	return el.Value.(*pathEntry).body, true
}

// entryOverhead is what an entry costs besides its id and body: the list
// element, the entry and the map slot. Counted so that tiny bodies (a cached
// "no license") cannot pile up far past the budget.
const entryOverhead = 128

// cost is what an entry counts against the budget.
func (e *pathEntry) cost() int { return len(e.id) + len(e.body) + entryOverhead }

// putLocked stores a list, evicting the least recently used ones to stay in
// budget. A list over a quarter of the budget is not stored.
func (c *pathCache) putLocked(id string, body []byte) {
	if len(body) > c.budget/4 {
		return
	}
	if el, ok := c.byID[id]; ok {
		e := el.Value.(*pathEntry)
		c.size -= e.cost()
		e.body = body
		c.size += e.cost()
		c.lru.MoveToFront(el)
	} else {
		e := &pathEntry{id: id, body: body}
		c.byID[id] = c.lru.PushFront(e)
		c.size += e.cost()
	}
	for c.size > c.budget {
		el := c.lru.Back()
		e := el.Value.(*pathEntry)
		c.lru.Remove(el)
		delete(c.byID, e.id)
		c.size -= e.cost()
	}
}

// load returns the list of a commit: from the cache, or from build, which
// runs once however many callers of a repository ask at the same time. The
// build runs in its own goroutine, so no caller is held by it: each one,
// the one that started it included, waits for the build or for its own ctx.
//
// Only pathBuilds builds run at a time, whatever their commits: a caller that
// would start one waits for a free slot, or for its ctx. The build keeps the
// slot until it ends, even if every caller has left.
//
// Builds in progress are keyed by repository and commit, though the cache is
// keyed by commit only: a failure of one repository (the commit is missing
// there, its access rule changed) must not be handed to the requests of
// another one that would have succeeded.
func (c *pathCache) load(ctx context.Context, repo, id string, build func() ([]byte, error)) ([]byte, error) {
	key := repo + "\x00" + id // a repository name has no NUL
	f, body, ok := c.join(key, id)
	if ok {
		return body, nil
	}
	if f == nil {
		// Nothing to join: take a slot, then look again, since the commit
		// may have been built while this caller waited.
		select {
		case c.slots <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if f, body, ok = c.joinOrStart(ctx, key, id, build); ok {
			return body, nil
		}
	}
	select {
	case <-f.done:
		return f.body, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// join returns the list of a commit if it is cached, or the build in progress
// for it.
func (c *pathCache) join(key, id string) (f *pathBuild, body []byte, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b, ok := c.getLocked(id); ok {
		return nil, b, true
	}
	f = c.building[key]
	if f != nil {
		f.waiters++
	}
	return f, nil, false
}

// joinOrStart is join for a caller that holds a slot: it starts the build when
// there is still none, and gives the slot back when it does not need it.
func (c *pathCache) joinOrStart(ctx context.Context, key, id string, build func() ([]byte, error)) (f *pathBuild, body []byte, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b, ok := c.getLocked(id); ok {
		<-c.slots
		return nil, b, true
	}
	if f, ok := c.building[key]; ok {
		<-c.slots
		f.waiters++
		return f, nil, false
	}
	f = &pathBuild{done: make(chan struct{}), waiters: 1}
	c.building[key] = f
	go c.run(ctx, key, id, f, build)
	return f, nil, false
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
	<-c.slots // free before the waiters wake: one of them may start the next build
	close(f.done)
}
