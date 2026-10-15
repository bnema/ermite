// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"encoding/json"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
)

// CI-3 D5, AC5: workflow_dispatch.

const dispatchWorkflow = `name: deploy
on:
  workflow_dispatch:
    inputs:
      version:
        type: string
        required: true
      dry:
        type: boolean
        default: false
      count:
        type: number
        default: 3
      level:
        type: choice
        options: [low, high]
      target:
        type: environment
        required: false
run-name: deploy ${{ inputs.version }} dry=${{ inputs.dry }}
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`

const pushOnlyWorkflow = "name: ci\non: push\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"

const jobTokenDispatcher = `name: dispatcher
on: push
permissions:
  actions: write
  contents: read
jobs:
  with:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
  without:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      - run: echo hi
  actionsonly:
    runs-on: ubuntu-latest
    permissions:
      actions: write
    steps:
      - run: echo hi
  readonly:
    runs-on: ubuntu-latest
    permissions: {}
    steps:
      - run: echo hi
`

func (c *ciHarness) dispatch(api *clients, path, ref string, inputs map[string]string) (*pb.Run, error) {
	c.t.Helper()
	res, err := api.run.DispatchWorkflow(c.ctx, connect.NewRequest(&pb.DispatchWorkflowRequest{
		Repo: c.repo, WorkflowPath: path, Ref: ref, Inputs: inputs,
	}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetRun(), nil
}

func TestDispatchInputsAreValidatedAndTyped(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/deploy.yml": dispatchWorkflow, ".github/workflows/ci.yml": pushOnlyWorkflow}, "")
	runner.runJob(runner.await(), pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "push run")

	_, err := c.dispatch(c.root, "deploy.yml", "", nil)
	assert.Equal(t, connect.CodeInvalidArgument, ciErrCode(err), "a required input is missing")
	assert.Contains(t, err.Error(), "Required input 'version'")
	_, err = c.dispatch(c.root, "deploy.yml", "", map[string]string{"version": "1", "nope": "x"})
	assert.Equal(t, connect.CodeInvalidArgument, ciErrCode(err), "an undeclared input")
	_, err = c.dispatch(c.root, "deploy.yml", "", map[string]string{"version": "1", "dry": "maybe"})
	assert.Equal(t, connect.CodeInvalidArgument, ciErrCode(err), "a boolean")
	_, err = c.dispatch(c.root, "deploy.yml", "", map[string]string{"version": "1", "count": "0x10"})
	assert.Equal(t, connect.CodeInvalidArgument, ciErrCode(err), "a number")
	_, err = c.dispatch(c.root, "deploy.yml", "", map[string]string{"version": "1", "level": "medium"})
	assert.Equal(t, connect.CodeInvalidArgument, ciErrCode(err), "a choice outside its options")
	_, err = c.dispatch(c.root, "ci.yml", "", nil)
	assert.Equal(t, connect.CodeFailedPrecondition, ciErrCode(err), "no workflow_dispatch trigger")
	_, err = c.dispatch(c.root, "missing.yml", "", nil)
	assert.Equal(t, connect.CodeNotFound, ciErrCode(err))
	_, err = c.dispatch(c.root, "deploy.yml", "nobranch", map[string]string{"version": "1"})
	assert.Equal(t, connect.CodeNotFound, ciErrCode(err), "a ref that does not exist")
	assert.Len(t, c.runs(), 1, "no refused dispatch made a run")

	run, err := c.dispatch(c.root, ".github/workflows/deploy.yml", "main", map[string]string{"version": "1.2", "dry": "true"})
	require.NoError(t, err)
	assert.Equal(t, "workflow_dispatch", run.GetEvent())
	assert.Equal(t, "refs/heads/main", run.GetRef())
	assert.True(t, run.GetTrusted())
	assert.Equal(t, "root", run.GetActor(), "the caller, as for a push")
	assert.Equal(t, "deploy 1.2 dry=true", run.GetName(), "run-name sees the typed inputs")
	assert.Equal(t, pb.CiStatus_CI_STATUS_QUEUED, run.GetStatus())

	job := runner.await()
	gh := c.ghContext(job)
	assert.Equal(t, "workflow_dispatch", gh["event_name"])
	assert.Equal(t, "refs/heads/main", gh["ref"])
	ev := gh["event"].(map[string]any)
	assert.Equal(t, map[string]any{"version": "1.2", "dry": "true", "count": "3", "level": "low"}, ev["inputs"], "github.event.inputs are strings, defaults applied")
	assert.Equal(t, ".github/workflows/deploy.yml", ev["workflow"])
	assert.Equal(t, "refs/heads/main", ev["ref"])

	var ctxs map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(job.GetContexts()), &ctxs))
	var inputs map[string]any
	require.NoError(t, json.Unmarshal(ctxs["inputs"], &inputs))
	assert.Equal(t, map[string]any{"version": "1.2", "dry": true, "count": float64(3), "level": "low", "target": ""}, inputs, "the inputs context is typed")
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
}

func TestDispatchReadsTheWorkflowAtTheRef(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/ci.yml": pushOnlyWorkflow}, "")
	runner.runJob(runner.await(), pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "push run")
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	c.push("feat", map[string]string{".github/workflows/ci.yml": "name: ci\non: [push, workflow_dispatch]\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"}, "")
	runner.runJob(runner.await(), pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "push run")

	_, err := c.dispatch(c.root, "ci.yml", "", nil)
	assert.Equal(t, connect.CodeFailedPrecondition, ciErrCode(err), "main has no workflow_dispatch")
	run, err := c.dispatch(c.root, "ci.yml", "feat", nil)
	require.NoError(t, err)
	assert.Equal(t, "refs/heads/feat", run.GetRef())
	runner.runJob(runner.await(), pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
}

func TestDispatchAccess(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/deploy.yml": dispatchWorkflow}, "")
	in := map[string]string{"version": "1"}

	readOnly := c.api(c.token(scopes(domain.ScopeRepoRead), "proj"))
	_, err := c.dispatch(readOnly, "deploy.yml", "", in)
	assert.Equal(t, connect.CodePermissionDenied, ciErrCode(err), "ci:write is needed")

	// A token with ci:write but limited to branch prefixes: its run is not trusted,
	// and it may only dispatch on a branch under its prefixes.
	secret, _ := c.branchToken(scopes(domain.ScopeRepoRead, domain.ScopeCIWrite), []string{"agent/claude/"}, "proj")
	limited := c.api(secret)
	_, err = c.dispatch(limited, "deploy.yml", "main", in)
	assert.Equal(t, connect.CodePermissionDenied, ciErrCode(err), "main is outside the prefixes")
	c.mustGit(c.wt, "checkout", "-q", "-b", "agent/claude/x")
	c.push("agent/claude/x", nil, "")
	run, err := c.dispatch(limited, "deploy.yml", "agent/claude/x", in)
	require.NoError(t, err)
	assert.False(t, run.GetTrusted(), "a branch-limited caller gets an untrusted run")
	job := runner.await()
	assert.False(t, job.GetTrusted())
	assert.Empty(t, job.GetSecrets())
}

func TestDispatchWithAJobToken(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/deploy.yml": dispatchWorkflow, ".github/workflows/d.yml": jobTokenDispatcher}, "")
	jobs := runner.jobsByKey(4)
	with, without := jobs["with"], jobs["without"]
	require.NotNil(t, with)
	require.NotNil(t, without)
	onlyActions, none := jobs["actionsonly"], jobs["readonly"]

	in := map[string]string{"version": "9"}
	_, err := c.dispatch(c.api(without.GetGithubToken()), "deploy.yml", "", in)
	assert.Equal(t, connect.CodePermissionDenied, ciErrCode(err), "no actions: write")
	run, err := c.dispatch(c.api(with.GetGithubToken()), "deploy.yml", "", in)
	require.NoError(t, err, "actions: write lets a job token dispatch (GitHub's exception)")
	assert.Equal(t, "workflow_dispatch", run.GetEvent())
	assert.Equal(t, "github-actions[bot]", run.GetActor())
	assert.True(t, run.GetTrusted(), "the dispatching run was trusted")

	// The repository is private and this token has no contents permission at all:
	// actions: write alone is enough, and a token without it is told nothing.
	_, err = c.dispatch(c.api(none.GetGithubToken()), "deploy.yml", "", in)
	assert.Equal(t, connect.CodeNotFound, ciErrCode(err), "no permission at all on a private repository: concealed")
	run, err = c.dispatch(c.api(onlyActions.GetGithubToken()), "deploy.yml", "", in)
	require.NoError(t, err, "actions: write without contents: read")
	assert.Equal(t, "workflow_dispatch", run.GetEvent())

	for _, j := range []*pb.Job{with, without, onlyActions, none} {
		runner.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
	}
	runner.runJob(runner.await(), pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "dispatched")
	runner.runJob(runner.await(), pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "dispatched")
}

func TestWorkflowRunCommand(t *testing.T) {
	c := newCIHarness(t, nil)
	h := &cliHarness{harness: c.harness, url: c.srv.URL}
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/deploy.yml": dispatchWorkflow}, "")

	bad := h.cli(t, adminToken, "workflow", "run", "deploy.yml", "--repo", "proj", "-f", "oops")
	require.Error(t, bad.err)
	missing := h.cli(t, adminToken, "workflow", "run", "deploy.yml", "--repo", "proj")
	require.Error(t, missing.err)
	assert.Contains(t, missing.err.Error(), "Required input 'version'")

	ok := h.cli(t, adminToken, "workflow", "run", "deploy.yml", "--repo", "proj", "--ref", "main", "-f", "version=a=b", "-f", "dry=true")
	require.NoError(t, ok.err, ok.stderr)
	assert.Contains(t, ok.stderr, "started on main")
	job := runner.await()
	ev := c.ghContext(job)["event"].(map[string]any)
	assert.Equal(t, "a=b", ev["inputs"].(map[string]any)["version"])

	list := h.cli(t, adminToken, "run", "list", "proj")
	require.NoError(t, list.err)
	assert.Contains(t, list.stdout, "EVENT")
	assert.Contains(t, list.stdout, "workflow_dispatch")
}
