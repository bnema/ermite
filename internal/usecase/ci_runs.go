// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

const runTokenPrefix = "ru1:"

// --- runs and jobs ---

// ListRuns returns a page of the runs of a repository, newest first. The
// principal needs the access of reading the repository.
func (c *CI) ListRuns(ctx context.Context, p domain.Principal, name domain.RepoName, params ports.ListRunsParams) (ports.RunPage, error) {
	if _, err := c.readableRepo(ctx, p, name); err != nil {
		return ports.RunPage{}, err
	}
	n, err := pageSize(params.PageSize)
	if err != nil {
		return ports.RunPage{}, err
	}
	var before int64
	if params.PageToken != "" {
		raw, ok := decodeToken(params.PageToken, runTokenPrefix)
		id, perr := strconv.ParseInt(raw, 10, 64)
		if !ok || perr != nil || id < 1 {
			return ports.RunPage{}, fmt.Errorf("%w: runs", domain.ErrInvalidPageToken)
		}
		before = id
	}
	match := runFilter(params)
	// The store returns runs by id; the filters are applied while paging, so a
	// page of a filtered listing is full unless the runs ran out.
	var kept []domain.CIRun
	cursor := before
	for len(kept) <= n {
		batch, err := c.cfg.Store.ListRuns(ctx, name, cursor, n+1)
		if err != nil {
			return ports.RunPage{}, err
		}
		for _, r := range batch {
			if match(r) {
				kept = append(kept, r)
			}
		}
		if len(batch) < n+1 {
			break
		}
		cursor = batch[len(batch)-1].ID
	}
	page := ports.RunPage{Runs: kept}
	if len(kept) > n {
		page.Runs = kept[:n]
		page.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(runTokenPrefix + strconv.FormatInt(kept[n-1].ID, 10)))
	}
	return page, nil
}

// runFilter returns the test of the filters of a listing.
func runFilter(p ports.ListRunsParams) func(domain.CIRun) bool {
	return func(r domain.CIRun) bool {
		switch {
		case p.Status != "" && r.Status != p.Status:
			return false
		case p.Branch != "" && r.Ref != "refs/heads/"+p.Branch:
			return false
		case p.Workflow != "" && r.WorkflowPath != p.Workflow && path.Base(r.WorkflowPath) != p.Workflow:
			return false
		}
		return true
	}
}

// runOf finds a run of the repository; a run of another repository is not
// found.
func (c *CI) runOf(ctx context.Context, name domain.RepoName, runID int64) (domain.CIRun, error) {
	run, err := c.cfg.Store.GetRun(ctx, name, runID)
	if err != nil {
		return domain.CIRun{}, err
	}
	return run, nil
}

// view adds the live line count to a job.
func (c *CI) view(ctx context.Context, j domain.CIJob) ports.JobView {
	v := ports.JobView{CIJob: j}
	if j.Phase != domain.CIPhaseWaiting && j.Phase != domain.CIPhaseReady {
		v.Lines, _ = c.cfg.Logs.Count(ctx, logRef(j))
	}
	if j.RunnerID != "" {
		if _, err := c.cfg.Store.GetRunner(ctx, j.RunnerID); errors.Is(err, domain.ErrNotFound) {
			v.RunnerGone = true
		}
	}
	return v
}

// GetRun returns a run with the jobs of an attempt (0 for the latest).
func (c *CI) GetRun(ctx context.Context, p domain.Principal, name domain.RepoName, runID int64, attempt int32) (ports.RunDetail, error) {
	if _, err := c.readableRepo(ctx, p, name); err != nil {
		return ports.RunDetail{}, err
	}
	run, err := c.runOf(ctx, name, runID)
	if err != nil {
		return ports.RunDetail{}, err
	}
	latest := run.Attempt
	if attempt < 0 || attempt > latest {
		return ports.RunDetail{}, domain.ErrNotFound
	}
	if attempt == 0 {
		attempt = latest
	}
	jobs, err := c.cfg.Store.ListJobs(ctx, runID, attempt)
	if err != nil {
		return ports.RunDetail{}, err
	}
	if attempt != latest {
		run = c.runAtAttempt(run, jobs, attempt)
	}
	d := ports.RunDetail{Run: run, LatestAttempt: latest}
	for _, j := range jobs {
		d.Jobs = append(d.Jobs, c.view(ctx, j))
	}
	return d, nil
}

// runAtAttempt describes an earlier attempt of a run from its jobs: the run
// record holds the latest attempt only.
func (c *CI) runAtAttempt(run domain.CIRun, jobs []domain.CIJob, attempt int32) domain.CIRun {
	rs := &runState{c: c, run: run, jobs: jobs, attempt: attempt}
	r := rs.recompute(c.now())
	r.Attempt, r.Version = attempt, run.Version
	r.CancelRequested = false
	return r
}

// jobOf finds a job of the repository by its id and attempt (0 for the
// latest). The attempt is the one of the run it was kept by, or executed in.
func (c *CI) jobOf(ctx context.Context, name domain.RepoName, jobID string, attempt int32) (domain.CIRun, domain.CIJob, error) {
	runID, _, _, ok := domain.ParseJobID(jobID)
	if !ok {
		return domain.CIRun{}, domain.CIJob{}, domain.ErrNotFound
	}
	run, err := c.runOf(ctx, name, runID)
	if err != nil {
		return domain.CIRun{}, domain.CIJob{}, err
	}
	if attempt < 0 || attempt > run.Attempt {
		return domain.CIRun{}, domain.CIJob{}, domain.ErrNotFound
	}
	if attempt == 0 {
		attempt = run.Attempt
	}
	j, err := c.cfg.Store.GetJob(ctx, runID, attempt, jobID)
	if err != nil {
		return domain.CIRun{}, domain.CIJob{}, err
	}
	return run, j, nil
}

// GetJob returns one job with its steps.
func (c *CI) GetJob(ctx context.Context, p domain.Principal, name domain.RepoName, jobID string, attempt int32) (ports.JobView, error) {
	if _, err := c.readableRepo(ctx, p, name); err != nil {
		return ports.JobView{}, err
	}
	_, j, err := c.jobOf(ctx, name, jobID, attempt)
	if err != nil {
		return ports.JobView{}, err
	}
	return c.view(ctx, j), nil
}

// --- log streaming ---

// acquireStream counts a log stream against the limits.
func (c *CI) acquireStream(client string) (release func(), err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.streams >= MaxCILogStreams || (client != "" && c.perClient[client] >= MaxCILogStreamsPerClient) {
		return nil, fmt.Errorf("%w: too many log streams", domain.ErrResourceExhausted)
	}
	c.streams++
	if client != "" {
		c.perClient[client]++
	}
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.streams--
		if client != "" {
			if c.perClient[client]--; c.perClient[client] <= 0 {
				delete(c.perClient, client)
			}
		}
	}, nil
}

// GetJobLog streams the lines of a job log from an offset, in batches of at
// most 1,000 lines and 64 KiB. With Follow it goes on while the job runs, and
// ends once the job completed and every line was sent.
func (c *CI) GetJobLog(ctx context.Context, p domain.Principal, name domain.RepoName, params ports.GetJobLogParams, send func(ports.LogBatch) error) error {
	if _, err := c.readableRepo(ctx, p, name); err != nil {
		return err
	}
	if params.Offset < 0 {
		return fmt.Errorf("%w: negative offset", domain.ErrInvalid)
	}
	_, j, err := c.jobOf(ctx, name, params.JobID, params.Attempt)
	if err != nil {
		return err
	}
	release, err := c.acquireStream(params.Client)
	if err != nil {
		return err
	}
	defer release()
	if params.Ready != nil {
		params.Ready()
	}
	ref := logRef(j)
	jref := jobRef(j)
	offset := params.Offset
	checked := time.Now()
	// revalidate asks again whether the token of a stored-token caller still
	// exists, at most every few seconds, whether the stream is busy or idle: a
	// token revoked while a long log is sent stops the stream.
	revalidate := func() error {
		if p.TokenID() == "" || c.cfg.Auth == nil || time.Since(checked) <= DefaultRevalidateEvery {
			return nil
		}
		if err := c.cfg.Auth.Revalidate(ctx, p); err != nil {
			return err
		}
		checked = time.Now()
		return nil
	}
	for {
		wakeJob, wakeLog := c.jobs.wait(jref), c.cfg.Logs.Changed(ref)
		cur, err := c.cfg.Store.GetJob(ctx, j.RunID, j.RunAttempt, j.ID)
		if err != nil {
			return err
		}
		done := cur.Ended()
		lines, err := c.cfg.Logs.Read(ctx, ref, offset, domain.CIMaxLogLines, domain.CIMaxLogChunkBytes)
		switch {
		case errors.Is(err, domain.ErrNotFound), errors.Is(err, domain.ErrOutOfRange):
			// No line yet (a queued job has no log), or fewer lines than the
			// offset. Without follow, and for a job that ended (follow changes
			// nothing then), that is the answer (contract: not_found,
			// out_of_range). A job that runs may still write the line: wait.
			if !params.Follow || done {
				if errors.Is(err, domain.ErrNotFound) {
					return domain.ErrNotFound
				}
				return fmt.Errorf("%w: the log has fewer than %d lines", domain.ErrOutOfRange, offset)
			}
			lines = nil
		case err != nil:
			return err
		}
		if len(lines) > 0 {
			if err := revalidate(); err != nil {
				return err
			}
			batch := ports.LogBatch{Offset: offset, Lines: c.withSteps(ctx, cur, offset, lines)}
			if err := send(batch); err != nil {
				return err
			}
			offset += int64(len(lines))
			continue // more may be waiting
		}
		if !params.Follow || done {
			return nil
		}
		if err := revalidate(); err != nil {
			return err
		}
		select {
		case <-wakeJob:
		case <-wakeLog:
		case <-time.After(DefaultRevalidateEvery):
		case <-ctx.Done():
			return nil
		case <-c.ctx.Done():
			return nil
		}
	}
}

// withSteps tags the lines of a batch with their step from the step ranges.
func (c *CI) withSteps(_ context.Context, j domain.CIJob, offset int64, lines []domain.CILogLine) []ports.JobLogLine {
	out := make([]ports.JobLogLine, len(lines))
	for i, l := range lines {
		out[i] = ports.JobLogLine{Step: domain.StepOfLine(j.Steps, offset+int64(i)), CILogLine: l}
	}
	return out
}

// --- cancel and rerun ---

// CancelRun cancels the latest attempt of a run. The principal needs ci:write
// on the repository. Jobs not given to a runner end at once; the others get a
// cancel request, and the run completes when they have ended.
func (c *CI) CancelRun(ctx context.Context, p domain.Principal, name domain.RepoName, runID int64) (domain.CIRun, error) {
	if _, err := c.writableRepo(ctx, p, name); err != nil {
		return domain.CIRun{}, err
	}
	run, err := c.runOf(ctx, name, runID)
	if err != nil {
		return domain.CIRun{}, err
	}
	if err := runWriteAccess(p, run); err != nil {
		return domain.CIRun{}, err
	}
	var out domain.CIRun
	err = c.withRun(ctx, runID, 0, func(rs *runState) error {
		if rs.run.Status == domain.CICompleted {
			return fmt.Errorf("%w: run %d has completed", domain.ErrPreconditionFailed, runID)
		}
		if !rs.run.CancelRequested {
			rs.run.CancelRequested, rs.run.CancelRequestedAt, rs.runDirty = true, c.now(), true
			rs.cancelAll("Canceled by " + authorOf(p))
		}
		if err := rs.flush(ctx); err != nil {
			return err
		}
		out = rs.run
		return nil
	})
	if err != nil {
		return domain.CIRun{}, err
	}
	ctxLog(ctx).Info().Str("repo", name.String()).Int64("run_id", runID).Str("principal", p.Label()).Str("token_id", p.TokenID()).Msg("ci run cancelled")
	return out, nil
}

// RerunRun starts a new attempt of a completed run, from the stored workflow
// file: all the jobs, or with failedOnly the jobs that did not succeed and
// those that need them; the others are kept with their result, steps and log.
func (c *CI) RerunRun(ctx context.Context, p domain.Principal, name domain.RepoName, runID int64, failedOnly bool) (domain.CIRun, error) {
	repo, err := c.writableRepo(ctx, p, name)
	if err != nil {
		return domain.CIRun{}, err
	}
	run, err := c.runOf(ctx, name, runID)
	if err != nil {
		return domain.CIRun{}, err
	}
	if err := runWriteAccess(p, run); err != nil {
		return domain.CIRun{}, err
	}
	err = c.withRun(ctx, runID, 0, func(rs *runState) error {
		if rs.run.Status != domain.CICompleted {
			return fmt.Errorf("%w: run %d has not completed", domain.ErrPreconditionFailed, runID)
		}
		if len(rs.jobs) == 0 {
			return fmt.Errorf("%w: run %d has no job to run again", domain.ErrPreconditionFailed, runID)
		}
		redo, err := rerunSet(rs, failedOnly)
		if err != nil {
			return err
		}
		next := rs.run.Attempt + 1
		now := c.now()
		w := ports.CIWrite{}
		var fresh []domain.CIJob
		for _, j := range rs.jobs {
			if !redo[j.Key] {
				kept := j // kept with its own attempt, steps and log
				kept.RunAttempt, kept.Version = next, 0
				w.Jobs = append(w.Jobs, kept)
				fresh = append(fresh, kept)
				continue
			}
			if j.Combo > 0 {
				continue // the matrix is expanded again when the job is planned
			}
			n := domain.CIJob{
				ID: domain.NewJobID(runID, j.Index, 0), RunID: runID, Attempt: next, RunAttempt: next, Repo: rs.run.Repo,
				Key: j.Key, Name: j.Key, Phase: domain.CIPhaseWaiting, Needs: slices.Clone(j.Needs), Index: j.Index, CreatedAt: now,
			}
			w.Jobs = append(w.Jobs, n)
			fresh = append(fresh, n)
		}
		run := rs.run
		run.Attempt, run.Status, run.Conclusion = next, domain.CIQueued, domain.CINoConclusion
		run.StartedAt, run.CompletedAt, run.CreatedAt = time.Time{}, time.Time{}, now
		run.CancelRequested, run.CancelRequestedAt = false, time.Time{}
		run.Concurrency = domain.CIConcurrency{} // evaluated again: the re-run joins its group again
		// Kept or lowered, never raised: the owner who runs an agent's run
		// again does not give it secrets (D1).
		run.Trusted = domain.RerunTrusted(rs.run.Trusted, p)
		w.Runs = []domain.CIRun{run}
		rs2 := &runState{c: c, run: run, attempt: next, jobs: fresh, src: rs.src, dirty: map[string]bool{}, checks: map[string]bool{}}
		for _, j := range fresh {
			w.Checks = append(w.Checks, rs2.checkOf(j))
		}
		v, err := c.cfg.Store.Apply(ctx, w)
		if err != nil {
			return err
		}
		run.Version = v
		c.flows.drop(runID)
		c.publishRun(ctx, repo, run)
		for _, j := range w.Jobs {
			j.Version = v
			c.cfg.Bus.Publish(ctx, domain.JobUpdated{Job: j, At: now, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt})
		}
		return nil
	})
	if err != nil {
		return domain.CIRun{}, err
	}
	// Plan the new attempt: the kept jobs count as the results of their needs.
	if err := c.advanceRun(ctx, runID); err != nil {
		return domain.CIRun{}, err
	}
	run, err = c.runOf(ctx, name, runID)
	if err != nil {
		return domain.CIRun{}, err
	}
	ctxLog(ctx).Info().Str("repo", name.String()).Int64("run_id", runID).Int32("attempt", run.Attempt).Bool("failed_only", failedOnly).
		Str("principal", p.Label()).Str("token_id", p.TokenID()).Msg("ci run started again")
	return run, nil
}

// rerunSet returns the keys of the jobs to run again: all of them, or the ones
// that did not succeed and those that need them, directly or not.
func rerunSet(rs *runState, failedOnly bool) (map[string]bool, error) {
	redo := map[string]bool{}
	if !failedOnly {
		for _, j := range rs.jobs {
			redo[j.Key] = true
		}
		return redo, nil
	}
	for _, j := range rs.jobs {
		switch j.Conclusion {
		case domain.CIFailure, domain.CICancelled, domain.CITimedOut, domain.CIActionRequired:
			redo[j.Key] = true
		}
	}
	if len(redo) == 0 {
		return nil, fmt.Errorf("%w: no job of run %d failed", domain.ErrPreconditionFailed, rs.run.ID)
	}
	for changed := true; changed; {
		changed = false
		for _, j := range rs.jobs {
			if redo[j.Key] {
				continue
			}
			if slices.ContainsFunc(j.Needs, func(n string) bool { return redo[n] }) {
				redo[j.Key], changed = true, true
			}
		}
	}
	return redo, nil
}

func (w *wfCache) drop(runID int64) {
	w.mu.Lock()
	delete(w.m, runID)
	delete(w.uses, runID)
	w.mu.Unlock()
}

// --- checks ---

// CommitChecks returns the checks of a commit, without access control. A
// commit with more than MaxCIChecks checks is an error (domain.ErrResourceExhausted):
// the merge rule cannot answer from a part of them.
func (c *CI) CommitChecks(ctx context.Context, repo domain.RepoName, sha string) ([]domain.CICheck, error) {
	checks, more, err := c.cfg.Store.ListChecks(ctx, repo, sha, MaxCIChecks)
	if err != nil {
		return nil, err
	}
	if more {
		return nil, fmt.Errorf("%w: the commit has more than %d checks", domain.ErrResourceExhausted, MaxCIChecks)
	}
	return checks, nil
}

// ListChecks returns the checks of a commit: the one of each job of the latest
// attempt of each run on it. A commit without a run has none.
func (c *CI) ListChecks(ctx context.Context, p domain.Principal, name domain.RepoName, sha string) (ports.ChecksResult, error) {
	if _, err := c.readableRepo(ctx, p, name); err != nil {
		return ports.ChecksResult{}, err
	}
	if !isHex40(sha) {
		return ports.ChecksResult{}, fmt.Errorf("%w: a commit is 40 lowercase hexadecimal characters", domain.ErrInvalidObjectID)
	}
	checks, more, err := c.cfg.Store.ListChecks(ctx, name, sha, MaxCIChecks)
	if err != nil {
		return ports.ChecksResult{}, err
	}
	res := ports.ChecksResult{Truncated: more}
	var runners []domain.CIRunner
	for _, ck := range checks {
		v := ports.CheckView{CICheck: ck}
		if ck.Status == domain.CIQueued {
			if runners == nil {
				runners, _ = c.cfg.Store.ListRunners(ctx)
			}
			v.WaitingForRunner = c.noRunnerFor(ctx, ck, runners)
		}
		res.Checks = append(res.Checks, v)
	}
	return res, nil
}

// noRunnerFor reports whether no registered runner can take a queued check's
// job. A job that waits for its needs is not waiting for a runner.
func (c *CI) noRunnerFor(ctx context.Context, ck domain.CICheck, runners []domain.CIRunner) bool {
	if ck.JobID == "" {
		return false
	}
	j, err := c.cfg.Store.GetJob(ctx, ck.RunID, ck.Attempt, ck.JobID)
	if err != nil || j.Phase != domain.CIPhaseReady {
		return false
	}
	return !slices.ContainsFunc(runners, func(r domain.CIRunner) bool { return domain.LabelsMatch(j.Labels, r.Labels) })
}

// --- runners ---

// ListRunners returns a page of the runners ordered by name. The principal
// needs runner:register on a token that is not restricted to repositories.
func (c *CI) ListRunners(ctx context.Context, p domain.Principal, size int, token string) (ports.RunnerPage, error) {
	if err := canManageRunners(p); err != nil {
		return ports.RunnerPage{}, err
	}
	n, err := pageSize(size)
	if err != nil {
		return ports.RunnerPage{}, err
	}
	after := ""
	if token != "" {
		raw, ok := decodeToken(token, "rn1:")
		if !ok || raw == "" {
			return ports.RunnerPage{}, fmt.Errorf("%w: runners", domain.ErrInvalidPageToken)
		}
		after = raw
	}
	all, err := c.cfg.Store.ListRunners(ctx)
	if err != nil {
		return ports.RunnerPage{}, err
	}
	busy := map[string]bool{}
	if active, err := c.cfg.Store.ListActiveJobs(ctx); err == nil {
		for _, j := range active {
			if j.Phase == domain.CIPhaseRunning {
				busy[j.RunnerID] = true
			}
		}
	}
	var page ports.RunnerPage
	var rows []domain.CIRunner
	rows, page.NextPageToken = pageAfter(all, n,
		func(r domain.CIRunner) bool { return after == "" || strings.ToLower(r.Name) > after },
		func(r domain.CIRunner) string {
			return base64.RawURLEncoding.EncodeToString([]byte("rn1:" + strings.ToLower(r.Name)))
		})
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range rows {
		page.Runners = append(page.Runners, ports.RunnerView{CIRunner: r, LastSeen: c.seen[r.ID], Busy: busy[r.ID]})
	}
	return page, nil
}

// RemoveRunner removes a runner: its token stops working at once and a poll of
// it in progress ends with unauthenticated. A job it runs is not affected.
func (c *CI) RemoveRunner(ctx context.Context, p domain.Principal, id string) error {
	if err := canManageRunners(p); err != nil {
		return err
	}
	r, err := c.cfg.Store.RemoveRunner(ctx, id)
	if err != nil {
		return err
	}
	c.forgetRunners([]domain.CIRunner{r})
	ctxLog(ctx).Info().Str("runner_id", r.ID).Str("runner", r.Name).Str("principal", p.Label()).Str("token_id", p.TokenID()).Msg("runner removed")
	return nil
}
