// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"cmp"
	"slices"
	"time"
)

// DepsQueueEntry is a batch waiting in Bernard's queue: Queued (its server
// step, the draft or the server-side commit, is still to do) or Drafted (its
// draft is pushed, it waits for a dependency run slot).
type DepsQueueEntry struct {
	Repo     RepoName
	Key      DepBatchKey
	State    DepsState
	Manual   bool
	QueuedAt time.Time
}

// Queued reports whether the batch waits in the queue.
func (b DepBatch) Queued() bool { return b.State == DepsQueued || b.State == DepsDrafted }

// DepsQueueOrder lists the waiting batches of recs in queue order: manual ones
// first, then the oldest QueuedAt, then repository and key for a stable order.
func DepsQueueOrder(recs []RepoDeps) []DepsQueueEntry {
	var out []DepsQueueEntry
	for _, d := range recs {
		for _, b := range d.Batches {
			if b.Queued() {
				out = append(out, DepsQueueEntry{Repo: d.Repo, Key: b.Key, State: b.State, Manual: b.Manual, QueuedAt: b.QueuedAt})
			}
		}
	}
	slices.SortStableFunc(out, func(a, b DepsQueueEntry) int {
		if a.Manual != b.Manual {
			if a.Manual {
				return -1
			}
			return 1
		}
		return cmp.Or(a.QueuedAt.Compare(b.QueuedAt), cmp.Compare(a.Repo.String(), b.Repo.String()), cmp.Compare(a.Key, b.Key))
	})
	return out
}

// DepsRunning counts the batches of recs applying a dependency run.
func DepsRunning(recs []RepoDeps) int {
	n := 0
	for _, d := range recs {
		for _, b := range d.Batches {
			if b.State == DepsApplying {
				n++
			}
		}
	}
	return n
}

// NextDepsWork picks the next queue step, in queue order. A Queued batch
// always may take its server step (no run slot). A Drafted batch takes a run
// slot when fewer than limit runs are running and, for an automatic batch,
// when no normal workflow job waits for a runner (normal CI goes first).
func NextDepsWork(recs []RepoDeps, running, limit int, workflowsWaiting bool) (RepoName, DepBatchKey, bool) {
	for _, e := range DepsQueueOrder(recs) {
		switch {
		case e.State == DepsQueued:
			return e.Repo, e.Key, true
		case running < limit && (e.Manual || !workflowsWaiting):
			return e.Repo, e.Key, true
		}
	}
	return RepoName{}, "", false
}

// DepsQueuePosition is the position of the batch key of repo in order, 1 for
// the next; zero when it does not wait.
func DepsQueuePosition(order []DepsQueueEntry, repo RepoName, key DepBatchKey) int {
	for i, e := range order {
		if e.Repo == repo && e.Key == key {
			return i + 1
		}
	}
	return 0
}
