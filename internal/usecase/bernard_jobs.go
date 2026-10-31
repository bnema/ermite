// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"runtime/debug"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// defaultDepsJobTimeout is the time budget of a dependency job once a runner
// has it.
const defaultDepsJobTimeout = 10 * time.Minute

// errJobSuperseded stops the publication of a run the record left.
var errJobSuperseded = errors.New("the dependency run is no longer current")

var _ ports.DepsRunObserver = (*Bernard)(nil)

// keepDepsError keeps the summary and the newest printable output of a failure
// report, within 12 lines and maxDepsError bytes.
func keepDepsError(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '\n' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, strings.ToValidUTF8(s, "")))
	summary, output, _ := strings.Cut(s, "\n")
	// An oversized summary alone exhausts the budget.
	if len(summary) >= maxDepsError {
		summary = summary[:maxDepsError]
		for !utf8.ValidString(summary) {
			summary = summary[:len(summary)-1]
		}
		return strings.TrimSpace(summary)
	}
	lines := strings.Split(output, "\n")
	if len(lines) > 11 {
		lines = lines[len(lines)-11:]
	}
	output = strings.Join(lines, "\n")
	budget := maxDepsError - len(summary) - 1 // separator
	if len(output) > budget {
		output = output[len(output)-budget:]
		for !utf8.ValidString(output) {
			output = output[1:]
		}
	}
	output = strings.TrimSpace(output)
	if output == "" {
		return summary
	}
	return summary + "\n" + output
}

// failRun sets the record to Failed with a bounded reason when it is still
// applying that run. The log is on the run, which the record keeps linking.
func (b *Bernard) failRun(ctx context.Context, repo domain.RepoName, runID int64, reason string) {
	defer b.repoLock(repo)()
	wrote := false
	_, err := b.cfg.Store.Mutate(context.WithoutCancel(ctx), repo, func(d *domain.RepoDeps) (bool, error) {
		wrote = false
		batch := applyingBatch(d, runID)
		if batch == nil {
			return false, nil // the record left this run: the owner closed the merge request, say
		}
		wrote = true
		batch.State, batch.Error, batch.RunUpdates = domain.DepsFailed, keepDepsError(reason), nil
		return true, nil
	})
	if err != nil {
		ctxLog(ctx).Error().Err(err).Str("repo", repo.String()).Msg("bernard: record the failure of a dependency run")
		return
	}
	if wrote {
		b.changed(ctx, repo)
	}
}

// depsReconcileGrace is how long the daily pass leaves a run that just ended to
// the observer, which may still be publishing its result.
const depsReconcileGrace = 10 * time.Minute

// reconcileRuns settles the records left applying: the run is the truth. One
// still open is left alone (the observer hears of its end); one that ended
// unseen (a previous process, an observer that failed), or that is gone, fails
// the record so that it can be retried. A run that ended less than grace ago is
// left to the observer.
func (b *Bernard) reconcileRuns(ctx context.Context, grace time.Duration) {
	recs, err := b.cfg.Store.List(ctx)
	if err != nil {
		ctxLog(ctx).Error().Err(err).Msg("bernard: list the records")
		return
	}
	for _, r := range recs {
		for _, batch := range r.Batches {
			if batch.State != domain.DepsApplying {
				continue
			}
			if b.cfg.Runs == nil || batch.RunID == 0 {
				b.failRun(ctx, r.Repo, batch.RunID, "the run no longer exists")
				continue
			}
			run, err := b.cfg.Runs.GetDepsRun(ctx, r.Repo, batch.RunID)
			switch {
			case errors.Is(err, domain.ErrNotFound):
				b.failRun(ctx, r.Repo, batch.RunID, "the run no longer exists")
			case err != nil:
				ctxLog(ctx).Error().Err(err).Str("repo", r.Repo.String()).Int64("run_id", batch.RunID).Msg("bernard: read the run to reconcile")
			case run.Status == domain.CICompleted && b.cfg.Clock.Now().Sub(run.CompletedAt) < grace:
				// The observer is probably applying it.
			case run.Status == domain.CICompleted && run.Conclusion == domain.CISuccess:
				b.failRun(ctx, r.Repo, batch.RunID, "the result of the run was not applied")
			case run.Status == domain.CICompleted:
				b.failRun(ctx, r.Repo, batch.RunID, fmt.Sprintf("the run ended: %s", run.Conclusion))
			}
		}
	}
}

// DepsJobEnded applies the outcome of a dependency job that ended: the runner's
// report is checked and published, or the record fails with a bounded reason
// (the log is on the run). A run the record left (the owner closed the merge
// request meanwhile) is dropped. It implements ports.DepsRunObserver.
func (b *Bernard) DepsJobEnded(ctx context.Context, run domain.CIRun, job domain.CIJob, report *ports.DepsReport) {
	defer func() {
		if r := recover(); r != nil {
			ctxLog(ctx).Error().Interface("panic", r).Bytes("stack", debug.Stack()).Str("repo", run.Repo.String()).Msg("bernard: a dependency run panicked")
		}
	}()
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return // shutting down: the next start settles the record from the run
	}
	rec, err := b.currentRecord(ctx, run)
	if err != nil {
		ctxLog(ctx).Error().Err(err).Str("repo", run.Repo.String()).Msg("bernard: read the record of a dependency run")
		return
	}
	if applyingBatch(&rec, run.ID) == nil {
		return // superseded
	}
	switch {
	case report != nil && report.Error != "":
		b.failRun(ctx, run.Repo, run.ID, report.Error)
	case job.Conclusion != domain.CISuccess:
		reason := fmt.Sprintf("the run ended: %s", job.Conclusion)
		if msg := keepDepsError(job.Error); msg != "" {
			reason += ": " + msg
		}
		b.failRun(ctx, run.Repo, run.ID, reason)
	case report == nil:
		b.failRun(ctx, run.Repo, run.ID, "the runner sent no report")
	default:
		if err := b.applyRunnerReport(ctx, rec, run, job, *report); err != nil {
			b.failRun(ctx, run.Repo, run.ID, err.Error())
		}
	}
}

// currentRecord reads the record of the run's repository under its lock, so
// that a transition in progress (Open recording the run) has finished.
func (b *Bernard) currentRecord(ctx context.Context, run domain.CIRun) (domain.RepoDeps, error) {
	defer b.repoLock(run.Repo)()
	return b.cfg.Store.Get(ctx, run.Repo)
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
func (b *Bernard) applyRunnerReport(ctx context.Context, rec domain.RepoDeps, run domain.CIRun, job domain.CIJob, report ports.DepsReport) error {
	changes, base, effects, err := b.checkRunnerReport(ctx, rec, run, job, report)
	if err != nil {
		return err
	}

	started := applyingBatch(&rec, run.ID) // the caller checked it
	var serverUps []domain.DepUpdate
	for _, u := range started.RunUpdates {
		if e := b.ecosystem(u.Ecosystem); e != nil && e.ApplyInServer() {
			serverUps = append(serverUps, u)
		}
	}
	if len(serverUps) > 0 {
		more, err := b.changes(ctx, run.Repo, base, serverUps)
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

	defer b.repoLock(run.Repo)()
	d, err := b.cfg.Store.Get(ctx, run.Repo)
	if err != nil {
		return fmt.Errorf("read the dependency record: %w", err)
	}
	jobGuard := func(r domain.RepoDeps) error {
		if applyingBatch(&r, run.ID) == nil {
			return errJobSuperseded
		}
		return nil
	}
	current := applyingBatch(&d, run.ID)
	if current == nil {
		return nil // the owner closed the merge request meanwhile: drop the report
	}
	target, exists, err := b.cfg.Repos.DefaultBranch(ctx, run.Repo)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: %s has no default branch", domain.ErrPreconditionFailed, run.Repo)
	}
	_, err = b.publish(ctx, run.Repo, target, base, changes, domain.DepBatch{Key: started.Key, Updates: started.RunUpdates, MR: current.MR}, jobGuard, effects)
	return err
}

// CheckDepsReport checks the same report that asynchronous publication consumes.
// It reads the record without the repository lock, which publication holds
// during its git writes: only the state, the run and its updates are needed,
// and DepsJobEnded checks the record again under the lock (the asynchronous
// path re-checks on purpose and stays authoritative). An error that wraps
// domain.ErrInvalid or domain.ErrPreconditionFailed means the report is
// refused for good; any other error is transient.
func (b *Bernard) CheckDepsReport(ctx context.Context, run domain.CIRun, job domain.CIJob, report ports.DepsReport) error {
	rec, err := b.cfg.Store.Get(ctx, run.Repo)
	if err != nil {
		return fmt.Errorf("read the dependency record: %w", err)
	}
	if applyingBatch(&rec, run.ID) == nil {
		return nil
	}
	_, _, _, err = b.checkRunnerReport(ctx, rec, run, job, report)
	return err
}

func (b *Bernard) checkRunnerReport(ctx context.Context, rec domain.RepoDeps, run domain.CIRun, job domain.CIJob, report ports.DepsReport) (map[domain.TreePath][]byte, domain.ObjectID, []domain.DepSideEffect, error) {
	if job.Deps == nil {
		return nil, domain.ObjectID{}, nil, fmt.Errorf("%w: the job holds no dependency update", domain.ErrInvalid)
	}
	// The updates the runner applied: those of the job among the ones the run
	// started with (the daily check may have rewritten rec.Updates since).
	var runnerUps []domain.DepUpdate
	for _, u := range applyingBatch(&rec, run.ID).RunUpdates {
		if slices.ContainsFunc(job.Deps.Details, func(d domain.CIDepsUpdate) bool { return d.Ref == u.DepRef }) {
			runnerUps = append(runnerUps, u)
		}
	}
	groups := map[domain.DepsEcosystem][]domain.DepUpdate{}
	var ecosystems []ports.DepsEcosystem
	for _, u := range runnerUps {
		eco := b.ecosystem(u.Ecosystem)
		if eco == nil || eco.ApplyInServer() {
			return nil, domain.ObjectID{}, nil, fmt.Errorf("%w: no runner ecosystem %s", domain.ErrPreconditionFailed, u.Ecosystem)
		}
		if _, ok := groups[u.Ecosystem]; !ok {
			ecosystems = append(ecosystems, eco)
		}
		groups[u.Ecosystem] = append(groups[u.Ecosystem], u)
	}
	base, err := domain.ParseObjectID(run.Sha)
	if err != nil {
		return nil, domain.ObjectID{}, nil, fmt.Errorf("%w: job commit: %v", domain.ErrInvalid, err)
	}
	if len(report.Files) > maxReportFiles {
		return nil, domain.ObjectID{}, nil, fmt.Errorf("%w: the report has %d files, at most %d", domain.ErrInvalid, len(report.Files), maxReportFiles)
	}
	after := map[domain.TreePath][]byte{}
	total := 0
	for _, f := range report.Files {
		tp, err := domain.ParseTreePath(f.Path)
		if err != nil {
			return nil, domain.ObjectID{}, nil, fmt.Errorf("%w: report path %q: %v", domain.ErrInvalid, f.Path, err)
		}
		allowed := false
		for _, eco := range ecosystems {
			allowed = allowed || eco.ReportAllowed(groups[eco.Name()], tp)
		}
		if !allowed {
			return nil, domain.ObjectID{}, nil, fmt.Errorf("%w: unexpected report path %q", domain.ErrInvalid, f.Path)
		}
		if _, dup := after[tp]; dup {
			return nil, domain.ObjectID{}, nil, fmt.Errorf("%w: duplicate report path %q", domain.ErrInvalid, f.Path)
		}
		if len(f.Content) > maxReportFileBytes {
			return nil, domain.ObjectID{}, nil, fmt.Errorf("%w: %q is larger than %d bytes", domain.ErrInvalid, f.Path, maxReportFileBytes)
		}
		if total += len(f.Content); total > maxReportBytes {
			return nil, domain.ObjectID{}, nil, fmt.Errorf("%w: the report is larger than %d bytes", domain.ErrInvalid, maxReportBytes)
		}
		after[tp] = f.Content
	}

	rev := domain.RevisionOf(base)
	before, partial := map[domain.TreePath][]byte{}, map[domain.TreePath]bool{}
	var effects []domain.DepSideEffect
	for _, eco := range ecosystems {
		ups := groups[eco.Name()]
		var changed []domain.TreePath
		ownAfter := map[domain.TreePath][]byte{}
		for tp, content := range after {
			if eco.ReportAllowed(ups, tp) {
				changed = append(changed, tp)
				ownAfter[tp] = content
			}
		}
		slices.SortFunc(changed, func(a, b domain.TreePath) int { return strings.Compare(a.String(), b.String()) })
		ownBefore, ownPartial, err := b.readInputs(ctx, run.Repo, rev, eco.Inputs(ups, changed), changed)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) || errors.Is(err, errDepsOversize) || domain.IsInvalid(err) {
				// The file is not at the job's commit, is not a file or is too
				// large: the report can never be checked against it.
				err = fmt.Errorf("%w: %v", domain.ErrPreconditionFailed, err)
			}
			return nil, domain.ObjectID{}, nil, err
		}
		maps.Copy(before, ownBefore)
		maps.Copy(partial, ownPartial)
		more, err := eco.Validate(ownBefore, ownAfter, ups)
		if err != nil {
			return nil, domain.ObjectID{}, nil, fmt.Errorf("check the %s updates: %w", eco.Name(), err)
		}
		effects = append(effects, more...)
	}
	changes := map[domain.TreePath][]byte{}
	for tp, content := range after {
		if old, ok := before[tp]; !ok || partial[tp] || !bytes.Equal(old, content) {
			changes[tp] = content
		}
	}

	return changes, base, effects, nil
}

// applyingBatch finds only an active run, never a historical RunID link.
func applyingBatch(d *domain.RepoDeps, runID int64) *domain.DepBatch {
	for i := range d.Batches {
		batch := &d.Batches[i]
		if batch.State == domain.DepsApplying && batch.RunID == runID {
			return batch
		}
	}
	return nil
}
