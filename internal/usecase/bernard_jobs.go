// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"slices"
	"time"
	"unicode/utf8"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

const (
	// depsLogTailBytes bounds the log tail kept per deps job.
	depsLogTailBytes = 64 << 10
	// defaultDepsJobTimeout is the deadline of a job from its assignment.
	defaultDepsJobTimeout = 10 * time.Minute
)

// errJobSuperseded stops the publication of a job the record left.
var errJobSuperseded = errors.New("the dependency job is no longer current")

var _ ports.DepsJobs = (*Bernard)(nil)

// depsJob is a queued or running runner job. The queue is in memory: a restart
// loses it and Start fails the records that were applying.
type depsJob struct {
	id        string
	repo      domain.RepoName
	sha       string
	updates   []domain.DepRef
	goVersion string
	// runnerUps are the updates the runner applies; all are those of the
	// record when the job was queued, the ones applied in the server included.
	runnerUps, all []domain.DepUpdate

	runner   string    // "" while pending
	deadline time.Time // from the assignment; while pending, from the enqueue
	lines    int64     // lines received, for the offsets
	tail     []byte    // last depsLogTailBytes of the log
}

func newDepsJobID() string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return depsJobPrefix + hex.EncodeToString(raw[:])
}

// enqueue queues the job of a repository and returns its id; a repository
// that already has a job keeps it and gets its id.
func (b *Bernard) enqueue(repo domain.RepoName, sha string, updates []domain.DepRef, goVersion string) string {
	ups := make([]domain.DepUpdate, len(updates))
	for i, r := range updates {
		ups[i] = domain.DepUpdate{DepRef: r}
	}
	id, _ := b.enqueueJob(repo, sha, ups, ups, goVersion)
	return id
}

// enqueueJob queues the job of runnerUps at sha; all are the updates of the
// record. existed is true when the repository had a job already: that one is
// kept and its id returned.
func (b *Bernard) enqueueJob(repo domain.RepoName, sha string, runnerUps, all []domain.DepUpdate, goVersion string) (id string, existed bool) {
	b.jobMu.Lock()
	defer b.jobMu.Unlock()
	for _, j := range b.jobs {
		if j.repo == repo {
			return j.id, true
		}
	}
	refs := make([]domain.DepRef, len(runnerUps))
	for i, u := range runnerUps {
		refs[i] = u.DepRef
	}
	j := &depsJob{
		id: newDepsJobID(), repo: repo, sha: sha, updates: refs, goVersion: goVersion,
		runnerUps: slices.Clone(runnerUps), all: slices.Clone(all),
		deadline: b.cfg.Clock.Now().Add(b.cfg.JobTimeout), // nobody may take it for longer than a run lasts
	}
	if b.jobs == nil {
		b.jobs = map[string]*depsJob{}
	}
	b.jobs[j.id] = j
	b.jobOrder = append(b.jobOrder, j.id)
	return j.id, false
}

// dropJob removes a job that was never started.
func (b *Bernard) dropJob(id string) {
	b.jobMu.Lock()
	b.takeLocked(id)
	b.jobMu.Unlock()
}

// cancelJob removes the queued or running job of a repository and returns its
// id, empty when there is none.
func (b *Bernard) cancelJob(repo domain.RepoName) string {
	b.jobMu.Lock()
	defer b.jobMu.Unlock()
	for id, j := range b.jobs {
		if j.repo == repo {
			b.takeLocked(id)
			return id
		}
	}
	return ""
}

// takeLocked removes a job from the queue.
func (b *Bernard) takeLocked(id string) {
	delete(b.jobs, id)
	for i, o := range b.jobOrder {
		if o == id {
			b.jobOrder = append(b.jobOrder[:i], b.jobOrder[i+1:]...)
			break
		}
	}
}

// expireLocked removes the jobs past their deadline, assigned or still waiting
// for a runner, and returns them.
func (b *Bernard) expireLocked(now time.Time) []*depsJob {
	var out []*depsJob
	for _, id := range append([]string(nil), b.jobOrder...) {
		if j := b.jobs[id]; !now.Before(j.deadline) {
			b.takeLocked(id)
			out = append(out, j)
		}
	}
	return out
}

// expireJobs fails the jobs past their deadline. It runs on the tick, on Next
// and on Heartbeat.
func (b *Bernard) expireJobs(ctx context.Context) {
	b.jobMu.Lock()
	dead := b.expireLocked(b.cfg.Clock.Now())
	b.jobMu.Unlock()
	for _, j := range dead {
		b.finishJob(ctx, j, "deps job timed out")
	}
}

// finishJob ends a job that failed on the server side: revoke its token and
// record the failure with the log tail.
func (b *Bernard) finishJob(ctx context.Context, j *depsJob, reason string) {
	if b.cfg.RevokeJob != nil {
		b.cfg.RevokeJob(ctx, j.id)
	}
	b.failJob(ctx, j, reason)
}

// failJob sets the record of the job's repository to Failed with the reason and
// the end of the log, within maxDepsError.
func (b *Bernard) failJob(ctx context.Context, j *depsJob, reason string) {
	msg := reason
	if room := maxDepsError - len(msg) - 1; room > 0 && len(j.tail) > 0 {
		tail := j.tail
		if len(tail) > room {
			tail = tail[len(tail)-room:]
			for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
				tail = tail[1:]
			}
		}
		msg += "\n" + string(tail)
	}
	if len(msg) > maxDepsError {
		msg = msg[:maxDepsError]
		for !utf8.ValidString(msg) {
			msg = msg[:len(msg)-1]
		}
	}
	defer b.repoLock(j.repo)()
	wrote := false
	_, err := b.cfg.Store.Mutate(context.WithoutCancel(ctx), j.repo, func(d *domain.RepoDeps) (bool, error) {
		wrote = false
		if j.id != "" && (d.State != domain.DepsApplying || d.JobID != j.id) {
			return false, nil // the record left this job: the owner closed the merge request, say
		}
		wrote = true
		d.Repo, d.State, d.Error, d.JobID = j.repo, domain.DepsFailed, msg, ""
		return true, nil
	})
	if err != nil {
		ctxLog(ctx).Error().Err(err).Str("repo", j.repo.String()).Msg("bernard: record the failure of a deps job")
		return
	}
	if wrote {
		b.changed(ctx, j.repo)
	}
}

// failInterrupted fails every record left applying by a previous process.
func (b *Bernard) failInterrupted(ctx context.Context) {
	recs, err := b.cfg.Store.List(ctx)
	if err != nil {
		ctxLog(ctx).Error().Err(err).Msg("bernard: list the records")
		return
	}
	for _, r := range recs {
		if r.State == domain.DepsApplying {
			b.failJob(ctx, &depsJob{repo: r.Repo}, "interrupted by a restart")
		}
	}
}

// ownedLocked returns the job when it is assigned to the runner.
func (b *Bernard) ownedLocked(runnerID, jobID string) (*depsJob, error) {
	j, ok := b.jobs[jobID]
	if !ok || j.runner == "" || j.runner != runnerID {
		return nil, domain.ErrNotFound
	}
	return j, nil
}

// Next hands the oldest pending job to the runner.
func (b *Bernard) Next(ctx context.Context, runnerID string) (ports.DepsAssignment, bool, error) {
	b.expireJobs(ctx)
	b.jobMu.Lock()
	defer b.jobMu.Unlock()
	for _, id := range b.jobOrder {
		j := b.jobs[id]
		if j.runner != "" {
			continue
		}
		j.runner, j.deadline = runnerID, b.cfg.Clock.Now().Add(b.cfg.JobTimeout)
		return ports.DepsAssignment{
			JobID: j.id, Repo: j.repo, Sha: j.sha, Deadline: j.deadline,
			Job: ports.DepsJob{Updates: append([]domain.DepRef(nil), j.updates...), GoVersion: j.goVersion},
		}, true, nil
	}
	return ports.DepsAssignment{}, false, nil
}

// Update checks the job; a deps job has no steps to keep.
func (b *Bernard) Update(_ context.Context, runnerID, jobID string, _ []ports.StepUpdate, _ []domain.CIAnnotation) error {
	b.jobMu.Lock()
	defer b.jobMu.Unlock()
	_, err := b.ownedLocked(runnerID, jobID)
	return err
}

// AppendLog keeps the last 64 KiB of the log.
func (b *Bernard) AppendLog(_ context.Context, runnerID, jobID string, offset int64, lines []domain.CILogLine) (int64, error) {
	b.jobMu.Lock()
	defer b.jobMu.Unlock()
	j, err := b.ownedLocked(runnerID, jobID)
	if err != nil {
		return 0, err
	}
	if offset > j.lines {
		return 0, &ports.ErrCILogGap{Count: j.lines}
	}
	skip := j.lines - offset
	if skip >= int64(len(lines)) {
		return j.lines, nil
	}
	for _, l := range lines[skip:] {
		if j.lines > 0 && !l.Continued {
			j.tail = append(j.tail, '\n')
		}
		j.tail = append(j.tail, l.Text...)
		j.lines++
	}
	if len(j.tail) > depsLogTailBytes {
		j.tail = append([]byte(nil), j.tail[len(j.tail)-depsLogTailBytes:]...)
		for len(j.tail) > 0 && !utf8.RuneStart(j.tail[0]) {
			j.tail = j.tail[1:]
		}
	}
	return j.lines, nil
}

// Heartbeat checks the job and its deadline.
func (b *Bernard) Heartbeat(ctx context.Context, runnerID, jobID string) error {
	b.expireJobs(ctx)
	b.jobMu.Lock()
	defer b.jobMu.Unlock()
	_, err := b.ownedLocked(runnerID, jobID)
	return err
}

// Complete ends the job with the runner's report.
func (b *Bernard) Complete(ctx context.Context, runnerID, jobID string, report *ports.DepsReport, conclusion domain.CIConclusion) error {
	b.expireJobs(ctx)
	b.jobMu.Lock()
	j, err := b.ownedLocked(runnerID, jobID)
	if err == nil {
		b.takeLocked(jobID)
	}
	b.jobMu.Unlock()
	if err != nil {
		return err
	}
	if b.cfg.RevokeJob != nil {
		b.cfg.RevokeJob(ctx, j.id)
	}
	rep := ports.DepsReport{Error: "the runner sent no report"}
	if report != nil {
		rep = *report
	}
	b.onDepsJobDone(ctx, j, rep, conclusion)
	return nil
}

// onDepsJobDone records the outcome of a job the runner ended.
func (b *Bernard) onDepsJobDone(ctx context.Context, j *depsJob, report ports.DepsReport, conclusion domain.CIConclusion) {
	switch {
	case report.Error != "":
		b.failJob(ctx, j, report.Error)
	case conclusion != domain.CISuccess:
		b.failJob(ctx, j, fmt.Sprintf("the runner reported %s", conclusion))
	default:
		if err := b.applyRunnerReport(ctx, j, report); err != nil {
			b.failJob(ctx, j, err.Error())
		}
	}
}

// Limits of a runner report.
const (
	maxReportFiles     = 64
	maxReportFileBytes = 2 << 20
	maxReportBytes     = 8 << 20
)

// applyRunnerReport checks the runner's report against the files at the job's
// commit, applies the updates of the other ecosystems in the server, merges
// both sets of changes and publishes them as the Bernard commit on top of that
// commit.
func (b *Bernard) applyRunnerReport(ctx context.Context, j *depsJob, report ports.DepsReport) error {
	eco := b.ecosystem(domain.EcosystemGoMod)
	if eco == nil {
		return fmt.Errorf("%w: no Go module ecosystem", domain.ErrPreconditionFailed)
	}
	base, err := domain.ParseObjectID(j.sha)
	if err != nil {
		return fmt.Errorf("%w: job commit: %v", domain.ErrInvalid, err)
	}
	if len(report.Files) > maxReportFiles {
		return fmt.Errorf("%w: the report has %d files, at most %d", domain.ErrInvalid, len(report.Files), maxReportFiles)
	}
	allowed := map[domain.TreePath]bool{}
	for _, u := range j.runnerUps {
		for _, f := range []string{"go.mod", "go.sum"} {
			p := path.Join(u.Directory, f)
			tp, err := domain.ParseTreePath(p)
			if err != nil {
				return fmt.Errorf("%w: %v", domain.ErrInvalid, err)
			}
			allowed[tp] = true
		}
	}
	after := map[domain.TreePath][]byte{}
	total := 0
	for _, f := range report.Files {
		tp, err := domain.ParseTreePath(f.Path)
		if err != nil {
			return fmt.Errorf("%w: report path %q: %v", domain.ErrInvalid, f.Path, err)
		}
		if !allowed[tp] {
			return fmt.Errorf("%w: unexpected report path %s", domain.ErrInvalid, f.Path)
		}
		if _, dup := after[tp]; dup {
			return fmt.Errorf("%w: duplicate report path %s", domain.ErrInvalid, f.Path)
		}
		if len(f.Content) > maxReportFileBytes {
			return fmt.Errorf("%w: %s is larger than %d bytes", domain.ErrInvalid, f.Path, maxReportFileBytes)
		}
		if total += len(f.Content); total > maxReportBytes {
			return fmt.Errorf("%w: the report is larger than %d bytes", domain.ErrInvalid, maxReportBytes)
		}
		after[tp] = f.Content
	}

	rev := domain.RevisionOf(base)
	before := map[domain.TreePath][]byte{}
	for tp := range allowed {
		content, ok, err := b.readManifest(ctx, j.repo, rev, tp)
		switch {
		case errors.Is(err, domain.ErrNotFound) && path.Base(tp.String()) == "go.sum":
			continue // a module without dependencies has none
		case err != nil:
			return err
		case !ok:
			return fmt.Errorf("%w: %s is larger than %d bytes", domain.ErrPreconditionFailed, tp, maxDepsManifestBytes)
		}
		before[tp] = content
	}
	if err := eco.Validate(before, after, j.runnerUps); err != nil {
		return fmt.Errorf("check the %s updates: %w", domain.EcosystemGoMod, err)
	}
	changes := map[domain.TreePath][]byte{}
	for tp, content := range after {
		if old, ok := before[tp]; !ok || !bytes.Equal(old, content) {
			changes[tp] = content
		}
	}

	var serverUps []domain.DepUpdate
	for _, u := range j.all {
		if e := b.ecosystem(u.Ecosystem); e != nil && e.ApplyInServer() {
			serverUps = append(serverUps, u)
		}
	}
	if len(serverUps) > 0 {
		more, err := b.changes(ctx, j.repo, base, serverUps)
		if err != nil {
			return err
		}
		for tp, content := range more {
			if _, clash := changes[tp]; clash {
				return fmt.Errorf("%w: %s is changed by the runner and by the server", domain.ErrPreconditionFailed, tp)
			}
			changes[tp] = content
		}
	}

	defer b.repoLock(j.repo)()
	d, err := b.cfg.Store.Get(ctx, j.repo)
	if err != nil {
		return fmt.Errorf("read the dependency record: %w", err)
	}
	jobGuard := func(r domain.RepoDeps) error {
		if r.State != domain.DepsApplying || r.JobID != j.id {
			return errJobSuperseded
		}
		return nil
	}
	if jobGuard(d) != nil {
		return nil // the owner closed the merge request meanwhile: drop the report
	}
	target, exists, err := b.cfg.Repos.DefaultBranch(ctx, j.repo)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: %s has no default branch", domain.ErrPreconditionFailed, j.repo)
	}
	_, err = b.publish(ctx, j.repo, target, base, changes, j.all, d.MR, jobGuard)
	return err
}
