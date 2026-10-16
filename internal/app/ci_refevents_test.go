// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
)

// CI-4 D6: the create and delete events.

const refEventWorkflow = `name: %s
on: %s
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ secrets.API_TOKEN }} %s
`

func refEventWF(event, mark string) map[string]string {
	return map[string]string{".github/workflows/" + event + ".yml": fmt.Sprintf(refEventWorkflow, event, event, mark)}
}

// newRefEventHarness is a harness with a repository secret, the workflow of
// event on main and the create run of main itself finished when event is create.
func newRefEventHarness(t *testing.T, event string) (*ciHarness, *fakeRunner, string) {
	t.Helper()
	c := newSecretsHarness(t, secretsKeyA)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.setSecret(repoScope("proj"), "API_TOKEN", "repo-token")
	main := c.push("main", refEventWF(event, "from-main"), "")
	if event == "create" {
		job := c.finish(runner)
		assert.Equal(t, "refs/heads/main", job.GetRef(), "creating main is a create event")
	}
	return c, runner, main
}

func eventOf(c *ciHarness, j *pb.Job) map[string]any {
	return c.ghContext(j)["event"].(map[string]any)
}

func TestCreateRunsOnTheCreatedRef(t *testing.T) {
	c, runner, _ := newRefEventHarness(t, "create")

	// A new branch: its own workflow file runs, on its commit.
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	feat := c.push("feat", refEventWF("create", "from-feat"), "")
	job := c.finish(runner)
	assert.Contains(t, job.GetWorkflowYaml(), "from-feat")
	assert.Equal(t, "refs/heads/feat", job.GetRef())
	assert.Equal(t, feat, job.GetSha())
	assert.True(t, job.GetTrusted())
	assert.Equal(t, "repo-token", job.GetSecrets()["API_TOKEN"])
	gh := c.ghContext(job)
	assert.Equal(t, "create", gh["event_name"])
	assert.Equal(t, "refs/heads/feat", gh["ref"])
	assert.Equal(t, "feat", gh["ref_name"])
	assert.Equal(t, "branch", gh["ref_type"])
	ev := eventOf(c, job)
	assert.Equal(t, "feat", ev["ref"])
	assert.Equal(t, "branch", ev["ref_type"])
	assert.Equal(t, "main", ev["master_branch"])
	assert.Equal(t, "user", ev["pusher_type"])
	assert.Contains(t, ev, "description")
	assert.Nil(t, ev["description"])
	assert.NotNil(t, ev["repository"])
	assert.NotNil(t, ev["sender"])
	assert.NotContains(t, ev, "commits", "not a push payload")
	assert.Equal(t, "refs/heads/feat", job.GetCache().GetWriteRef(), "a trusted run saves under its ref")

	// A tag.
	c.mustGit(c.wt, "tag", "v1")
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), "v1")
	job = c.finish(runner)
	assert.Equal(t, "refs/tags/v1", job.GetRef())
	ev = eventOf(c, job)
	assert.Equal(t, "v1", ev["ref"])
	assert.Equal(t, "tag", ev["ref_type"])

	// Moving an existing branch creates nothing.
	c.push("feat", map[string]string{"b.txt": "b\n"}, "")
	c.mustGit(c.wt, "checkout", "-q", "main")
	c.push("main", map[string]string{"sentinel.txt": "s\n"}, "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "sentinel")
	c.push("sentinel", map[string]string{"s.txt": "s\n"}, "")
	job = c.finish(runner)
	assert.Equal(t, "refs/heads/sentinel", job.GetRef(), "the update of feat and main started no run")
	for _, r := range c.runs() {
		assert.Equal(t, "create", r.GetEvent())
	}
	assert.Len(t, c.runs(), 4, "main, feat, v1, sentinel")
}

// More than three tags at once create no event, as on GitHub.
func TestCreateSkipsMoreThanThreeTags(t *testing.T) {
	c, runner, _ := newRefEventHarness(t, "create")
	for _, tag := range []string{"t1", "t2", "t3", "t4"} {
		c.mustGit(c.wt, "tag", tag)
	}
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), "t1", "t2", "t3", "t4")
	c.mustGit(c.wt, "checkout", "-q", "-b", "sentinel")
	c.push("sentinel", map[string]string{"s.txt": "s\n"}, "")
	job := c.finish(runner)
	assert.Equal(t, "refs/heads/sentinel", job.GetRef())
	assert.Len(t, c.runs(), 2, "main and sentinel")
}

func TestDeleteRunsTheDefaultBranchWorkflow(t *testing.T) {
	c, runner, main := newRefEventHarness(t, "delete")
	// The deleted branch carries another version of the workflow: it is not
	// the one that runs.
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	c.push("feat", refEventWF("delete", "from-feat"), "")
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), ":feat")

	job := c.finish(runner)
	assert.Contains(t, job.GetWorkflowYaml(), "from-main")
	assert.Equal(t, "refs/heads/main", job.GetRef())
	assert.Equal(t, main, job.GetSha())
	assert.True(t, job.GetTrusted())
	assert.Equal(t, "repo-token", job.GetSecrets()["API_TOKEN"])
	gh := c.ghContext(job)
	assert.Equal(t, "delete", gh["event_name"])
	assert.Equal(t, "refs/heads/main", gh["ref"])
	assert.Equal(t, main, gh["sha"])
	ev := eventOf(c, job)
	assert.Equal(t, "feat", ev["ref"])
	assert.Equal(t, "branch", ev["ref_type"])
	assert.Equal(t, "user", ev["pusher_type"])
	assert.NotContains(t, ev, "master_branch")
	assert.NotNil(t, ev["repository"])
	assert.NotNil(t, ev["sender"])
	assert.Equal(t, "refs/heads/main", job.GetCache().GetWriteRef())

	runs := c.runs()
	require.Len(t, runs, 1)
	assert.Equal(t, "delete", runs[0].GetEvent())
	assert.Equal(t, "refs/heads/main", runs[0].GetRef())

	// A tag.
	c.mustGit(c.wt, "tag", "v1", "main")
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), "v1")
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), ":refs/tags/v1")
	ev = eventOf(c, c.finish(runner))
	assert.Equal(t, "v1", ev["ref"])
	assert.Equal(t, "tag", ev["ref_type"])
}

// An agent creating or deleting runs untrusted; an owner's delete runs
// untrusted when the default tip was not pushed by a trusted principal.
func TestCreateAndDeleteTrust(t *testing.T) {
	c := newSecretsHarness(t, secretsKeyA)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.setSecret(repoScope("proj"), "API_TOKEN", "repo-token")
	files := refEventWF("create", "c")
	for k, v := range refEventWF("delete", "d") {
		files[k] = v
	}
	main := c.push("main", files, "")
	require.True(t, c.finish(runner).GetTrusted(), "the owner created main")
	secret, _ := c.branchToken(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite), []string{"agent/"}, "proj")
	agentURL := c.urlAuth(secret, "/proj.git")

	// The agent creates a branch: untrusted.
	c.mustGit(c.wt, "checkout", "-q", "-b", "agent/x")
	c.writeFile(c.wt, "x.txt", []byte("x\n"))
	c.mustGit(c.wt, "push", "-q", agentURL, "agent/x")
	job := c.finish(runner)
	assert.Equal(t, "create", c.ghContext(job)["event_name"])
	assert.False(t, job.GetTrusted())
	assert.Empty(t, job.GetSecrets())
	assert.Equal(t, "refs/untrusted/heads/agent/x", job.GetCache().GetWriteRef())

	// The agent deletes it: untrusted, and the default branch is not written.
	c.mustGit(c.wt, "push", "-q", agentURL, ":agent/x")
	job = c.finish(runner)
	assert.Equal(t, "delete", c.ghContext(job)["event_name"])
	assert.Equal(t, main, job.GetSha())
	assert.False(t, job.GetTrusted())
	assert.Empty(t, job.GetSecrets())
	assert.Empty(t, job.GetCache().GetWriteRef(), "an untrusted run never saves under the default branch")

	// The owner deletes a branch the agent pushed: trusted, the deleted ref's
	// record does not matter (it is gone with the ref).
	c.mustGit(c.wt, "push", "-q", agentURL, "agent/x")
	c.finish(runner) // create
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), ":agent/x")
	job = c.finish(runner)
	assert.Equal(t, "delete", c.ghContext(job)["event_name"])
	assert.True(t, job.GetTrusted())
	assert.Equal(t, "repo-token", job.GetSecrets()["API_TOKEN"])

	// The owner deletes while main's record says an untrusted principal set
	// it: the default tip's code runs, so untrusted.
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), "agent/x:refs/heads/other")
	c.finish(runner) // create
	require.NoError(t, c.forge.store.CI().PutRefTip(c.ctx, domain.CIRefTip{
		Repo: c.name("proj"), Ref: "refs/heads/main", Sha: main, Actor: "claude", Trusted: false,
	}))
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), ":other")
	job = c.finish(runner)
	assert.Equal(t, "delete", c.ghContext(job)["event_name"])
	assert.False(t, job.GetTrusted())
	assert.Empty(t, job.GetSecrets())
}
