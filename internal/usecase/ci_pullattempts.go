// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"sync"

	"bnema.dev/ermite/internal/domain"
)

// DefaultCIPullAttempts bounds the merge requests whose last conflicting merge
// is remembered.
const DefaultCIPullAttempts = 4096

// pullAttempt is the pair of tips a merge was tried on.
type pullAttempt struct{ base, head domain.ObjectID }

// pullAttempts remembers, per merge request, the tips of the last test merge
// that conflicted. A merge request whose head conflicts has no merge ref, so it
// never looks up to date to a rescan: without this memory every rescan would try
// the same merge again, for ever. A rescan skips a merge request while its tips
// are the remembered ones, and tries it again as soon as one moves. Only
// conflicts are remembered: a merge that went through is judged by its refs.
//
// A least recently used cache bounded by max: a conflict that was forgotten is
// tried once more by the next rescan, never more often than that.
type pullAttempts struct {
	mu    sync.Mutex
	max   int
	items map[pullKey]pullAttempt
	order []pullKey // oldest use first
}

func newPullAttempts(max int) *pullAttempts {
	if max <= 0 {
		max = DefaultCIPullAttempts
	}
	return &pullAttempts{max: max, items: map[pullKey]pullAttempt{}}
}

// tried reports whether the last conflict of k was on these tips.
func (a *pullAttempts) tried(k pullKey, base, head domain.ObjectID) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	at, ok := a.items[k]
	if ok {
		a.touch(k)
	}
	return ok && at.base == base && at.head == head
}

// conflicted remembers that the merge of head into base conflicted.
func (a *pullAttempts) conflicted(k pullKey, base, head domain.ObjectID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.items[k]; !ok && len(a.items) >= a.max {
		delete(a.items, a.order[0])
		a.order = a.order[1:]
	}
	a.items[k] = pullAttempt{base: base, head: head}
	a.touch(k)
}

// merged forgets k: its last merge went through.
func (a *pullAttempts) merged(k pullKey) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.items[k]; !ok {
		return
	}
	delete(a.items, k)
	for i, o := range a.order {
		if o == k {
			a.order = append(a.order[:i], a.order[i+1:]...)
			break
		}
	}
}

// touch makes k the most recently used. The caller holds mu.
func (a *pullAttempts) touch(k pullKey) {
	for i, o := range a.order {
		if o == k {
			a.order = append(a.order[:i], a.order[i+1:]...)
			break
		}
	}
	a.order = append(a.order, k)
}
