// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// depsQueueTick is how often the queue is drained without a wake-up: the
// safety net for an event missed.
const depsQueueTick = time.Minute

// maxDrainSteps bounds one drain: every step moves a batch out of the state
// it was picked in, so a drain that does more is stuck on one.
const maxDrainSteps = 256

// errQueueChanged fails a manual batch whose versions changed while it waited.
var errQueueChanged = fmt.Errorf("%w: the pending dependency updates changed, reload and retry", domain.ErrPreconditionFailed)

// wakeQueue asks the queue loop for a drain; it never blocks.
func (b *Bernard) wakeQueue() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// queueLoop drains the queue at start (what a previous process left Queued or
// Drafted), at each wake-up and every depsQueueTick, until Bernard stops.
func (b *Bernard) queueLoop() {
	defer b.wg.Done()
	t := time.NewTicker(depsQueueTick)
	defer t.Stop()
	for {
		b.drain(b.ctx)
		select {
		case <-b.ctx.Done():
			return
		case <-b.wake:
		case <-t.C:
		}
	}
}

// drain takes queue steps, in the order of domain.NextDepsWork, until none
// can be taken. A Queued batch takes its server step (the draft, or the
// commit of server-side updates); a Drafted batch starts its run when a slot
// is free. Steps run one at a time, so a slot is never given twice.
func (b *Bernard) drain(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			ctxLog(ctx).Error().Interface("panic", r).Bytes("stack", debug.Stack()).Msg("bernard: the queue panicked")
		}
	}()
	b.drainMu.Lock()
	defer b.drainMu.Unlock()
	type step struct {
		repo  domain.RepoName
		key   domain.DepBatchKey
		state domain.DepsState
	}
	var last step
	for range maxDrainSteps {
		if ctx.Err() != nil {
			return
		}
		recs, err := b.cfg.Store.List(ctx)
		if err != nil {
			ctxLog(ctx).Error().Err(err).Msg("bernard: list the records of the queue")
			return
		}
		// The ready jobs are read only when an automatic batch would take a
		// slot: what the queue does without them is decided first.
		running := domain.DepsRunning(recs)
		repo, key, ok := domain.NextDepsWork(recs, running, b.cfg.Concurrency, true)
		if !ok {
			if repo, key, ok = domain.NextDepsWork(recs, running, b.cfg.Concurrency, false); !ok || b.workflowsWaiting(ctx) {
				return
			}
		}
		state := domain.DepsQueued
		for _, d := range recs {
			if d.Repo == repo {
				if batch, _, ok := d.Batch(key); ok {
					state = batch.State
				}
			}
		}
		if (step{repo, key, state}) == last {
			ctxLog(ctx).Error().Str("repo", repo.String()).Str("batch", string(key)).Msg("bernard: a queue step left its batch where it was")
			return
		}
		last = step{repo, key, state}
		b.stepErr = b.queueStep(ctx, repo, key)
	}
}

// workflowsWaiting reports whether a normal (non-dependency) workflow job
// waits for a runner: automatic dependency runs let it go first. Every ready
// job is looked at (a limit would hide normal jobs behind dependency ones);
// when they cannot be read, the automatic run waits too.
func (b *Bernard) workflowsWaiting(ctx context.Context) bool {
	if b.cfg.Jobs == nil {
		return false
	}
	jobs, err := b.cfg.Jobs.ListReadyJobs(ctx, 0)
	if err != nil {
		ctxLog(ctx).Error().Err(err).Msg("bernard: list the jobs waiting for a runner")
		return true
	}
	return slices.ContainsFunc(jobs, func(j domain.CIJob) bool { return j.Deps == nil })
}

// queueStep takes the step of the batch key of repo, under the repository
// lock. A failure is recorded on the batch and returned.
func (b *Bernard) queueStep(ctx context.Context, repo domain.RepoName, key domain.DepBatchKey) error {
	defer b.repoLock(repo)()
	d, err := b.cfg.Store.Get(ctx, repo)
	if err != nil {
		ctxLog(ctx).Error().Err(err).Str("repo", repo.String()).Msg("bernard: read the record of a queued batch")
		return err
	}
	batch, _, ok := d.Batch(key)
	if !ok {
		return nil
	}
	switch batch.State {
	case domain.DepsQueued:
		err = b.serverStep(ctx, repo, batch)
	case domain.DepsDrafted:
		err = b.runStep(ctx, d, batch)
	default:
		return nil
	}
	switch {
	case err == nil, errors.Is(err, errRecordMoved):
	case errors.Is(err, errBranchAdopted):
		b.markAdopted(ctx, repo, key, true)
	default:
		b.fail(ctx, repo, key, err)
	}
	return err
}

// serverStep checks the batch against what the owner accepted (a manual one
// fails when its versions changed; an automatic one takes the current ones),
// then writes the draft of a batch with runner-side updates (it goes Drafted)
// or publishes a batch of server-side updates (it goes Idle).
func (b *Bernard) serverStep(ctx context.Context, repo domain.RepoName, batch domain.DepBatch) error {
	if batch.Adopted {
		return errBranchAdopted
	}
	if len(batch.Updates) == 0 {
		return fmt.Errorf("%w: no dependency update is pending", domain.ErrPreconditionFailed)
	}
	if batch.Manual && len(batch.RunUpdates) > 0 && !domain.SameRefs(refsOf(batch.RunUpdates), batch.Refs()) {
		return errQueueChanged
	}
	runnerSide := false
	for _, u := range batch.Updates {
		eco := b.ecosystem(u.Ecosystem)
		if eco == nil {
			return fmt.Errorf("%w: unknown ecosystem %q", domain.ErrPreconditionFailed, u.Ecosystem)
		}
		runnerSide = runnerSide || !eco.ApplyInServer()
	}
	expected := batch.Refs()
	if runnerSide {
		_, err := b.writeDraft(ctx, repo, batch, expected)
		return err
	}
	_, err := b.apply(ctx, repo, batch, func(r domain.RepoDeps) error { return movedPrecondition(r, batch.Key, expected) })
	return err
}

// runStep starts the run of a Drafted batch. Its draft must still hold the
// batch's versions: otherwise a manual batch fails and an automatic one goes
// back to Queued for a new draft.
func (b *Bernard) runStep(ctx context.Context, d domain.RepoDeps, batch domain.DepBatch) error {
	// A human's branch gets no run: the batch settles Idle, its merge
	// request description says why.
	if batch.Adopted {
		return errBranchAdopted
	}
	if !batch.Head.IsZero() {
		cur, err := b.cfg.Git.BranchHead(ctx, d.Repo, batch.BranchName())
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("read %s: %w", batch.BranchName(), err)
		}
		if cur != batch.Head {
			return errBranchAdopted
		}
	}
	if !domain.SameRefs(refsOf(batch.MRUpdates), batch.Refs()) {
		if batch.Manual {
			return errQueueChanged
		}
		_, err := b.cfg.Store.Mutate(ctx, d.Repo, func(r *domain.RepoDeps) (bool, error) {
			_, i, ok := r.Batch(batch.Key)
			if !ok || r.Batches[i].State != domain.DepsDrafted {
				return false, errRecordMoved
			}
			r.Batches[i].State = domain.DepsQueued
			return true, nil
		})
		return err
	}
	_, err := b.startRun(ctx, d, batch.Key)
	return err
}

func refsOf(ups []domain.DepUpdate) []domain.DepRef {
	out := make([]domain.DepRef, len(ups))
	for i, u := range ups {
		out[i] = u.DepRef
	}
	return out
}

// Queue returns the state of the dependency run queue and its order.
func (b *Bernard) Queue(ctx context.Context, p domain.Principal) (ports.DepsQueue, []domain.DepsQueueEntry, error) {
	if !p.HasOwnerRights() {
		return ports.DepsQueue{}, nil, domain.ErrNotFound
	}
	recs, err := b.cfg.Store.List(ctx)
	if err != nil {
		return ports.DepsQueue{}, nil, fmt.Errorf("list the dependency records: %w", err)
	}
	return depsQueueOf(recs, b.cfg.Concurrency)
}

// depsQueueOf summarizes the queue of recs under the run limit.
func depsQueueOf(recs []domain.RepoDeps, limit int) (ports.DepsQueue, []domain.DepsQueueEntry, error) {
	order := domain.DepsQueueOrder(recs)
	return ports.DepsQueue{Running: domain.DepsRunning(recs), Limit: limit, Queued: len(order)}, order, nil
}
