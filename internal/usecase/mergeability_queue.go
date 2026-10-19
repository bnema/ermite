// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"sync"

	"bnema.dev/ermite/internal/domain"
)

// mergeKey names one open merge request.
type mergeKey struct {
	repo   domain.RepoName
	number int64
}

// mergeQueue is the bounded work queue of the mergeability checks: a key
// waits at most once (pending), is checked by one worker at a time (running),
// and a key queued while it runs is checked again afterwards (again). When
// the queue is full the repository is marked dirty instead: a worker then
// rescans all its open merge requests.
type mergeQueue struct {
	mu         sync.Mutex
	max        int
	pending    map[mergeKey]bool
	order      []mergeKey
	running    map[mergeKey]bool
	again      map[mergeKey]bool
	dirty      map[domain.RepoName]bool
	dirtyOrder []domain.RepoName
	// seen holds the keys a reader answered "checking" for: the result of
	// their next check is published whatever it is.
	seen map[mergeKey]bool
	wake chan struct{}
}

func newMergeQueue(max int) *mergeQueue {
	return &mergeQueue{
		max: max, pending: map[mergeKey]bool{}, running: map[mergeKey]bool{}, again: map[mergeKey]bool{},
		dirty: map[domain.RepoName]bool{}, seen: map[mergeKey]bool{}, wake: make(chan struct{}, 1),
	}
}

func (q *mergeQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// add queues a check. The caller does not hold mu.
func (q *mergeQueue) add(k mergeKey) {
	q.mu.Lock()
	switch {
	case q.running[k]:
		q.again[k] = true
	case q.pending[k]:
	case len(q.pending) < q.max:
		q.pending[k] = true
		q.order = append(q.order, k)
	default:
		q.markDirtyLocked(k.repo)
	}
	q.mu.Unlock()
	q.signal()
}

func (q *mergeQueue) markDirty(repo domain.RepoName) {
	q.mu.Lock()
	q.markDirtyLocked(repo)
	q.mu.Unlock()
	q.signal()
}

func (q *mergeQueue) markDirtyLocked(repo domain.RepoName) {
	if !q.dirty[repo] {
		q.dirty[repo] = true
		q.dirtyOrder = append(q.dirtyOrder, repo)
	}
}

// next takes the next check, which it marks running, or the next dirty
// repository to rescan; pending checks first.
func (q *mergeQueue) next() (key *mergeKey, dirty *domain.RepoName) {
	q.mu.Lock()
	defer q.mu.Unlock()
	switch {
	case len(q.order) > 0:
		k := q.order[0]
		q.order = q.order[1:]
		delete(q.pending, k)
		q.running[k] = true
		key = &k
	case len(q.dirtyOrder) > 0:
		r := q.dirtyOrder[0]
		q.dirtyOrder = q.dirtyOrder[1:]
		delete(q.dirty, r)
		dirty = &r
	}
	if len(q.order) > 0 || len(q.dirtyOrder) > 0 {
		q.signal() // let another worker take the rest
	}
	return key, dirty
}

// tryRun marks k running for a rescan; when it already runs it asks for
// another pass instead and reports false.
func (q *mergeQueue) tryRun(k mergeKey) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.running[k] {
		q.again[k] = true
		return false
	}
	delete(q.pending, k) // a pending check is covered by this run
	q.running[k] = true
	return true
}

// finish ends a pass of a running key; it reports whether another is due.
func (q *mergeQueue) finish(k mergeKey) (again bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.again[k] {
		delete(q.again, k)
		return true
	}
	delete(q.running, k)
	return false
}

// markSeen records that a reader was told "checking" for k.
func (q *mergeQueue) markSeen(k mergeKey) {
	q.mu.Lock()
	q.seen[k] = true
	q.mu.Unlock()
}

// takeSeen reports and clears the seen flag.
func (q *mergeQueue) takeSeen(k mergeKey) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	s := q.seen[k]
	delete(q.seen, k)
	return s
}
