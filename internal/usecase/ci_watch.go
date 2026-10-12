// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"runtime/debug"
	"strconv"
	"time"

	"bnema.dev/ermite/internal/domain"
)

const sweepEvery = 24 * time.Hour

// advanceRun plans what can start in the latest attempt of a run.
func (c *CI) advanceRun(ctx context.Context, runID int64) error {
	return c.withRun(ctx, runID, 0, func(rs *runState) error {
		if err := rs.advance(ctx); err != nil {
			return err
		}
		return rs.flush(ctx)
	})
}

// resume plans again the runs that were open when the server stopped: jobs
// whose needs ended while it was down are planned now. Running jobs go on:
// their tokens are in the store, and the heartbeat deadline counts from the
// start of this process.
func (c *CI) resume() {
	ctx := c.ctx
	runs, err := c.cfg.Store.ListOpenRuns(ctx)
	if err != nil {
		ctxLog(ctx).Error().Err(err).Msg("ci: list the open runs")
		return
	}
	for _, r := range runs {
		if err := c.advanceRun(ctx, r.ID); err != nil && !errors.Is(err, domain.ErrNotFound) && ctx.Err() == nil {
			ctxLog(ctx).Error().Err(err).Int64("run_id", r.ID).Msg("ci: plan an open run again")
		}
	}
	c.wakeQueue()
}

// watchdog looks at the jobs in flight every tick, and sweeps the old runs
// once a day.
func (c *CI) watchdog() {
	defer c.wg.Done()
	tick := time.NewTicker(c.cfg.Tick)
	defer tick.Stop()
	lastSweep := time.Time{}
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-tick.C:
		}
		c.guard("watchdog", func() {
			c.checkActive(c.ctx)
			c.checkQueue(c.ctx)
			if now := c.now(); now.Sub(lastSweep) >= sweepEvery {
				lastSweep = now
				c.sweep(c.ctx)
			}
		})
	}
}

// guard runs fn and logs a panic instead of letting it end the worker.
func (c *CI) guard(what string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			ctxLog(c.ctx).Error().Interface("panic", r).Bytes("stack", debug.Stack()).Str("worker", what).Msg("ci worker panicked")
		}
	}()
	fn()
}

// checkActive applies the server's deadlines to the jobs given to runners:
// acknowledgement, heartbeat, time budget, and the grace after a cancel.
func (c *CI) checkActive(ctx context.Context) {
	jobs, err := c.cfg.Store.ListActiveJobs(ctx)
	if err != nil {
		ctxLog(ctx).Error().Err(err).Msg("ci: list the active jobs")
		return
	}
	now := c.now()
	for _, j := range jobs {
		if !c.due(j, now) {
			continue
		}
		err := c.withRun(ctx, j.RunID, j.RunAttempt, func(rs *runState) error {
			cur := rs.find(j.ID)
			if cur == nil || !cur.Active() {
				return nil
			}
			if !c.enforce(ctx, rs, cur, c.now()) {
				return nil
			}
			if err := rs.advance(ctx); err != nil {
				return err
			}
			return rs.flush(ctx)
		})
		if err != nil && !errors.Is(err, domain.ErrNotFound) && ctx.Err() == nil {
			ctxLog(ctx).Error().Err(err).Str("job_id", j.ID).Msg("ci: enforce a deadline")
		}
	}
}

// lastBeat is the time of the last sign of life of an active job: the later
// of its last heartbeat, its acknowledgement and the start of the server.
func (c *CI) lastBeat(j domain.CIJob) time.Time {
	c.mu.Lock()
	beat := c.heartbeats[jobRef(j)]
	c.mu.Unlock()
	for _, t := range []time.Time{j.AckedAt, c.boot} {
		if t.After(beat) {
			beat = t
		}
	}
	return beat
}

// due is a cheap test, on the listed copy, of whether a deadline may have
// passed.
func (c *CI) due(j domain.CIJob, now time.Time) bool {
	switch {
	case j.Phase == domain.CIPhaseAssigned:
		return now.Sub(c.assignedSince(j)) > c.cfg.AckTimeout
	case j.CancelReason != domain.CICancelNone:
		return now.Sub(j.CancelRequestedAt) > c.cfg.CancelGrace
	}
	return now.Sub(c.lastBeat(j)) > c.cfg.HeartbeatDeadline || now.After(c.deadline(j))
}

// assignedSince is when the acknowledgement of an assignment starts to count:
// its assignment, or the start of this process when that is later, so that a
// runner that could not reach a server that was down is not punished for it
// (the same grace as the heartbeat deadline).
func (c *CI) assignedSince(j domain.CIJob) time.Time {
	if c.boot.After(j.AssignedAt) {
		return c.boot
	}
	return j.AssignedAt
}

// deadline is when the job's time budget ends for the server: timeout-minutes
// from the assignment, plus the slack that lets the runner's own timeout win.
func (c *CI) deadline(j domain.CIJob) time.Time {
	return j.AssignedAt.Add(time.Duration(j.TimeoutMinutes) * time.Minute).Add(c.cfg.TimeoutSlack)
}

// enforce applies the deadlines to a job read under the lock of its run and
// reports whether it changed.
func (c *CI) enforce(ctx context.Context, rs *runState, j *domain.CIJob, now time.Time) bool {
	switch {
	case j.Phase == domain.CIPhaseAssigned && now.Sub(c.assignedSince(*j)) > c.cfg.AckTimeout:
		c.cancelAssignment(ctx, rs, j)
		return true
	case j.CancelReason != domain.CICancelNone && now.Sub(j.CancelRequestedAt) > c.cfg.CancelGrace:
		conclusion := domain.CICancelled
		if j.CancelReason == domain.CICancelTimeout {
			conclusion = domain.CITimedOut
		}
		rs.end(j, conclusion, "The runner "+j.RunnerName+" did not answer the cancellation request in time; the job was ended by the server.")
		return true
	case j.Phase == domain.CIPhaseRunning && now.Sub(c.lastBeat(*j)) > c.cfg.HeartbeatDeadline:
		rs.end(j, domain.CIFailure, domain.CILostMessage(j.RunnerName))
		return true
	case j.CancelReason == domain.CICancelNone && now.After(c.deadline(*j)):
		rs.requestCancel(j, domain.CICancelTimeout, "The job exceeded its timeout of "+itoa(int64(j.TimeoutMinutes))+" minutes")
		return true
	}
	return false
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// cancelAssignment takes a job back from a runner that did not acknowledge
// it: the token is revoked, what the assignment stored is dropped, and the job
// is queued again with a fresh token at its next assignment. Three in a row
// fail the job.
func (c *CI) cancelAssignment(ctx context.Context, rs *runState, j *domain.CIJob) {
	ctxLog(ctx).Warn().Str("job_id", j.ID).Str("runner", j.RunnerName).Msg("job assignment not acknowledged: cancelled")
	_ = c.cfg.Logs.Reset(ctx, logRef(*j))
	j.AssignCancels++
	j.TokenHash, j.TokenValidUntil = domain.TokenHash{}, time.Time{}
	j.RunnerID, j.RunnerName, j.Isolation = "", "", domain.CIIsolationNone
	j.Steps, j.Outputs, j.AssignedAt = nil, nil, time.Time{}
	c.forgetPoll(jobRef(*j))
	if j.CancelReason != domain.CICancelNone {
		// Cancelled (by the owner, fail-fast or the time budget) before the
		// runner answered: it is over, not queued again for another runner.
		conclusion := domain.CICancelled
		if j.CancelReason == domain.CICancelTimeout {
			conclusion = domain.CITimedOut
		}
		rs.end(j, conclusion, "")
		return
	}
	if j.AssignCancels >= domain.CIMaxAssignCancels {
		rs.end(j, domain.CIFailure, "The job was given to a runner "+itoa(int64(domain.CIMaxAssignCancels))+" times and none of them acknowledged it.")
		return
	}
	j.Phase, j.ReadyAt = domain.CIPhaseReady, c.now()
	rs.touch(j)
}

// checkQueue fails the jobs that no runner took within the queue timeout.
func (c *CI) checkQueue(ctx context.Context) {
	ready, err := c.cfg.Store.ListReadyJobs(ctx, 0)
	if err != nil {
		return
	}
	now := c.now()
	for _, j := range ready {
		if now.Sub(j.ReadyAt) <= c.cfg.QueueTimeout {
			continue
		}
		err := c.withRun(ctx, j.RunID, j.RunAttempt, func(rs *runState) error {
			cur := rs.find(j.ID)
			if cur == nil || cur.Phase != domain.CIPhaseReady || c.now().Sub(cur.ReadyAt) <= c.cfg.QueueTimeout {
				return nil
			}
			rs.end(cur, domain.CIFailure, domain.CIQueueTimeoutMessage)
			if err := rs.advance(ctx); err != nil {
				return err
			}
			return rs.flush(ctx)
		})
		if err != nil && !errors.Is(err, domain.ErrNotFound) && ctx.Err() == nil {
			ctxLog(ctx).Error().Err(err).Str("job_id", j.ID).Msg("ci: end a job nobody took")
		}
	}
}

// sweep removes the runs older than the retention, and the logs of runs that
// no longer exist (a repository deleted while the server was down). Every
// failure is logged and the next step still runs: a log directory that cannot
// be removed is tried again at the next sweep.
func (c *CI) sweep(ctx context.Context) {
	log := ctxLog(ctx)
	ids, err := c.cfg.Store.Sweep(ctx, c.now().Add(-c.cfg.Retention))
	if err != nil {
		log.Error().Err(err).Msg("ci: sweep the old runs")
		return
	}
	if len(ids) > 0 {
		if err := c.cfg.Logs.RemoveRuns(ctx, ids); err != nil {
			log.Error().Err(err).Int("runs", len(ids)).Msg("ci: remove the logs of the old runs")
		}
		for _, id := range ids {
			c.locks.Delete(id)
		}
		log.Info().Int("runs", len(ids)).Msg("ci: old runs removed")
	}
	onDisk, err := c.cfg.Logs.RunIDs(ctx)
	if err != nil {
		log.Error().Err(err).Msg("ci: list the log directories")
		return
	}
	var orphans []int64
	for _, id := range onDisk {
		switch _, err := c.cfg.Store.GetRunSource(ctx, id); {
		case errors.Is(err, domain.ErrNotFound):
			orphans = append(orphans, id)
		case err != nil:
			log.Error().Err(err).Int64("run_id", id).Msg("ci: look up a run with a log directory")
		}
	}
	if len(orphans) > 0 {
		if err := c.cfg.Logs.RemoveRuns(ctx, orphans); err != nil {
			log.Error().Err(err).Int("runs", len(orphans)).Msg("ci: remove the logs of runs that no longer exist")
		}
	}
}

// RunIDs lists the runs of a repository, for the cleanup that follows its
// deletion.
func (c *CI) RunIDs(ctx context.Context, repo domain.RepoName) ([]int64, error) {
	return c.cfg.Store.RunIDs(ctx, repo)
}

// RemoveLogs deletes the logs of runs of a repository that was deleted.
func (c *CI) RemoveLogs(ctx context.Context, runIDs []int64) error {
	for _, id := range runIDs {
		c.locks.Delete(id)
	}
	return c.cfg.Logs.RemoveRuns(ctx, runIDs)
}
