// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"fmt"
	"slices"
	"time"

	"bnema.dev/ermite/internal/domain"
)

// SetAutoOpen turns the automatic merge request of the repository's base
// batch on or off. Turning it on queues the base batch at once when it is due
// (autoEnqueue).
func (b *Bernard) SetAutoOpen(ctx context.Context, p domain.Principal, repo domain.RepoName, on bool) (domain.RepoDeps, error) {
	if !p.HasOwnerRights() {
		return domain.RepoDeps{}, domain.ErrNotFound
	}
	r, err := lookupRepo(ctx, b.cfg.Meta, repo)
	if err != nil {
		return domain.RepoDeps{}, err
	}
	defer b.repoLock(repo)()
	queued := false
	rec, err := b.cfg.Store.Mutate(ctx, repo, func(d *domain.RepoDeps) (bool, error) {
		d.Repo, d.AutoOpenDisabled = repo, !on
		queued = on && !r.Archived && b.autoEnqueue(d, b.cfg.Clock.Now())
		return true, nil
	})
	if err != nil {
		return domain.RepoDeps{}, fmt.Errorf("set the automatic merge requests: %w", err)
	}
	b.changed(ctx, repo)
	if queued {
		b.wakeQueue()
	}
	return rec, nil
}

// autoWindowDue reports whether the repository has an unused opening window.
// The first check consumes a window even if no batch can be queued.
func (b *Bernard) autoWindowDue(d domain.RepoDeps, now time.Time) bool {
	if d.AutoOpenWindowAt.IsZero() {
		return true
	}
	next, ok := b.autoCron.Next(d.AutoOpenWindowAt)
	return ok && !next.After(now)
}

// autoEnqueue queues the base batch of d as an automatic batch when Bernard
// opens merge requests by itself (cfg.AutoOpen and the repository's setting),
// the repository is not snoozed and the batch is due: pending updates, no
// merge request or an outdated one, a branch Bernard still owns, nothing in
// progress, and not the same failure again (a failed batch is queued again
// only once its versions changed). Majors are never queued automatically.
// It reports whether it queued the batch. The caller saves d; an archived
// repository is never checked, so never reaches it.
func (b *Bernard) autoEnqueue(d *domain.RepoDeps, now time.Time) bool {
	if !b.cfg.AutoOpen || d.AutoOpenDisabled || d.SnoozedUntil.After(now) || !b.canOpen() {
		return false
	}
	_, i, ok := d.Batch("")
	if !ok {
		return false
	}
	batch := &d.Batches[i]
	switch {
	case len(batch.Updates) == 0, batch.Adopted:
		return false
	case batch.State == domain.DepsApplying, batch.Queued():
		return false
	case batch.MR != 0 && !batch.Outdated():
		return false
	case batch.State == domain.DepsFailed && domain.SameRefs(failedRefs(*batch), batch.Refs()):
		return false
	}
	batch.State, batch.QueuedAt, batch.Manual = domain.DepsQueued, now, false
	batch.Error, batch.Repro, batch.RunUpdates = "", nil, slices.Clone(batch.Updates)
	return true
}

// failedRefs are the versions a failed batch tried: those the queue took
// (RunUpdates, kept by a failure before the run) or, once a run ended, those
// of its draft.
func failedRefs(b domain.DepBatch) []domain.DepRef {
	if len(b.RunUpdates) > 0 {
		return refsOf(b.RunUpdates)
	}
	return refsOf(b.MRUpdates)
}
