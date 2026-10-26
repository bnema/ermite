// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Dependency runs (Bernard). A dependency job is a CIRun of event
// "dependencies" with one CIJob created ready: no workflow file is planned. It
// is assigned by assignTo like any job (only to a runner that asks for the
// kind "deps" and offers the label), authenticated by a job token, logged,
// heartbeated, timed out and cancelled by the engine, and its repository token
// is the read-only one of an untrusted run. It posts no check and starts no
// workflow_run workflow. Bernard learns of its end through the DepsRunObserver.

var _ ports.DepsRunner = (*CI)(nil)

// depsActor is the actor of the dependency runs.
var depsActor = domain.AuthorName(domain.Bot(domain.RepoName{}))

const depsJobName = "Update dependencies"

// SetDepsObserver installs the observer of the dependency jobs after
// construction (Bernard is built after CI); nil turns the notifications off.
func (c *CI) SetDepsObserver(o ports.DepsRunObserver) {
	c.mu.Lock()
	c.depsObserver = o
	c.mu.Unlock()
}

// observer returns the installed dependency observer, nil when there is none.
func (c *CI) observer() ports.DepsRunObserver {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.depsObserver
}

// StartDepsRun creates the run of a dependency job, ready for a runner of the
// label.
func (c *CI) StartDepsRun(ctx context.Context, p ports.DepsRunParams) (domain.CIRun, error) {
	label := strings.ToLower(strings.TrimSpace(p.Label))
	if label == "" {
		return domain.CIRun{}, fmt.Errorf("%w: a dependency run needs a runner label", domain.ErrInvalid)
	}
	repo, err := lookupRepo(ctx, c.cfg.Meta, p.Repo)
	if err != nil {
		return domain.CIRun{}, err
	}
	now := c.now()
	minutes := max(int32(1), int32((p.Timeout+time.Minute-1)/time.Minute)) //nolint:gosec // a few minutes
	pusher := domain.PusherOf(domain.Bot(p.Repo))
	job := p.Job
	draft, err := c.cfg.Store.CreateRun(ctx, p.Repo, domain.DepsWorkflowPath, func(id, number int64) ports.CIRunDraft {
		return ports.CIRunDraft{
			Run: domain.CIRun{
				Repo: p.Repo, ID: id, Number: number, Attempt: 1, WorkflowPath: domain.DepsWorkflowPath, WorkflowName: domain.DepsWorkflowName,
				Name: p.Title, Title: p.Title, Event: domain.CIEventDependencies, Ref: p.Ref, Sha: p.Sha, Actor: depsActor,
				Status: domain.CIQueued, Trusted: false, CreatedAt: now,
			},
			Source: domain.CIRunSource{Repo: p.Repo, GithubJSON: "{}", Pusher: pusher},
			Jobs: []domain.CIJob{{
				ID: domain.NewJobID(id, 0, 0), RunID: id, Attempt: 1, RunAttempt: 1, Repo: p.Repo, Key: "deps", Name: depsJobName,
				Phase: domain.CIPhaseReady, Labels: []string{label}, Index: 0, Combo: 0, ComboTotal: 1, Planned: true,
				TimeoutMinutes: minutes, ContextsJSON: "{}", CreatedAt: now, ReadyAt: now, Deps: &job,
			}},
		}
	})
	if err != nil {
		return domain.CIRun{}, err
	}
	c.publishRun(ctx, repo, draft.Run)
	for _, j := range draft.Jobs {
		c.cfg.Bus.Publish(ctx, domain.JobUpdated{Job: j, At: now, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt})
	}
	c.wakeQueue()
	lg := logRun(ctx, draft.Run)
	lg.Info().Str("ref", p.Ref).Int("updates", len(p.Job.Updates)).Msg("dependency run created")
	return draft.Run, nil
}

// CancelDepsRun cancels a dependency run like the owner's cancel does, on a
// server-side decision. A completed run is left alone; a run that is not a
// dependency run of repo is domain.ErrNotFound.
func (c *CI) CancelDepsRun(ctx context.Context, repo domain.RepoName, runID int64) error {
	return c.withRun(ctx, runID, 0, func(rs *runState) error {
		if !rs.run.IsDeps() || rs.run.Repo != repo {
			return domain.ErrNotFound
		}
		if rs.run.Status == domain.CICompleted || rs.run.CancelRequested {
			return nil
		}
		rs.run.CancelRequested, rs.run.CancelRequestedAt, rs.runDirty = true, c.now(), true
		rs.cancelAll("Canceled by " + depsActor)
		return rs.flush(ctx)
	})
}

// GetDepsRun reads a run for Bernard.
func (c *CI) GetDepsRun(ctx context.Context, repo domain.RepoName, id int64) (domain.CIRun, error) {
	return c.runOf(ctx, repo, id)
}

// notifyDepsEnded tells the observer, in the background and outside the lock of
// the run, that the dependency jobs ended. It is called after the write that
// ended them.
func (c *CI) notifyDepsEnded(ctx context.Context, run domain.CIRun, jobs []domain.CIJob, report *ports.DepsReport) {
	obs := c.observer()
	if obs == nil {
		return
	}
	c.mu.Lock()
	closed := c.closed
	if !closed {
		c.wg.Add(1)
	}
	c.mu.Unlock()
	if closed {
		return
	}
	nctx := zerowrap.WithCtx(c.ctx, zerowrap.FromCtx(ctx))
	go func() {
		defer c.wg.Done()
		for _, j := range jobs {
			c.guard(fmt.Sprintf("dependency job %s", j.ID), func() { obs.DepsJobEnded(nctx, run, j, report) })
		}
	}()
}

// checkDepsResult asks the observer to check the report of a successful
// dependency job before it ends. A non-empty refusal is the reason the server
// refuses the report for good; an error is transient and leaves the job open
// so that the runner sends the report again.
func (c *CI) checkDepsResult(ctx context.Context, j domain.CIJob, params ports.CompleteJobParams) (refusal string, err error) {
	if j.Deps == nil || params.Conclusion != domain.CISuccess || params.DepsReport == nil || params.DepsReport.Error != "" {
		return "", nil
	}
	observer := c.observer()
	if observer == nil {
		return "", nil
	}
	run, err := c.runOf(ctx, j.Repo, j.RunID)
	if err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(ctx, depsCheckTimeout)
	defer cancel()
	err = observer.CheckDepsReport(cctx, run, j, *params.DepsReport)
	switch {
	case err == nil:
		return "", nil
	case errors.Is(err, domain.ErrInvalid), errors.Is(err, domain.ErrPreconditionFailed):
		// keepDepsError bounds the reason below maxJobError.
		return keepDepsError("the server refused the dependency result: " + err.Error()), nil
	default:
		// A deadline, a store or a git failure says nothing about the report.
		return "", fmt.Errorf("check the dependency report: %w", err)
	}
}
