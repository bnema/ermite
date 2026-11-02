// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"errors"
	"io"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
)

const everyBranchWorkflow = `name: all
on:
  push:
    branches: ['**']
    tags: ['v*']
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`

// A token limited to branch prefixes (ADR 0027) and CI. Reading runs, jobs and
// logs is not a write and stays open to whoever can read the repository.
// Cancelling and re-running a run is a write on the ref of the run (it stops or
// repeats the workflow of that ref), so it follows the prefixes: allowed on a
// branch under one, refused on any other branch and on a tag. Runners serve
// every repository and branch, so a limited token never manages them, even with
// runner:register.
func TestCIBranchLimitedToken(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	wf := map[string]string{".github/workflows/all.yml": everyBranchWorkflow}

	c.push("main", wf, "")
	c.waitFor("the run of main", func() bool { return len(c.runs()) == 1 })
	runner.runJob(runner.await(), pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "log of main")
	c.push("agent/claude/x", nil, "")
	c.waitFor("the run of the agent branch", func() bool { return len(c.runs()) == 2 })
	runner.runJob(runner.await(), pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "log of the agent branch")
	c.mustGit(c.wt, "tag", "v1")
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), "v1")
	c.waitFor("the run of the tag", func() bool { return len(c.runs()) == 3 })
	c.waitFor("main is done", func() bool { return c.runs()[2].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })

	var mainRun, agentRun, tagRun *pb.Run
	for _, r := range c.runs() {
		switch r.GetRef() {
		case "refs/heads/main":
			mainRun = r
		case "refs/heads/agent/claude/x":
			agentRun = r
		case "refs/tags/v1":
			tagRun = r
		}
	}
	require.NotNil(t, mainRun)
	require.NotNil(t, agentRun)
	require.NotNil(t, tagRun)

	all := scopes(domain.ScopeRepoRead, domain.ScopeCIWrite, domain.ScopeRunnerRegister)
	secret, _ := c.branchToken(all, []string{"agent/claude/"}, "proj")
	limited := c.api(secret)
	// The same token without the limit, to prove the refusals come from it.
	unlimited := c.api(c.token(all, "proj"))
	cancel := func(api *clients, r *pb.Run) error {
		_, err := api.run.CancelRun(c.ctx, &pb.CancelRunRequest{Repo: "proj", RunId: r.GetId()})
		return err
	}
	rerun := func(api *clients, r *pb.Run) error {
		_, err := api.run.RerunRun(c.ctx, &pb.RerunRunRequest{Repo: "proj", RunId: r.GetId()})
		return err
	}

	t.Run("reads", func(t *testing.T) {
		list, err := limited.run.ListRuns(c.ctx, &pb.ListRunsRequest{Repo: "proj"})
		require.NoError(t, err)
		assert.Len(t, list.GetRuns(), 3, "every run is readable, not only those of its prefixes")
		got, err := limited.run.GetRun(c.ctx, &pb.GetRunRequest{Repo: "proj", RunId: mainRun.GetId()})
		require.NoError(t, err)
		require.NotEmpty(t, got.GetJobs())
		log, err := limited.run.GetJobLog(c.ctx, &pb.GetJobLogRequest{Repo: "proj", JobId: got.GetJobs()[0].GetId()})
		require.NoError(t, err)
		var text string
		var streamErr error
		for {
			msg, err := log.Receive()
			if err != nil {
				streamErr = err
				break
			}
			for _, l := range msg.GetLines() {
				text += l.GetText()
			}
		}
		if errors.Is(streamErr, io.EOF) {
			streamErr = nil
		}
		require.NoError(t, streamErr)
		assert.Contains(t, text, "log of main")
		_, err = limited.run.ListChecks(c.ctx, &pb.ListChecksRequest{Repo: "proj", Sha: mainRun.GetSha()})
		require.NoError(t, err)
	})

	t.Run("writes outside the prefixes are refused", func(t *testing.T) {
		for name, r := range map[string]*pb.Run{"main": mainRun, "a tag": tagRun} {
			assert.Equal(t, connect.CodePermissionDenied, ciErrCode(cancel(limited, r)), "cancel "+name)
			assert.Equal(t, connect.CodePermissionDenied, ciErrCode(rerun(limited, r)), "rerun "+name)
		}
		// Still there: the refused rerun started no new attempt.
		assert.EqualValues(t, 1, c.getRun(mainRun.GetId()).GetRun().GetAttempt())
		// Without the limit the same token is allowed (rerun) or reaches the
		// state rule of the run (cancel of a completed run).
		assert.Equal(t, connect.CodeFailedPrecondition, ciErrCode(cancel(unlimited, mainRun)))
		require.NoError(t, rerun(unlimited, mainRun))
	})

	t.Run("writes under the prefixes are allowed", func(t *testing.T) {
		require.NoError(t, rerun(limited, agentRun))
		assert.EqualValues(t, 2, c.getRun(agentRun.GetId()).GetRun().GetAttempt())
		require.NoError(t, cancel(limited, agentRun))
	})

	t.Run("runners are out of reach", func(t *testing.T) {
		// Not restricted to a repository, so only the branch limit can refuse it.
		secret, _ := c.branchToken(scopes(domain.ScopeRunnerRegister), []string{"agent/claude/"})
		limited := c.api(secret)
		_, err := limited.runner.RegisterRunner(c.ctx, &pb.RegisterRunnerRequest{
			Name: "evil", Version: "v1", Isolation: pb.RunnerIsolation_RUNNER_ISOLATION_VM, Arch: "x64", Os: "linux", Replace: true,
		})
		assert.Equal(t, connect.CodePermissionDenied, ciErrCode(err), "register")
		_, err = limited.run.ListRunners(c.ctx, &pb.ListRunnersRequest{})
		assert.Equal(t, connect.CodePermissionDenied, ciErrCode(err), "list")
		_, err = limited.run.RemoveRunner(c.ctx, &pb.RemoveRunnerRequest{RunnerId: runner.id})
		assert.Equal(t, connect.CodePermissionDenied, ciErrCode(err), "remove")

		// The same scopes without the limit manage runners (not restricted to a
		// repository, which the runner rules already demand).
		open := c.api(c.token(scopes(domain.ScopeRunnerRegister)))
		list, err := open.run.ListRunners(c.ctx, &pb.ListRunnersRequest{})
		require.NoError(t, err)
		assert.Len(t, list.GetRunners(), 1, "the runner is still registered")
	})
}
