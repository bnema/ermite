// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-actions/workflow"
	"bnema.dev/ermite/internal/domain"
)

// The workflow_run event (CI-3, D5). A run that is created ("requested") or
// completed ("completed") triggers the workflows of the default branch that list
// its workflow by name under workflow_run.workflows.
//
// The triggered run is read from the default branch's latest commit and runs on
// it. It inherits the actor, the pusher and the trusted flag of the triggering
// run, and never gets more: an agent's untrusted run cannot launder secrets
// through a workflow_run (D1). A job token's push starts no run, but a run
// started by a push of anyone else triggers workflow_run workflows even when the
// triggering run was itself a workflow_run, up to the chain limit.
const (
	// maxWorkflowRunDepth is the number of workflow_run levels in a chain:
	// docs.github.com ("Events that trigger workflows", workflow_run): "You can't
	// use workflow_run to chain together more than three levels of workflows"
	// (A -> B -> C -> D runs, E does not). Recalled from memory, not fetched.
	maxWorkflowRunDepth = 3
	runRequested        = "requested"
	runCompleted        = "completed"
)

// runEvent is a change of a run that workflow_run workflows may be waiting for.
type runEvent struct {
	run    domain.CIRun
	action string
	log    zerowrap.Logger
}

// onRunEvent queues the "requested" or "completed" event of a run and returns:
// a worker handles it, so the run's lock is never held while workflows are read.
// A full queue drops it, logged, like the queue of pushes.
func (c *CI) onRunEvent(ctx context.Context, run domain.CIRun, action string) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return
	}
	e := runEvent{run: run, action: action, log: zerowrap.FromCtx(ctx)}
	select {
	case c.runEvents <- e:
	default:
		e.log.Error().Int64("run_id", run.ID).Str("action", action).Msg("ci workflow_run queue is full: the workflows waiting for this run are not run")
	}
}

func (c *CI) runEventLoop() {
	defer c.wg.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case e := <-c.runEvents:
			c.planRunEvent(e)
		}
	}
}

// planRunEvent handles one event; a panic or an error is logged and never
// escapes.
func (c *CI) planRunEvent(e runEvent) {
	ctx := zerowrap.WithCtx(c.ctx, e.log)
	defer func() {
		if r := recover(); r != nil {
			e.log.Error().Interface("panic", r).Bytes("stack", debug.Stack()).Int64("run_id", e.run.ID).Msg("ci workflow_run panicked")
		}
		if c.cfg.RunEventHandled != nil {
			c.cfg.RunEventHandled(e.run.ID, e.action)
		}
	}()
	if err := c.handleRunEvent(ctx, e); err != nil && ctx.Err() == nil {
		e.log.Error().Err(err).Int64("run_id", e.run.ID).Str("action", e.action).Msg("ci workflow_run failed")
	}
}

// runLinks are the two facts of the github context of a run that workflow_run
// reads back: the head branch of a merge request, and the run that triggered a
// workflow_run run.
type runLinks struct {
	HeadRef string `json:"head_ref"`
	Event   struct {
		WorkflowRun struct {
			ID int64 `json:"id"`
		} `json:"workflow_run"`
	} `json:"event"`
}

func (c *CI) runLinks(ctx context.Context, runID int64) (runLinks, domain.CIRunSource, error) {
	src, err := c.cfg.Store.GetRunSource(ctx, runID)
	if err != nil {
		return runLinks{}, src, err
	}
	var l runLinks
	if err := json.Unmarshal([]byte(src.GithubJSON), &l); err != nil {
		return runLinks{}, src, nil // a context that does not read: no link
	}
	return l, src, nil
}

// headBranchOf is workflow_run.head_branch: the source branch of a merge
// request, else the branch or tag of the run.
func headBranchOf(run domain.CIRun, l runLinks) string {
	if l.HeadRef != "" {
		return l.HeadRef
	}
	return strings.TrimPrefix(strings.TrimPrefix(run.Ref, "refs/heads/"), "refs/tags/")
}

// workflowRunDepth is the number of workflow_run levels above and including run:
// 0 for a run that is not a workflow_run run. The walk stops at a triggering
// run that cannot be found, which counts for nothing more.
func (c *CI) workflowRunDepth(ctx context.Context, run domain.CIRun) int {
	depth := 0
	for cur := run; cur.Event == eventWorkflowRun && depth <= maxWorkflowRunDepth; {
		depth++
		l, _, err := c.runLinks(ctx, cur.ID)
		if err != nil || l.Event.WorkflowRun.ID == 0 {
			break
		}
		parent, err := c.cfg.Store.GetRun(ctx, cur.Repo, l.Event.WorkflowRun.ID)
		if err != nil {
			break
		}
		cur = parent
	}
	return depth
}

// matchWorkflowRun reports whether the workflow runs for the action of a run of
// the workflow called name on headBranch: the event is listed, the name is in
// "workflows", the action is one of its "types" (requested and completed when it
// lists none; ermite has no in_progress activity), and the branches or
// branches-ignore filter, which applies to the head branch of the triggering
// run, lets it through (the filters of the push event, under another name).
func matchWorkflowRun(wf *workflow.Workflow, name, action, headBranch string) bool {
	if wf == nil || wf.On == nil {
		return false
	}
	ev := wf.On.Event(eventWorkflowRun)
	if ev == nil || !slices.Contains(ev.Workflows, name) {
		return false
	}
	types := ev.Types
	if types == nil {
		types = []string{runRequested, runCompleted}
	}
	if !slices.Contains(types, action) {
		return false
	}
	as := *ev
	as.Name = "push"
	probe := &workflow.Workflow{On: &workflow.On{Events: []*workflow.Event{&as}}}
	return probe.MatchPush(workflow.PushInfo{Ref: "refs/heads/" + headBranch})
}

// handleRunEvent starts the workflow_run workflows of the default branch that
// match the event.
func (c *CI) handleRunEvent(ctx context.Context, e runEvent) error {
	runners, err := c.cfg.Store.ListRunners(ctx)
	if err != nil {
		return fmt.Errorf("list the runners: %w", err)
	}
	if len(runners) == 0 {
		return nil
	}
	if c.workflowRunDepth(ctx, e.run) >= maxWorkflowRunDepth {
		e.log.Debug().Int64("run_id", e.run.ID).Msg("workflow_run chain limit reached: no run is triggered")
		return nil
	}
	repo, err := c.cfg.Meta.Get(ctx, e.run.Repo)
	if err != nil {
		return nil // deleted meanwhile
	}
	def, exists, err := c.cfg.Repos.DefaultBranch(ctx, repo.Name)
	if err != nil || !exists {
		return nil
	}
	tip, err := c.cfg.Git.BranchHead(ctx, repo.Name, def)
	if err != nil {
		return nil
	}
	files, err := c.workflowFiles(ctx, repo.Name, tip)
	if err != nil || len(files) == 0 {
		return err
	}
	links, src, err := c.runLinks(ctx, e.run.ID)
	if err != nil {
		return nil // the run is gone
	}
	head := headBranchOf(e.run, links)

	var matching []workflowFile
	var parsed []*workflow.Workflow
	for _, f := range files {
		if f.problem != "" {
			continue
		}
		wf, perrs := parseWorkflow(f.path, f.content)
		if len(perrs) > 0 || !matchWorkflowRun(wf, e.run.WorkflowName, e.action, head) {
			continue
		}
		matching, parsed = append(matching, f), append(parsed, wf)
	}
	if len(matching) == 0 {
		return nil
	}
	commits, _, err := c.cfg.Git.ListRange(ctx, repo.Name, domain.ObjectID{}, tip, 1)
	if err != nil || len(commits) == 0 {
		return fmt.Errorf("read the head commit of %s: %w", def, err)
	}
	facts := pushFacts{
		update: domain.RefUpdate{Ref: "refs/heads/" + def, Old: domain.ZeroOID, New: tip.String()}, commit: tip, head: commits[0],
		defaultBranch: def,
		custom:        &customEvent{name: eventWorkflowRun, triggered: &workflowRunFacts{run: e.run, action: e.action, headBranch: head}},
	}
	// The actor, the pusher and the trust of the triggering run, never more.
	t := ciTrigger{repo: repo.Name, actor: e.run.Actor, pusher: src.Pusher, trusted: e.run.Trusted, at: c.now(), log: e.log}
	var errs []error
	for i, f := range matching {
		wf := parsed[i]
		if problem := c.matrixProblem(ctx, repo, t, facts, f.path, wf); problem.text != "" {
			name := f.path
			if problem.named {
				name = workflowName(wf, f.path)
			}
			errs = append(errs, c.startInvalid(ctx, repo, t, facts, f.path, name, problem.text))
			continue
		}
		if _, err := c.launchRun(ctx, repo, t, facts, f.path, string(f.content), wf); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, domain.ErrWorkflowDisabled) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
