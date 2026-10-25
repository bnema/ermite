// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// maxVersionLen bounds RegisterRunner.version.
const maxVersionLen = 64

// --- registration ---

// Register adds a runner and returns its token, shown once. The principal
// needs runner:register on a token that is not restricted to repositories, or
// to be root. A name in use is domain.ErrAlreadyExists unless Replace is set,
// which removes the old runner (its token stops working at once).
func (c *CI) Register(ctx context.Context, p domain.Principal, params ports.RegisterRunnerParams) (ports.RegisteredRunner, error) {
	if err := canManageRunners(p); err != nil {
		return ports.RegisteredRunner{}, err
	}
	if err := domain.ParseRunnerName(params.Name); err != nil {
		return ports.RegisteredRunner{}, err
	}
	labels, err := domain.ParseRunnerLabels(params.Labels)
	if err != nil {
		return ports.RegisteredRunner{}, err
	}
	var v domain.Violations
	if len(params.Version) > maxVersionLen || !utf8.ValidString(params.Version) {
		v.Add("version", "is at most 64 characters")
	}
	switch params.Isolation {
	case domain.CIIsolationVM, domain.CIIsolationContainerSysbox, domain.CIIsolationContainerPrivileged:
	default:
		v.Add("isolation", "is required")
	}
	if params.Arch != "x64" && params.Arch != "arm64" {
		v.Add("arch", `must be "x64" or "arm64"`)
	}
	if params.OS != "linux" {
		v.Add("os", `must be "linux"`)
	}
	if err := v.Err(); err != nil {
		return ports.RegisteredRunner{}, err
	}

	token := newToken(domain.RunnerTokenPrefix)
	r := domain.CIRunner{
		ID: newRunnerID(), Name: params.Name, Labels: domain.RunnerLabels(labels, params.OS, params.Arch),
		Isolation: params.Isolation, Version: params.Version, Arch: params.Arch, OS: params.OS,
		RegisteredAt: c.now(), TokenHash: domain.HashTokenSecret(token),
	}
	removed, err := c.cfg.Store.CreateRunner(ctx, r, params.Replace)
	if err != nil {
		return ports.RegisteredRunner{}, err
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("runner_id", r.ID).Str("runner", r.Name).Str("isolation", string(r.Isolation)).
		Int("replaced", len(removed)).Str("principal", p.Label()).Str("token_id", p.TokenID()).Msg("runner registered")
	c.forgetRunners(removed)
	return ports.RegisteredRunner{Runner: r, Token: token}, nil
}

// forgetRunners drops what is kept in memory for removed runners and ends
// their polls in progress.
func (c *CI) forgetRunners(rs []domain.CIRunner) {
	c.mu.Lock()
	for _, r := range rs {
		delete(c.polls, r.ID)
		delete(c.seen, r.ID)
	}
	c.mu.Unlock()
	c.wakeQueue()
}

// runnerFor authenticates a runner token.
func (c *CI) runnerFor(ctx context.Context, secret string) (domain.CIRunner, error) {
	if !domain.IsRunnerToken(secret) {
		return domain.CIRunner{}, domain.ErrUnauthenticated
	}
	r, err := c.cfg.Store.FindRunnerByToken(ctx, domain.HashTokenSecret(secret))
	if errors.Is(err, domain.ErrNotFound) {
		return domain.CIRunner{}, domain.ErrUnauthenticated
	}
	return r, err
}

// RunnerOnlineWindow is how long after its last sign of life (a poll or a
// heartbeat) a runner is online, by the server's clock.
const RunnerOnlineWindow = 2 * time.Minute

func (c *CI) runnerOnline(lastSeen, now time.Time) bool {
	return !lastSeen.IsZero() && now.Sub(lastSeen) < RunnerOnlineWindow
}

// noteVersion records the version a poll reports, once: the runner read for
// this poll already holds the stored one, so an unchanged version costs no
// write. A runner that sends none keeps its stored version. A failure to
// record is logged and does not fail the poll.
func (c *CI) noteVersion(ctx context.Context, r domain.CIRunner, version string) {
	if version == "" || version == r.Version {
		return
	}
	if err := c.cfg.Store.SetRunnerVersion(ctx, r.ID, version); err != nil && !errors.Is(err, domain.ErrNotFound) {
		log := zerowrap.FromCtx(ctx)
		log.Warn().Err(err).Str("runner_id", r.ID).Msg("runner version not recorded")
	}
}

func (c *CI) markSeen(runnerID string) {
	c.mu.Lock()
	c.seen[runnerID] = c.now()
	c.mu.Unlock()
}

// --- FetchJob ---

var errNoJob = errors.New("no job")

// FetchJob holds the call until a queued job matches the runner, or the poll
// time passes. A job is given to exactly one runner: assignments are made one
// at a time under a lock and recorded before the answer is sent.
func (c *CI) FetchJob(ctx context.Context, secret string, params ports.FetchJobParams) (ports.FetchJobResult, error) {
	runner, err := c.runnerFor(ctx, secret)
	if err != nil {
		return ports.FetchJobResult{}, err
	}
	if len(params.Version) > maxVersionLen || !utf8.ValidString(params.Version) {
		return ports.FetchJobResult{}, fmt.Errorf("%w: version is at most 64 characters", domain.ErrInvalid)
	}
	offered, err := offeredLabels(runner, params.Labels)
	if err != nil {
		return ports.FetchJobResult{}, err
	}
	if params.PollID != "" {
		if err := checkPollID(params.PollID); err != nil {
			return ports.FetchJobResult{}, err
		}
		j, ok, err := c.repeatPoll(ctx, runner, params.PollID)
		if err != nil {
			return ports.FetchJobResult{}, err // the runner asks again with the same poll id
		}
		if ok {
			return ports.FetchJobResult{Job: j}, nil
		}
	}
	c.markSeen(runner.ID)
	c.noteVersion(ctx, runner, params.Version)
	deadline := time.NewTimer(c.cfg.PollTimeout)
	defer deadline.Stop()
	for {
		wake := c.pending.wait(queueKey) // before looking: a job made meanwhile wakes us
		job, err := c.assignTo(ctx, runner, offered, params.Kinds, params.PollID)
		switch {
		case err == nil:
			c.markSeen(runner.ID)
			return ports.FetchJobResult{Job: job}, nil
		case !errors.Is(err, errNoJob):
			return ports.FetchJobResult{}, err
		}
		select {
		case <-wake:
		case <-deadline.C:
			c.markSeen(runner.ID)
			return ports.FetchJobResult{}, nil
		case <-ctx.Done():
			return ports.FetchJobResult{}, ctx.Err()
		case <-c.ctx.Done():
			return ports.FetchJobResult{RetryAfter: time.Second}, nil
		}
		// A removed runner's poll ends with unauthenticated.
		if _, err := c.cfg.Store.GetRunner(ctx, runner.ID); errors.Is(err, domain.ErrNotFound) {
			return ports.FetchJobResult{}, domain.ErrUnauthenticated
		}
	}
}

func checkPollID(id string) error {
	if len(id) > 64 {
		return fmt.Errorf("%w: poll_id is at most 64 characters", domain.ErrInvalid)
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return fmt.Errorf("%w: poll_id has letters, digits, '_' and '-' only", domain.ErrInvalid)
		}
	}
	return nil
}

// offeredLabels are the labels of one poll: all those of the runner, or the
// subset the call names.
func offeredLabels(r domain.CIRunner, asked []string) ([]string, error) {
	if len(asked) == 0 {
		return r.Labels, nil
	}
	var out []string
	for _, l := range asked {
		l = strings.ToLower(l)
		if !slices.Contains(r.Labels, l) {
			return nil, fmt.Errorf("%w: label %q is not a label of the runner", domain.ErrInvalid, l)
		}
		out = append(out, l)
	}
	return out, nil
}

// repeatPoll answers a retried poll with the assignment the first call made,
// as long as it has not been acknowledged or cancelled.
func (c *CI) repeatPoll(ctx context.Context, runner domain.CIRunner, pollID string) (*ports.RunnerJob, bool, error) {
	c.mu.Lock()
	rec, ok := c.polls[runner.ID]
	c.mu.Unlock()
	if !ok || rec.pollID != pollID {
		return nil, false, nil
	}
	var rid int64
	var ra int32
	var id string
	if _, err := fmt.Sscanf(rec.ref, "%d/%d/%s", &rid, &ra, &id); err != nil {
		return nil, false, nil
	}
	var out *ports.RunnerJob
	err := c.withRun(ctx, rid, ra, func(rs *runState) error {
		j := rs.find(id)
		if j == nil || j.Phase != domain.CIPhaseAssigned || j.TokenHash != rec.hash || j.RunnerID != runner.ID {
			return errNoJob
		}
		// The secrets are not kept with the poll: they are read again. A
		// dependency job has none.
		var settings ports.JobSettings
		if j.Deps == nil {
			var err error
			if settings, err = c.settingsOf(ctx, rs, *j); err != nil {
				return err
			}
		}
		out = c.runnerJob(rs, *j, jobSecrets{token: rec.token, ghToken: rec.ghToken, idToken: rec.idToken}, settings)
		return nil
	})
	switch {
	case err == nil && out != nil:
		return out, true, nil
	case isSettingsError(err):
		return nil, false, err // checked first: it may wrap ErrConflict or ErrNotFound
	case err == nil, errors.Is(err, errNoJob), errors.Is(err, domain.ErrNotFound), errors.Is(err, domain.ErrConflict):
		return nil, false, nil // nothing to repeat: the poll goes on as a new one
	}
	return nil, false, err // the settings could not be read: not a lost assignment
}

// runnerJob is the job as the runner gets it. settings are read when the job
// is assigned and live in this value only: j is a copy, so the contexts it
// carries (with the variables as they are now) are not stored, and the secrets
// go nowhere but to the runner (never to the run record, the github context or
// a log; RunnerJob prints without them).
//
// TODO(ci3-c2): the runner writes the "Set up job" log; it states there that
// secrets were withheld when Trusted is false (the server adds no setup line).
func (c *CI) runnerJob(rs *runState, j domain.CIJob, sec jobSecrets, settings ports.JobSettings) *ports.RunnerJob {
	if j.Deps != nil {
		return c.depsRunnerJob(rs, j, sec)
	}
	if settings.Vars != nil {
		j.ContextsJSON = withVars(j.ContextsJSON, settings.Vars)
	}
	path, yaml := rs.run.WorkflowPath, rs.src.WorkflowYAML
	if call := rs.scopeOf(j); call != nil && call.Call != nil {
		// A job of a called workflow runs from the file of the call, under its
		// id in that file.
		path, yaml, j.Key = call.Call.Path, call.Call.YAML, j.CallID
	}
	out := &ports.RunnerJob{
		Job: j, Token: sec.token, GithubToken: sec.ghToken, Repo: rs.run.Repo, Sha: rs.run.Sha, Ref: rs.run.Ref,
		WorkflowPath: path, WorkflowYAML: yaml,
		Environment: j.Environment, Vars: settings.Vars, Secrets: settings.Secrets, Trusted: rs.run.Trusted,
		Permissions: domain.TokenPermissions(j.Permissions, rs.run.Trusted),
		Cache:       c.cacheScope(rs.run, rs.src.GithubJSON),
	}
	if sec.idToken != "" && c.cfg.OIDC.Enabled() {
		out.IDTokenURL, out.IDTokenRequestToken = c.cfg.OIDC.TokenURL(), sec.idToken
	}
	return out
}

// depsRunnerJob is a dependency job as the runner gets it: no workflow, no
// secrets, variables, environment, cache or ID token, and the read-only
// repository token of an untrusted run.
func (c *CI) depsRunnerJob(rs *runState, j domain.CIJob, sec jobSecrets) *ports.RunnerJob {
	return &ports.RunnerJob{
		Job: j, Deps: j.Deps, Token: sec.token, GithubToken: sec.ghToken, Repo: rs.run.Repo, Sha: rs.run.Sha, Ref: rs.run.Ref,
		Trusted: false, Permissions: domain.TokenPermissions(nil, false),
	}
}

// jobSecrets are the secrets issued with an assignment: the job token, the
// repository token and the ID token request token ("" when the job gets
// none).
type jobSecrets struct {
	token, ghToken, idToken string
}

// wantsKind reports whether a poll asking for kinds takes jobs of the kind
// (an empty list asks for workflow jobs only, like the first runners did).
func wantsKind(kinds []string, kind string) bool {
	if len(kinds) == 0 {
		return kind == "workflow"
	}
	return slices.Contains(kinds, kind)
}

// assignTo gives the oldest queued job that matches the labels and the kinds to
// the runner, or errNoJob. The assignment lock makes the choice and the write
// one step.
func (c *CI) assignTo(ctx context.Context, runner domain.CIRunner, offered, kinds []string, pollID string) (*ports.RunnerJob, error) {
	c.assign.Lock()
	defer c.assign.Unlock()
	ready, err := c.cfg.Store.ListReadyJobs(ctx, 0)
	if err != nil {
		return nil, err
	}
	for _, cand := range ready {
		kind := "workflow"
		if cand.Deps != nil {
			kind = "deps"
		}
		if !wantsKind(kinds, kind) || !domain.LabelsMatch(cand.Labels, offered) {
			continue
		}
		var out *ports.RunnerJob
		err := c.withRun(ctx, cand.RunID, cand.RunAttempt, func(rs *runState) error {
			j := rs.find(cand.ID)
			if j == nil || j.Phase != domain.CIPhaseReady || rs.run.CancelRequested {
				return errNoJob
			}
			// Before anything changes: a failure here leaves the job ready. A
			// dependency job gets no variables or secrets.
			var settings ports.JobSettings
			if j.Deps == nil {
				var err error
				if settings, err = c.settingsOf(ctx, rs, *j); err != nil {
					return err
				}
			}
			token := newToken(domain.JobTokenPrefix)
			ghToken := rs.issueRepoToken(j)
			idToken := rs.issueIDTokenRequest(j)
			j.Phase, j.AssignedAt = domain.CIPhaseAssigned, c.now()
			j.TokenHash, j.TokenValidUntil = domain.HashTokenSecret(token), time.Time{}
			j.RunnerID, j.RunnerName, j.Isolation = runner.ID, runner.Name, runner.Isolation
			rs.touch(j)
			if err := rs.flush(ctx); err != nil {
				return err
			}
			out = c.runnerJob(rs, *j, jobSecrets{token: token, ghToken: ghToken, idToken: idToken}, settings)
			if pollID != "" {
				c.mu.Lock()
				c.polls[runner.ID] = pollRecord{pollID: pollID, ref: jobRef(*j), token: token, ghToken: ghToken, idToken: idToken, hash: j.TokenHash}
				c.mu.Unlock()
			}
			return nil
		})
		switch {
		case err == nil:
			log := zerowrap.FromCtx(ctx)
			log.Info().Str("runner", runner.Name).Str("job_id", out.Job.ID).Int64("run_id", out.Job.RunID).Msg("job assigned")
			return out, nil
		case isSettingsError(err):
			return nil, err // the job stays ready; the runner polls again
		case errors.Is(err, errNoJob), errors.Is(err, domain.ErrNotFound), errors.Is(err, domain.ErrConflict):
			continue // taken or gone meanwhile
		default:
			return nil, err
		}
	}
	return nil, errNoJob
}

// --- the calls of a job token ---

// jobFor authenticates a job token and checks that it is the one of jobID.
func (c *CI) jobFor(ctx context.Context, secret, jobID string) (domain.CIJob, error) {
	if !domain.IsJobToken(secret) {
		return domain.CIJob{}, domain.ErrUnauthenticated
	}
	j, err := c.cfg.Store.FindJobByToken(ctx, domain.HashTokenSecret(secret), c.now())
	if errors.Is(err, domain.ErrNotFound) {
		return domain.CIJob{}, domain.ErrUnauthenticated
	}
	if err != nil {
		return domain.CIJob{}, err
	}
	if j.ID != jobID {
		return domain.CIJob{}, fmt.Errorf("%w: this token is the token of another job", domain.ErrForbidden)
	}
	return j, nil
}

// notDepsJob refuses a call that a dependency job has no use for (workflow
// actions, step summaries, artifacts): it runs no workflow.
func notDepsJob(j domain.CIJob) error {
	if j.Deps != nil {
		return fmt.Errorf("%w: a dependency job has no workflow: %s is not available to it", domain.ErrPreconditionFailed, j.ID)
	}
	return nil
}

func endedError(j domain.CIJob) error {
	return &domain.JobEndedError{Conclusion: j.Conclusion, Message: j.Error}
}

// lockedJob runs fn with the lock of the run and the job read again.
func (c *CI) lockedJob(ctx context.Context, j domain.CIJob, fn func(cur *domain.CIJob) error) error {
	lock := c.runLock(j.RunID)
	lock.Lock()
	defer lock.Unlock()
	cur, err := c.cfg.Store.GetJob(ctx, j.RunID, j.RunAttempt, j.ID)
	if err != nil {
		return err
	}
	if err := sameAssignment(j, cur); err != nil {
		return err
	}
	return fn(&cur)
}

// sameAssignment checks, under the lock of the run, that the job a token was
// found for is still given under that token: an assignment cancelled and given
// again since has a new one, and the old runner is unauthenticated. A job that
// has ended answers with its end instead (the callers do).
func sameAssignment(was, cur domain.CIJob) error {
	if !cur.Ended() && cur.TokenHash != was.TokenHash {
		return domain.ErrUnauthenticated
	}
	return nil
}

// saveJob writes one job (its steps changed) with the annotations that change
// with it, and publishes JobUpdated. The status of the run does not depend on
// steps.
func (c *CI) saveJob(ctx context.Context, j *domain.CIJob, anns ...ports.CIStepAnnotations) error {
	v, err := c.cfg.Store.Apply(ctx, ports.CIWrite{Jobs: []domain.CIJob{*j}, Annotations: anns})
	if err != nil {
		return fmt.Errorf("write job %s: %w", j.ID, err)
	}
	j.Version = v
	if repo, err := c.cfg.Meta.Get(ctx, j.Repo); err == nil {
		lines, _ := c.cfg.Logs.Count(ctx, logRef(*j))
		c.cfg.Bus.Publish(ctx, domain.JobUpdated{Job: *j, Lines: lines, At: c.now(), Visibility: repo.Visibility, CreatedAt: repo.CreatedAt})
	}
	c.jobs.notify(jobRef(*j))
	return nil
}

func logRef(j domain.CIJob) ports.CILogRef {
	return ports.CILogRef{RunID: j.RunID, JobID: j.ID, Attempt: j.Attempt}
}

// UpdateJob applies the steps and the annotations of the request whole or not
// at all. The annotations of each step they name replace those stored, whether
// the step is completed or not.
func (c *CI) UpdateJob(ctx context.Context, secret, jobID string, steps []ports.StepUpdate, annotations []domain.CIAnnotation) error {
	j, err := c.jobFor(ctx, secret, jobID)
	if err != nil {
		return err
	}
	if j.Ended() {
		return endedError(j)
	}
	if err := checkStepUpdates(steps); err != nil {
		return err
	}
	if err := domain.CheckAnnotations(annotations); err != nil {
		return err
	}
	return c.lockedJob(ctx, j, func(cur *domain.CIJob) error {
		if cur.Ended() {
			return endedError(*cur)
		}
		next, changed, err := applySteps(cur.Steps, steps)
		if err != nil {
			return err
		}
		writes, err := c.annotationWrites(ctx, *cur, annotations)
		if err != nil {
			return err
		}
		if !changed && len(writes) == 0 {
			return nil
		}
		cur.Steps = next
		return c.saveJob(ctx, cur, writes...)
	})
}

// checkStepUpdates validates the shape of the steps of a request.
func checkStepUpdates(steps []ports.StepUpdate) error {
	if len(steps) > domain.CIMaxSteps {
		return fmt.Errorf("%w: at most %d steps", domain.ErrInvalid, domain.CIMaxSteps)
	}
	for _, s := range steps {
		if s.Number < 1 || s.Number > domain.CIMaxSteps {
			return fmt.Errorf("%w: step number %d is not between 1 and %d", domain.ErrInvalid, s.Number, domain.CIMaxSteps)
		}
		switch s.State {
		case domain.CIStepPending:
			if s.Conclusion != domain.CINoConclusion || s.Outcome != domain.CINoConclusion {
				return fmt.Errorf("%w: step %d is not completed: it has no conclusion or outcome", domain.ErrInvalid, s.Number)
			}
		case domain.CIStepInProgress:
			if s.Conclusion != domain.CINoConclusion || s.Outcome != domain.CINoConclusion || s.StartedAt.IsZero() {
				return fmt.Errorf("%w: step %d is in progress: started_at is required, conclusion and outcome are not allowed", domain.ErrInvalid, s.Number)
			}
		case domain.CIStepCompleted:
			switch s.Conclusion {
			case domain.CISuccess, domain.CIFailure, domain.CICancelled, domain.CISkipped:
			default:
				return fmt.Errorf("%w: step %d is completed: its conclusion must be success, failure, cancelled or skipped", domain.ErrInvalid, s.Number)
			}
			switch s.Outcome {
			case domain.CINoConclusion, domain.CISuccess, domain.CIFailure, domain.CICancelled, domain.CISkipped:
			default:
				return fmt.Errorf("%w: step %d is completed: its outcome must be success, failure, cancelled or skipped", domain.ErrInvalid, s.Number)
			}
			if s.StartedAt.IsZero() && s.Conclusion != domain.CISkipped {
				return fmt.Errorf("%w: step %d: started_at is required", domain.ErrInvalid, s.Number)
			}
			if s.CompletedAt.IsZero() {
				return fmt.Errorf("%w: step %d is completed: completed_at is required", domain.ErrInvalid, s.Number)
			}
		default:
			return fmt.Errorf("%w: step %d has no state", domain.ErrInvalid, s.Number)
		}
	}
	return nil
}

// applySteps returns the steps after the updates, and whether anything
// changed. The input is not modified.
func applySteps(have []domain.CIStep, ups []ports.StepUpdate) ([]domain.CIStep, bool, error) {
	out := slices.Clone(have)
	changed := false
	for _, u := range ups {
		name := truncateRunes(u.Name, domain.CIMaxStepName)
		i := slices.IndexFunc(out, func(s domain.CIStep) bool { return s.Number == u.Number })
		if i < 0 {
			out = append(out, domain.CIStep{
				Number: u.Number, Name: name, State: u.State, Conclusion: u.Conclusion, Outcome: u.Outcome,
				StartedAt: u.StartedAt, CompletedAt: u.CompletedAt,
			})
			changed = true
			continue
		}
		cur := out[i]
		next := cur
		next.Name, next.State, next.Conclusion, next.Outcome = name, u.State, u.Conclusion, u.Outcome
		next.StartedAt, next.CompletedAt = u.StartedAt, u.CompletedAt
		if cur.Name != "" && name == "" {
			next.Name = cur.Name // an empty name never erases one
		}
		switch {
		case cur.State == domain.CIStepCompleted:
			same := next.State == cur.State && next.Conclusion == cur.Conclusion && effectiveOutcome(next) == effectiveOutcome(cur) && next.Name == cur.Name &&
				next.StartedAt.Equal(cur.StartedAt) && next.CompletedAt.Equal(cur.CompletedAt)
			if !same {
				return nil, false, fmt.Errorf("%w: step %d is completed: it cannot change", domain.ErrPreconditionFailed, u.Number)
			}
			continue
		case cur.State == domain.CIStepInProgress && next.State == domain.CIStepPending:
			return nil, false, fmt.Errorf("%w: step %d cannot go from in progress back to pending", domain.ErrPreconditionFailed, u.Number)
		}
		if !stepsEqual(cur, next) {
			out[i], changed = next, true
		}
	}
	slices.SortFunc(out, func(a, b domain.CIStep) int { return int(a.Number) - int(b.Number) })
	return out, changed, nil
}

// effectiveOutcome is the outcome of a step, read as its conclusion when the
// runner did not report one.
func effectiveOutcome(s domain.CIStep) domain.CIConclusion {
	if s.Outcome == domain.CINoConclusion {
		return s.Conclusion
	}
	return s.Outcome
}

func stepsEqual(a, b domain.CIStep) bool {
	return a.Number == b.Number && a.Name == b.Name && a.State == b.State && a.Conclusion == b.Conclusion && a.Outcome == b.Outcome &&
		a.StartedAt.Equal(b.StartedAt) && a.CompletedAt.Equal(b.CompletedAt) && a.LogStart == b.LogStart && a.HasLines == b.HasLines
}

// lastLineStep returns the highest step number that has lines, 0 for none.
func lastLineStep(steps []domain.CIStep) int32 {
	var last int32
	for _, s := range steps {
		if s.HasLines {
			last = max(last, s.Number)
		}
	}
	return last
}

// checkLines validates the shape of the lines of an AppendLog request.
func checkLines(step int32, offset int64, lines []domain.CILogLine) error {
	if step < 1 || step > domain.CIMaxSteps {
		return fmt.Errorf("%w: step %d is not between 1 and %d", domain.ErrInvalid, step, domain.CIMaxSteps)
	}
	if offset < 0 {
		return fmt.Errorf("%w: negative offset", domain.ErrInvalid)
	}
	if len(lines) > domain.CIMaxLogLines {
		return fmt.Errorf("%w: at most %d lines per request", domain.ErrInvalid, domain.CIMaxLogLines)
	}
	total := 0
	for i, l := range lines {
		if strings.ContainsRune(l.Text, '\n') || !utf8.ValidString(l.Text) {
			return fmt.Errorf("%w: line %d has a line feed or is not valid UTF-8", domain.ErrInvalid, i)
		}
		if l.Time.IsZero() {
			return fmt.Errorf("%w: line %d has no time", domain.ErrInvalid, i)
		}
		if l.Continued && offset == 0 && i == 0 {
			return fmt.Errorf("%w: the first line of a job cannot be continued", domain.ErrInvalid)
		}
		total += len(l.Text)
	}
	if total > domain.CIMaxLogChunkBytes {
		return fmt.Errorf("%w: at most %d bytes of text per request", domain.ErrInvalid, domain.CIMaxLogChunkBytes)
	}
	return nil
}

// AppendLog appends lines of a step to the log of the job; see runner.proto
// for the rules of offsets. The metadata store is written only when a step
// gets its first line.
func (c *CI) AppendLog(ctx context.Context, secret, jobID string, step int32, offset int64, lines []domain.CILogLine) (int64, error) {
	j, err := c.jobFor(ctx, secret, jobID)
	if err != nil {
		return 0, err
	}
	if j.Ended() {
		return 0, endedError(j)
	}
	if err := checkLines(step, offset, lines); err != nil {
		return 0, err
	}
	var next int64
	limitHit := false
	err = c.lockedJob(ctx, j, func(cur *domain.CIJob) error {
		if cur.Ended() {
			return endedError(*cur)
		}
		ref := logRef(*cur)
		count, err := c.cfg.Logs.Count(ctx, ref)
		if err != nil {
			return err
		}
		if offset > count {
			return &ports.ErrCILogGap{Count: count}
		}
		skip := count - offset
		if skip >= int64(len(lines)) {
			next = count // everything is stored already
			return nil
		}
		last := lastLineStep(cur.Steps)
		if step < last {
			return fmt.Errorf("%w: step %d is below step %d, which already has lines", domain.ErrPreconditionFailed, step, last)
		}
		if first := lines[skip]; first.Continued && count > 0 && step != last {
			return fmt.Errorf("%w: a continued line must be in the step of the line before it", domain.ErrPreconditionFailed)
		}
		n, added, err := c.cfg.Logs.Append(ctx, ref, offset, lines, domain.CIMaxLogBytes)
		if errors.Is(err, ports.ErrCILogLimit) {
			limitHit = true
			return nil
		}
		if err != nil {
			return err
		}
		next = n
		if added == 0 {
			return nil
		}
		return c.startStepLines(ctx, cur, step, count, lines[skip].Time)
	})
	if err != nil {
		return 0, err
	}
	if limitHit {
		return 0, c.stopForLogLimit(ctx, j)
	}
	return next, nil
}

// startStepLines records where the lines of a step begin, creating the step
// when UpdateJob did not announce it.
func (c *CI) startStepLines(ctx context.Context, cur *domain.CIJob, step int32, firstLine int64, at time.Time) error {
	i := slices.IndexFunc(cur.Steps, func(s domain.CIStep) bool { return s.Number == step })
	switch {
	case i < 0:
		cur.Steps = append(cur.Steps, domain.CIStep{
			Number: step, State: domain.CIStepInProgress, StartedAt: at, HasLines: true, LogStart: firstLine,
		})
		slices.SortFunc(cur.Steps, func(a, b domain.CIStep) int { return int(a.Number) - int(b.Number) })
	case cur.Steps[i].HasLines:
		return nil
	default:
		cur.Steps[i].HasLines, cur.Steps[i].LogStart = true, firstLine
	}
	return c.saveJob(ctx, cur)
}

// stopForLogLimit ends a job whose log reached its limit: a last line says
// so, the job fails, and the call answers like any call after the end.
func (c *CI) stopForLogLimit(ctx context.Context, j domain.CIJob) error {
	var ended *domain.JobEndedError
	err := c.withRun(ctx, j.RunID, j.RunAttempt, func(rs *runState) error {
		cur := rs.find(j.ID)
		if cur == nil {
			return domain.ErrNotFound
		}
		if cur.Ended() {
			ended = &domain.JobEndedError{Conclusion: cur.Conclusion, Message: cur.Error}
			return nil
		}
		if err := sameAssignment(j, *cur); err != nil {
			return err
		}
		_, _ = c.cfg.Logs.AppendForced(ctx, logRef(*cur), []domain.CILogLine{{Time: c.now(), Text: "##[error]" + domain.CILogLimitMessage}})
		rs.end(cur, domain.CIFailure, domain.CILogLimitMessage)
		ended = &domain.JobEndedError{Conclusion: domain.CIFailure, Message: domain.CILogLimitMessage}
		if err := rs.advance(ctx); err != nil {
			return err
		}
		return rs.flush(ctx)
	})
	if err != nil {
		return err
	}
	if ended == nil {
		return domain.ErrNotFound // a typed nil would be taken for an error
	}
	return ended
}

// Heartbeat tells the server that the runner is alive. The first one
// acknowledges the assignment; the others only move the deadline.
func (c *CI) Heartbeat(ctx context.Context, secret, jobID string) error {
	j, err := c.jobFor(ctx, secret, jobID)
	if err != nil {
		return err
	}
	if j.Ended() {
		return endedError(j)
	}
	// Everything is done under the lock of the run: the acknowledgement, and
	// the note of the heartbeat, which must not be written once the job ended
	// (end() forgets it under the same lock).
	return c.withRun(ctx, j.RunID, j.RunAttempt, func(rs *runState) error {
		cur := rs.find(j.ID)
		switch {
		case cur == nil:
			return domain.ErrUnauthenticated
		case cur.Ended():
			return endedError(*cur)
		case cur.TokenHash != j.TokenHash:
			return domain.ErrUnauthenticated // the assignment was cancelled and given again
		}
		now := c.now()
		if cur.Phase == domain.CIPhaseAssigned {
			cur.Phase, cur.AckedAt, cur.StartedAt, cur.AssignCancels = domain.CIPhaseRunning, now, now, 0
			rs.touch(cur)
			if err := rs.flush(ctx); err != nil {
				return err
			}
			c.forgetPoll(jobRef(*cur)) // the token it holds is the runner's now
		}
		c.mu.Lock()
		c.heartbeats[jobRef(*cur)] = now
		c.seen[cur.RunnerID] = now
		c.mu.Unlock()
		return nil
	})
}

// forgetPoll drops the remembered assignment (and its plaintext token) of a
// job: after the acknowledgement or the cancel of the assignment, a retried
// poll has nothing to repeat.
func (c *CI) forgetPoll(ref string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, p := range c.polls {
		if p.ref == ref {
			delete(c.polls, id)
		}
	}
}

// WatchJob streams what the runner must react to: the cancel request (the
// first message when one is already pending), keepalives, and the end.
func (c *CI) WatchJob(ctx context.Context, secret, jobID string, ready func(), send func(ports.JobSignal) error) error {
	j, err := c.jobFor(ctx, secret, jobID)
	if err != nil {
		return err
	}
	if j.Ended() {
		return endedError(j)
	}
	ref := jobRef(j)
	keepalive := time.NewTicker(c.cfg.Keepalive)
	defer keepalive.Stop()
	cancelSent := false
	if ready != nil {
		ready()
	}
	for {
		wake := c.jobs.wait(ref)
		cur, err := c.cfg.Store.GetJob(ctx, j.RunID, j.RunAttempt, j.ID)
		if err != nil {
			return err
		}
		if cur.TokenHash != j.TokenHash && !cur.Ended() {
			return domain.ErrUnauthenticated // given again
		}
		if cur.Ended() {
			return send(ports.JobSignal{Ended: &domain.JobEndedError{Conclusion: cur.Conclusion, Message: cur.Error}})
		}
		if cur.CancelReason != domain.CICancelNone && !cancelSent {
			cancelSent = true
			if err := send(ports.JobSignal{Cancel: &ports.JobCancel{
				Reason: cur.CancelReason, Message: cur.CancelMessage, RequestedAt: cur.CancelRequestedAt,
			}}); err != nil {
				return err
			}
		}
		select {
		case <-wake:
		case <-keepalive.C:
			if err := send(ports.JobSignal{Keepalive: true}); err != nil {
				return err
			}
		case <-ctx.Done():
			return nil
		case <-c.ctx.Done():
			return nil
		}
	}
}

// CompleteJob ends a job, once. A later call returns the stored result.
func (c *CI) CompleteJob(ctx context.Context, secret string, params ports.CompleteJobParams) (ports.CompleteJobResult, error) {
	j, err := c.jobFor(ctx, secret, params.JobID)
	if err != nil {
		return ports.CompleteJobResult{}, err
	}
	if j.Ended() {
		return ports.CompleteJobResult{Conclusion: j.Conclusion, Error: j.Error}, nil
	}
	switch params.Conclusion {
	case domain.CISuccess, domain.CIFailure, domain.CICancelled, domain.CITimedOut:
	default:
		return ports.CompleteJobResult{}, fmt.Errorf("%w: a runner reports success, failure, cancelled or timed_out", domain.ErrInvalid)
	}
	if params.RunnerShutdown && params.Conclusion != domain.CICancelled {
		return ports.CompleteJobResult{}, fmt.Errorf("%w: runner_shutdown goes with cancelled", domain.ErrInvalid)
	}
	if err := domain.CheckOutputs(params.Outputs); err != nil {
		return ports.CompleteJobResult{}, err
	}
	if err := checkStepUpdates(params.Steps); err != nil {
		return ports.CompleteJobResult{}, err
	}
	if err := domain.CheckAnnotations(params.Annotations); err != nil {
		return ports.CompleteJobResult{}, err
	}
	var res ports.CompleteJobResult
	err = c.withRun(ctx, j.RunID, j.RunAttempt, func(rs *runState) error {
		cur := rs.find(j.ID)
		if cur == nil {
			return domain.ErrNotFound
		}
		if cur.Ended() { // ended by the server since
			res = ports.CompleteJobResult{Conclusion: cur.Conclusion, Error: cur.Error}
			return nil
		}
		if err := sameAssignment(j, *cur); err != nil {
			return err
		}
		steps, _, err := applySteps(cur.Steps, params.Steps)
		if err != nil {
			return err
		}
		writes, err := c.annotationWrites(ctx, *cur, params.Annotations)
		if err != nil {
			return err
		}
		rs.annotations = append(rs.annotations, writes...)
		cur.Steps = steps
		cur.Outputs = params.Outputs
		msg := ""
		if params.RunnerShutdown {
			msg = domain.CIShutdownMessage
		}
		if cur.Deps != nil {
			rs.depsReport = params.DepsReport // outputs mean nothing to a dependency job
			cur.Outputs = nil
		}
		rs.end(cur, params.Conclusion, msg)
		if err := rs.advance(ctx); err != nil {
			return err
		}
		res = ports.CompleteJobResult{Conclusion: cur.Conclusion, Error: cur.Error, Applied: true}
		return rs.flush(ctx)
	})
	if err != nil {
		return ports.CompleteJobResult{}, err
	}
	if res.Applied {
		log := zerowrap.FromCtx(ctx)
		log.Info().Str("job_id", j.ID).Int64("run_id", j.RunID).Str("conclusion", string(res.Conclusion)).Msg("job completed")
	}
	return res, nil
}
