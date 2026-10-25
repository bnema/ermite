// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/adapters/depsregistry"
	"bnema.dev/ermite/internal/config"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/usecase"
)

const bernardWorkflow = `name: ci
on:
  push:
    branches: ['bernard/**']
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      - run: echo hello
`

// AC5, AC7: Open MR on a repository with an outdated action creates the
// signed commit on bernard/deps, a merge request authored by bernard and an
// untrusted run; a second Open updates the same merge request.
func TestBernardOpenMRForActions(t *testing.T) {
	remote := newFakeRemote(t)
	remote.set("actions/checkout",
		ports.RemoteRef{Name: "refs/tags/v3", ID: shaTag},
		ports.RemoteRef{Name: "refs/tags/v4", ID: shaTagObj, Peeled: shaPeeled},
	)
	h := newHarnessWith(t, harnessOpts{env: map[string]string{config.EnvDepsCooldown: "1ns"}},
		func(o *options) { o.remoteLister = remote.m })
	c := &ciHarness{harness: h, root: h.api(adminToken), repo: "proj"}
	h.seed("proj", domain.VisibilityPrivate)
	c.wt = h.workTree("main")
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/ci.yml": bernardWorkflow}, "")
	main := strings.TrimSpace(c.mustGit(c.wt, "rev-parse", "HEAD"))

	deps := ermiteconnect.NewDepsServiceClient(&http.Client{Transport: bearerTransport{secret: adminToken, next: http.DefaultTransport}}, h.url("/-/api"))
	// The first check records when v4 was first seen; the cooldown (1ns) has
	// passed at the second.
	var pending *pb.RepoDependencyUpdates
	c.waitFor("a pending update", func() bool {
		res, err := deps.CheckDependencies(c.ctx, connect.NewRequest(&pb.CheckDependenciesRequest{Repo: "proj"}))
		require.NoError(t, err)
		pending = res.Msg.GetUpdates()
		return len(pending.GetUpdates()) == 1
	})
	expected := func() []*pb.DependencyVersion {
		var out []*pb.DependencyVersion
		for _, u := range pending.GetUpdates() {
			out = append(out, &pb.DependencyVersion{Ecosystem: u.GetEcosystem(), Directory: u.GetDirectory(), Name: u.GetName(), Version: u.GetLatest()})
		}
		return out
	}
	require.Equal(t, "v4", pending.GetUpdates()[0].GetLatest())

	// A stale confirmation is refused.
	_, err := deps.OpenDependencyUpdates(c.ctx, connect.NewRequest(&pb.OpenDependencyUpdatesRequest{Repo: "proj"}))
	assert.Equal(t, connect.CodeFailedPrecondition, ciErrCode(err))

	res, err := deps.OpenDependencyUpdates(c.ctx, connect.NewRequest(&pb.OpenDependencyUpdatesRequest{Repo: "proj", ExpectedVersions: expected()}))
	require.NoError(t, err)
	number := res.Msg.GetUpdates().GetMergeRequestNumber()
	require.NotZero(t, number)
	assert.Equal(t, pb.RepoDependencyUpdates_STATE_IDLE, res.Msg.GetUpdates().GetState())

	// One signed commit on top of main that only changes the version.
	assert.Equal(t, "1", c.bareGit("proj", "rev-list", "--count", main+"..refs/heads/bernard/deps"))
	head := c.bareGit("proj", "rev-parse", "refs/heads/bernard/deps")
	assert.Equal(t, main, c.bareGit("proj", "rev-parse", head+"^"))
	assert.Equal(t, "Bernard", c.bareGit("proj", "log", "-1", "--format=%an", head))
	assert.Equal(t, "build(deps): update 1 dependency", c.bareGit("proj", "log", "-1", "--format=%s", head))
	assert.Equal(t, strings.Replace(bernardWorkflow, "checkout@v3", "checkout@v4", 1), c.mustGit(c.repoDir("proj"), "show", head+":.github/workflows/ci.yml"))
	assert.Equal(t, strings.TrimSpace(c.bareGit("proj", "diff", "--numstat", main, head)), "1\t1\t.github/workflows/ci.yml")

	list, err := c.root.git.ListCommits(c.ctx, connect.NewRequest(&pb.ListCommitsRequest{Repo: "proj", Revision: "bernard/deps", PageSize: 1}))
	require.NoError(t, err)
	require.Len(t, list.Msg.GetCommits(), 1)
	assert.Equal(t, head, list.Msg.GetCommits()[0].GetId())
	assert.Equal(t, pb.SignatureStatus_SIGNATURE_STATUS_VERIFIED, list.Msg.GetCommits()[0].GetVerification().GetStatus())

	// The merge request is Bernard's, and nothing enabled auto-merge.
	mr, err := c.root.mr.GetMergeRequest(c.ctx, connect.NewRequest(&pb.GetMergeRequestRequest{Repo: "proj", Number: number}))
	require.NoError(t, err)
	assert.Equal(t, "bernard", mr.Msg.GetMergeRequest().GetAuthor())
	assert.Equal(t, "bernard/deps", mr.Msg.GetMergeRequest().GetSourceBranch())
	assert.Equal(t, "main", mr.Msg.GetMergeRequest().GetTargetBranch())
	assert.Nil(t, mr.Msg.GetMergeRequest().GetAutoMerge())

	// Its run is untrusted.
	job := runner.await()
	assert.False(t, job.GetTrusted())
	var run *pb.Run
	for _, r := range c.runs() {
		if r.GetSha() == head {
			run = r
		}
	}
	require.NotNil(t, run)
	assert.Equal(t, "refs/heads/bernard/deps", run.GetRef())
	assert.Equal(t, "bernard", run.GetActor())
	assert.False(t, run.GetTrusted())

	// Update MR: the same merge request, a new commit on the target tip.
	c.mustGit(c.wt, "checkout", "-q", "main")
	c.push("main", map[string]string{"README": "more\n"}, "")
	main2 := strings.TrimSpace(c.mustGit(c.wt, "rev-parse", "HEAD"))
	up, err := deps.CheckDependencies(c.ctx, connect.NewRequest(&pb.CheckDependenciesRequest{Repo: "proj"}))
	require.NoError(t, err)
	pending = up.Msg.GetUpdates()
	require.Equal(t, number, pending.GetMergeRequestNumber())
	res, err = deps.OpenDependencyUpdates(c.ctx, connect.NewRequest(&pb.OpenDependencyUpdatesRequest{Repo: "proj", ExpectedVersions: expected()}))
	require.NoError(t, err)
	assert.Equal(t, number, res.Msg.GetUpdates().GetMergeRequestNumber())
	head2 := c.bareGit("proj", "rev-parse", "refs/heads/bernard/deps")
	assert.NotEqual(t, head, head2)
	assert.Equal(t, main2, c.bareGit("proj", "rev-parse", head2+"^"))
}

// goDepsRegistry answers the Go module questions of Bernard in tests and
// leaves the action tags to the real registry over the fake remote.
type goDepsRegistry struct {
	ports.DepsRegistry
}

func (goDepsRegistry) GoVersions(_ context.Context, module, _ string) ([]domain.DepVersion, error) {
	if module != "golang.org/x/text" {
		return nil, domain.ErrNotFound
	}
	return []domain.DepVersion{{Version: "v0.21.0", Published: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}}, nil
}

func (goDepsRegistry) GoDirective(context.Context, string, string) (string, error) {
	return "1.21", nil
}

func (g goDepsRegistry) Malicious(ctx context.Context, eco domain.DepsEcosystem, name, version string) (bool, error) {
	if eco == domain.EcosystemGoMod {
		return false, nil
	}
	return false, nil
}

const bernardSumHash = "h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func bernardGoMod(textVersion string) string {
	return "module example.com/proj\n\ngo 1.24\n\nrequire golang.org/x/text " + textVersion + "\n"
}

// bernardGoRig is a repository with a Go module and an outdated action, a
// runner of the deps kind, and the Dependencies client with the pending updates.
type bernardGoRig struct {
	*ciHarness
	runner   *fakeRunner
	deps     ermiteconnect.DepsServiceClient
	expected []*pb.DependencyVersion
	main     string
	oldSum   string
}

func newBernardGoRig(t *testing.T) *bernardGoRig {
	t.Helper()
	remote := newFakeRemote(t)
	remote.set("actions/checkout",
		ports.RemoteRef{Name: "refs/tags/v3", ID: shaTag},
		ports.RemoteRef{Name: "refs/tags/v4", ID: shaTagObj, Peeled: shaPeeled},
	)
	h := newHarnessWith(t, harnessOpts{env: map[string]string{config.EnvDepsCooldown: "1ns"}},
		func(o *options) {
			o.remoteLister = remote.m
			o.ci = func(cc *usecase.CIConfig) { cc.PollTimeout = 300 * time.Millisecond }
			o.depsRegistry = goDepsRegistry{DepsRegistry: depsregistry.New(depsregistry.Config{GitRemote: remote.m})}
		})
	c := &ciHarness{harness: h, root: h.api(adminToken), repo: "proj"}
	h.seed("proj", domain.VisibilityPrivate)
	c.wt = h.workTree("main")
	rig := &bernardGoRig{ciHarness: c, runner: c.registerRunner("alpha", "ubuntu-latest")}
	rig.oldSum = "golang.org/x/text v0.20.0 " + bernardSumHash + "\ngolang.org/x/text v0.20.0/go.mod " + bernardSumHash + "\n"
	c.push("main", map[string]string{
		".github/workflows/ci.yml": bernardWorkflow,
		"go.mod":                   bernardGoMod("v0.20.0"),
		"go.sum":                   rig.oldSum,
	}, "")
	rig.main = strings.TrimSpace(c.mustGit(c.wt, "rev-parse", "HEAD"))

	rig.deps = ermiteconnect.NewDepsServiceClient(&http.Client{Transport: bearerTransport{secret: adminToken, next: http.DefaultTransport}}, h.url("/-/api"))
	var pending *pb.RepoDependencyUpdates
	c.waitFor("two pending updates", func() bool {
		res, err := rig.deps.CheckDependencies(c.ctx, connect.NewRequest(&pb.CheckDependenciesRequest{Repo: "proj"}))
		require.NoError(t, err)
		pending = res.Msg.GetUpdates()
		return len(pending.GetUpdates()) == 2
	})
	for _, u := range pending.GetUpdates() {
		rig.expected = append(rig.expected, &pb.DependencyVersion{Ecosystem: u.GetEcosystem(), Directory: u.GetDirectory(), Name: u.GetName(), Version: u.GetLatest()})
	}
	return rig
}

func (rig *bernardGoRig) open() *pb.RepoDependencyUpdates {
	rig.t.Helper()
	res, err := rig.deps.OpenDependencyUpdates(rig.ctx, connect.NewRequest(&pb.OpenDependencyUpdatesRequest{Repo: "proj", ExpectedVersions: rig.expected}))
	require.NoError(rig.t, err)
	return res.Msg.GetUpdates()
}

// record is the dependency record of the repository.
func (rig *bernardGoRig) record() *pb.RepoDependencyUpdates {
	rig.t.Helper()
	res, err := rig.deps.ListDependencyUpdates(rig.ctx, connect.NewRequest(&pb.ListDependencyUpdatesRequest{}))
	require.NoError(rig.t, err)
	for _, r := range res.Msg.GetRepos() {
		if r.GetRepo() == "proj" {
			return r
		}
	}
	rig.t.Fatal("no record for proj")
	return nil
}

// depsRun returns the only dependency run of the repository.
func (rig *bernardGoRig) depsRun() *pb.Run {
	rig.t.Helper()
	var out *pb.Run
	for _, r := range rig.runs() {
		if r.GetEvent() == "dependencies" {
			require.Nil(rig.t, out, "one dependency run")
			out = r
		}
	}
	require.NotNil(rig.t, out, "a dependency run")
	return out
}

// P4.2: Open on a repository with a Go module and an action starts a
// dependency run for a runner of the deps kind; its report is applied with the
// action update in one signed commit on bernard/deps, authored by Bernard. The
// job is an ordinary job of an ordinary run: it takes its job token, its log is
// the run's log, and the Actions list shows it.
func TestBernardOpenMRForGoModulesThroughARunner(t *testing.T) {
	rig := newBernardGoRig(t)
	c, runner, main := rig.ciHarness, rig.runner, rig.main

	got := rig.open()
	assert.Equal(t, pb.RepoDependencyUpdates_STATE_APPLYING, got.GetState())
	assert.Zero(t, got.GetMergeRequestNumber())
	_, gitErr := c.git(c.repoDir("proj"), "rev-parse", "--verify", "-q", "refs/heads/bernard/deps")
	assert.Error(t, gitErr, "nothing is committed before the report")

	// A queued run, visible in Actions, with no check on the commit.
	run := rig.depsRun()
	assert.Equal(t, pb.CiStatus_CI_STATUS_QUEUED, run.GetStatus())
	assert.Equal(t, "bernard", run.GetActor())
	assert.Equal(t, "Dependencies", run.GetWorkflowName())
	assert.Equal(t, "bernard/deps", run.GetWorkflowPath())
	assert.Equal(t, "build(deps): update 2 dependencies", run.GetTitle())
	assert.Equal(t, "refs/heads/main", run.GetRef())
	assert.Equal(t, main, run.GetSha())
	assert.False(t, run.GetTrusted())
	assert.Empty(t, c.checks(main), "a dependency run posts no check")

	// A runner that did not ask for the deps kind never gets the job.
	old, err := runner.fetch("")
	require.NoError(t, err)
	assert.Nil(t, old)

	fetch, err := runner.api.runner.FetchJob(c.ctx, connect.NewRequest(&pb.FetchJobRequest{Kinds: []string{"workflow", "deps"}}))
	require.NoError(t, err)
	job := fetch.Msg.GetJob()
	require.NotNil(t, job)
	require.NotNil(t, job.GetDeps())
	assert.Equal(t, run.GetId(), job.GetRunId())
	assert.Equal(t, fmt.Sprintf("%d-0-0", run.GetId()), job.GetJobId())
	assert.NotEmpty(t, job.GetJobToken())
	assert.EqualValues(t, 10, job.GetTimeoutMinutes())
	assert.Equal(t, []string{"ubuntu-latest"}, job.GetLabels())
	assert.Equal(t, main, job.GetSha())
	assert.Empty(t, job.GetWorkflowYaml())
	assert.Equal(t, "1.24", job.GetDeps().GetGoVersion())
	require.Len(t, job.GetDeps().GetUpdates(), 1)
	assert.Equal(t, "golang.org/x/text", job.GetDeps().GetUpdates()[0].GetName())
	assert.Equal(t, "v0.21.0", job.GetDeps().GetUpdates()[0].GetVersion())
	assert.False(t, job.GetTrusted())
	assert.Empty(t, job.GetSecrets())
	assert.Empty(t, job.GetVars())
	assert.NotEmpty(t, job.GetGithubToken())

	// The job runs with its job token: heartbeat, steps, log, completion. The
	// runner token is no key to it.
	_, err = runner.api.runner.Heartbeat(c.ctx, connect.NewRequest(&pb.HeartbeatRequest{JobId: job.GetJobId()}))
	assert.Equal(t, connect.CodeUnauthenticated, ciErrCode(err))
	jobAPI := runner.jobAPI(job)
	_, err = jobAPI.runner.Heartbeat(c.ctx, connect.NewRequest(&pb.HeartbeatRequest{JobId: job.GetJobId()}))
	require.NoError(t, err)
	c.waitFor("the run to be in progress", func() bool { return c.getRun(run.GetId()).GetRun().GetStatus() == pb.CiStatus_CI_STATUS_IN_PROGRESS })
	start := now()
	_, err = jobAPI.runner.UpdateJob(c.ctx, connect.NewRequest(&pb.UpdateJobRequest{JobId: job.GetJobId(), Steps: []*pb.StepUpdate{
		{Number: 1, Name: "Update dependencies", State: pb.StepState_STEP_STATE_IN_PROGRESS, StartedAt: start},
	}}))
	require.NoError(t, err)
	_, err = jobAPI.runner.AppendLog(c.ctx, connect.NewRequest(&pb.AppendLogRequest{
		JobId: job.GetJobId(), Step: 1, Lines: []*pb.LogLine{{Time: now(), Text: "go get golang.org/x/text@v0.21.0"}, {Time: now(), Text: "go mod tidy"}},
	}))
	require.NoError(t, err)
	newSum := rig.oldSum + "golang.org/x/text v0.21.0 " + bernardSumHash + "\ngolang.org/x/text v0.21.0/go.mod " + bernardSumHash + "\n"
	done, err := jobAPI.runner.CompleteJob(c.ctx, connect.NewRequest(&pb.CompleteJobRequest{
		JobId: job.GetJobId(), Conclusion: pb.CiConclusion_CI_CONCLUSION_SUCCESS,
		Steps: []*pb.StepUpdate{{Number: 1, Name: "Update dependencies", State: pb.StepState_STEP_STATE_COMPLETED, Conclusion: pb.CiConclusion_CI_CONCLUSION_SUCCESS, StartedAt: start, CompletedAt: now()}},
		DepsReport: &pb.DepsReport{Files: []*pb.DepsFile{
			{Path: "go.mod", Content: []byte(bernardGoMod("v0.21.0"))},
			{Path: "go.sum", Content: []byte(newSum)},
		}},
	}))
	require.NoError(t, err)
	assert.True(t, done.Msg.GetApplied())

	// The report is applied after the job ended: the card goes idle with its
	// merge request, and keeps the link to the run.
	var rec *pb.RepoDependencyUpdates
	c.waitFor("the card to be idle", func() bool {
		rec = rig.record()
		return rec.GetState() == pb.RepoDependencyUpdates_STATE_IDLE && rec.GetMergeRequestNumber() != 0
	})

	// One signed commit on the run's commit: go.mod, go.sum and the workflow.
	assert.Equal(t, "1", c.bareGit("proj", "rev-list", "--count", main+"..refs/heads/bernard/deps"))
	head := c.bareGit("proj", "rev-parse", "refs/heads/bernard/deps")
	assert.Equal(t, main, c.bareGit("proj", "rev-parse", head+"^"))
	assert.Equal(t, "Bernard", c.bareGit("proj", "log", "-1", "--format=%an", head))
	assert.Equal(t, "build(deps): update 2 dependencies", c.bareGit("proj", "log", "-1", "--format=%s", head))
	changed := strings.Fields(c.bareGit("proj", "diff", "--name-only", main, head))
	assert.ElementsMatch(t, []string{".github/workflows/ci.yml", "go.mod", "go.sum"}, changed)
	assert.Equal(t, bernardGoMod("v0.21.0"), c.mustGit(c.repoDir("proj"), "show", head+":go.mod"))
	assert.Equal(t, newSum, c.mustGit(c.repoDir("proj"), "show", head+":go.sum"))
	assert.Equal(t, strings.Replace(bernardWorkflow, "checkout@v3", "checkout@v4", 1), c.mustGit(c.repoDir("proj"), "show", head+":.github/workflows/ci.yml"))
	list, err := c.root.git.ListCommits(c.ctx, connect.NewRequest(&pb.ListCommitsRequest{Repo: "proj", Revision: "bernard/deps", PageSize: 1}))
	require.NoError(t, err)
	require.Len(t, list.Msg.GetCommits(), 1)
	assert.Equal(t, pb.SignatureStatus_SIGNATURE_STATUS_VERIFIED, list.Msg.GetCommits()[0].GetVerification().GetStatus())

	mr, err := c.root.mr.GetMergeRequest(c.ctx, connect.NewRequest(&pb.GetMergeRequestRequest{Repo: "proj", Number: rec.GetMergeRequestNumber()}))
	require.NoError(t, err)
	assert.Equal(t, "bernard", mr.Msg.GetMergeRequest().GetAuthor())
	assert.Equal(t, "bernard/deps", mr.Msg.GetMergeRequest().GetSourceBranch())

	// The run completed, with its steps and its log, and no check.
	detail := c.getRun(run.GetId())
	assert.Equal(t, pb.CiStatus_CI_STATUS_COMPLETED, detail.GetRun().GetStatus())
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, detail.GetRun().GetConclusion())
	require.Len(t, detail.GetJobs(), 1)
	assert.Equal(t, "Update dependencies", detail.GetJobs()[0].GetName())
	require.Len(t, detail.GetJobs()[0].GetSteps(), 1)
	assert.EqualValues(t, 2, detail.GetJobs()[0].GetSteps()[0].GetLogEnd())
	stream, err := c.root.run.GetJobLog(c.ctx, connect.NewRequest(&pb.GetJobLogRequest{Repo: "proj", JobId: job.GetJobId()}))
	require.NoError(t, err)
	var lines []string
	for stream.Receive() {
		for _, l := range stream.Msg().GetLines() {
			lines = append(lines, l.GetText())
		}
	}
	require.NoError(t, stream.Err())
	assert.Equal(t, []string{"go get golang.org/x/text@v0.21.0", "go mod tidy"}, lines)
	assert.Empty(t, c.checks(main))

	// The job is over: another call answers with its end, and the run cannot be
	// run again.
	_, err = jobAPI.runner.Heartbeat(c.ctx, connect.NewRequest(&pb.HeartbeatRequest{JobId: job.GetJobId()}))
	require.Error(t, err)
	_, err = c.root.run.RerunRun(c.ctx, connect.NewRequest(&pb.RerunRunRequest{Repo: "proj", RunId: run.GetId()}))
	assert.Equal(t, connect.CodeFailedPrecondition, ciErrCode(err))
}

// The owner cancels the dependency run from the Actions list: the runner is
// asked to stop, and the card fails with the reason.
func TestBernardCancelingTheRunFailsTheCard(t *testing.T) {
	rig := newBernardGoRig(t)
	c, runner := rig.ciHarness, rig.runner

	rig.open()
	run := rig.depsRun()
	fetch, err := runner.api.runner.FetchJob(c.ctx, connect.NewRequest(&pb.FetchJobRequest{Kinds: []string{"deps"}}))
	require.NoError(t, err)
	job := fetch.Msg.GetJob()
	require.NotNil(t, job)
	jobAPI := runner.jobAPI(job)
	_, err = jobAPI.runner.Heartbeat(c.ctx, connect.NewRequest(&pb.HeartbeatRequest{JobId: job.GetJobId()}))
	require.NoError(t, err)

	// The runner watches the job.
	watch, err := jobAPI.runner.WatchJob(c.ctx, connect.NewRequest(&pb.WatchJobRequest{JobId: job.GetJobId()}))
	require.NoError(t, err)
	cancelled := make(chan *pb.CancelRequest, 1)
	go func() {
		for watch.Receive() {
			if watch.Msg().GetCancel() != nil {
				cancelled <- watch.Msg().GetCancel()
				return
			}
		}
	}()

	_, err = c.root.run.CancelRun(c.ctx, connect.NewRequest(&pb.CancelRunRequest{Repo: "proj", RunId: run.GetId()}))
	require.NoError(t, err)
	select {
	case sig := <-cancelled:
		assert.NotNil(t, sig)
	case <-time.After(10 * time.Second):
		t.Fatal("the runner was not asked to stop")
	}
	_, err = jobAPI.runner.CompleteJob(c.ctx, connect.NewRequest(&pb.CompleteJobRequest{JobId: job.GetJobId(), Conclusion: pb.CiConclusion_CI_CONCLUSION_CANCELLED}))
	require.NoError(t, err)

	var rec *pb.RepoDependencyUpdates
	c.waitFor("the card to fail", func() bool {
		rec = rig.record()
		return rec.GetState() == pb.RepoDependencyUpdates_STATE_FAILED
	})
	assert.Contains(t, rec.GetError(), "the run ended: cancelled")
	assert.NotContains(t, rec.GetError(), "\n")
	assert.Zero(t, rec.GetMergeRequestNumber())
	_, gitErr := c.git(c.repoDir("proj"), "rev-parse", "--verify", "-q", "refs/heads/bernard/deps")
	assert.Error(t, gitErr, "nothing was committed")
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_CANCELLED, c.getRun(run.GetId()).GetRun().GetConclusion())
}
