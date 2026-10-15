// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"sync"

	"bnema.dev/ermite/internal/domain"
)

// maxPullFileLists bounds the file lists the files endpoint keeps.
const maxPullFileLists = 16

// pullFilesKey names a diff by the commits it is between: it never changes, so
// a cached list is never stale.
type pullFilesKey struct {
	repo       domain.RepoName
	base, head domain.ObjectID
}

// pullFilesCache keeps the last file lists computed by the files endpoint, so
// that the pages of one merge request (paths-filter reads them all, 100 files
// at a time) do not each run the whole diff again. A small least recently used
// cache: entries are the lists of at most 3000 files, and there are at most
// maxPullFileLists of them.
type pullFilesCache struct {
	mu    sync.Mutex
	items map[pullFilesKey][]domain.FileDiff
	order []pullFilesKey // oldest use first
}

func (c *pullFilesCache) get(k pullFilesKey) ([]domain.FileDiff, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	files, ok := c.items[k]
	if ok {
		c.touch(k)
	}
	return files, ok
}

func (c *pullFilesCache) put(k pullFilesKey, files []domain.FileDiff) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = map[pullFilesKey][]domain.FileDiff{}
	}
	if _, ok := c.items[k]; !ok && len(c.items) >= maxPullFileLists {
		delete(c.items, c.order[0])
		c.order = c.order[1:]
	}
	c.items[k] = files
	c.touch(k)
}

// touch makes k the most recently used. The caller holds mu.
func (c *pullFilesCache) touch(k pullFilesKey) {
	for i, o := range c.order {
		if o == k {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	c.order = append(c.order, k)
}
