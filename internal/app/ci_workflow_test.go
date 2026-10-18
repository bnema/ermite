// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/usecase"
)

// `ermite workflow disable|enable|list` (P8, D5): a disabled workflow starts no
// run for any event, and enabling it restores it.

const releaseWF = ".github/workflows/release.yml"

// wfOn is a workflow file with the given "on:" block (indented under on:).
func wfOn(name, on string) string {
	return "name: " + name + "\non:\n" + on + "jobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"
}

func (c *ciHarness) setWorkflow(api *clients, disable bool, file string) (*pb.Workflow, error) {
	c.t.Helper()
	if disable {
		res, err := api.run.DisableWorkflow(c.ctx, connect.NewRequest(&pb.DisableWorkflowRequest{Repo: c.repo, Workflow: file}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetWorkflow(), nil
	}
	res, err := api.run.EnableWorkflow(c.ctx, connect.NewRequest(&pb.EnableWorkflowRequest{Repo: c.repo, Workflow: file}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetWorkflow(), nil
}

func (c *ciHarness) listWorkflows(api *clients) map[string]*pb.Workflow {
	c.t.Helper()
	res, err := api.run.ListWorkflows(c.ctx, connect.NewRequest(&pb.ListWorkflowsRequest{Repo: c.repo}))
	require.NoError(c.t, err)
	out := map[string]*pb.Workflow{}
	for _, w := range res.Msg.GetWorkflows() {
		out[w.GetPath()] = w
	}
	return out
}

func runsOfWorkflow(runs []*pb.Run, path string) []*pb.Run {
	var out []*pb.Run
	for _, r := range runs {
		if r.GetWorkflowPath() == path {
			out = append(out, r)
		}
	}
	return out
}

// Disabled before the first push, on an empty repository where the file does
// not exist yet: the push of many refs starts nothing of it, the other
// workflows still run, and enabling restores it.
func TestWorkflowDisabledBeforeFirstPushStartsNoRun(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")

	w, err := c.setWorkflow(c.root, true, "release.yml")
	require.NoError(t, err, "the repository is empty and has no such file")
	assert.Equal(t, releaseWF, w.GetPath(), "stored by its normalized path")
	assert.Equal(t, pb.WorkflowState_WORKFLOW_STATE_DISABLED, w.GetState())
	_, err = c.setWorkflow(c.root, true, releaseWF)
	require.NoError(t, err, "idempotent, by path too")

	// The list shows it though no branch holds the file.
	listed := c.listWorkflows(c.root)
	require.Len(t, listed, 1)
	assert.Equal(t, pb.WorkflowState_WORKFLOW_STATE_DISABLED, listed[releaseWF].GetState())
	assert.Empty(t, listed[releaseWF].GetName(), "no file, no name")

	push := wfOn("release", "  push:\n    tags: ['v*']\n    branches: [main]\n")
	c.push("main", map[string]string{
		releaseWF:                      push,
		".github/workflows/z-ctrl.yml": wfOn("control", "  push:\n    branches: [main]\n"),
	}, "")
	c.waitFor("the control run", func() bool { return len(runsOfWorkflow(c.runs(), ".github/workflows/z-ctrl.yml")) == 1 })
	assert.Empty(t, runsOfWorkflow(c.runs(), releaseWF), "a disabled workflow starts no push run")
	runner.runJob(runner.await(), pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ctrl")

	// Tags: many at once.
	for _, tag := range []string{"v1", "v2", "v3"} {
		c.mustGit(c.wt, "tag", tag)
	}
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/"+c.repo+".git"), "v1", "v2", "v3")
	c.push("main", map[string]string{"x.txt": "x"}, "")
	c.waitFor("the control run of the next push", func() bool { return len(runsOfWorkflow(c.runs(), ".github/workflows/z-ctrl.yml")) == 2 })
	assert.Empty(t, runsOfWorkflow(c.runs(), releaseWF))

	listed = c.listWorkflows(c.root)
	assert.Equal(t, "release", listed[releaseWF].GetName(), "the file now exists and has a name")
	assert.Equal(t, pb.WorkflowState_WORKFLOW_STATE_DISABLED, listed[releaseWF].GetState())
	assert.Equal(t, pb.WorkflowState_WORKFLOW_STATE_ENABLED, listed[".github/workflows/z-ctrl.yml"].GetState())

	// Enabling restores it.
	w, err = c.setWorkflow(c.root, false, "release.yml")
	require.NoError(t, err)
	assert.Equal(t, pb.WorkflowState_WORKFLOW_STATE_ENABLED, w.GetState())
	_, err = c.setWorkflow(c.root, false, "release.yml")
	require.NoError(t, err, "idempotent")
	assert.Equal(t, pb.WorkflowState_WORKFLOW_STATE_ENABLED, c.listWorkflows(c.root)[releaseWF].GetState())
	c.push("main", map[string]string{"y.txt": "y"}, "")
	c.waitFor("the release run", func() bool { return len(runsOfWorkflow(c.runs(), releaseWF)) == 1 })
}

// A disabled workflow does not fire on its schedule, and fires again once
// enabled, with no reload of the schedules.
func TestWorkflowDisabledStartsNoScheduledRun(t *testing.T) {
	c, clk, _ := newScheduleHarness(t, "2026-03-04 10:00:30")
	c.push("main", scheduleWorkflow("*/5 * * * *"), "")
	ci := c.forge.CI
	tick := func(at string) {
		clk.set(at)
		ci.ScheduleNow(c.ctx)
	}
	tick("2026-03-04 10:05:00")
	require.Len(t, c.scheduled(), 1, "enabled: fires")
	assert.Equal(t, 1, ci.CachedSchedules(c.name("proj")))

	_, err := c.setWorkflow(c.root, true, "nightly.yml")
	require.NoError(t, err)
	assert.Equal(t, 1, ci.CachedSchedules(c.name("proj")), "the cache holds every workflow: the state is read when it fires")
	tick("2026-03-04 10:10:00")
	tick("2026-03-04 10:15:00")
	assert.Len(t, c.scheduled(), 1, "disabled: no run for the minutes 10:10 and 10:15")

	// A push of the file while disabled changes nothing.
	c.push("main", scheduleWorkflow("*/5 * * * *", "0 0 1 1 *"), "again")
	tick("2026-03-04 10:20:00")
	assert.Len(t, c.scheduled(), 1)

	_, err = c.setWorkflow(c.root, false, "nightly.yml")
	require.NoError(t, err)
	tick("2026-03-04 10:25:00")
	assert.Len(t, c.scheduled(), 2, "enabled again: fires, without catching up the missed minutes")
}

// workflow_dispatch and pull_request are refused/skipped too, per file.
func TestWorkflowDisabledStartsNoDispatchOrPullRequestRun(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{
		".github/workflows/deploy.yml": dispatchWorkflow,
		".github/workflows/pr.yml":     wfOn("pr", "  pull_request:\n"),
		".github/workflows/other.yml":  wfOn("other", "  pull_request:\n"),
	}, "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	c.push("feat", map[string]string{"f.txt": "f"}, "")

	_, err := c.setWorkflow(c.root, true, "deploy.yml")
	require.NoError(t, err)
	_, err = c.setWorkflow(c.root, true, ".github/workflows/pr.yml")
	require.NoError(t, err)

	_, err = c.dispatch(c.root, "deploy.yml", "", map[string]string{"version": "1"})
	assert.Equal(t, connect.CodeFailedPrecondition, ciErrCode(err), "a disabled workflow is not dispatched")
	assert.Contains(t, err.Error(), "disabled")

	c.openMR(c.root, "feat")
	c.waitFor("the other workflow's run", func() bool { return len(runsOfWorkflow(c.runs(), ".github/workflows/other.yml")) == 1 })
	assert.Empty(t, runsOfWorkflow(c.runs(), ".github/workflows/pr.yml"), "no pull_request run")
	runner.runJob(runner.await(), pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "other")

	_, err = c.setWorkflow(c.root, false, "deploy.yml")
	require.NoError(t, err)
	run, err := c.dispatch(c.root, "deploy.yml", "", map[string]string{"version": "1"})
	require.NoError(t, err)
	assert.Equal(t, "workflow_dispatch", run.GetEvent())
}

// A disabled workflow does not start a workflow_run workflow.
func TestWorkflowDisabledStartsNoWorkflowRunRun(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{
		".github/workflows/ci.yml":     wfOn("ci", "  push:\n"),
		".github/workflows/after.yml":  wfOn("after", "  workflow_run:\n    workflows: [ci]\n    types: [completed]\n"),
		".github/workflows/after2.yml": wfOn("after2", "  workflow_run:\n    workflows: [ci]\n    types: [completed]\n"),
	}, "")
	_, err := c.setWorkflow(c.root, true, "after.yml")
	require.NoError(t, err)
	// Pushed before the disable took effect? The first push ran ci: finish it.
	job := runner.await()
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ci")
	c.waitFor("after2 runs", func() bool { return len(runsOfWorkflow(c.runs(), ".github/workflows/after2.yml")) == 1 })
	assert.Empty(t, runsOfWorkflow(c.runs(), ".github/workflows/after.yml"))
}

// Scopes: repo:admin changes the state, repo:read lists, ci:write and
// repo:write do not change it; the shape is checked, not the existence.
func TestWorkflowStateAccessAndValidation(t *testing.T) {
	c := newCIHarness(t, nil)
	admin := c.api(c.token(scopes(domain.ScopeRepoRead, domain.ScopeRepoAdmin), "proj"))
	writer := c.api(c.token(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite, domain.ScopeCIWrite), "proj"))
	reader := c.api(c.token(scopes(domain.ScopeRepoRead), "proj"))
	stranger := c.api(c.token(scopes(domain.ScopeRepoAdmin), "other"))
	limited, _ := c.branchToken(scopes(domain.ScopeRepoRead, domain.ScopeRepoAdmin), []string{"agent/"}, "proj")

	_, err := c.setWorkflow(admin, true, "release.yml")
	require.NoError(t, err)
	_, err = c.setWorkflow(writer, true, "aur.yml")
	assert.Equal(t, connect.CodePermissionDenied, ciErrCode(err), "repo:write and ci:write are not enough")
	_, err = c.setWorkflow(writer, false, "release.yml")
	assert.Equal(t, connect.CodePermissionDenied, ciErrCode(err))
	_, err = c.setWorkflow(reader, true, "aur.yml")
	assert.Equal(t, connect.CodePermissionDenied, ciErrCode(err))
	_, err = c.setWorkflow(c.api(limited), true, "aur.yml")
	assert.Equal(t, connect.CodePermissionDenied, ciErrCode(err), "a token limited to branches may not")
	_, err = c.setWorkflow(stranger, true, "aur.yml")
	assert.Equal(t, connect.CodeNotFound, ciErrCode(err), "a repository the token cannot read")

	for _, bad := range []string{"", "release", "release.txt", "sub/release.yml", ".github/workflows/sub/release.yml", "../release.yml", ".github/other/release.yml"} {
		_, err = c.setWorkflow(admin, true, bad)
		assert.Equal(t, connect.CodeInvalidArgument, ciErrCode(err), "%q", bad)
	}

	assert.Equal(t, pb.WorkflowState_WORKFLOW_STATE_DISABLED, c.listWorkflows(reader)[releaseWF].GetState(), "repo:read lists")
	assert.Len(t, c.listWorkflows(reader), 1, "the refused changes left nothing")
	_, err = stranger.run.ListWorkflows(c.ctx, connect.NewRequest(&pb.ListWorkflowsRequest{Repo: c.repo}))
	assert.Equal(t, connect.CodeNotFound, ciErrCode(err))
	_, err = c.setWorkflow(admin, true, ".github/workflows/Odd Name.YAML")
	require.NoError(t, err, "only the shape is checked")

	_, err = admin.run.DisableWorkflow(c.ctx, connect.NewRequest(&pb.DisableWorkflowRequest{Repo: "nope", Workflow: "a.yml"}))
	assert.Equal(t, connect.CodeNotFound, ciErrCode(err))
}

// GO-001: the state is enforced where the run is stored. A dispatch that passed
// the check and is paused after resolving its ref (DispatchResolved) while the
// workflow is disabled creates no run.
func TestWorkflowDisabledBetweenDispatchCheckAndCreation(t *testing.T) {
	var c *ciHarness
	var disable func()
	c = newCIHarness(t, func(cfg *usecase.CIConfig) {
		cfg.DispatchResolved = func() {
			if disable != nil {
				disable()
			}
		}
	})
	c.push("main", map[string]string{".github/workflows/deploy.yml": dispatchWorkflow}, "")
	before := len(c.runs())

	disable = func() {
		disable = nil
		_, err := c.setWorkflow(c.root, true, "deploy.yml")
		require.NoError(t, err)
	}
	_, err := c.dispatch(c.root, "deploy.yml", "", map[string]string{"version": "1"})
	assert.Equal(t, connect.CodeFailedPrecondition, ciErrCode(err), "the disable landed after the check")
	assert.Contains(t, err.Error(), "disabled")
	assert.Len(t, c.runs(), before, "no run was created")

	_, err = c.setWorkflow(c.root, false, "deploy.yml")
	require.NoError(t, err)
	_, err = c.dispatch(c.root, "deploy.yml", "", map[string]string{"version": "1"})
	require.NoError(t, err, "enabled again")
}

// GO-001: an invalid workflow file gets no failed run either while it is
// disabled, on a push and on the failed-run path of a disable that races.
func TestWorkflowDisabledInvalidFileStartsNoFailedRun(t *testing.T) {
	c := newCIHarness(t, nil)
	c.registerRunner("alpha", "ubuntu-latest")
	_, err := c.setWorkflow(c.root, true, "broken.yml")
	require.NoError(t, err)
	c.push("main", map[string]string{
		".github/workflows/broken.yml": "on: push\njobs: [this is not a workflow\n",
		".github/workflows/bad2.yml":   "on: push\njobs: [this is not a workflow\n",
	}, "")
	c.waitFor("the invalid enabled file fails", func() bool { return len(runsOfWorkflow(c.runs(), ".github/workflows/bad2.yml")) == 1 })
	assert.Empty(t, runsOfWorkflow(c.runs(), ".github/workflows/broken.yml"), "a disabled invalid file reports nothing")
}

// GO-002: a schedule load that is overtaken by an enable (or a disable) neither
// loses the workflow nor fires a disabled one: the cache holds every workflow
// and the state is read when the schedule fires.
func TestScheduleLoadRacingWithEnableAndDisable(t *testing.T) {
	clk := &testClock{}
	clk.set("2026-03-04 10:00:30")
	var c *ciHarness
	var inLoad func()
	c = newCIHarness(t, func(cfg *usecase.CIConfig) {
		cfg.Clock = clk
		cfg.ScheduleTick = time.Hour
		cfg.ScheduleLoaded = func(domain.RepoName) {
			if f := inLoad; f != nil {
				inLoad = nil
				f()
			}
		}
	})
	c.registerRunner("alpha", "ubuntu-latest")
	ci := c.forge.CI
	tick := func(at string) {
		clk.set(at)
		ci.ScheduleNow(c.ctx)
	}
	// Disabled before the first push; the load of the schedules sees the file,
	// and an enable lands while it is being cached.
	_, err := c.setWorkflow(c.root, true, "nightly.yml")
	require.NoError(t, err)
	c.push("main", scheduleWorkflow("*/5 * * * *"), "")
	inLoad = func() {
		_, err := c.setWorkflow(c.root, false, "nightly.yml")
		require.NoError(t, err)
	}
	tick("2026-03-04 10:05:00")
	require.Len(t, c.scheduled(), 1, "enabled while the load was running: the workflow is there")
	assert.Equal(t, 1, ci.CachedSchedules(c.name("proj")))

	// And the other way: disabled while a load is running.
	require.NoError(t, func() error { _, err := c.setWorkflow(c.root, false, "nightly.yml"); return err }())
	c.push("main", scheduleWorkflow("*/5 * * * *", "0 0 1 1 *"), "second") // the tip moves: a new load
	inLoad = func() {
		_, err := c.setWorkflow(c.root, true, "nightly.yml")
		require.NoError(t, err)
	}
	tick("2026-03-04 10:10:00")
	assert.Len(t, c.scheduled(), 1, "disabled while the load was running: it does not fire")
	_, err = c.setWorkflow(c.root, false, "nightly.yml")
	require.NoError(t, err)
	tick("2026-03-04 10:15:00")
	assert.Len(t, c.scheduled(), 2, "enabled again: fires with the cached entry, nothing missing")
}

// GO-003: one file has one key. Non-canonical spellings are refused by the API,
// for disable, enable and dispatch alike.
func TestWorkflowNonCanonicalPathsAreRefused(t *testing.T) {
	c := newCIHarness(t, nil)
	c.push("main", map[string]string{".github/workflows/deploy.yml": dispatchWorkflow}, "")
	for _, bad := range []string{
		".github/workflows//release.yml", "./.github/workflows/release.yml", ".github/workflows/sub/../release.yml",
		".github/workflows/./release.yml",
	} {
		_, err := c.setWorkflow(c.root, true, bad)
		assert.Equal(t, connect.CodeInvalidArgument, ciErrCode(err), "disable %q", bad)
		_, err = c.setWorkflow(c.root, false, bad)
		assert.Equal(t, connect.CodeInvalidArgument, ciErrCode(err), "enable %q", bad)
		_, err = c.dispatch(c.root, bad, "", nil)
		assert.Equal(t, connect.CodeInvalidArgument, ciErrCode(err), "dispatch %q", bad)
	}
	assert.Empty(t, c.listWorkflows(c.root)[".github/workflows/release.yml"], "nothing was stored")
	_, err := c.dispatch(c.root, ".github/workflows/deploy.yml", "", map[string]string{"version": "1"})
	require.NoError(t, err, "the canonical path still dispatches")
}
