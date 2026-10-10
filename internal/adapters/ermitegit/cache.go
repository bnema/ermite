// SPDX-License-Identifier: AGPL-3.0-or-later

package ermitegit

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	git "bnema.dev/ermite-git"

	"bnema.dev/ermite/internal/domain"
)

// DefaultMaxRepos is the number of repositories kept open when Config.MaxRepos
// is zero.
const DefaultMaxRepos = 64

// DefaultRefreshEvery is how long a repository goes without a Refresh when
// Config.RefreshEvery is zero. See Config.RefreshEvery for what depends on it.
const DefaultRefreshEvery = time.Second

// unsupportedRetry is how long a repository that ermite-git cannot read is
// served by git before Open is tried again (a repack, or the removal of a
// replace ref, can make it readable).
const unsupportedRetry = time.Minute

// entry is the state of one repository directory in the cache: an open
// *git.Repo, or the reason it is not readable by ermite-git.
//
// The fields below the line belong to the cache and are guarded by its mutex.
type entry struct {
	dir  string
	ino  uint64 // of the directory: a new repository under the same name differs
	dev  uint64
	once sync.Once
	// opened is the result of git.Open, published by the Once; nil before.
	opened atomic.Pointer[opened]

	refreshed atomic.Int64 // unix nanoseconds of the last Refresh

	elem    *list.Element
	users   int
	dropped bool // out of the cache: close when the last user is done
}

// opened is what git.Open answered for an entry.
type opened struct {
	repo *git.Repo // nil when unsupported
	err  error     // Open failed with something else than ErrUnsupported
	at   time.Time
}

// handle is a use of an entry; release it when the read is over. repo is nil
// when git must serve the read.
type handle struct {
	c    *cache
	e    *entry
	repo *git.Repo
}

func (h *handle) release() { h.c.release(h.e) }

// cache is a bounded LRU of open repositories keyed by directory. A repository
// leaves it by eviction, by Forget (the directory was deleted) or because the
// directory is no longer the same one (checked on each use through its
// inode); it is closed once the reads that use it are done.
type cache struct {
	max          int
	refreshEvery time.Duration
	onUnsupport  func(ctx context.Context, dir string, name domain.RepoName, cause error)

	mu      sync.Mutex
	entries map[string]*entry
	lru     *list.List // front = most recently used
	closed  bool
}

func newCache(max int, refreshEvery time.Duration, onUnsupport func(context.Context, string, domain.RepoName, error)) *cache {
	if max <= 0 {
		max = DefaultMaxRepos
	}
	return &cache{max: max, refreshEvery: refreshEvery, onUnsupport: onUnsupport, entries: make(map[string]*entry), lru: list.New()}
}

// get returns the entry of dir, opened and refreshed, or a nil repo when
// ermite-git does not read the repository. A missing directory is
// domain.ErrNotFound.
func (c *cache) get(ctx context.Context, name domain.RepoName, dir string) (*handle, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("repository %q: %w", name, err)
	}
	if !fi.IsDir() {
		return nil, domain.ErrNotFound
	}
	dev, ino := identity(fi)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	e := c.entries[dir]
	if e != nil && (e.ino != ino || e.dev != dev || e.stale()) {
		c.dropLocked(e)
		e = nil
	}
	if e == nil {
		e = &entry{dir: dir, ino: ino, dev: dev}
		e.elem = c.lru.PushFront(e)
		c.entries[dir] = e
		c.evictLocked()
	} else {
		c.lru.MoveToFront(e.elem)
	}
	e.users++
	c.mu.Unlock()

	h := &handle{c: c, e: e}
	e.once.Do(func() { c.open(ctx, name, e) })
	op := e.opened.Load()
	if op.err != nil {
		c.forgetEntry(e) // the next read tries again
		h.release()
		return nil, fmt.Errorf("open repository %q: %w", name, mapOpenErr(op.err))
	}
	if op.repo == nil {
		return h, nil // unsupported
	}
	if err := c.refresh(e, op.repo); err != nil {
		if errors.Is(err, git.ErrUnsupported) {
			h.fallBack(ctx, name, err)
			return h, nil
		}
		h.release()
		return nil, fmt.Errorf("refresh repository %q: %w", name, err)
	}
	h.repo = op.repo
	return h, nil
}

// refresh calls Refresh on the repository of e unless one ran less than
// refreshEvery ago (a negative interval refreshes on every read). The library
// reads loose refs live, stat-checks packed-refs on each ref read and rescans
// the packs when an object is missing, so a read sees a push whether or not
// Refresh ran; Refresh is what drops replaced packs, picks up a new
// commit-graph and notices that the repository became unsupported.
func (c *cache) refresh(e *entry, repo *git.Repo) error {
	now := time.Now().UnixNano()
	if c.refreshEvery >= 0 && now-e.refreshed.Load() < int64(c.refreshEvery) {
		return nil
	}
	if err := repo.Refresh(); err != nil {
		return err
	}
	e.refreshed.Store(now)
	return nil
}

// stale reports whether a repository that ermite-git did not read is due for
// another try. The caller holds the cache mutex.
func (e *entry) stale() bool {
	op := e.opened.Load()
	return op != nil && op.repo == nil && op.err == nil && time.Since(op.at) > unsupportedRetry
}

// open runs git.Open once per entry; concurrent users wait in Once.Do.
func (c *cache) open(ctx context.Context, name domain.RepoName, e *entry) {
	op := &opened{at: time.Now()}
	repo, err := git.Open(e.dir)
	switch {
	case err == nil:
		op.repo = repo
	case errors.Is(err, git.ErrUnsupported):
		c.onUnsupport(ctx, e.dir, name, err)
	default:
		op.err = err
	}
	e.opened.Store(op)
}

// mapOpenErr maps the failure of git.Open: a directory that is not a
// repository is a missing repository.
func mapOpenErr(err error) error {
	if errors.Is(err, git.ErrNotFound) {
		return fmt.Errorf("%w: %w", domain.ErrNotFound, err)
	}
	return err
}

// markUnsupported turns a repository that stopped being readable (a replace
// ref was pushed, say) into an unsupported one: git serves it from now on.
func (c *cache) markUnsupported(ctx context.Context, name domain.RepoName, e *entry, cause error) {
	c.onUnsupport(ctx, e.dir, name, cause)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries[e.dir] == e {
		c.dropLocked(e)
		n := &entry{dir: e.dir, ino: e.ino, dev: e.dev}
		n.once.Do(func() { n.opened.Store(&opened{at: time.Now()}) }) // no repo, no error: unsupported
		n.elem = c.lru.PushFront(n)
		c.entries[e.dir] = n
	}
}

// fallBack records that the repository stopped being readable by ermite-git
// (a replace ref was pushed, a feature appeared midway): git serves it from
// now on, and this read too.
func (h *handle) fallBack(ctx context.Context, name domain.RepoName, cause error) {
	h.repo = nil
	h.c.markUnsupported(ctx, name, h.e, cause)
}

func (c *cache) release(e *entry) {
	c.mu.Lock()
	e.users--
	closeNow := e.dropped && e.users == 0
	c.mu.Unlock()
	if closeNow {
		e.closeRepo()
	}
}

// closeRepo closes the repository of an entry that left the cache and has no
// user. An entry whose open has not finished has no repository yet: its last
// user closes it.
func (e *entry) closeRepo() {
	if op := e.opened.Load(); op != nil && op.repo != nil {
		_ = op.repo.Close()
	}
}

// dropLocked removes e from the cache. The caller holds the mutex. The
// repository is closed by the last user (or by the caller if it has none).
func (c *cache) dropLocked(e *entry) {
	if e.dropped {
		return
	}
	e.dropped = true
	c.lru.Remove(e.elem)
	delete(c.entries, e.dir)
	if e.users == 0 {
		go e.closeRepo() // not under the mutex: Close takes the repo's own
	}
}

// evictLocked drops the least recently used entries beyond the bound.
func (c *cache) evictLocked() {
	for c.lru.Len() > c.max {
		c.dropLocked(c.lru.Back().Value.(*entry))
	}
}

// forgetEntry drops e if it is still the entry of its directory.
func (c *cache) forgetEntry(e *entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries[e.dir] == e {
		c.dropLocked(e)
	}
}

// forget drops the entry of dir.
func (c *cache) forget(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[dir]; e != nil {
		c.dropLocked(e)
	}
}

// close drops every entry; later gets fail with ErrClosed.
func (c *cache) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for _, e := range c.entries {
		c.dropLocked(e)
	}
}

// size returns the number of cached repositories.
func (c *cache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func identity(fi fs.FileInfo) (dev, ino uint64) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev), st.Ino //nolint:unconvert // Dev is uint32 on some platforms
	}
	return 0, 0
}
