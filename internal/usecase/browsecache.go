// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"container/list"
	"slices"
	"sync"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Default bounds of the history cache of Browse.
const (
	// DefaultHistoryCacheEntries bounds the cached answers.
	DefaultHistoryCacheEntries = 256
	// DefaultHistoryCacheBytes bounds their estimated size.
	DefaultHistoryCacheBytes = 8 << 20
)

// historyKey names an answer that depends only on immutable data: a commit
// designated by its full id, the path inside it, and for a page of history
// where it starts and how long it is. The repository is part of the key so
// that an answer is never served across a name, and so is its creation time
// (in nanoseconds): a repository deleted and created again under the same
// name is another repository, whose commit ids must not hit the old answers.
type historyKey struct {
	repo    domain.RepoName
	created int64
	rev     string // full commit id
	path    string
	kind    historyKind
	limit   int
}

type historyKind uint8

const (
	kindTreeLast historyKind = iota // a directory with the last commit of its entries
	kindPage                        // the first page of the history of a path
)

// historyCache is a small LRU of answers about commits, bounded by entry count
// and by estimated bytes. The answers are immutable (a commit id fixes its
// whole history), so there is no invalidation: entries leave by eviction only.
// It is safe for concurrent use. Values are stored and returned as copies, so
// a caller cannot alter what another one gets.
type historyCache struct {
	maxEntries int
	maxBytes   int64

	mu    sync.Mutex
	order *list.List // front is the most recently used
	items map[historyKey]*list.Element
	bytes int64
}

type historyEntry struct {
	key  historyKey
	tree domain.Tree
	page ports.CommitPage
	size int64
}

// newHistoryCache returns nil (no cache) unless both bounds are positive.
func newHistoryCache(maxEntries int, maxBytes int64) *historyCache {
	if maxEntries <= 0 || maxBytes <= 0 {
		return nil
	}
	return &historyCache{
		maxEntries: maxEntries, maxBytes: maxBytes,
		order: list.New(), items: map[historyKey]*list.Element{},
	}
}

// get returns the entry of key and marks it recently used.
func (c *historyCache) get(key historyKey) (*historyEntry, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*historyEntry), true
}

// put stores e, then evicts the least recently used entries until both bounds
// hold. An entry larger than the byte bound is not stored.
func (c *historyCache) put(e *historyEntry) {
	if c == nil || e.size > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[e.key]; ok {
		c.bytes -= el.Value.(*historyEntry).size
		c.order.Remove(el)
	}
	c.items[e.key] = c.order.PushFront(e)
	c.bytes += e.size
	for c.order.Len() > c.maxEntries || c.bytes > c.maxBytes {
		oldest := c.order.Back()
		old := oldest.Value.(*historyEntry)
		c.order.Remove(oldest)
		delete(c.items, old.key)
		c.bytes -= old.size
	}
}

// treeEntry builds the entry of a tree; the tree is copied.
func treeEntry(key historyKey, t domain.Tree) *historyEntry {
	t = cloneTree(t)
	size := int64(256 + len(t.Entries)*160)
	for _, e := range t.Entries {
		size += int64(len(e.Name) + len(e.Path))
		if e.LastCommit != nil {
			size += commitSize(*e.LastCommit)
		}
	}
	return &historyEntry{key: key, tree: t, size: size}
}

// pageEntry builds the entry of a page of history; the page is copied.
func pageEntry(key historyKey, p ports.CommitPage) *historyEntry {
	p = clonePage(p)
	size := int64(128)
	for _, c := range p.Commits {
		size += commitSize(c)
	}
	return &historyEntry{key: key, page: p, size: size}
}

func commitSize(c domain.Commit) int64 {
	n := 384 + len(c.Parents)*48 + len(c.Summary) + len(c.Message)
	n += len(c.Author.Name) + len(c.Author.Email) + len(c.Committer.Name) + len(c.Committer.Email)
	return int64(n)
}

func cloneCommit(c domain.Commit) domain.Commit {
	c.Parents = slices.Clone(c.Parents)
	return c
}

func cloneTree(t domain.Tree) domain.Tree {
	t.Entries = slices.Clone(t.Entries)
	for i := range t.Entries {
		if lc := t.Entries[i].LastCommit; lc != nil {
			c := cloneCommit(*lc)
			t.Entries[i].LastCommit = &c
		}
	}
	return t
}

func clonePage(p ports.CommitPage) ports.CommitPage {
	p.Commits = slices.Clone(p.Commits)
	for i := range p.Commits {
		p.Commits[i] = cloneCommit(p.Commits[i])
	}
	return p
}
