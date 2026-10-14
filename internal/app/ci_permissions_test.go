// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
)

// CI-3 D3 / AC3: the permissions of the job token, the push guard on workflow
// files and the rule that a job token's push starts no run.

const permsWorkflow = `on: push
permissions:
  contents: write
jobs:
  wf:
    runs-on: ubuntu-latest
    steps:
      - run: echo wf
  readonly:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      pull-requests: write
    steps:
      - run: echo ro
  none:
    runs-on: ubuntu-latest
    permissions: {}
    steps:
      - run: echo none
  all:
    runs-on: ubuntu-latest
    permissions: write-all
    steps:
      - run: echo all
`

const defaultPermsWorkflow = "on: push\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"

// tokenPush pushes refspec to the repository with the job token, as a job that
// runs git push with the GITHUB_TOKEN does.
func (c *ciHarness) tokenPush(dir, token string, refspecs ...string) (string, error) {
	args := []string{"-c", "http.extraheader=" + checkoutHeader(token), "push", c.url("/owner/proj")}
	return c.git(dir, append(args, refspecs...)...)
}

// commitIn commits a file in dir on its current branch.
func (c *ciHarness) commitIn(dir, path, content string) {
	c.t.Helper()
	c.writeFile(dir, path, []byte(content))
}

// jobsOf collects the jobs of the one run by key.
func startedJobs(runner *fakeRunner, n int) map[string]*pb.Job {
	return runner.jobsByKey(n)
}

func levelsOf(j *pb.Job) map[string]string { return j.GetPermissions().GetLevels() }

func TestJobPermissionsAreResolvedStoredAndSent(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/p.yml": permsWorkflow}, "")
	jobs := startedJobs(runner, 4)

	wf := levelsOf(jobs["wf"])
	assert.Equal(t, map[string]string{"metadata": "read", "contents": "write"}, wf, "the workflow's permissions")
	assert.True(t, jobs["wf"].GetTrusted())
	assert.Equal(t, map[string]string{"metadata": "read", "contents": "read", "pull-requests": "write"}, levelsOf(jobs["readonly"]), "the job's replace the workflow's")
	assert.Equal(t, map[string]string{"metadata": "read"}, levelsOf(jobs["none"]), "permissions: {} is all none")
	assert.Equal(t, "write", levelsOf(jobs["all"])["issues"])
	assert.Equal(t, "write", levelsOf(jobs["all"])["contents"])
	assert.Equal(t, "read", levelsOf(jobs["all"])["models"], "write-all keeps models at read")

	// The record keeps what the workflow asked for.
	stored, err := c.forge.store.CI().ListJobs(c.ctx, c.runs()[0].GetId(), 1)
	require.NoError(t, err)
	byKey := map[string]domain.CIJob{}
	for _, j := range stored {
		byKey[j.Key] = j
	}
	assert.Equal(t, "write", byKey["wf"].Permissions["contents"])
	assert.Equal(t, map[string]string{"metadata": "read"}, byKey["none"].Permissions)

	// What each token may do on the repository.
	push := func(j *pb.Job, branch string) (string, error) {
		c.mustGit(c.wt, "checkout", "-q", "main")
		c.commitIn(c.wt, "n-"+branch+".txt", branch)
		return c.tokenPush(c.wt, j.GetGithubToken(), "HEAD:refs/heads/main")
	}
	_, err = push(jobs["readonly"], "r")
	require.Error(t, err, "contents: read cannot push")
	_, err = push(jobs["none"], "n")
	require.Error(t, err, "contents: none cannot push")
	c.mustGit(c.wt, "reset", "-q", "--hard", "HEAD~2")
	out, err := c.clone(jobs["none"].GetGithubToken(), c.url("/owner/proj"))
	require.Error(t, err, "contents: none cannot even clone a private repository: "+out)
	out, err = c.clone(jobs["readonly"].GetGithubToken(), c.url("/owner/proj"))
	require.NoError(t, err, out)
	out, err = push(jobs["wf"], "w")
	require.NoError(t, err, out)
	out, err = push(jobs["all"], "a")
	require.NoError(t, err, out)
}

func TestDefaultTokenCannotPush(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/p.yml": defaultPermsWorkflow}, "")
	job := runner.await()
	assert.Equal(t, map[string]string{"metadata": "read", "contents": "read", "packages": "read"}, levelsOf(job), "the restricted default")

	c.commitIn(c.wt, "x.txt", "x")
	before := c.bareGit("proj", "rev-parse", "refs/heads/main")
	out, err := c.tokenPush(c.wt, job.GetGithubToken(), "HEAD:refs/heads/main")
	require.Error(t, err)
	assert.Contains(t, out, "403")
	assert.Equal(t, before, c.bareGit("proj", "rev-parse", "refs/heads/main"))
	out, err = c.clone(job.GetGithubToken(), c.url("/owner/proj"))
	require.NoError(t, err, "it reads: "+out)
}

func TestJobTokenWithContentsWritePushesButNotWorkflows(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	wfYAML := "on:\n  push:\n  create:\npermissions:\n  contents: write\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"
	c.push("main", map[string]string{".github/workflows/p.yml": wfYAML}, "")
	c.push("feat", map[string]string{"feat.txt": "f"}, "")
	job := runner.await()
	tok := job.GetGithubToken()
	require.Equal(t, "write", levelsOf(job)["contents"])
	c.waitFor("two runs", func() bool { return len(c.runs()) == 2 })
	runsBefore := len(c.runs())
	ref := func(r string) string { return c.bareGit("proj", "rev-parse", "--verify", "-q", r) }
	refs := func() string { return c.bareGit("proj", "for-each-ref", "--format=%(refname) %(objectname)") }

	// A commit on an existing branch is accepted.
	c.mustGit(c.wt, "checkout", "-q", "main")
	c.commitIn(c.wt, "notes.txt", "generated")
	out, err := c.tokenPush(c.wt, tok, "HEAD:refs/heads/main")
	require.NoError(t, err, out)
	assert.Equal(t, strings.TrimSpace(c.mustGit(c.wt, "rev-parse", "HEAD")), ref("refs/heads/main"))

	// A new branch and a new tag are accepted when their commits carry no
	// workflow (a root commit with a README here).
	orphan := c.workTree("orphan", "README")
	out, err = c.tokenPush(orphan, tok, "orphan:refs/heads/orphan")
	require.NoError(t, err, out)
	require.NotEmpty(t, ref("refs/heads/orphan"))
	out, err = c.tokenPush(orphan, tok, "orphan:refs/tags/v0")
	require.NoError(t, err, out)
	require.NotEmpty(t, ref("refs/tags/v0"))
	// And the branch they made can be deleted, as can one that holds workflows.
	out, err = c.tokenPush(orphan, tok, ":refs/heads/orphan")
	require.NoError(t, err, out)
	out, err = c.tokenPush(c.wt, tok, ":refs/heads/feat")
	require.NoError(t, err, "deleting a branch changes no workflow: "+out)

	// Refused: the whole push, whatever the ref, with GitHub's message.
	refusal := func(path string) string {
		return "refusing to allow a GitHub App to create or update workflow `" + path + "` without `workflows` permission"
	}
	before := refs()
	c.mustGit(c.wt, "checkout", "-q", "-B", "w", "main")
	c.commitIn(c.wt, ".github/workflows/new.yml", wfYAML) // create a file
	out, err = c.tokenPush(c.wt, tok, "w:refs/heads/w")
	require.Error(t, err)
	assert.Contains(t, out, refusal(".github/workflows/new.yml"))
	assert.Equal(t, before, refs())

	c.mustGit(c.wt, "checkout", "-q", "-B", "m", "main")
	c.commitIn(c.wt, ".github/workflows/p.yml", wfYAML+"# changed\n") // modify one
	out, err = c.tokenPush(c.wt, tok, "m:refs/heads/main")
	require.Error(t, err, "an update of main that touches a workflow")
	assert.Contains(t, out, refusal(".github/workflows/p.yml"))
	out, err = c.tokenPush(c.wt, tok, "+m:refs/heads/main")
	require.Error(t, err, "forced too")
	assert.Contains(t, out, refusal(".github/workflows/p.yml"))
	assert.Equal(t, before, refs())

	c.mustGit(c.wt, "checkout", "-q", "-B", "r", "main")
	c.writeFile(c.wt, "moved.yml", []byte(wfYAML))
	c.mustGit(c.wt, "mv", "moved.yml", ".github/workflows/moved.yml") // rename into
	c.mustGit(c.wt, "commit", "-q", "-m", "mv")
	out, err = c.tokenPush(c.wt, tok, "r:refs/heads/main")
	require.Error(t, err, "a rename into .github/workflows")
	assert.Contains(t, out, refusal(".github/workflows/moved.yml"))

	// A ref cut from the head of main carries main's workflows and changes none:
	// a tag, an annotated tag and a branch there are allowed, as are new branches
	// with commits that touch no workflow (staging run 855).
	out, err = c.tokenPush(c.wt, tok, "main:refs/tags/v1")
	require.NoError(t, err, "a tag on main's head: "+out)
	c.mustGit(c.wt, "tag", "-a", "-m", "release", "va", "main")
	out, err = c.tokenPush(c.wt, tok, "refs/tags/va:refs/tags/va")
	require.NoError(t, err, "an annotated tag on main's head: "+out)
	out, err = c.tokenPush(c.wt, tok, "main:refs/heads/copy")
	require.NoError(t, err, "a branch at main's head: "+out)
	c.mustGit(c.wt, "checkout", "-q", "-B", "smoke/perms-ok", "main")
	c.commitIn(c.wt, "smoke.txt", "ok")
	out, err = c.tokenPush(c.wt, tok, "smoke/perms-ok:refs/heads/smoke/perms-ok")
	require.NoError(t, err, "a new branch with a commit that touches no workflow: "+out)
	c.mustGit(c.wt, "checkout", "-q", "-B", "smoke/wf", "main")
	c.commitIn(c.wt, ".github/workflows/smoke.yml", wfYAML)
	out, err = c.tokenPush(c.wt, tok, "smoke/wf:refs/heads/smoke/wf")
	require.Error(t, err, "a new branch with a commit that adds a workflow: "+out)
	assert.Contains(t, out, refusal(".github/workflows/smoke.yml"))

	// One ref that touches a workflow refuses a push of several.
	c.mustGit(c.wt, "checkout", "-q", "-B", "ok", "main")
	c.commitIn(c.wt, "fine.txt", "fine")
	before = refs()
	out, err = c.tokenPush(c.wt, tok, "ok:refs/heads/main", "r:refs/heads/other")
	require.Error(t, err)
	assert.Contains(t, out, refusal(".github/workflows/moved.yml"))
	assert.Equal(t, before, refs(), "nothing moved, not even the fine ref")

	// The owner's token is not held to it.
	out, err = c.git(c.wt, "push", c.urlAuth(adminToken, "/proj.git"), "r:refs/heads/rworkflow")
	require.NoError(t, err, out)

	// A job token cannot put a ref on that existing commit, whose workflows
	// differ from the default branch's, nor move main back onto it.
	out, err = c.tokenPush(c.wt, tok, "r:refs/heads/cut")
	require.Error(t, err, "a new branch at an existing commit with other workflows: "+out)
	assert.Contains(t, out, "without `workflows` permission")

	// None of the pushes of the job token started a run; a push of the owner
	// does, and queues after them.
	c.push("sentinel", map[string]string{"s.txt": "s"}, "")
	c.waitFor("the owner's runs", func() bool { return len(c.runs()) >= runsBefore+2 })
	for _, r := range c.runs() {
		assert.NotEqual(t, "refs/heads/orphan", r.GetRef(), "no run for a job token's push")
		assert.NotEqual(t, "refs/tags/v0", r.GetRef())
	}
	assert.Len(t, c.runs(), runsBefore+2, "the owner's rworkflow and sentinel pushes only")
}

func TestJobTokenCannotPushToAnotherRepository(t *testing.T) {
	c := newCIHarness(t, nil)
	c.seed("other", domain.VisibilityPublic, "a.txt")
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/p.yml": "on: push\npermissions:\n  contents: write\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"}, "")
	job := runner.await()
	require.Equal(t, "write", levelsOf(job)["contents"])
	before := c.bareGit("other", "rev-parse", "refs/heads/main")
	wt := c.workTree("main", "b.txt")
	out, err := c.git(wt, "-c", "http.extraheader="+checkoutHeader(job.GetGithubToken()), "push", "--force", c.url("/owner/other"), "main:refs/heads/main")
	require.Error(t, err, out)
	assert.Equal(t, before, c.bareGit("other", "rev-parse", "refs/heads/main"))
	status, _, _ := c.get("/owner/other/info/refs?service=git-receive-pack", basic(job.GetGithubToken()))
	assert.Equal(t, http.StatusForbidden, status)
	_ = out
}

// D1: the run of a token limited to branches is not trusted: whatever the
// workflow asks, its job token is read-only.
func TestBranchLimitedActorsJobTokenIsReadOnly(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	agent, _ := c.branchToken(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite), []string{"agent/x/"}, "proj")
	c.push("main", map[string]string{".github/workflows/p.yml": "on: push\npermissions: write-all\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"}, "")
	owner := runner.await()
	assert.Equal(t, "write", levelsOf(owner)["contents"])
	runner.runJob(owner, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)

	c.mustGit(c.wt, "checkout", "-q", "main")
	c.pushAs(agent, "agent/x/a", map[string]string{"a.txt": "a"})
	job := runner.await()
	assert.False(t, job.GetTrusted())
	lv := levelsOf(job)
	assert.Equal(t, "read", lv["contents"], "write lowered to read")
	assert.Equal(t, "read", lv["issues"])
	assert.Equal(t, "read", lv["models"])
	assert.NotContains(t, lv, "id-token", "a permission with no read level is none")
	assert.NotContains(t, lv, "copilot-requests")
	for k, v := range lv {
		assert.NotEqual(t, "write", v, k)
	}

	c.commitIn(c.wt, "evil.txt", "x")
	out, err := c.tokenPush(c.wt, job.GetGithubToken(), "HEAD:refs/heads/agent/x/a")
	require.Error(t, err, "its token cannot push: "+out)
	out, err = c.clone(job.GetGithubToken(), c.url("/owner/proj"))
	require.NoError(t, err, "but it reads: "+out)
}

// D1 / D3 on re-runs: the trust of a run is kept or lowered and the token of the
// new attempt follows it, whoever runs it again.
func TestRerunsNeverRaiseTheJobTokenPermissions(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	agent, _ := c.branchToken(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite, domain.ScopeCIWrite), []string{"agent/x/"}, "proj")
	wf := "on: push\npermissions: write-all\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"
	rerun := func(api *clients, id int64) error {
		_, err := api.run.RerunRun(c.ctx, connect.NewRequest(&pb.RerunRunRequest{Repo: "proj", RunId: id}))
		return err
	}
	byRef := func(ref string) *pb.Run {
		for _, r := range c.runs() {
			if r.GetRef() == ref {
				return r
			}
		}
		return nil
	}
	done := func(j *pb.Job) {
		runner.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	}
	noPush := func(j *pb.Job, ref string) {
		t.Helper()
		assert.False(t, j.GetTrusted())
		assert.Equal(t, "read", levelsOf(j)["contents"])
		for k, v := range levelsOf(j) {
			assert.NotEqual(t, "write", v, k)
		}
		c.mustGit(c.wt, "checkout", "-q", "-B", "tmp", "main")
		c.commitIn(c.wt, "x-"+ref[len(ref)-1:]+".txt", "x")
		out, err := c.tokenPush(c.wt, j.GetGithubToken(), "HEAD:refs/heads/"+ref)
		require.Error(t, err, "the token cannot push: "+out)
	}

	// The owner's run: write-all, trusted.
	c.push("main", map[string]string{".github/workflows/p.yml": wf}, "")
	owner := runner.await()
	assert.True(t, owner.GetTrusted())
	assert.Equal(t, "write", levelsOf(owner)["contents"])
	done(owner)
	c.waitFor("owner run done", func() bool {
		r := byRef("refs/heads/main")
		return r != nil && r.GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED
	})
	mainRun := byRef("refs/heads/main")

	// The agent's run: untrusted, read-only, and the owner's re-run of it too.
	c.mustGit(c.wt, "checkout", "-q", "main")
	c.pushAs(agent, "agent/x/a", map[string]string{"a.txt": "a"})
	first := runner.await()
	noPush(first, "agent/x/a")
	done(first)
	c.waitFor("agent run done", func() bool {
		r := byRef("refs/heads/agent/x/a")
		return r != nil && r.GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED
	})
	agentRun := byRef("refs/heads/agent/x/a")
	require.NoError(t, rerun(c.root, agentRun.GetId()))
	again := runner.await()
	assert.Equal(t, int32(2), again.GetAttempt())
	noPush(again, "agent/x/a")
	done(again)

	// The agent runs the owner's run again: refused, as it is outside its
	// branches, and the run stays trusted with a token that writes.
	require.Error(t, rerun(c.api(agent), mainRun.GetId()))
	assert.True(t, c.getRun(mainRun.GetId()).GetRun().GetTrusted())

	// A run of the owner on a branch the agent may touch: the agent's re-run of
	// it is allowed, and its attempt is read-only.
	c.mustGit(c.wt, "checkout", "-q", "-B", "agent/x/own", "main")
	c.commitIn(c.wt, "own.txt", "own")
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), "agent/x/own")
	ownerOnBranch := runner.await()
	assert.True(t, ownerOnBranch.GetTrusted())
	assert.Equal(t, "write", levelsOf(ownerOnBranch)["contents"])
	done(ownerOnBranch)
	c.waitFor("owner branch run done", func() bool {
		r := byRef("refs/heads/agent/x/own")
		return r != nil && r.GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED
	})
	branchRun := byRef("refs/heads/agent/x/own")
	require.NoError(t, rerun(c.api(agent), branchRun.GetId()))
	lowered := runner.await()
	noPush(lowered, "agent/x/own")
	assert.False(t, c.getRun(branchRun.GetId()).GetRun().GetTrusted(), "the re-run is not trusted")
	done(lowered)
}
