// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/usecase"
)

const oneJob = `on:
  push:
    %s
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`

func workflowWith(filter string) string { return fmt.Sprintf(oneJob, filter) }

// AC3: filters, tags, skip directives, invalid files and deletions.
func TestCITriggerRules(t *testing.T) {
	c := newCIHarness(t, nil)
	c.registerRunner("alpha", "ubuntu-latest")
	count := func() int { return len(c.runs()) }
	settle := func() { time.Sleep(300 * time.Millisecond) } // a push that runs nothing leaves nothing to wait for

	// A workflow with a branch filter does not run for another branch.
	c.push("main", map[string]string{".github/workflows/only-main.yml": workflowWith("branches: [main]"), "src/a.go": "package a\n"}, "")
	c.waitFor("main runs", func() bool { return count() == 1 })
	c.push("other", map[string]string{"x.txt": "x\n"}, "")
	settle()
	assert.Equal(t, 1, count(), "branches: [main] does not run for other")

	// A paths filter looks at the files of the push.
	c.push("main", map[string]string{".github/workflows/paths.yml": workflowWith("paths: ['docs/**']")}, "")
	c.waitFor("only-main runs again for the new commit", func() bool { return count() == 2 })
	c.push("main", map[string]string{"docs/readme.md": "doc\n"}, "")
	c.waitFor("both run for docs", func() bool { return count() == 4 })
	c.push("main", map[string]string{"src/b.go": "package b\n"}, "")
	c.waitFor("only-main runs for src", func() bool { return count() == 5 })
	settle()
	assert.Equal(t, 5, count(), "the paths filter kept paths.yml out")

	// [skip ci] on the head commit.
	c.push("main", map[string]string{"src/c.go": "package c\n"}, "wip [skip ci]")
	settle()
	assert.Equal(t, 5, count())

	// An invalid workflow file is a failed run without a job.
	c.push("main", map[string]string{".github/workflows/broken.yml": "on: push\njobs:\n  j:\n    steps: [\n"}, "")
	c.waitFor("the invalid file gives a run", func() bool {
		for _, r := range c.runs() {
			if r.GetWorkflowPath() == ".github/workflows/broken.yml" {
				return true
			}
		}
		return false
	})
	var broken *pb.Run
	for _, r := range c.runs() {
		if r.GetWorkflowPath() == ".github/workflows/broken.yml" {
			broken = r
		}
	}
	assert.Equal(t, pb.CiStatus_CI_STATUS_COMPLETED, broken.GetStatus())
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_FAILURE, broken.GetConclusion())
	assert.NotEmpty(t, broken.GetError())
	assert.Empty(t, c.getRun(broken.GetId()).GetJobs(), "an invalid file has no job")
	sha := strings.TrimSpace(c.mustGit(c.wt, "rev-parse", "HEAD"))
	var failed []string
	for _, ck := range c.checks(sha) {
		if ck.GetConclusion() == pb.CiConclusion_CI_CONCLUSION_FAILURE {
			failed = append(failed, ck.GetName())
		}
	}
	assert.Equal(t, []string{".github/workflows/broken.yml"}, failed, "and a failed check on the commit")

	// A push never fails for CI: the push above succeeded (mustGit). A deletion
	// runs nothing.
	before := count()
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), ":other")
	settle()
	assert.Equal(t, before, count())
}

// AC3: tags run tag-filtered workflows; more than 3 tags in one push run none.
func TestCITags(t *testing.T) {
	c := newCIHarness(t, nil)
	c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/rel.yml": workflowWith("tags: ['v*']")}, "")
	settle := func() { time.Sleep(300 * time.Millisecond) }
	settle()
	assert.Empty(t, c.runs(), "a tags-only workflow does not run for a branch")

	c.mustGit(c.wt, "tag", "v1")
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), "v1")
	c.waitFor("a tag runs", func() bool { return len(c.runs()) == 1 })
	assert.Equal(t, "refs/tags/v1", c.runs()[0].GetRef())

	for _, tag := range []string{"v2", "v3", "v4", "v5"} {
		c.mustGit(c.wt, "tag", tag)
	}
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), "v2", "v3", "v4", "v5")
	settle()
	assert.Len(t, c.runs(), 1, "more than 3 tags in one push run nothing")
}

// AC6: cancelling a run asks the runner to stop through WatchJob; a runner that
// does not answer is ended by the server.
func TestCICancelReachesTheRunnerAndIsEnforced(t *testing.T) {
	c := newCIHarness(t, func(cfg *usecase.CIConfig) {
		cfg.CancelGrace, cfg.Tick, cfg.PollTimeout = 400*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond
	})
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/ci.yml": ciWorkflow}, "")
	job := runner.await()
	api := runner.jobAPI(job)
	_, err := api.runner.Heartbeat(c.ctx, connect.NewRequest(&pb.HeartbeatRequest{JobId: job.GetJobId()}))
	require.NoError(t, err)

	watch, err := api.runner.WatchJob(c.ctx, connect.NewRequest(&pb.WatchJobRequest{JobId: job.GetJobId()}))
	require.NoError(t, err)
	run := c.runs()[0]

	// A reader without ci:write cannot cancel.
	reader := c.api(c.token(scopes(domain.ScopeRepoRead)))
	_, err = reader.run.CancelRun(c.ctx, connect.NewRequest(&pb.CancelRunRequest{Repo: "proj", RunId: run.GetId()}))
	assert.Equal(t, connect.CodePermissionDenied, ciErrCode(err))

	_, err = c.root.run.CancelRun(c.ctx, connect.NewRequest(&pb.CancelRunRequest{Repo: "proj", RunId: run.GetId()}))
	require.NoError(t, err)
	// Cancelling again changes nothing.
	_, err = c.root.run.CancelRun(c.ctx, connect.NewRequest(&pb.CancelRunRequest{Repo: "proj", RunId: run.GetId()}))
	require.NoError(t, err)

	var cancel *pb.CancelRequest
	for watch.Receive() {
		if m := watch.Msg().GetCancel(); m != nil {
			cancel = m
			break
		}
	}
	require.NotNil(t, cancel, "the runner is told")
	assert.Equal(t, pb.CancelReason_CANCEL_REASON_USER, cancel.GetReason())

	// The queued job that needs it never ran: cancelled at once. The running
	// one is ended by the server after the grace period.
	c.waitFor("the server ends the job", func() bool {
		return c.runs()[0].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED
	})
	detail := c.getRun(run.GetId())
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_CANCELLED, detail.GetRun().GetConclusion())
	for _, j := range detail.GetJobs() {
		assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_CANCELLED, j.GetConclusion(), j.GetKey())
	}
	assert.Contains(t, detail.GetJobs()[0].GetError(), "did not answer")

	// The runner's late CompleteJob returns the server's result.
	res, err := api.runner.CompleteJob(c.ctx, connect.NewRequest(&pb.CompleteJobRequest{
		JobId: job.GetJobId(), Conclusion: pb.CiConclusion_CI_CONCLUSION_CANCELLED,
	}))
	require.NoError(t, err)
	assert.False(t, res.Msg.GetApplied())

	// A completed run cannot be cancelled.
	_, err = c.root.run.CancelRun(c.ctx, connect.NewRequest(&pb.CancelRunRequest{Repo: "proj", RunId: run.GetId()}))
	assert.Equal(t, connect.CodeFailedPrecondition, ciErrCode(err))
}

// A running job without heartbeats fails with GitHub's lost-runner message.
func TestCILostRunnerFailsTheJob(t *testing.T) {
	c := newCIHarness(t, func(cfg *usecase.CIConfig) {
		cfg.HeartbeatDeadline, cfg.Tick, cfg.PollTimeout = 300*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond
	})
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/ci.yml": strings.Split(ciWorkflow, "  test:")[0]}, "")
	job := runner.await()
	_, err := runner.jobAPI(job).runner.Heartbeat(c.ctx, connect.NewRequest(&pb.HeartbeatRequest{JobId: job.GetJobId()}))
	require.NoError(t, err)

	c.waitFor("the job is lost", func() bool { return c.runs()[0].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })
	j := c.getRun(c.runs()[0].GetId()).GetJobs()[0]
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_FAILURE, j.GetConclusion())
	assert.Contains(t, j.GetError(), "The self-hosted runner: alpha lost communication with the server")
}

// AC6: a re-run is a new attempt of the same run; the failed-only variant keeps
// the jobs that passed.
func TestCIRerun(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/ci.yml": ciWorkflow}, "")
	run := func() *pb.Run { return c.runs()[0] }

	build := runner.await()
	runner.runJob(build, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
	test := runner.await()
	runner.runJob(test, pb.CiConclusion_CI_CONCLUSION_FAILURE, nil, "red")
	c.waitFor("the run fails", func() bool { return run().GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_FAILURE, run().GetConclusion())
	id, number := run().GetId(), run().GetNumber()

	// A running run cannot be re-run; a completed one can, by who has ci:write.
	reader := c.api(c.token(scopes(domain.ScopeRepoRead)))
	_, err := reader.run.RerunRun(c.ctx, connect.NewRequest(&pb.RerunRunRequest{Repo: "proj", RunId: id}))
	assert.Equal(t, connect.CodePermissionDenied, ciErrCode(err))

	res, err := c.root.run.RerunRun(c.ctx, connect.NewRequest(&pb.RerunRunRequest{Repo: "proj", RunId: id, FailedOnly: true}))
	require.NoError(t, err)
	assert.Equal(t, id, res.Msg.GetRun().GetId())
	assert.Equal(t, number, res.Msg.GetRun().GetNumber())
	assert.EqualValues(t, 2, res.Msg.GetRun().GetAttempt())

	// build keeps its attempt-1 result; only test runs again.
	again := runner.await()
	assert.Equal(t, "test", again.GetJobKey())
	assert.EqualValues(t, 2, again.GetAttempt())
	runner.runJob(again, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "green")
	c.waitFor("attempt 2 passes", func() bool {
		r := run()
		return r.GetAttempt() == 2 && r.GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED
	})
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, run().GetConclusion())
	detail := c.getRun(id)
	require.Len(t, detail.GetJobs(), 2)
	assert.EqualValues(t, 1, detail.GetJobs()[0].GetAttempt(), "build was kept from attempt 1")
	assert.EqualValues(t, 2, detail.GetJobs()[1].GetAttempt())

	// Attempt 1 is still readable, with its failure.
	first, err := c.root.run.GetRun(c.ctx, connect.NewRequest(&pb.GetRunRequest{Repo: "proj", RunId: id, Attempt: 1}))
	require.NoError(t, err)
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_FAILURE, first.Msg.GetRun().GetConclusion())
	assert.EqualValues(t, 2, first.Msg.GetLatestAttempt())

	// Re-running a run with nothing failed is refused with failed-only.
	_, err = c.root.run.RerunRun(c.ctx, connect.NewRequest(&pb.RerunRunRequest{Repo: "proj", RunId: id, FailedOnly: true}))
	assert.Equal(t, connect.CodeFailedPrecondition, ciErrCode(err))
}

// A matrix is expanded when its job is ready, with fail-fast.
func TestCIMatrixAndFailFast(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/m.yml": `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        go: ['1.26', '1.27']
    steps:
      - run: echo ${{ matrix.go }}
  after:
    needs: build
    if: always()
    runs-on: ubuntu-latest
    steps:
      - run: echo done
`}, "")
	a := runner.await()
	b := runner.await()
	assert.Equal(t, "build", a.GetJobKey())
	assert.JSONEq(t, `{"go":"1.26"}`, a.GetMatrix())
	assert.JSONEq(t, `{"go":"1.27"}`, b.GetMatrix())
	assert.Contains(t, a.GetContexts(), `"job-total":2`)

	jobs := c.getRun(c.runs()[0].GetId()).GetJobs()
	var names []string
	for _, j := range jobs {
		names = append(names, j.GetName())
	}
	assert.Equal(t, []string{"build (1.26)", "build (1.27)", "after"}, names)

	// The first fails: fail-fast asks the second to stop.
	apiB := runner.jobAPI(b)
	_, err := apiB.runner.Heartbeat(c.ctx, connect.NewRequest(&pb.HeartbeatRequest{JobId: b.GetJobId()}))
	require.NoError(t, err)
	watch, err := apiB.runner.WatchJob(c.ctx, connect.NewRequest(&pb.WatchJobRequest{JobId: b.GetJobId()}))
	require.NoError(t, err)
	runner.runJob(a, pb.CiConclusion_CI_CONCLUSION_FAILURE, nil, "x")
	var reason pb.CancelReason
	for watch.Receive() {
		if m := watch.Msg().GetCancel(); m != nil {
			reason = m.GetReason()
			break
		}
	}
	assert.Equal(t, pb.CancelReason_CANCEL_REASON_FAIL_FAST, reason)
	_, err = apiB.runner.CompleteJob(c.ctx, connect.NewRequest(&pb.CompleteJobRequest{JobId: b.GetJobId(), Conclusion: pb.CiConclusion_CI_CONCLUSION_CANCELLED}))
	require.NoError(t, err)

	// "after" has if: always(), so it runs once both ended.
	after := runner.await()
	assert.Equal(t, "after", after.GetJobKey())
	assert.Contains(t, after.GetContexts(), `"result":"failure"`)
	runner.runJob(after, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "done")
	c.waitFor("the run ends", func() bool { return c.runs()[0].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_FAILURE, c.runs()[0].GetConclusion())
}

// A job with a feature of a later phase fails clearly, never silently.
func TestCIUnsupportedFeatureFailsTheJob(t *testing.T) {
	c := newCIHarness(t, nil)
	c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/u.yml": `on: push
jobs:
  j:
    runs-on: ubuntu-latest
    concurrency: group
    steps:
      - run: echo
`}, "")
	c.waitFor("the run fails", func() bool { return len(c.runs()) == 1 && c.runs()[0].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })
	j := c.getRun(c.runs()[0].GetId()).GetJobs()[0]
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_FAILURE, j.GetConclusion())
	assert.Equal(t, "not supported yet: concurrency", j.GetError())
}

// A job for a label no runner has is "waiting for a runner" and fails after the
// queue timeout.
func TestCIQueueTimeout(t *testing.T) {
	c := newCIHarness(t, func(cfg *usecase.CIConfig) { cfg.QueueTimeout, cfg.Tick = 300*time.Millisecond, 20*time.Millisecond })
	c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/q.yml": strings.Replace(workflowWith("branches: [main]"), "ubuntu-latest", "gpu", 1)}, "")
	c.waitFor("the job fails", func() bool { return len(c.runs()) == 1 && c.runs()[0].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })
	j := c.getRun(c.runs()[0].GetId()).GetJobs()[0]
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_FAILURE, j.GetConclusion())
	assert.Contains(t, j.GetError(), "was not acquired")
}

// The runner API validates what it is given.
func TestCIRunnerRegistrationRules(t *testing.T) {
	c := newCIHarness(t, nil)
	reg := func(api *clients, name string, replace bool) error {
		_, err := api.runner.RegisterRunner(c.ctx, connect.NewRequest(&pb.RegisterRunnerRequest{
			Name: name, Version: "v1", Isolation: pb.RunnerIsolation_RUNNER_ISOLATION_VM, Arch: "x64", Os: "linux", Replace: replace,
		}))
		return err
	}
	assert.Equal(t, connect.CodeUnauthenticated, ciErrCode(reg(c.api(""), "a", false)))
	assert.Equal(t, connect.CodePermissionDenied, ciErrCode(reg(c.api(c.token(scopes(domain.ScopeRepoRead))), "a", false)))
	assert.Equal(t, connect.CodePermissionDenied, ciErrCode(reg(c.api(c.token(scopes(domain.ScopeRunnerRegister), "proj")), "a", false)), "a repository-restricted token cannot register a runner")
	reg1 := c.api(c.token(scopes(domain.ScopeRunnerRegister)))
	require.NoError(t, reg(reg1, "a", false))
	assert.Equal(t, connect.CodeAlreadyExists, ciErrCode(reg(reg1, "a", false)))
	require.NoError(t, reg(reg1, "a", true), "replace")
	assert.Equal(t, connect.CodeInvalidArgument, ciErrCode(reg(reg1, "bad name!", false)))

	list, err := reg1.run.ListRunners(c.ctx, connect.NewRequest(&pb.ListRunnersRequest{}))
	require.NoError(t, err)
	require.Len(t, list.Msg.GetRunners(), 1)
	assert.Equal(t, []string{"linux", "self-hosted", "x64"}, list.Msg.GetRunners()[0].GetLabels(), "implicit labels, sorted")
}

// AC2: after a restart of the server, running jobs go on with their tokens,
// queued jobs stay queued, and a job whose runner kept quiet during the outage
// is not failed for it.
func TestCIRestartKeepsRunningAndQueuedJobs(t *testing.T) {
	c := newCIHarness(t, func(cfg *usecase.CIConfig) { cfg.HeartbeatDeadline = 2 * time.Second })
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/ci.yml": ciWorkflow}, "")
	job := runner.await() // "build"; "test" waits for it
	api := runner.jobAPI(job)
	_, err := api.runner.Heartbeat(c.ctx, connect.NewRequest(&pb.HeartbeatRequest{JobId: job.GetJobId()}))
	require.NoError(t, err)
	_, err = api.runner.AppendLog(c.ctx, connect.NewRequest(&pb.AppendLogRequest{
		JobId: job.GetJobId(), Step: 2, Offset: 0, Lines: []*pb.LogLine{{Time: now(), Text: "before the restart"}},
	}))
	require.NoError(t, err)

	// Stop the server and start another on the same data directory.
	cfg, hookExe := c.forge.cfg, binary(t)
	c.srv.Close()
	c.forge.Close(c.ctx)
	f2, err := newForge(c.ctx, cfg, options{hookExe: hookExe, ci: func(cc *usecase.CIConfig) { cc.HeartbeatDeadline = 2 * time.Second }})
	require.NoError(t, err)
	t.Cleanup(func() { f2.Close(context.WithoutCancel(c.ctx)) })
	srv := httptest.NewServer(f2.server.Handler())
	t.Cleanup(srv.Close)
	api2 := newClients(srv.URL+"/-/api", job.GetJobToken())

	// The job token still works, and the log goes on from where it was.
	_, err = api2.runner.Heartbeat(c.ctx, connect.NewRequest(&pb.HeartbeatRequest{JobId: job.GetJobId()}))
	require.NoError(t, err, "the job token survives a restart")
	next, err := api2.runner.AppendLog(c.ctx, connect.NewRequest(&pb.AppendLogRequest{
		JobId: job.GetJobId(), Step: 2, Offset: 1, Lines: []*pb.LogLine{{Time: now(), Text: "after the restart"}},
	}))
	require.NoError(t, err)
	assert.EqualValues(t, 2, next.Msg.GetNextOffset())

	root2 := newClients(srv.URL+"/-/api", adminToken)
	runs, err := root2.run.ListRuns(c.ctx, connect.NewRequest(&pb.ListRunsRequest{Repo: "proj"}))
	require.NoError(t, err)
	require.Len(t, runs.Msg.GetRuns(), 1)
	assert.Equal(t, pb.CiStatus_CI_STATUS_IN_PROGRESS, runs.Msg.GetRuns()[0].GetStatus())

	// Finish the job on the new server: its dependant is planned and ready.
	_, err = api2.runner.CompleteJob(c.ctx, connect.NewRequest(&pb.CompleteJobRequest{
		JobId: job.GetJobId(), Conclusion: pb.CiConclusion_CI_CONCLUSION_SUCCESS,
	}))
	require.NoError(t, err)
	got := make(chan *pb.Job, 1)
	go func() {
		for range 50 {
			res, err := newClients(srv.URL+"/-/api", c.runnerSecret(runner)).runner.FetchJob(c.ctx, connect.NewRequest(&pb.FetchJobRequest{}))
			if err == nil && res.Msg.GetJob() != nil {
				got <- res.Msg.GetJob()
				return
			}
		}
		got <- nil
	}()
	select {
	case j := <-got:
		require.NotNil(t, j, "the runner token and the queue survive a restart")
		assert.Equal(t, "test", j.GetJobKey())
	case <-time.After(30 * time.Second):
		t.Fatal("no job after the restart")
	}
}

// AC5: a head without any check merges, and so does one whose checks passed; a
// merge runs the push workflows on the target.
func TestCIMergeWithoutChecksAndWithPassingChecks(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{"README": "x\n"}, "")
	headOf := func(branch string) string { return strings.TrimSpace(c.bareGit("proj", "rev-parse", branch)) }
	merge := func(n int64, head string) error {
		_, err := c.root.mr.MergeMergeRequest(c.ctx, connect.NewRequest(&pb.MergeMergeRequestRequest{Repo: "proj", Number: n, HeadId: head}))
		return err
	}

	// No workflow on the branch: no check, the merge goes through.
	c.push("plain", map[string]string{"a.txt": "a\n"}, "")
	mr1, err := c.root.mr.CreateMergeRequest(c.ctx, connect.NewRequest(&pb.CreateMergeRequestRequest{Repo: "proj", Title: "plain", SourceBranch: "plain"}))
	require.NoError(t, err)
	require.NoError(t, merge(mr1.Msg.GetMergeRequest().GetNumber(), headOf("plain")))

	// A branch with a workflow: merge after its check passed. The merge itself
	// triggers the workflow on main (GitHub runs push workflows on a merge).
	c.mustGit(c.wt, "checkout", "-q", "main")
	c.mustGit(c.wt, "pull", "-q", c.urlAuth(adminToken, "/proj.git"), "main")
	c.mustGit(c.wt, "checkout", "-q", "-b", "ci")
	c.writeFile(c.wt, ".github/workflows/ci.yml", []byte(strings.Replace(workflowWith("branches: [main, ci]"), "echo hi", "echo ci", 1)))
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), "ci")
	head := headOf("ci")
	c.waitFor("the run on ci", func() bool { return len(c.checks(head)) == 1 })
	mr2, err := c.root.mr.CreateMergeRequest(c.ctx, connect.NewRequest(&pb.CreateMergeRequestRequest{Repo: "proj", Title: "ci", SourceBranch: "ci"}))
	require.NoError(t, err)
	job := runner.await()
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
	c.waitFor("the check passes", func() bool {
		cs := c.checks(head)
		return len(cs) == 1 && cs[0].GetConclusion() == pb.CiConclusion_CI_CONCLUSION_SUCCESS
	})
	require.NoError(t, merge(mr2.Msg.GetMergeRequest().GetNumber(), head))

	// The fast-forward merge moved main to the same commit: the workflow runs
	// again for main, with the owner as the actor.
	c.waitFor("the merge triggers a run on main", func() bool {
		for _, r := range c.runs() {
			if r.GetRef() == "refs/heads/main" && r.GetSha() == head {
				return true
			}
		}
		return false
	})
	for _, r := range c.runs() {
		if r.GetRef() == "refs/heads/main" {
			assert.Equal(t, "owner", r.GetActor())
		}
	}
}

// Repository deletion removes the runs, the checks and the logs; a repository
// created again starts at run number 1.
func TestCIRepositoryDeletionRemovesRunsAndLogs(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	sha := c.push("main", map[string]string{".github/workflows/ci.yml": strings.Split(ciWorkflow, "  test:")[0]}, "")
	job := runner.await()
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "line")
	logDir := filepath.Join(c.data, "ci", "logs")
	entries, err := os.ReadDir(logDir)
	require.NoError(t, err)
	require.NotEmpty(t, entries, "the log is a file under ci/logs")

	_, err = c.root.repo.DeleteRepo(c.ctx, connect.NewRequest(&pb.DeleteRepoRequest{Repo: "proj"}))
	require.NoError(t, err)
	_, err = c.root.run.ListRuns(c.ctx, connect.NewRequest(&pb.ListRunsRequest{Repo: "proj"}))
	assert.Equal(t, connect.CodeNotFound, ciErrCode(err))
	entries, err = os.ReadDir(logDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "the logs are removed with the repository")

	_, err = c.forge.Repos.Create(c.ctx, domain.Root(), ports.CreateRepoParams{Name: c.name("proj"), Visibility: domain.VisibilityPrivate})
	require.NoError(t, err)
	assert.Empty(t, c.checks(sha), "no check survives the repository")
	c.push("main", map[string]string{".github/workflows/ci.yml": strings.Split(ciWorkflow, "  test:")[0] + "# again\n"}, "again")
	c.waitFor("a run", func() bool { return len(c.runs()) == 1 })
	assert.EqualValues(t, 1, c.runs()[0].GetNumber(), "run numbers start again with the repository")
	assert.Greater(t, c.runs()[0].GetId(), int64(1), "run ids are never reused")
}

// The events of a run and its jobs are published on the repository topic.
func TestCIEventsAreStreamedOnTheRepositoryTopic(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	s := c.watch(c.root, "", "repo:proj")
	c.push("main", map[string]string{".github/workflows/ci.yml": strings.Split(ciWorkflow, "  test:")[0]}, "")
	job := runner.await()
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "x")

	var runUpdates, jobUpdates int
	var lastRun *pb.Run
	deadline := time.After(10 * time.Second)
	for lastRun.GetStatus() != pb.CiStatus_CI_STATUS_COMPLETED {
		select {
		case ev := <-s.events:
			if r := ev.GetRunUpdated(); r != nil {
				runUpdates++
				if r.GetRun().GetVersion() >= lastRun.GetVersion() {
					lastRun = r.GetRun()
				}
			}
			if ev.GetJobUpdated() != nil {
				jobUpdates++
			}
		case <-deadline:
			t.Fatalf("no completed run event: %d run and %d job events", runUpdates, jobUpdates)
		}
	}
	assert.GreaterOrEqual(t, runUpdates, 2)
	assert.GreaterOrEqual(t, jobUpdates, 2)

	// They are never sent on "home".
	home := c.watch(c.root, "", "home")
	_, err := c.root.run.RerunRun(c.ctx, connect.NewRequest(&pb.RerunRunRequest{Repo: "proj", RunId: lastRun.GetId()}))
	require.NoError(t, err)
	c.sentinel(t, home, "sentinel")
}

// A new branch at a commit that already exists, and a branch rewound to an
// ancestor, bring no new commit: GitHub still runs the push workflows (those
// without a paths filter, as no file changed).
func TestCIPushWithoutNewCommitsStillRuns(t *testing.T) {
	c := newCIHarness(t, nil)
	c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/any.yml": workflowWith("branches: ['**']")}, "")
	c.waitFor("main runs", func() bool { return len(c.runs()) == 1 })
	first := strings.TrimSpace(c.mustGit(c.wt, "rev-parse", "HEAD"))

	// A new branch at the commit main already has.
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), "HEAD:refs/heads/same")
	c.waitFor("the new branch runs", func() bool { return len(c.runs()) == 2 })
	assert.Equal(t, "refs/heads/same", c.runs()[0].GetRef())
	assert.Equal(t, first, c.runs()[0].GetSha())

	// A branch moved forward, then rewound to its ancestor (a forced push).
	c.push("main", map[string]string{"a.txt": "a\n"}, "")
	c.waitFor("the push runs", func() bool { return len(c.runs()) == 3 })
	c.mustGit(c.wt, "push", "-q", "-f", c.urlAuth(adminToken, "/proj.git"), first+":refs/heads/main")
	c.waitFor("the rewind runs", func() bool { return len(c.runs()) == 4 })
	assert.Equal(t, first, c.runs()[0].GetSha())
}

// CI is on once a runner is registered: before that a push makes no run and no
// check; after, it does.
func TestCINoRunnerNoRunsNoChecks(t *testing.T) {
	c := newCIHarness(t, nil)
	sha := c.push("main", map[string]string{".github/workflows/ci.yml": ciWorkflow}, "")
	time.Sleep(400 * time.Millisecond)
	assert.Empty(t, c.runs())
	assert.Empty(t, c.checks(sha))

	c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{"a.txt": "a\n"}, "")
	c.waitFor("a run once a runner exists", func() bool { return len(c.runs()) == 1 })
}

// A job that fail-fast cancelled while its assignment was not acknowledged is
// over when the assignment is cancelled: it is not given to a runner again.
func TestCICancelledUnacknowledgedAssignmentIsNotAssignedAgain(t *testing.T) {
	c := newCIHarness(t, func(cfg *usecase.CIConfig) {
		cfg.AckTimeout, cfg.Tick, cfg.PollTimeout = 300*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond
	})
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/m.yml": `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        n: [1, 2]
    steps:
      - run: echo ${{ matrix.n }}
`}, "")
	a, b := runner.await(), runner.await() // both assigned, neither acknowledged yet
	assert.NotEqual(t, a.GetJobId(), b.GetJobId())

	runner.runJob(a, pb.CiConclusion_CI_CONCLUSION_FAILURE, nil, "x") // fail-fast asks b to stop

	c.waitFor("the run ends", func() bool { return c.runs()[0].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })
	for _, j := range c.getRun(c.runs()[0].GetId()).GetJobs() {
		if j.GetId() == b.GetJobId() {
			assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_CANCELLED, j.GetConclusion())
		}
	}
	again, err := runner.fetch("")
	require.NoError(t, err)
	assert.Nil(t, again, "a cancelled job is not given again")
}

// Once the runner acknowledged a job, a retried poll with the same poll_id has
// nothing to repeat: the plaintext token is no longer kept.
func TestCIAcknowledgedAssignmentIsNotRepeated(t *testing.T) {
	c := newCIHarness(t, func(cfg *usecase.CIConfig) { cfg.PollTimeout = 150 * time.Millisecond })
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/ci.yml": strings.Split(ciWorkflow, "  test:")[0]}, "")
	var job *pb.Job
	c.waitFor("a job", func() bool {
		j, err := runner.fetch("poll-9")
		require.NoError(t, err)
		job = j
		return j != nil
	})
	_, err := runner.jobAPI(job).runner.Heartbeat(c.ctx, connect.NewRequest(&pb.HeartbeatRequest{JobId: job.GetJobId()}))
	require.NoError(t, err)
	again, err := runner.fetch("poll-9")
	require.NoError(t, err)
	assert.Nil(t, again, "the acknowledged assignment is not handed out again")
}

// The push payload follows GitHub's (checked against the recordings of
// ermite-conformance): 12-character ids in "compare", the commit page for a
// created ref and a compare range for an update, the default branch of the
// repository, a tree id, and string counters.
func TestCIGithubContextOfAPush(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	oneJobWF := strings.Split(ciWorkflow, "  test:")[0]
	first := c.push("main", map[string]string{".github/workflows/ci.yml": oneJobWF}, "")
	ev := func() (map[string]any, map[string]any) {
		job := runner.await()
		var ctxs map[string]map[string]any
		require.NoError(t, json.Unmarshal([]byte(job.GetContexts()), &ctxs))
		gh := ctxs["github"]
		runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "x")
		return gh, gh["event"].(map[string]any)
	}

	gh, event := ev()
	assert.Equal(t, publicOrigin+"/owner/proj/commit/"+first[:12], event["compare"], "a created ref compares as its commit")
	assert.Equal(t, true, event["created"])
	assert.Equal(t, domain.ZeroOID, event["before"])
	repo := event["repository"].(map[string]any)
	assert.Equal(t, "main", repo["default_branch"])
	assert.Equal(t, "main", repo["master_branch"])
	assert.Equal(t, "90", gh["retention_days"], "a string, from the configured retention")
	head := event["head_commit"].(map[string]any)
	tree := strings.TrimSpace(c.bareGit("proj", "rev-parse", first+"^{tree}"))
	assert.Equal(t, tree, head["tree_id"])

	second := c.push("main", map[string]string{"a.txt": "a\n"}, "")
	_, event = ev()
	assert.Equal(t, publicOrigin+"/owner/proj/compare/"+first[:12]+"..."+second[:12], event["compare"])
	assert.Equal(t, false, event["created"])
}
