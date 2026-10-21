// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/usecase"
)

// ciHarness is a server with a repository whose pushes run workflows, and a
// fake runner that talks to the real Connect handlers.
type ciHarness struct {
	*harness
	root *clients
	wt   string
	repo string
}

const ciWorkflow = `name: ci
on:
  push:
    branches: [main, feat]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hello
  test:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - run: echo test
`

func newCIHarness(t *testing.T, tweak func(*usecase.CIConfig)) *ciHarness {
	t.Helper()
	h := newHarnessWith(t, harnessOpts{}, func(o *options) { o.ci = tweak })
	c := &ciHarness{harness: h, root: h.api(adminToken), repo: "proj"}
	_, err := h.forge.Repos.Create(h.ctx, domain.Root(), ports.CreateRepoParams{Name: h.name("proj"), Visibility: domain.VisibilityPrivate})
	require.NoError(t, err)
	c.wt = h.workTree("main")
	return c
}

// push commits the files to branch and pushes it.
func (c *ciHarness) push(branch string, files map[string]string, msg string) string {
	c.t.Helper()
	if cur, err := c.git(c.wt, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || strings.TrimSpace(cur) != branch {
		if _, err := c.git(c.wt, "checkout", "-q", branch); err != nil {
			c.mustGit(c.wt, "checkout", "-q", "-b", branch)
		}
	}
	for name, content := range files {
		c.writeFile(c.wt, name, []byte(content))
	}
	if msg != "" {
		c.mustGit(c.wt, "commit", "-q", "--amend", "-m", msg)
	}
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/"+c.repo+".git"), branch)
	return strings.TrimSpace(c.mustGit(c.wt, "rev-parse", "HEAD"))
}

// registerRunner registers a runner and returns a fake that polls with it.
func (c *ciHarness) registerRunner(name string, labels ...string) *fakeRunner {
	c.t.Helper()
	res, err := c.root.runner.RegisterRunner(c.ctx, connect.NewRequest(&pb.RegisterRunnerRequest{
		Name: name, Labels: labels, Version: "devel", Isolation: pb.RunnerIsolation_RUNNER_ISOLATION_VM, Arch: "x64", Os: "linux",
	}))
	require.NoError(c.t, err)
	return &fakeRunner{c: c, id: res.Msg.GetRunnerId(), secret: res.Msg.GetRunnerToken(), api: c.api(res.Msg.GetRunnerToken())}
}

// fakeRunner is the runner side of the protocol, as a test.
type fakeRunner struct {
	c      *ciHarness
	id     string
	secret string
	api    *clients
}

// runnerSecret is the runner token of a fake runner.
func (c *ciHarness) runnerSecret(r *fakeRunner) string { return r.secret }

// fetch polls once for a job.
func (r *fakeRunner) fetch(pollID string) (*pb.Job, error) {
	res, err := r.api.runner.FetchJob(r.c.ctx, connect.NewRequest(&pb.FetchJobRequest{PollId: pollID}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetJob(), nil
}

// await polls until a job is given.
func (r *fakeRunner) await() *pb.Job {
	r.c.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		j, err := r.fetch("")
		require.NoError(r.c.t, err)
		if j != nil {
			return j
		}
	}
	r.c.t.Fatal("no job was given to the runner")
	return nil
}

// jobAPI is the client of a job: its token is the bearer secret.
func (r *fakeRunner) jobAPI(j *pb.Job) *clients { return r.c.api(j.GetJobToken()) }

func now() *timestamppb.Timestamp { return timestamppb.Now() }

// runJob executes a job as a runner does: heartbeat, steps, log, completion.
func (r *fakeRunner) runJob(j *pb.Job, conclusion pb.CiConclusion, outputs map[string]string, text ...string) {
	r.c.t.Helper()
	ctx, api := r.c.ctx, r.jobAPI(j)
	_, err := api.runner.Heartbeat(ctx, connect.NewRequest(&pb.HeartbeatRequest{JobId: j.GetJobId()}))
	require.NoError(r.c.t, err)
	start := now()
	_, err = api.runner.UpdateJob(ctx, connect.NewRequest(&pb.UpdateJobRequest{JobId: j.GetJobId(), Steps: []*pb.StepUpdate{
		{Number: 1, Name: "Set up job", State: pb.StepState_STEP_STATE_IN_PROGRESS, StartedAt: start},
	}}))
	require.NoError(r.c.t, err)
	var lines []*pb.LogLine
	for _, t := range text {
		lines = append(lines, &pb.LogLine{Time: now(), Text: t})
	}
	if len(lines) > 0 {
		_, err = api.runner.AppendLog(ctx, connect.NewRequest(&pb.AppendLogRequest{JobId: j.GetJobId(), Step: 2, Offset: 0, Lines: lines}))
		require.NoError(r.c.t, err)
	}
	step := pb.CiConclusion_CI_CONCLUSION_SUCCESS
	if conclusion != pb.CiConclusion_CI_CONCLUSION_SUCCESS {
		step = conclusion
	}
	res, err := api.runner.CompleteJob(ctx, connect.NewRequest(&pb.CompleteJobRequest{
		JobId: j.GetJobId(), Conclusion: conclusion, Outputs: outputs,
		Steps: []*pb.StepUpdate{
			{Number: 1, Name: "Set up job", State: pb.StepState_STEP_STATE_COMPLETED, Conclusion: pb.CiConclusion_CI_CONCLUSION_SUCCESS, StartedAt: start, CompletedAt: now()},
			{Number: 2, Name: "Run echo", State: pb.StepState_STEP_STATE_COMPLETED, Conclusion: step, StartedAt: start, CompletedAt: now()},
		},
	}))
	require.NoError(r.c.t, err)
	assert.True(r.c.t, res.Msg.GetApplied())
}

// waitFor polls cond until it holds.
func (c *ciHarness) waitFor(what string, cond func() bool) {
	c.t.Helper()
	require.Eventually(c.t, cond, 15*time.Second, 20*time.Millisecond, what)
}

func (c *ciHarness) runs() []*pb.Run {
	c.t.Helper()
	res, err := c.root.run.ListRuns(c.ctx, connect.NewRequest(&pb.ListRunsRequest{Repo: c.repo}))
	require.NoError(c.t, err)
	return res.Msg.GetRuns()
}

func (c *ciHarness) checks(sha string) []*pb.Check {
	c.t.Helper()
	res, err := c.root.run.ListChecks(c.ctx, connect.NewRequest(&pb.ListChecksRequest{Repo: c.repo, Sha: sha}))
	require.NoError(c.t, err)
	return res.Msg.GetChecks()
}

func (c *ciHarness) getRun(id int64) *pb.GetRunResponse {
	c.t.Helper()
	res, err := c.root.run.GetRun(c.ctx, connect.NewRequest(&pb.GetRunRequest{Repo: c.repo, RunId: id}))
	require.NoError(c.t, err)
	return res.Msg
}

func ciErrCode(err error) connect.Code {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Code()
	}
	return connect.Code(0)
}

// AC3, AC5: a push runs the workflow on a runner; its jobs follow their needs;
// the checks appear on the commit; the log is readable.
func TestCIPushRunsAWorkflowOnARunner(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	sha := c.push("main", map[string]string{".github/workflows/ci.yml": ciWorkflow}, "")

	c.waitFor("a run is created", func() bool { return len(c.runs()) == 1 })
	run := c.runs()[0]
	assert.Equal(t, "ci", run.GetWorkflowName())
	assert.Equal(t, "refs/heads/main", run.GetRef())
	assert.Equal(t, sha, run.GetSha())
	assert.Equal(t, "push", run.GetEvent())
	assert.EqualValues(t, 1, run.GetNumber())
	assert.Equal(t, "root", run.GetActor())

	// Only the first job is ready: the second needs it.
	job := runner.await()
	assert.Equal(t, "build", job.GetJobKey())
	assert.Equal(t, run.GetId(), job.GetRunId())
	assert.Equal(t, sha, job.GetSha())
	assert.Equal(t, "refs/heads/main", job.GetRef())
	assert.Equal(t, "proj", job.GetRepo())
	assert.Equal(t, ciWorkflow, job.GetWorkflowYaml())
	assert.Equal(t, []string{"ubuntu-latest"}, job.GetLabels())
	assert.EqualValues(t, 360, job.GetTimeoutMinutes())
	assert.Equal(t, "{}", job.GetMatrix())
	assert.True(t, strings.HasPrefix(job.GetJobToken(), "ermitejob_"))
	assert.Contains(t, job.GetContexts(), `"repository":"owner/proj"`)
	assert.Contains(t, job.GetContexts(), `"needs":{}`)

	// Assigned but not acknowledged: still queued for the owner.
	assert.Equal(t, pb.CiStatus_CI_STATUS_QUEUED, c.getRun(run.GetId()).GetJobs()[0].GetStatus())

	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, map[string]string{"artifact": "out.bin"}, "hello", "world")

	next := runner.await()
	assert.Equal(t, "test", next.GetJobKey())
	assert.Contains(t, next.GetContexts(), `"needs":{"build":{"result":"success","outputs":{"artifact":"out.bin"}}}`)
	runner.runJob(next, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "tested")

	c.waitFor("the run completes", func() bool {
		return c.runs()[0].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED
	})
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, c.runs()[0].GetConclusion())

	checks := c.checks(sha)
	require.Len(t, checks, 2)
	assert.Equal(t, "build", checks[0].GetName())
	assert.Equal(t, "test", checks[1].GetName())
	for _, ck := range checks {
		assert.Equal(t, pb.CiStatus_CI_STATUS_COMPLETED, ck.GetStatus())
		assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, ck.GetConclusion())
	}

	detail := c.getRun(run.GetId())
	require.Len(t, detail.GetJobs(), 2)
	steps := detail.GetJobs()[0].GetSteps()
	require.Len(t, steps, 2)
	assert.EqualValues(t, 0, steps[1].GetLogStart(), "step 2 has the lines")
	assert.EqualValues(t, 2, steps[1].GetLogEnd())

	// The log is streamed back with its steps.
	stream, err := c.root.run.GetJobLog(c.ctx, connect.NewRequest(&pb.GetJobLogRequest{Repo: "proj", JobId: job.GetJobId()}))
	require.NoError(t, err)
	var got []string
	for stream.Receive() {
		for _, l := range stream.Msg().GetLines() {
			got = append(got, l.GetText())
			assert.EqualValues(t, 2, l.GetStep())
		}
	}
	require.NoError(t, stream.Err())
	assert.Equal(t, []string{"hello", "world"}, got)
}

// AC5: merge refused while checks are pending or failing, allowed when they
// pass, or when explicitly bypassed by an authorized caller.
func TestCIMergeIsBlockedByChecks(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{"README": "x\n"}, "")

	feat := c.push("feat", map[string]string{".github/workflows/ci.yml": strings.ReplaceAll(ciWorkflow, "  test:\n    needs: build\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo test\n", "")}, "")
	mr, err := c.root.mr.CreateMergeRequest(c.ctx, connect.NewRequest(&pb.CreateMergeRequestRequest{Repo: "proj", Title: "Add CI", SourceBranch: "feat"}))
	require.NoError(t, err)
	merge := func() error {
		_, err := c.root.mr.MergeMergeRequest(c.ctx, connect.NewRequest(&pb.MergeMergeRequestRequest{Repo: "proj", Number: mr.Msg.GetMergeRequest().GetNumber(), HeadId: feat}))
		return err
	}

	c.waitFor("the feature run is queued", func() bool { return len(c.checks(feat)) == 1 })
	err = merge()
	require.Error(t, err)
	assert.Equal(t, connect.CodeFailedPrecondition, ciErrCode(err), "a pending check blocks the merge")
	assert.Contains(t, err.Error(), "have not finished")

	job := runner.await()
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_FAILURE, nil, "boom")
	c.waitFor("the check fails", func() bool {
		cs := c.checks(feat)
		return len(cs) == 1 && cs[0].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED
	})
	err = merge()
	require.Error(t, err)
	assert.Equal(t, connect.CodeFailedPrecondition, ciErrCode(err))
	assert.Contains(t, err.Error(), "failing checks block the merge")

	// An explicit override by root allows the merge despite failure.
	_, err = c.root.mr.MergeMergeRequest(c.ctx, connect.NewRequest(&pb.MergeMergeRequestRequest{Repo: "proj", Number: mr.Msg.GetMergeRequest().GetNumber(), HeadId: feat, IgnoreChecks: true}))
	require.NoError(t, err)
}

// AC2: a job token works for its job only, and not once the job has ended.
func TestCIJobTokensAreScopedToTheirJob(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/a.yml": ciWorkflow, ".github/workflows/b.yml": ciWorkflow}, "")
	c.waitFor("two runs", func() bool { return len(c.runs()) == 2 })
	j1, j2 := runner.await(), runner.await()
	require.NotEqual(t, j1.GetJobId(), j2.GetJobId())

	// Another job's id with this job's token: permission_denied.
	_, err := runner.jobAPI(j1).runner.Heartbeat(c.ctx, connect.NewRequest(&pb.HeartbeatRequest{JobId: j2.GetJobId()}))
	assert.Equal(t, connect.CodePermissionDenied, ciErrCode(err))
	// A runner token is not a job token, and a stored token is neither.
	_, err = runner.api.runner.Heartbeat(c.ctx, connect.NewRequest(&pb.HeartbeatRequest{JobId: j1.GetJobId()}))
	assert.Equal(t, connect.CodeUnauthenticated, ciErrCode(err))
	_, err = c.root.runner.Heartbeat(c.ctx, connect.NewRequest(&pb.HeartbeatRequest{JobId: j1.GetJobId()}))
	assert.Equal(t, connect.CodeUnauthenticated, ciErrCode(err))
	// A job token does not poll.
	_, err = runner.jobAPI(j1).runner.FetchJob(c.ctx, connect.NewRequest(&pb.FetchJobRequest{}))
	assert.Equal(t, connect.CodeUnauthenticated, ciErrCode(err))

	runner.runJob(j1, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "a")
	// After the end: failed_precondition with the JobEnded detail; CompleteJob
	// still answers with the stored result.
	_, err = runner.jobAPI(j1).runner.AppendLog(c.ctx, connect.NewRequest(&pb.AppendLogRequest{
		JobId: j1.GetJobId(), Step: 2, Offset: 1, Lines: []*pb.LogLine{{Time: now(), Text: "late"}},
	}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeFailedPrecondition, ciErrCode(err))
	var ce *connect.Error
	require.True(t, errors.As(err, &ce))
	require.Len(t, ce.Details(), 1)
	msg, derr := ce.Details()[0].Value()
	require.NoError(t, derr)
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, msg.(*pb.JobEnded).GetConclusion())

	res, err := runner.jobAPI(j1).runner.CompleteJob(c.ctx, connect.NewRequest(&pb.CompleteJobRequest{
		JobId: j1.GetJobId(), Conclusion: pb.CiConclusion_CI_CONCLUSION_FAILURE,
	}))
	require.NoError(t, err)
	assert.False(t, res.Msg.GetApplied(), "a second CompleteJob changes nothing")
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, res.Msg.GetConclusion(), "and answers the stored conclusion")
}

// AC2: a job is given to exactly one runner, however many poll at once.
func TestCIConcurrentFetchJobAssignsOnce(t *testing.T) {
	c := newCIHarness(t, func(cfg *usecase.CIConfig) { cfg.PollTimeout = 300 * time.Millisecond })
	var runners []*fakeRunner
	for i := range 8 {
		runners = append(runners, c.registerRunner(fmt.Sprintf("r%d", i), "ubuntu-latest"))
	}
	var wf strings.Builder
	wf.WriteString("on: push\njobs:\n")
	for i := range 3 {
		fmt.Fprintf(&wf, "  j%d:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo %d\n", i, i)
	}
	c.push("main", map[string]string{".github/workflows/many.yml": wf.String()}, "")
	c.waitFor("the run exists", func() bool { return len(c.runs()) == 1 })

	var mu sync.Mutex
	got := map[string]int{}
	var wg sync.WaitGroup
	for _, r := range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				j, err := r.fetch("")
				if err != nil {
					return
				}
				if j != nil {
					mu.Lock()
					got[j.GetJobId()]++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	assert.Len(t, got, 3, "every job was given")
	for id, n := range got {
		assert.Equal(t, 1, n, "job %s was given %d times", id, n)
	}
}

// AC2: the first heartbeat acknowledges; without it the assignment is
// cancelled and the job is queued again, three times in a row at most.
func TestCIUnacknowledgedAssignmentGoesBackToTheQueue(t *testing.T) {
	c := newCIHarness(t, func(cfg *usecase.CIConfig) {
		cfg.AckTimeout, cfg.Tick, cfg.PollTimeout = 150*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond
	})
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/ci.yml": strings.Split(ciWorkflow, "  test:")[0]}, "")

	first := runner.await()
	// No heartbeat: the token is revoked and the job comes back with a new one.
	second := runner.await()
	assert.Equal(t, first.GetJobId(), second.GetJobId())
	assert.NotEqual(t, first.GetJobToken(), second.GetJobToken())
	_, err := runner.jobAPI(first).runner.Heartbeat(c.ctx, connect.NewRequest(&pb.HeartbeatRequest{JobId: first.GetJobId()}))
	assert.Equal(t, connect.CodeUnauthenticated, ciErrCode(err), "the replaced token is rejected")

	// The third assignment is the last: ignoring it fails the job.
	runner.await()
	c.waitFor("the job fails", func() bool {
		return c.runs()[0].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED
	})
	detail := c.getRun(c.runs()[0].GetId())
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_FAILURE, detail.GetJobs()[0].GetConclusion())
	assert.Contains(t, detail.GetJobs()[0].GetError(), "acknowledged")
}

// A retried poll with the same poll_id returns the same assignment.
func TestCIRetriedPollReturnsTheSameAssignment(t *testing.T) {
	c := newCIHarness(t, func(cfg *usecase.CIConfig) { cfg.PollTimeout = 200 * time.Millisecond })
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/ci.yml": strings.Split(ciWorkflow, "  test:")[0]}, "")
	c.waitFor("a run", func() bool { return len(c.runs()) == 1 })

	var first *pb.Job
	c.waitFor("a job", func() bool {
		j, err := runner.fetch("poll-1")
		require.NoError(t, err)
		first = j
		return j != nil
	})
	again, err := runner.fetch("poll-1")
	require.NoError(t, err)
	require.NotNil(t, again)
	assert.Equal(t, first.GetJobId(), again.GetJobId())
	assert.Equal(t, first.GetJobToken(), again.GetJobToken())
}
