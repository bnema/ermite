// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
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

// P4.2: Open on a repository with a Go module and an action queues a deps job
// for a runner of the deps kind; its report is applied with the action update
// in one signed commit on bernard/deps, authored by Bernard.
func TestBernardOpenMRForGoModulesThroughARunner(t *testing.T) {
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
	runner := c.registerRunner("alpha", "ubuntu-latest")
	oldSum := "golang.org/x/text v0.20.0 " + bernardSumHash + "\ngolang.org/x/text v0.20.0/go.mod " + bernardSumHash + "\n"
	c.push("main", map[string]string{
		".github/workflows/ci.yml": bernardWorkflow,
		"go.mod":                   bernardGoMod("v0.20.0"),
		"go.sum":                   oldSum,
	}, "")
	main := strings.TrimSpace(c.mustGit(c.wt, "rev-parse", "HEAD"))

	deps := ermiteconnect.NewDepsServiceClient(&http.Client{Transport: bearerTransport{secret: adminToken, next: http.DefaultTransport}}, h.url("/-/api"))
	var pending *pb.RepoDependencyUpdates
	c.waitFor("two pending updates", func() bool {
		res, err := deps.CheckDependencies(c.ctx, connect.NewRequest(&pb.CheckDependenciesRequest{Repo: "proj"}))
		require.NoError(t, err)
		pending = res.Msg.GetUpdates()
		return len(pending.GetUpdates()) == 2
	})
	var expected []*pb.DependencyVersion
	for _, u := range pending.GetUpdates() {
		expected = append(expected, &pb.DependencyVersion{Ecosystem: u.GetEcosystem(), Directory: u.GetDirectory(), Name: u.GetName(), Version: u.GetLatest()})
	}

	res, err := deps.OpenDependencyUpdates(c.ctx, connect.NewRequest(&pb.OpenDependencyUpdatesRequest{Repo: "proj", ExpectedVersions: expected}))
	require.NoError(t, err)
	assert.Equal(t, pb.RepoDependencyUpdates_STATE_APPLYING, res.Msg.GetUpdates().GetState())
	assert.Zero(t, res.Msg.GetUpdates().GetMergeRequestNumber())
	_, gitErr := c.git(c.repoDir("proj"), "rev-parse", "--verify", "-q", "refs/heads/bernard/deps")
	assert.Error(t, gitErr, "nothing is committed before the report")

	// A runner that did not ask for the deps kind never gets the job.
	old, err := runner.fetch("")
	require.NoError(t, err)
	assert.Nil(t, old)

	fetch, err := runner.api.runner.FetchJob(c.ctx, connect.NewRequest(&pb.FetchJobRequest{Kinds: []string{"workflow", "deps"}}))
	require.NoError(t, err)
	job := fetch.Msg.GetJob()
	require.NotNil(t, job)
	require.NotNil(t, job.GetDeps())
	assert.Regexp(t, `^deps-[0-9a-f]{16}$`, job.GetJobId())
	assert.Equal(t, main, job.GetSha())
	assert.Equal(t, "1.24", job.GetDeps().GetGoVersion())
	require.Len(t, job.GetDeps().GetUpdates(), 1)
	assert.Equal(t, "golang.org/x/text", job.GetDeps().GetUpdates()[0].GetName())
	assert.Equal(t, "v0.21.0", job.GetDeps().GetUpdates()[0].GetVersion())
	assert.False(t, job.GetTrusted())
	assert.Empty(t, job.GetSecrets())
	assert.Empty(t, job.GetVars())
	assert.NotEmpty(t, job.GetGithubToken())

	// The job runs with the runner secret: heartbeat, log, completion.
	_, err = runner.api.runner.Heartbeat(c.ctx, connect.NewRequest(&pb.HeartbeatRequest{JobId: job.GetJobId()}))
	require.NoError(t, err)
	_, err = runner.api.runner.AppendLog(c.ctx, connect.NewRequest(&pb.AppendLogRequest{
		JobId: job.GetJobId(), Step: 1, Lines: []*pb.LogLine{{Time: now(), Text: "go get golang.org/x/text@v0.21.0"}},
	}))
	require.NoError(t, err)
	newSum := oldSum + "golang.org/x/text v0.21.0 " + bernardSumHash + "\ngolang.org/x/text v0.21.0/go.mod " + bernardSumHash + "\n"
	done, err := runner.api.runner.CompleteJob(c.ctx, connect.NewRequest(&pb.CompleteJobRequest{
		JobId: job.GetJobId(), Conclusion: pb.CiConclusion_CI_CONCLUSION_SUCCESS,
		DepsReport: &pb.DepsReport{Files: []*pb.DepsFile{
			{Path: "go.mod", Content: []byte(bernardGoMod("v0.21.0"))},
			{Path: "go.sum", Content: []byte(newSum)},
		}},
	}))
	require.NoError(t, err)
	assert.True(t, done.Msg.GetApplied())

	// One signed commit on the job's commit: go.mod, go.sum and the workflow.
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

	// The card is idle again with the merge request, authored by bernard.
	var number int64
	c.waitFor("the card to be idle", func() bool {
		res, err := deps.ListDependencyUpdates(c.ctx, connect.NewRequest(&pb.ListDependencyUpdatesRequest{}))
		require.NoError(t, err)
		for _, r := range res.Msg.GetRepos() {
			if r.GetState() == pb.RepoDependencyUpdates_STATE_IDLE && r.GetMergeRequestNumber() != 0 {
				number = r.GetMergeRequestNumber()
				return true
			}
		}
		return false
	})
	mr, err := c.root.mr.GetMergeRequest(c.ctx, connect.NewRequest(&pb.GetMergeRequestRequest{Repo: "proj", Number: number}))
	require.NoError(t, err)
	assert.Equal(t, "bernard", mr.Msg.GetMergeRequest().GetAuthor())
	assert.Equal(t, "bernard/deps", mr.Msg.GetMergeRequest().GetSourceBranch())

	// The job is gone: a second completion is refused.
	_, err = runner.api.runner.Heartbeat(c.ctx, connect.NewRequest(&pb.HeartbeatRequest{JobId: job.GetJobId()}))
	assert.Equal(t, connect.CodeNotFound, ciErrCode(err))
}
