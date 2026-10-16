// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
)

// CI-4 D4: the pull_request_target event runs the workflows of the default
// branch's tip for the activities of a merge request.

const targetWorkflow = `name: prt
on:
  pull_request_target:
%s
jobs:
  j:
    runs-on: ubuntu-latest
    permissions:
      contents: write
    steps:
      - run: echo ${{ secrets.API_TOKEN }} %s
`

func targetWF(on, mark string) map[string]string {
	return map[string]string{".github/workflows/prt.yml": fmt.Sprintf(targetWorkflow, on, mark)}
}

// newTargetHarness is a harness with a repository secret and the
// pull_request_target workflow of on on main.
func newTargetHarness(t *testing.T, on string) (*ciHarness, *fakeRunner, string) {
	t.Helper()
	c := newSecretsHarness(t, secretsKeyA)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.setSecret(repoScope("proj"), "API_TOKEN", "repo-token")
	files := targetWF(on, "from-main")
	files["a.txt"] = "base\n"
	return c, runner, c.push("main", files, "")
}

func TestPullRequestTargetRunsTheDefaultBranchWorkflow(t *testing.T) {
	c, runner, main := newTargetHarness(t, "")
	// The head changes the workflow: that version is not the one that runs.
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	files := targetWF("", "from-head")
	files["b.txt"] = "b\n"
	feat1 := c.push("feat", files, "")

	n := c.openMR(c.root, "feat")
	job := c.finish(runner)
	assert.Contains(t, job.GetWorkflowYaml(), "from-main")
	assert.NotContains(t, job.GetWorkflowYaml(), "from-head")
	assert.Equal(t, main, job.GetSha())
	assert.Equal(t, "refs/heads/main", job.GetRef())

	// Trusted: the owner opened it and pushed main.
	assert.True(t, job.GetTrusted())
	assert.Equal(t, "repo-token", job.GetSecrets()["API_TOKEN"])
	assert.Equal(t, "write", job.GetPermissions().GetLevels()["contents"])

	runs := c.runs()
	require.Len(t, runs, 1)
	assert.Equal(t, "pull_request_target", runs[0].GetEvent())
	assert.Equal(t, "refs/heads/main", runs[0].GetRef())
	assert.Equal(t, main, runs[0].GetSha())
	assert.Equal(t, "Title of feat", runs[0].GetTitle())
	assert.True(t, runs[0].GetTrusted())

	// The checks attach to the head, as those of pull_request, not to main.
	require.Len(t, c.checks(feat1), 1)
	assert.Empty(t, c.checks(main))

	gh := c.ghContext(job)
	assert.Equal(t, "pull_request_target", gh["event_name"])
	assert.Equal(t, "refs/heads/main", gh["ref"])
	assert.Equal(t, "main", gh["ref_name"])
	assert.Equal(t, main, gh["sha"])
	assert.Equal(t, "feat", gh["head_ref"])
	assert.Equal(t, "main", gh["base_ref"])
	ev := gh["event"].(map[string]any)
	assert.Equal(t, "opened", ev["action"])
	assert.EqualValues(t, n, ev["number"])
	pr := ev["pull_request"].(map[string]any)
	assert.Equal(t, feat1, pr["head"].(map[string]any)["sha"])
	assert.Equal(t, main, pr["base"].(map[string]any)["sha"])
	assert.Equal(t, c.ref(domain.PullMergeRef(n)), pr["merge_commit_sha"], "the test merge, as for pull_request")

	// The cache: read-only, restores main's entries only, even trusted.
	cache := job.GetCache()
	require.NotNil(t, cache)
	assert.Equal(t, "owner/proj", cache.GetRepository())
	assert.Empty(t, cache.GetWriteRef())
	assert.Equal(t, []string{"refs/heads/main"}, cache.GetReadRefs())

	// A synchronize runs main's workflow again.
	feat2 := c.push("feat", map[string]string{"c.txt": "c\n"}, "")
	job = c.finish(runner)
	gh = c.ghContext(job)
	assert.Equal(t, "synchronize", gh["event"].(map[string]any)["action"])
	assert.Equal(t, main, gh["sha"])
	assert.Equal(t, feat2, gh["event"].(map[string]any)["after"])
	assert.True(t, job.GetTrusted())
}

// GitHub runs pull_request_target even when the request conflicts, while
// pull_request does not run.
func TestPullRequestTargetRunsOnAConflict(t *testing.T) {
	c, runner, _ := newTargetHarness(t, "")
	c.push("main", pullWF("", ""), "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "conflict")
	c.push("conflict", map[string]string{"a.txt": "feature\n"}, "")
	c.mustGit(c.wt, "checkout", "-q", "main")
	main := c.push("main", map[string]string{"a.txt": "main\n"}, "")

	n := c.openMR(c.root, "conflict")
	job := c.finish(runner)
	gh := c.ghContext(job)
	assert.Equal(t, "pull_request_target", gh["event_name"])
	assert.Equal(t, main, gh["sha"])
	assert.Nil(t, gh["event"].(map[string]any)["pull_request"].(map[string]any)["merge_commit_sha"], "no test merge")
	assert.Empty(t, c.ref(domain.PullMergeRef(n)))
	require.Len(t, c.runs(), 1, "the pull_request workflow does not run")
}

// An agent's activity runs the default branch's workflow without secrets and
// with a read-only token, unlike GitHub (T1). The head's provenance does not
// matter: its files do not run.
func TestPullRequestTargetTrustFollowsTheActor(t *testing.T) {
	c, runner, _ := newTargetHarness(t, "    types: [opened, synchronize, closed]")
	secret, _ := c.branchToken(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite, domain.ScopeMRWrite), []string{"agent/"}, "proj")
	c.mustGit(c.wt, "checkout", "-q", "-b", "agent/x")
	c.writeFile(c.wt, "x.txt", []byte("x\n"))
	c.mustGit(c.wt, "push", "-q", c.urlAuth(secret, "/proj.git"), "agent/x")

	// Opened by the agent: not trusted.
	n := c.openMR(c.api(secret), "agent/x")
	job := c.finish(runner)
	assert.Equal(t, "pull_request_target", c.ghContext(job)["event_name"])
	assert.False(t, job.GetTrusted())
	assert.Empty(t, job.GetSecrets())
	assert.NotEqual(t, "write", job.GetPermissions().GetLevels()["contents"])
	assert.Equal(t, "claude", c.runs()[0].GetActor())
	agentRun := c.runs()[0].GetId()

	// The owner re-running it does not unlock the secrets.
	_, err := c.root.run.RerunRun(c.ctx, connect.NewRequest(&pb.RerunRunRequest{Repo: "proj", RunId: agentRun}))
	require.NoError(t, err)
	job = c.finish(runner)
	assert.False(t, job.GetTrusted())
	assert.Empty(t, job.GetSecrets())

	// The owner pushes on the agent's branch: trusted, as main's code runs.
	c.push("agent/x", map[string]string{"y.txt": "y\n"}, "")
	job = c.finish(runner)
	assert.Equal(t, "synchronize", c.ghContext(job)["event"].(map[string]any)["action"])
	assert.True(t, job.GetTrusted())

	// The agent pushes: not trusted.
	c.writeFile(c.wt, "z.txt", []byte("z\n"))
	c.mustGit(c.wt, "push", "-q", c.urlAuth(secret, "/proj.git"), "agent/x")
	assert.False(t, c.finish(runner).GetTrusted())

	// The owner closes it, the head being the agent's: trusted.
	_, err = c.root.mr.CloseMergeRequest(c.ctx, connect.NewRequest(&pb.CloseMergeRequestRequest{Repo: "proj", Number: n}))
	require.NoError(t, err)
	job = c.finish(runner)
	assert.Equal(t, "closed", c.ghContext(job)["event"].(map[string]any)["action"])
	assert.True(t, job.GetTrusted())

	// The owner opens a request of a branch the agent pushed: trusted too.
	c.mustGit(c.wt, "checkout", "-q", "-b", "agent/y", "main")
	c.writeFile(c.wt, "w.txt", []byte("w\n"))
	c.mustGit(c.wt, "push", "-q", c.urlAuth(secret, "/proj.git"), "agent/y")
	c.openMR(c.root, "agent/y")
	job = c.finish(runner)
	assert.True(t, job.GetTrusted())
	assert.Equal(t, "repo-token", job.GetSecrets()["API_TOKEN"])
}

// The default tip runs: it must have been pushed by a trusted principal, as
// taken when the activity happens.
func TestPullRequestTargetTrustFollowsTheDefaultTip(t *testing.T) {
	c, hook := newHookedHarness(t)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	main := c.push("main", targetWF("", "from-main"), "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "f1")
	c.push("f1", map[string]string{"1.txt": "1\n"}, "")

	// main's record says an untrusted principal set it there.
	require.NoError(t, c.forge.store.CI().PutRefTip(c.ctx, domain.CIRefTip{
		Repo: c.name("proj"), Ref: "refs/heads/main", Sha: main, Actor: "claude", Trusted: false,
	}))
	c.openMR(c.root, "f1")
	job := c.finish(runner)
	assert.Equal(t, main, job.GetSha())
	assert.False(t, job.GetTrusted(), "main was not pushed by a trusted principal")

	// The owner pushes main: trusted again.
	c.mustGit(c.wt, "checkout", "-q", "main")
	main2 := c.push("main", map[string]string{"m.txt": "m\n"}, "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "f2", "main")
	c.push("f2", map[string]string{"2.txt": "2\n"}, "")
	c.openMR(c.root, "f2")
	job = c.finish(runner)
	assert.Equal(t, main2, job.GetSha())
	assert.True(t, job.GetTrusted())

	// The tip is the one of the activity: main moving before the event is
	// handled changes neither the code nor the trust.
	c.mustGit(c.wt, "checkout", "-q", "-b", "f3", "main")
	c.push("f3", map[string]string{"3.txt": "3\n"}, "")
	reached, release := hook.block(t, "event")
	c.openMR(c.root, "f3")
	await(t, reached, "the opening event")
	c.mustGit(c.wt, "checkout", "-q", "main")
	main3 := c.push("main", targetWF("", "moved"), "")
	require.NoError(t, c.forge.store.CI().PutRefTip(c.ctx, domain.CIRefTip{
		Repo: c.name("proj"), Ref: "refs/heads/main", Sha: main3, Actor: "claude", Trusted: false,
	}))
	release()
	job = c.finish(runner)
	assert.Equal(t, main2, job.GetSha())
	assert.Contains(t, job.GetWorkflowYaml(), "from-main")
	assert.True(t, job.GetTrusted())
}

// Types, branches and paths filter as for pull_request: the base branch and the
// files between the merge base and the head.
func TestPullRequestTargetTypesAndFilters(t *testing.T) {
	c, runner, _ := newTargetHarness(t, "    types: [closed]\n    branches: [main]\n    paths: ['docs/**']")
	c.mustGit(c.wt, "checkout", "-q", "-b", "f1")
	c.push("f1", map[string]string{"src/x.go": "x\n"}, "")
	n1 := c.openMR(c.root, "f1")
	c.mustGit(c.wt, "checkout", "-q", "-b", "f2", "main")
	f2 := c.push("f2", map[string]string{"docs/a.md": "a\n"}, "")
	n2 := c.openMR(c.root, "f2")
	c.waitFor("the head refs", func() bool { return c.ref(domain.PullHeadRef(n1)) != "" && c.ref(domain.PullHeadRef(n2)) == f2 })

	for _, n := range []int64{n1, n2} {
		_, err := c.root.mr.CloseMergeRequest(c.ctx, connect.NewRequest(&pb.CloseMergeRequestRequest{Repo: "proj", Number: n}))
		require.NoError(t, err)
	}
	job := c.finish(runner)
	ev := c.ghContext(job)["event"].(map[string]any)
	assert.Equal(t, "closed", ev["action"])
	assert.EqualValues(t, n2, ev["number"])
	require.Len(t, c.runs(), 1, "neither opened nor the request without docs")
	assert.True(t, strings.HasPrefix(c.runs()[0].GetRef(), "refs/heads/"))
}

// Job.cache of the other events (CI-3 D8): save under the run's ref, restore
// from it, the base branch of a merge request, then the default branch.
func TestJobCacheScopes(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	files := pullWF("", "")
	files[".github/workflows/push.yml"] = "on: push\njobs:\n  p:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"
	c.push("main", files, "")
	job := c.finish(runner)
	assert.Equal(t, "refs/heads/main", job.GetCache().GetWriteRef())
	assert.Equal(t, []string{"refs/heads/main"}, job.GetCache().GetReadRefs())

	c.mustGit(c.wt, "checkout", "-q", "-b", "dev")
	c.push("dev", map[string]string{"d.txt": "d\n"}, "")
	job = c.finish(runner)
	assert.Equal(t, "refs/heads/dev", job.GetCache().GetWriteRef())
	assert.Equal(t, []string{"refs/heads/dev", "refs/heads/main"}, job.GetCache().GetReadRefs())

	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	c.push("feat", map[string]string{"f.txt": "f\n"}, "")
	c.finish(runner) // its push run
	c.openMRInto("feat", "dev")
	job = c.finish(runner)
	assert.Equal(t, "pull_request", c.ghContext(job)["event_name"])
	assert.Equal(t, "owner/proj", job.GetCache().GetRepository())
	assert.Equal(t, "refs/pull/1/merge", job.GetCache().GetWriteRef())
	assert.Equal(t, []string{"refs/pull/1/merge", "refs/heads/dev", "refs/heads/main"}, job.GetCache().GetReadRefs())
}

// An untrusted run never saves under the default branch: the cache main's
// trusted runs restore is written by trusted runs only.
func TestJobCacheOfUntrustedRuns(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	files := merge(map[string]string{".github/workflows/push.yml": "name: push\non: [push, workflow_dispatch]\njobs:\n  p:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"},
		followWorkflow("follow", "    workflows: [push]\n    types: [requested]\n    branches: ['agent/**']"))
	main := c.push("main", files, "")
	job := c.finish(runner)
	assert.True(t, job.GetTrusted())
	assert.Equal(t, "refs/heads/main", job.GetCache().GetWriteRef(), "the owner's push of main")

	// The agent's push of its branch: untrusted, saves under that branch only.
	agent, _ := c.branchToken(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite, domain.ScopeCIWrite), []string{"agent/"}, "proj")
	c.pushAs(agent, "agent/x", map[string]string{"x.txt": "x\n"})
	jobs := map[string]*pb.Job{}
	for range 2 {
		j := c.finish(runner)
		jobs[c.ghContext(j)["event_name"].(string)] = j
	}
	job = jobs["push"]
	require.NotNil(t, job)
	assert.False(t, job.GetTrusted())
	assert.Equal(t, "refs/untrusted/heads/agent/x", job.GetCache().GetWriteRef(), "in the namespace of untrusted entries")
	assert.Equal(t, []string{"refs/untrusted/heads/agent/x", "refs/heads/agent/x", "refs/heads/main"}, job.GetCache().GetReadRefs())

	// Its workflow_run runs on main and inherits the trust: read-only.
	job = jobs["workflow_run"]
	require.NotNil(t, job)
	assert.Equal(t, "refs/heads/main", job.GetRef())
	assert.False(t, job.GetTrusted())
	assert.Empty(t, job.GetCache().GetWriteRef())
	assert.Equal(t, []string{"refs/heads/main"}, job.GetCache().GetReadRefs())

	// A dispatch on main whose tip an untrusted principal set: read-only.
	require.NoError(t, c.forge.store.CI().PutRefTip(c.ctx, domain.CIRefTip{
		Repo: c.name("proj"), Ref: "refs/heads/main", Sha: main, Actor: "claude", Trusted: false,
	}))
	_, err := c.dispatch(c.root, "push.yml", "main", nil)
	require.NoError(t, err)
	job = c.finish(runner)
	assert.Equal(t, "workflow_dispatch", c.ghContext(job)["event_name"])
	assert.False(t, job.GetTrusted())
	assert.Empty(t, job.GetCache().GetWriteRef())
}

// The run pages show the merge request of a pull_request_target run, whose
// ref is the default branch.
func TestWebShowsTheMergeRequestOfAPullRequestTargetRun(t *testing.T) {
	c := newCIHarness(t, nil)
	b := browser{c.harness}
	runner := c.registerRunner("alpha", "ubuntu-latest")
	_, err := c.root.repo.UpdateRepo(c.ctx, connect.NewRequest(&pb.UpdateRepoRequest{
		Repo: "proj", Visibility: pb.Visibility_VISIBILITY_PUBLIC, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"visibility"}},
	}))
	require.NoError(t, err)
	c.push("main", targetWF("", "x"), "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	c.push("feat", map[string]string{"f.txt": "f\n"}, "")
	n := c.openMR(c.root, "feat")
	c.finish(runner)
	run := c.runs()[0]
	require.Equal(t, "pull_request_target", run.GetEvent())

	for _, page := range []string{"/proj/actions", fmt.Sprintf("/proj/actions/runs/%d", run.GetId())} {
		res := b.get(page, nil)
		require.Equal(t, 200, res.status, res.body)
		assert.Contains(t, res.body, fmt.Sprintf(`href="/proj/merge-requests/%d"`, n), page)
		assert.Contains(t, res.body, fmt.Sprintf("!%d", n), page)
	}
}

// A trusted run never restores what an untrusted run saved: the owner pushing
// on top of an agent's branch, or on the merge request the agent opened, gets
// the trusted entries only.
func TestJobCacheTrustedRunsNeverReadUntrustedEntries(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	files := pullWF("", "")
	files[".github/workflows/push.yml"] = "on: push\njobs:\n  p:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"
	c.push("main", files, "")
	c.finish(runner)
	agent, _ := c.branchToken(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite, domain.ScopeMRWrite), []string{"agent/"}, "proj")
	c.pushAs(agent, "agent/x", map[string]string{"x.txt": "x\n"})
	assert.Equal(t, "refs/untrusted/heads/agent/x", c.finish(runner).GetCache().GetWriteRef())

	// The agent opens the request: an untrusted pull_request run.
	c.openMR(c.api(agent), "agent/x")
	job := c.finish(runner)
	require.Equal(t, "pull_request", c.ghContext(job)["event_name"])
	assert.False(t, job.GetTrusted())
	assert.Equal(t, "refs/untrusted/pull/1/merge", job.GetCache().GetWriteRef())
	assert.Equal(t, []string{"refs/untrusted/pull/1/merge", "refs/pull/1/merge", "refs/heads/main"}, job.GetCache().GetReadRefs())

	// The owner pushes on top: the push run and the synchronize are trusted
	// and see no untrusted entry, at any level.
	c.push("agent/x", map[string]string{"y.txt": "y\n"}, "")
	jobs := map[string]*pb.Job{}
	for range 2 {
		j := c.finish(runner)
		jobs[c.ghContext(j)["event_name"].(string)] = j
	}
	push, pull := jobs["push"], jobs["pull_request"]
	require.NotNil(t, push)
	require.NotNil(t, pull)
	assert.True(t, push.GetTrusted())
	assert.Equal(t, "refs/heads/agent/x", push.GetCache().GetWriteRef())
	assert.Equal(t, []string{"refs/heads/agent/x", "refs/heads/main"}, push.GetCache().GetReadRefs())
	assert.True(t, pull.GetTrusted())
	assert.Equal(t, "refs/pull/1/merge", pull.GetCache().GetWriteRef())
	assert.Equal(t, []string{"refs/pull/1/merge", "refs/heads/main"}, pull.GetCache().GetReadRefs())
	for _, j := range jobs {
		for _, r := range j.GetCache().GetReadRefs() {
			assert.False(t, strings.HasPrefix(r, "refs/untrusted/"), r)
		}
	}
}
