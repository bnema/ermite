// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"bnema.dev/ermite-actions/expr"
	"bnema.dev/ermite-actions/template"
	"bnema.dev/ermite-actions/workflow"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Concurrency groups (CI-3 phase E). A group is per repository and compared
// without regard to case; a run (workflow level) or a job (job level) is an
// entry. With queue: single, at most one entry runs and one waits: a newer
// waiting entry cancels the older one, and cancel-in-progress also cancels the
// running one. An entry holds its group from the moment it may run until the
// run or job has ended.
//
// The record of a group lives under "concur/<repo>/<key(group)>" and is
// changed with a version-checked write (Store.UpdateConcurrency). Cancelling
// another entry needs the lock of another run, which the caller of a change
// does not hold in order (two runs would wait for each other): those effects
// are queued with later and run after the lock is released.

// errEmptyGroup and errLongGroup are GitHub's errors for a group that
// evaluates to nothing or too much (actions/runner,
// WorkflowTemplateConverter.cs ConvertToConcurrency, lines 679-687).
var (
	errEmptyGroup = errors.New("concurrency group name cannot be empty")
	// errQueueMaxCancel is the message of languageservices
	// (languageservice/src/validate.ts:743).
	errQueueMaxCancel = errors.New("'queue: max' cannot be combined with 'cancel-in-progress: true'.") //nolint:staticcheck // ST1005: the text of languageservices, kept as is
	errLongGroup      = fmt.Errorf("concurrency group name must be less than %d characters", domain.CIMaxConcurrencyGroup)
)

// evalConcurrency evaluates the "concurrency:" token of a workflow or a job:
// a string (or an expression) is the group; a mapping has group,
// cancel-in-progress (an expression is allowed) and queue. An empty group is
// an error, as in GitHub's runner; queue: max is not supported yet.
func evalConcurrency(ctx context.Context, tok template.Token, env template.Env) (domain.CIConcurrency, error) {
	var out domain.CIConcurrency
	if tok == nil {
		return out, nil
	}
	groupTok := tok
	if m, ok := tok.(*template.Mapping); ok {
		if q := m.Find("queue"); q != nil {
			queue, err := template.EvaluateString(ctx, q, env)
			if err != nil {
				return out, err
			}
			if queue == "max" {
				// languageservices, languageservice/src/validate.ts:702-743: the
				// literal pair is invalid (expressions are not checked there).
				if b, ok := m.Find("cancel-in-progress").(*template.Boolean); ok && b.Value {
					return out, errQueueMaxCancel
				}
				return out, errors.New("not supported yet: queue: max")
			}
		}
		if groupTok = m.Find("group"); groupTok == nil {
			return out, errEmptyGroup
		}
		if c := m.Find("cancel-in-progress"); c != nil {
			cancel, err := template.EvaluateBool(ctx, c, env)
			if err != nil {
				return out, err
			}
			out.Cancel = cancel
		}
	}
	group, err := template.EvaluateString(ctx, groupTok, env)
	switch {
	case err != nil:
		return out, err
	case group == "":
		return out, errEmptyGroup
	case domain.ConcurrencyGroupLength(group) > domain.CIMaxConcurrencyGroup:
		return out, errLongGroup
	}
	out.Group = group
	return out, nil
}

// later runs fn in the background, after the lock of the caller's run is
// released. Close waits for it.
func (c *CI) later(what string, fn func(ctx context.Context) error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		c.guard(what, func() {
			if err := fn(c.ctx); err != nil && c.ctx.Err() == nil && !errors.Is(err, domain.ErrNotFound) {
				ctxLog(c.ctx).Error().Err(err).Str("worker", what).Msg("ci concurrency")
			}
		})
	}()
}

// entryAlive reports whether the run or job of an entry can still hold or want
// its group: it exists, and it has not ended (a run: it is on the attempt of the
// entry and has not completed). Run ids are never reused, so a missing run or
// job is gone for good. A read that fails says alive: nothing is dropped on a
// doubt.
func (c *CI) entryAlive(ctx context.Context, repo domain.RepoName, e domain.CIConcurrencyEntry) bool {
	if e.JobID == "" {
		run, err := c.cfg.Store.GetRun(ctx, repo, e.RunID)
		if err != nil {
			return !errors.Is(err, domain.ErrNotFound)
		}
		return run.Attempt == e.Attempt && run.Status != domain.CICompleted
	}
	j, err := c.cfg.Store.GetJob(ctx, e.RunID, e.Attempt, e.JobID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		// A matrix leg that took its group is written with the flush that
		// follows: without its record yet, it lives as long as its run's attempt.
		return c.entryAlive(ctx, repo, domain.CIConcurrencyEntry{RunID: e.RunID, Attempt: e.Attempt})
	case err != nil:
		return true
	}
	return !j.Ended()
}

// mark sets the mark of an entry.
func mark(st *domain.CIConcurrencyGroupState, e domain.CIConcurrencyEntry, group string, state domain.CIConcurrencyState) {
	if st.Marks == nil {
		st.Marks = map[string]domain.CIConcurrencyMark{}
	}
	st.Marks[e.Key()] = domain.CIConcurrencyMark{Entry: e, Group: group, State: state}
}

// promote gives the running place to the waiting entry when it is free.
func promote(st *domain.CIConcurrencyGroupState, group string) {
	if st.Record.Running == nil && st.Record.Pending != nil {
		st.Record.Running, st.Record.Pending = st.Record.Pending, nil
		m := st.Marks[st.Record.Running.Key()]
		mark(st, *st.Record.Running, firstNonEmpty(m.Group, group), domain.CIConcurrencyHolding)
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// prune brings a group back to its invariant, in the transaction that is about to
// decide on it: the entries whose run or job is gone or over leave the record
// and lose their marks (a crash between the write of a job and the release of
// its group, a run that was deleted); the waiting entry takes a free running
// place; an entry named by the record has its mark; and an entry marked as
// pending or holding that the record does not name any more is marked replaced
// (it should not happen, the batch writes both, and it is cancelled if it does).
func (c *CI) prune(ctx context.Context, repo domain.RepoName, group string, st *domain.CIConcurrencyGroupState) (more bool) {
	rec := &st.Record
	if rec.Running != nil && !c.entryAlive(ctx, repo, *rec.Running) {
		delete(st.Marks, rec.Running.Key())
		rec.Running = nil
	}
	if rec.Pending != nil && !c.entryAlive(ctx, repo, *rec.Pending) {
		delete(st.Marks, rec.Pending.Key())
		rec.Pending = nil
	}
	promote(st, group)
	if rec.Running != nil {
		if m, ok := st.Marks[rec.Running.Key()]; !ok || m.State != domain.CIConcurrencyHolding {
			mark(st, *rec.Running, firstNonEmpty(m.Group, group), domain.CIConcurrencyHolding)
		}
	}
	if rec.Pending != nil {
		if m, ok := st.Marks[rec.Pending.Key()]; !ok || m.State != domain.CIConcurrencyPending {
			mark(st, *rec.Pending, firstNonEmpty(m.Group, group), domain.CIConcurrencyPending)
		}
	}
	// The other marks are pruned a bounded number per transaction, in key order:
	// a batch has a size limit, and a group with a long backlog of stale marks
	// must not wedge on one transaction that deletes them all. What is left is
	// reported, and the next decision or the reconciliation takes the next ones.
	left := c.cfg.ConcurrencyPruneBatch
	for _, k := range slices.Sorted(maps.Keys(st.Marks)) {
		m := st.Marks[k]
		var prune, replace bool
		switch {
		case !c.entryAlive(ctx, repo, m.Entry):
			prune = true
		case m.State != domain.CIConcurrencyReplaced && !rec.Names(m.Entry):
			replace = true
		default:
			continue
		}
		if left <= 0 {
			return true
		}
		left--
		if prune {
			delete(st.Marks, k)
		} else if replace {
			m.State = domain.CIConcurrencyReplaced
			st.Marks[k] = m
		}
	}
	return false
}

// updateGroup is Store.UpdateConcurrency over the pruned group. It returns the
// entry that holds the group after the change and did not before (the waiting
// entry that was given the turn), or nil: its run is to be planned again.
func (c *CI) updateGroup(ctx context.Context, repo domain.RepoName, group string, fn func(*domain.CIConcurrencyGroupState) error) (*domain.CIConcurrencyEntry, error) {
	promoted, _, err := c.updateGroupMore(ctx, repo, group, fn)
	return promoted, err
}

// updateGroupMore is updateGroup that also says whether the pruning stopped at
// its bound, with stale marks left for another transaction.
func (c *CI) updateGroupMore(ctx context.Context, repo domain.RepoName, group string, fn func(*domain.CIConcurrencyGroupState) error) (promoted *domain.CIConcurrencyEntry, more bool, err error) {
	err = c.cfg.Store.UpdateConcurrency(ctx, repo, group, func(st *domain.CIConcurrencyGroupState) error {
		promoted = nil
		var was *domain.CIConcurrencyEntry
		if r := st.Record.Running; r != nil {
			e := *r
			was = &e
		}
		more = c.prune(ctx, repo, group, st)
		if err := fn(st); err != nil {
			return err
		}
		if r := st.Record.Running; r != nil && (was == nil || !was.Same(*r)) {
			e := *r
			promoted = &e
		}
		return nil
	})
	return promoted, more, err
}

// concurrencyRetries bounds the retries of a delivery that failed; the delay
// doubles from the watchdog tick up to maxConcurrencyBackoff.
const (
	defaultConcurrencyRetries = 8
	maxConcurrencyBackoff     = 30 * time.Second
)

// retry calls fn until it succeeds, at most concurrencyRetries more times, with
// a doubling delay that ends with ctx. A domain.ErrNotFound is not retried.
func (c *CI) retry(ctx context.Context, fn func() error) error {
	delay := c.cfg.Tick
	err := fn()
	retries := c.cfg.ConcurrencyRetries
	switch {
	case retries == 0:
		retries = defaultConcurrencyRetries
	case retries < 0:
		retries = 0
	}
	for range retries {
		if err == nil || errors.Is(err, domain.ErrNotFound) {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(delay):
		}
		delay = min(delay*2, maxConcurrencyBackoff)
		err = fn()
	}
	return err
}

// wake plans again the run of an entry that was given its group, outside any
// run lock, and again if that failed. A run that is gone passes the group on.
// What the retries do not reach, the reconciliation (startup, and every
// minute) does: it advances every holder whose own record still says pending.
func (c *CI) wake(repo domain.RepoName, group string, e domain.CIConcurrencyEntry) {
	c.later("concurrency promote", func(ctx context.Context) error {
		return c.retry(ctx, func() error {
			err := c.advanceRun(ctx, e.RunID)
			if errors.Is(err, domain.ErrNotFound) {
				return c.releaseEntry(ctx, repo, group, e)
			}
			return err
		})
	})
}

// cancelLater cancels an entry that a newer one replaced, outside any run lock,
// and again if that failed. The decision that replaced it is in the group (the
// entry is marked replaced in the batch that unnamed it, or stays named with
// Canceling set): the reconciliation finds it from there if this cancellation is
// lost (crash, retries used up).
func (c *CI) cancelLater(group string, e domain.CIConcurrencyEntry) {
	c.later("concurrency cancel", func(ctx context.Context) error {
		return c.retry(ctx, func() error { return c.cancelEntry(ctx, e, group) })
	})
}

// entryNeeds reports whether the entry named by its group still has to be told
// where it stands: its own record does not say want yet (a promotion whose
// planning failed or was lost, or a crash between the write of the group and the
// write of the entry). An entry that is gone or over is the pruning's business.
func (c *CI) entryNeeds(ctx context.Context, repo domain.RepoName, e domain.CIConcurrencyEntry, want domain.CIConcurrencyState) bool {
	if e.JobID == "" {
		run, err := c.cfg.Store.GetRun(ctx, repo, e.RunID)
		return err == nil && run.Status != domain.CICompleted && run.Attempt == e.Attempt && run.Concurrency.State != want
	}
	j, err := c.cfg.Store.GetJob(ctx, e.RunID, e.Attempt, e.JobID)
	if errors.Is(err, domain.ErrNotFound) {
		// A leg the group names whose record is not written: the run has to be
		// planned, which expands its matrix, while its attempt is open.
		run, err := c.cfg.Store.GetRun(ctx, repo, e.RunID)
		return err == nil && run.Status != domain.CICompleted && run.Attempt == e.Attempt
	}
	return err == nil && !j.Ended() && j.Phase == domain.CIPhaseWaiting && j.Concurrency.State != want
}

// ReconcileConcurrency repairs the groups. It runs at startup and every minute.
// For each stored group it prunes the entries that are gone or over, asks again
// the running entry a cancel-in-progress entry replaced, cancels every entry
// marked replaced (their cancellation is owed), and plans the entries the record
// names whose own record does not know it yet.
//
// Cost, per pass: a scan of the stored groups and their marks, and for each
// group a version-checked transaction (a write only if something is repaired;
// at most CIConfig.ConcurrencyPruneBatch stale marks go in one, the group is
// taken again until none is left) plus a read of the run or job of each entry
// the record or the marks name. It grows with the groups in use, not with the
// open runs.
func (c *CI) ReconcileConcurrency(ctx context.Context) {
	log := ctxLog(ctx)
	groups, err := c.cfg.Store.ListConcurrency(ctx)
	if err != nil {
		log.Error().Err(err).Msg("ci: list the concurrency groups")
		return
	}
	for _, g := range groups {
		var st domain.CIConcurrencyGroupState
		var err error
		// One transaction prunes a bounded number of marks: go on until none is
		// left to prune (each pass removes some, so this ends).
		for more := true; more && err == nil && ctx.Err() == nil; {
			_, more, err = c.updateGroupMore(ctx, g.Repo, g.Group, func(cur *domain.CIConcurrencyGroupState) error {
				st = domain.CIConcurrencyGroupState{Marks: maps.Clone(cur.Marks)}
				if cur.Record.Running != nil {
					r := *cur.Record.Running
					st.Record.Running = &r
				}
				if cur.Record.Pending != nil {
					p := *cur.Record.Pending
					st.Record.Pending = &p
				}
				return nil
			})
		}
		if err != nil {
			if !errors.Is(err, domain.ErrNotFound) && ctx.Err() == nil {
				log.Error().Err(err).Str("group", g.Group).Msg("ci: repair a concurrency group")
			}
			continue
		}
		if r := st.Record.Running; r != nil {
			if r.Canceling {
				c.cancelLater(st.Marks[r.Key()].Group, *r)
			}
			if c.entryNeeds(ctx, g.Repo, *r, domain.CIConcurrencyHolding) {
				c.wake(g.Repo, g.Group, *r)
			}
		}
		if p := st.Record.Pending; p != nil && c.entryNeeds(ctx, g.Repo, *p, domain.CIConcurrencyPending) {
			c.wake(g.Repo, g.Group, *p)
		}
		for _, m := range st.Marks {
			if m.State == domain.CIConcurrencyReplaced {
				c.cancelLater(m.Group, m.Entry)
			}
		}
	}
}

// acquire asks for the turn of an entry in a group and returns where it stands:
// holding, pending behind the running entry, or replaced. The decision and the
// marks of every entry it touches are one batch (Store.UpdateConcurrency), so an
// entry is marked pending or holding if and only if the record names it.
//
// An entry marked replaced never acquires, whatever the state of the group
// (empty, or its waiting place free): it is owed a cancellation. Otherwise a
// pending entry stays so; a newer one replaces the waiting entry, which is
// marked replaced and cancelled, and with cancel-in-progress the running entry
// is cancelled too (it stays named, with Canceling set, until it has ended).
// The call is repeatable: an entry that holds or waits is told so again.
func (rs *runState) acquire(ctx context.Context, group string, e domain.CIConcurrencyEntry) (domain.CIConcurrencyState, error) {
	var state domain.CIConcurrencyState
	var victims []domain.CIConcurrencyEntry
	promoted, err := rs.c.updateGroup(ctx, rs.run.Repo, group, func(st *domain.CIConcurrencyGroupState) error {
		victims = nil
		rec := &st.Record
		if m, ok := st.Marks[e.Key()]; ok && m.State == domain.CIConcurrencyReplaced {
			state = domain.CIConcurrencyReplaced
			victims = append(victims, e) // its cancellation may have been lost
			return nil
		}
		switch {
		case rec.Running == nil:
			me := e
			rec.Running = &me
			if rec.Pending != nil && rec.Pending.Same(e) {
				rec.Pending = nil
			}
			mark(st, e, group, domain.CIConcurrencyHolding)
			state = domain.CIConcurrencyHolding
		case rec.Running.Same(e):
			state = domain.CIConcurrencyHolding
		case rec.Pending != nil && rec.Pending.Same(e):
			state = domain.CIConcurrencyPending
		default:
			state = domain.CIConcurrencyPending
			if rec.Pending != nil {
				old := *rec.Pending
				mark(st, old, group, domain.CIConcurrencyReplaced)
				victims = append(victims, old)
			}
			me := e
			rec.Pending = &me
			mark(st, e, group, domain.CIConcurrencyPending)
			if e.Cancel && !rec.Running.Canceling {
				run := *rec.Running
				run.Canceling = true
				rec.Running = &run
				victims = append(victims, run)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if promoted != nil && !promoted.Same(e) {
		rs.c.wake(rs.run.Repo, group, *promoted)
	}
	for _, v := range victims {
		rs.c.cancelLater(group, v)
	}
	if state == domain.CIConcurrencyReplaced {
		state = domain.CIConcurrencyPending // it shows as waiting until its cancellation lands
	}
	return state, nil
}

// releaseEntry takes an entry out of its group, because its run or job has
// ended, and gives the turn to the waiting entry, which is planned again. The
// mark of the entry goes in the same batch.
func (c *CI) releaseEntry(ctx context.Context, repo domain.RepoName, group string, e domain.CIConcurrencyEntry) error {
	promoted, err := c.updateGroup(ctx, repo, group, func(st *domain.CIConcurrencyGroupState) error {
		switch {
		case st.Record.Running != nil && st.Record.Running.Same(e):
			st.Record.Running = nil
			promote(st, group)
		case st.Record.Pending != nil && st.Record.Pending.Same(e):
			st.Record.Pending = nil
		}
		delete(st.Marks, e.Key())
		return nil
	})
	if err != nil || promoted == nil {
		return err
	}
	c.wake(repo, group, *promoted)
	return nil
}

// cancelEntry cancels a run or a job that a newer entry of the group
// replaced: it ends cancelled with GitHub's annotation, and a job a runner has
// gets the cancel request the owner's cancel sends.
func (c *CI) cancelEntry(ctx context.Context, e domain.CIConcurrencyEntry, group string) error {
	return c.withRun(ctx, e.RunID, e.Attempt, func(rs *runState) error {
		if group == "" { // no name in the mark: use the entry's own group
			group = rs.run.Concurrency.Group
			if j := rs.find(e.JobID); e.JobID != "" && j != nil {
				group = j.Concurrency.Group
			}
		}
		msg := domain.ConcurrencyCancelMessage(group)
		if e.JobID == "" {
			if !rs.latest() || rs.run.Status == domain.CICompleted || rs.run.CancelRequested {
				return nil
			}
			rs.run.CancelRequested, rs.run.CancelRequestedAt, rs.runDirty = true, c.now(), true
			rs.run.Concurrency.Preempted = true
			for i := range rs.jobs {
				if !rs.jobs[i].Ended() {
					rs.cancelForGroup(&rs.jobs[i], msg)
				}
			}
			return rs.flush(ctx)
		}
		j := rs.find(e.JobID)
		if j == nil || !j.Planned {
			// The group names a leg whose record the run has not written yet (the
			// flush that followed the planning was lost): the record found under
			// the id of leg 0 is the placeholder of the whole job, not the leg.
			// Plan the job first, so that every leg exists, and only then act on
			// the one that was targeted.
			if err := rs.advance(ctx); err != nil {
				return err
			}
			if j = rs.find(e.JobID); j == nil || !j.Planned {
				return rs.flush(ctx) // not plannable (yet): nothing to cancel, keep what advance did
			}
		}
		if j.Ended() || j.Concurrency.Preempted {
			return rs.flush(ctx) // over, or cancelled by an earlier delivery of the same item
		}
		rs.cancelForGroup(j, msg)
		if err := rs.advance(ctx); err != nil {
			return err
		}
		return rs.flush(ctx)
	})
}

// cancelForGroup ends a job a newer entry of its group replaced, or asks its
// runner to stop. The annotation is GitHub's.
func (rs *runState) cancelForGroup(j *domain.CIJob, msg string) {
	j.Concurrency.Preempted = true
	rs.annotations = append(rs.annotations, ports.CIStepAnnotations{
		Repo: j.Repo, Exec: logRef(*j), Step: 1,
		Items: []domain.CIAnnotation{{Level: domain.AnnotationWarning, Message: msg, Step: 1}},
	})
	if j.Active() {
		rs.requestCancel(j, domain.CICancelUser, msg)
		rs.touch(j)
		return
	}
	rs.end(j, domain.CICancelled, msg)
}

// acquireRun evaluates the workflow-level concurrency of the attempt once and
// asks for its turn. It returns whether the run may go on: a pending run
// plans nothing, and its jobs show the pending status.
func (rs *runState) acquireRun(ctx context.Context, wf *workflow.Workflow) (bool, error) {
	run := &rs.run
	if !slices.ContainsFunc(rs.jobs, func(j domain.CIJob) bool { return !j.Ended() }) {
		return true, nil // nothing left to run: the group is freed at the flush
	}
	if !run.Concurrency.Evaluated {
		conc, err := rs.evalRunConcurrency(ctx, wf)
		if err != nil {
			for i := range rs.jobs {
				if !rs.jobs[i].Ended() && !rs.jobs[i].Active() {
					rs.end(&rs.jobs[i], domain.CIFailure, err.Error())
				}
			}
			run.Concurrency, rs.runDirty = domain.CIConcurrency{Evaluated: true}, true
			return false, nil
		}
		conc.Evaluated = true
		run.Concurrency, rs.runDirty = conc, true
	}
	if run.Concurrency.Group == "" {
		return true, nil
	}
	st, err := rs.acquire(ctx, run.Concurrency.Group, domain.CIConcurrencyEntry{RunID: run.ID, Attempt: run.Attempt, Cancel: run.Concurrency.Cancel})
	if err != nil {
		return false, err
	}
	if st != run.Concurrency.State {
		run.Concurrency.State, rs.runDirty = st, true
	}
	want := run.Concurrency
	want.FromRun, want.Preempted = true, false
	for i := range rs.jobs {
		j := &rs.jobs[i]
		if j.Ended() || (j.Concurrency.Group != "" && !j.Concurrency.FromRun) {
			continue // a job with a group of its own shows that one
		}
		if j.Concurrency != want {
			j.Concurrency = want
			rs.touch(j)
		}
	}
	return st == domain.CIConcurrencyHolding, nil
}

func (rs *runState) evalRunConcurrency(ctx context.Context, wf *workflow.Workflow) (domain.CIConcurrency, error) {
	if wf.Concurrency == nil {
		return domain.CIConcurrency{}, nil
	}
	gh, err := rs.githubValue("", rs.run.Attempt)
	if err != nil {
		return domain.CIConcurrency{}, err
	}
	vars, err := rs.c.varsContext(ctx, rs.run.Repo)
	if err != nil {
		return domain.CIConcurrency{}, err
	}
	env := template.Env{Contexts: map[string]expr.Value{"github": gh, "inputs": expr.NewDictionary(), "vars": vars}}
	return evalConcurrency(ctx, wf.Concurrency, env)
}

// releaseEnded frees the groups of what ended in the write that was just
// applied: the jobs that hold one of their own, and the run itself when it
// completed.
func (rs *runState) releaseEnded(ctx context.Context, changed []int, runDone bool) {
	c := rs.c
	for _, i := range changed {
		j := rs.jobs[i]
		if !j.Ended() || j.Concurrency.Group == "" || j.Concurrency.FromRun {
			continue
		}
		e := domain.CIConcurrencyEntry{RunID: j.RunID, Attempt: j.RunAttempt, JobID: j.ID}
		if err := c.releaseEntry(ctx, rs.run.Repo, j.Concurrency.Group, e); err != nil && !errors.Is(err, domain.ErrNotFound) {
			ctxLog(ctx).Error().Err(err).Str("job_id", j.ID).Msg("ci: free a concurrency group")
		}
	}
	if runDone && rs.run.Concurrency.Group != "" {
		e := domain.CIConcurrencyEntry{RunID: rs.run.ID, Attempt: rs.run.Attempt}
		if err := c.releaseEntry(ctx, rs.run.Repo, rs.run.Concurrency.Group, e); err != nil && !errors.Is(err, domain.ErrNotFound) {
			ctxLog(ctx).Error().Err(err).Int64("run_id", rs.run.ID).Msg("ci: free a concurrency group")
		}
	}
}
