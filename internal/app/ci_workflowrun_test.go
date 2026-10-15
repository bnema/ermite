// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/usecase"
)

// CI-3 D5, AC5: workflow_run.

const triggeringWorkflow = "name: ci\non: push\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"

func followWorkflow(name, on string) map[string]string {
	return map[string]string{".github/workflows/" + name + ".yml": fmt.Sprintf(
		"name: %s\non:\n  workflow_run:\n%s\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n", name, on)}
}

func merge(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func (c *ciHarness) runsOf(workflowName string) []*pb.Run {
	var out []*pb.Run
	for _, r := range c.runs() {
		if r.GetWorkflowName() == workflowName {
			out = append(out, r)
		}
	}
	return out
}

func TestWorkflowRunRequestedCompletedFiltersAndPayload(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", merge(
		map[string]string{".github/workflows/ci.yml": triggeringWorkflow},
		followWorkflow("after", "    workflows: [ci]\n    types: [completed]\n    branches: [main]"),
		followWorkflow("started", "    workflows: [ci]\n    types: [requested]"),
		followWorkflow("release-only", "    workflows: [ci]\n    branches: [release]"),
		followWorkflow("other-name", "    workflows: [nobody]"),
	), "")
	c.waitFor("ci and the run it requested", func() bool { return len(c.runs()) == 2 })
	assert.Len(t, c.runsOf("started"), 1, "requested fires when the run is created")
	assert.Empty(t, c.runsOf("after"), "completed has not happened yet")

	// Both jobs are given to the runner: finish them.
	for range 2 {
		j := runner.await()
		runner.runJob(j, pb.CiConclusion_CI_CONCLUSION_FAILURE, nil, "ok")
	}
	c.waitFor("the completed run", func() bool { return len(c.runsOf("after")) == 1 })
	assert.Empty(t, c.runsOf("release-only"), "the branches filter applies to the head branch of the triggering run")
	assert.Empty(t, c.runsOf("other-name"))
	assert.Len(t, c.runsOf("started"), 1, "requested does not fire on completion")

	after := c.runsOf("after")[0]
	assert.Equal(t, "workflow_run", after.GetEvent())
	assert.Equal(t, "refs/heads/main", after.GetRef(), "it runs on the default branch")
	assert.True(t, after.GetTrusted())
	main := c.ref("refs/heads/main")
	assert.Equal(t, main, after.GetSha())

	job := runner.await()
	gh := c.ghContext(job)
	assert.Equal(t, "workflow_run", gh["event_name"])
	ev := gh["event"].(map[string]any)
	assert.Equal(t, "completed", ev["action"])
	wr := ev["workflow_run"].(map[string]any)
	trig := c.runsOf("ci")[0]
	assert.EqualValues(t, trig.GetId(), wr["id"])
	assert.Equal(t, "ci", wr["name"])
	assert.Equal(t, "main", wr["head_branch"])
	assert.Equal(t, main, wr["head_sha"])
	assert.Equal(t, "push", wr["event"])
	assert.Equal(t, "completed", wr["status"])
	assert.Equal(t, "failure", wr["conclusion"], "the conclusion of the triggering run")
	assert.EqualValues(t, trig.GetNumber(), wr["run_number"])
	assert.EqualValues(t, 1, wr["run_attempt"])
	assert.Equal(t, ".github/workflows/ci.yml", wr["path"])
	assert.NotEmpty(t, wr["actor"].(map[string]any)["login"])
	wf := ev["workflow"].(map[string]any)
	assert.Equal(t, "ci", wf["name"])
	assert.Equal(t, ".github/workflows/ci.yml", wf["path"])
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
}

func TestWorkflowRunInheritsTrustAndNeverRaisesIt(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", merge(
		map[string]string{".github/workflows/ci.yml": triggeringWorkflow},
		followWorkflow("after", "    workflows: [ci]\n    types: [completed]"),
	), "")
	runner.runJob(runner.await(), pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
	c.waitFor("the owner's workflow_run", func() bool { return len(c.runsOf("after")) == 1 })
	assert.True(t, c.runsOf("after")[0].GetTrusted())
	runner.runJob(runner.await(), pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")

	// An agent pushes a branch: its push run is untrusted, and so is the
	// workflow_run it triggers, though that workflow is the owner's, on main.
	secret, _ := c.branchToken(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite), []string{"agent/claude/"}, "proj")
	c.mustGit(c.wt, "checkout", "-q", "-b", "agent/claude/x")
	c.writeFile(c.wt, "a.txt", []byte("a"))
	c.mustGit(c.wt, "push", "-q", c.urlAuth(secret, "/proj.git"), "agent/claude/x")
	c.waitFor("the agent's push run", func() bool { return len(c.runsOf("ci")) == 2 })
	agentRun := c.runsOf("ci")[0]
	assert.False(t, agentRun.GetTrusted())
	job := runner.await()
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
	c.waitFor("the agent's workflow_run", func() bool { return len(c.runsOf("after")) == 2 })
	follow := c.runsOf("after")[0]
	assert.False(t, follow.GetTrusted(), "never raised")
	assert.Equal(t, agentRun.GetActor(), follow.GetActor(), "the actor of the triggering run")
	next := runner.await()
	assert.False(t, next.GetTrusted())
	assert.Empty(t, next.GetSecrets())
	assert.Equal(t, "agent/claude/x", c.ghContext(next)["event"].(map[string]any)["workflow_run"].(map[string]any)["head_branch"])
	runner.runJob(next, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
}

func TestWorkflowRunChainStopsAtDepthThree(t *testing.T) {
	// handled tells when the workflows waiting for an event have been looked at:
	// a barrier instead of a wait for something that must not happen.
	handled := make(chan [2]any, 64)
	c := newCIHarness(t, func(cfg *usecase.CIConfig) {
		cfg.RunEventHandled = func(id int64, action string) { handled <- [2]any{id, action} }
	})
	runner := c.registerRunner("alpha", "ubuntu-latest")
	files := map[string]string{".github/workflows/a.yml": "name: a\non: push\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"}
	prev := "a"
	for _, next := range []string{"b", "c", "d", "e"} {
		files = merge(files, followWorkflow(next, "    workflows: ["+prev+"]\n    types: [completed]"))
		prev = next
	}
	c.push("main", files, "")
	for _, name := range []string{"a", "b", "c", "d"} {
		c.waitFor("the run of "+name, func() bool { return len(c.runsOf(name)) == 1 })
		runner.runJob(runner.await(), pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
	}
	c.waitFor("d completes", func() bool { return c.runsOf("d")[0].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })
	// The next level would be e: three workflow_run levels (b, c, d) are the most.
	// Wait until the "completed" event of d was handled, then look: e did not start.
	dID := c.runsOf("d")[0].GetId()
	for done := false; !done; {
		select {
		case ev := <-handled:
			done = ev == [2]any{dID, "completed"}
		case <-time.After(15 * time.Second):
			t.Fatal("the completed event of d was never handled")
		}
	}
	assert.Empty(t, c.runsOf("e"), "a chain of more than three levels stops")
	assert.Len(t, c.runs(), 4)
}
