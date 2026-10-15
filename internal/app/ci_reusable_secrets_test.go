// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
)

const producerWorkflow = `on:
  workflow_call:
    outputs:
      payload:
        value: ${{ jobs.j.outputs.payload }}
jobs:
  j:
    runs-on: ubuntu-latest
    outputs:
      payload: ${{ steps.s.outputs.payload }}
    steps:
      - id: s
        run: echo
`

// secretCall runs a push whose caller maps TOKEN with value and returns the job
// of the callee that was assigned.
func secretCall(t *testing.T, setup func(c *ciHarness), value string) (*ciHarness, *fakeRunner, *pb.Job) {
	t.Helper()
	c, r := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
	if setup != nil {
		setup(c)
	}
	caller := "on: push\njobs:\n  call:\n    uses: ./.github/workflows/callee.yml\n    secrets:\n      TOKEN: " + value + "\n"
	c.push("main", map[string]string{".github/workflows/ci.yml": caller, ".github/workflows/callee.yml": secretsCallee}, "")
	run := c.awaitRunOf(".github/workflows/ci.yml", 0)
	return c, r, awaitRecord(c, r, run.GetId(), "call/j")
}

// GO-009: a string that an expression produces is data. It is not read again as
// an expression, so it cannot name a secret.
func TestCIReusableEscapedExpressionStaysALiteral(t *testing.T) {
	c, r, j := secretCall(t, func(c *ciHarness) {
		c.setSecret(repoScope("proj"), "UNPASSED", "private-value")
	}, "${{ '${{ secrets.UNPASSED }}' }}")
	assert.Equal(t, "${{ secrets.UNPASSED }}", j.GetSecrets()["TOKEN"])
	assert.NotContains(t, c.logs.String(), "private-value")
	r.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
}

// GO-009: nor does a name taken from a context select a secret.
func TestCIReusableContextDataCannotSelectASecret(t *testing.T) {
	for _, tt := range []struct{ name, token, payload, want string }{
		{"a text that looks like an expression", "${{ needs.prep.outputs.payload }}", "${{ secrets.UNPASSED }}", "${{ secrets.UNPASSED }}"},
		{"a former marker", "${{ needs.prep.outputs.payload }}", "\x01UNPASSED\x02", "\x01UNPASSED\x02"},
		{"an escaped expression over json", "${{ '${{ secrets[fromJSON(needs.prep.outputs.payload).name] }}' }}", `{"name":"UNPASSED"}`, "${{ secrets[fromJSON(needs.prep.outputs.payload).name] }}"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, r := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
			c.setSecret(repoScope("proj"), "UNPASSED", "private-value")
			caller := "on: push\njobs:\n  prep:\n    uses: ./.github/workflows/producer.yml\n  call:\n    needs: prep\n    uses: ./.github/workflows/callee.yml\n    secrets:\n      TOKEN: " + tt.token + "\n"
			c.push("main", map[string]string{".github/workflows/ci.yml": caller, ".github/workflows/producer.yml": producerWorkflow, ".github/workflows/callee.yml": secretsCallee}, "")
			run := c.awaitRunOf(".github/workflows/ci.yml", 0)
			prep := awaitRecord(c, r, run.GetId(), "prep/j")
			assert.Empty(t, prep.GetSecrets())
			r.runJob(prep, pb.CiConclusion_CI_CONCLUSION_SUCCESS, map[string]string{"payload": tt.payload})
			j := awaitRecord(c, r, run.GetId(), "call/j")
			assert.Equal(t, tt.want, j.GetSecrets()["TOKEN"])
			assert.NotEqual(t, "private-value", j.GetSecrets()["TOKEN"])
			r.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
		})
	}
}

// GO-009: an expression that selects a secret by data of the caller (an input of
// the workflow, here) does use the secret it names.
func TestCIReusableMappingUsesPlainContexts(t *testing.T) {
	_, r, j := secretCall(t, func(c *ciHarness) {
		c.setSecret(repoScope("proj"), "SOURCE", "private-value")
		c.setVar(repoScope("proj"), "KEY", "SOURCE")
	}, "${{ secrets[vars.KEY] }}")
	assert.Equal(t, "private-value", j.GetSecrets()["TOKEN"])
	r.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
}

// GO-010: an expression error never reaches the log: format() puts its
// arguments, here a secret, in its message.
func TestCIReusableSecretEvaluationErrorIsNotLogged(t *testing.T) {
	secret := "private-value{bad-format}"
	c, r, j := secretCall(t, func(c *ciHarness) {
		c.setSecret(repoScope("proj"), "SOURCE", secret)
	}, "${{ format(secrets.SOURCE) }}")
	assert.Empty(t, j.GetSecrets()["TOKEN"])
	logs := c.logs.String()
	assert.NotContains(t, logs, secret)
	assert.NotContains(t, logs, "private-value")
	assert.Contains(t, logs, "could not evaluate", "the mapping is named, with a fixed reason")
	r.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
}

// GO-011: planning does not evaluate the mapping, so a value that needs a real
// secret (here a JSON one) is not refused for lack of it.
func TestCIReusableMappingNeedsNoSecretAtPlanning(t *testing.T) {
	c, r, j := secretCall(t, func(c *ciHarness) {
		c.setSecret(repoScope("proj"), "JSON", `{"token":"private-value"}`)
	}, "${{ fromJSON(secrets.JSON).token }}")
	assert.Equal(t, "private-value", j.GetSecrets()["TOKEN"])
	r.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	_ = c
}

// GO-012: the mapping of a call reads the vars of the calling scope, not those
// of the environment of the job that is assigned.
func TestCIReusableMappingUsesTheCallersVars(t *testing.T) {
	c, r := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
	c.setSecret(repoScope("proj"), "PUBLIC", "public-value")
	c.setSecret(repoScope("proj"), "PRIVATE", "private-value")
	c.setVar(repoScope("proj"), "KEY", "PUBLIC")
	_, err := c.root.env.CreateEnvironment(c.ctx, connect.NewRequest(&pb.CreateEnvironmentRequest{Repo: "proj", Name: "prod"}))
	require.NoError(t, err)
	c.setVar(envScope("proj", "prod"), "KEY", "PRIVATE")
	caller := "on: push\njobs:\n  call:\n    uses: ./.github/workflows/callee.yml\n    secrets:\n      TOKEN: ${{ secrets[vars.KEY] }}\n"
	callee := strings.Replace(secretsCallee, "    runs-on: ubuntu-latest", "    runs-on: ubuntu-latest\n    environment: prod", 1)
	c.push("main", map[string]string{".github/workflows/ci.yml": caller, ".github/workflows/callee.yml": callee}, "")
	run := c.awaitRunOf(".github/workflows/ci.yml", 0)
	j := awaitRecord(c, r, run.GetId(), "call/j")
	assert.Equal(t, "public-value", j.GetSecrets()["TOKEN"])
	r.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
}

// An input handed down as an escaped expression is data in a callee that
// inherits secrets, and an untrusted run gets nothing.
func TestCIReusableInheritScopeTrustAndInputData(t *testing.T) {
	for _, limited := range []bool{false, true} {
		t.Run(map[bool]string{false: "trusted", true: "untrusted"}[limited], func(t *testing.T) {
			c, r := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
			c.setSecret(repoScope("proj"), "TOP", "top-value")
			c.setSecret(repoScope("proj"), "UNPASSED", "private-value")
			leaf := "on:\n  workflow_call:\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ${{ toJSON(secrets) }}\n"
			mid := "on:\n  workflow_call:\n    inputs:\n      text:\n        type: string\n    secrets:\n      MID:\njobs:\n  inner:\n    uses: ./.github/workflows/leaf.yml\n    secrets: inherit\n"
			caller := "on: push\njobs:\n  top:\n    uses: ./.github/workflows/mid.yml\n    with:\n      text: '${{ ''${{ secrets.UNPASSED }}'' }}'\n    secrets:\n      MID: ${{ secrets.TOP }}\n"
			files := map[string]string{".github/workflows/ci.yml": caller, ".github/workflows/mid.yml": mid, ".github/workflows/leaf.yml": leaf}
			if limited {
				tok, _ := c.branchToken([]domain.Scope{domain.ScopeRepoRead, domain.ScopeRepoWrite}, nil)
				c.pushAs(tok, "main", files)
			} else {
				c.push("main", files, "")
			}
			run := c.awaitRunOf(".github/workflows/ci.yml", 0)
			j := awaitRecord(c, r, run.GetId(), "top/inner/j")
			if limited {
				assert.Empty(t, j.GetSecrets())
			} else {
				assert.Equal(t, map[string]string{"MID": "top-value"}, j.GetSecrets())
			}
			r.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
		})
	}
}

// A secret cannot be handed to the callee as an input.
func TestCIReusableSecretsAreRefusedInWith(t *testing.T) {
	c, _ := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
	c.setSecret(repoScope("proj"), "SOURCE", "private-value")
	caller := "on: push\njobs:\n  call:\n    uses: ./.github/workflows/callee.yml\n    with:\n      go-version: ${{ secrets.SOURCE }}\n"
	c.push("main", map[string]string{".github/workflows/ci.yml": caller, ".github/workflows/callee.yml": ghCallee}, "")
	run := c.awaitRunOf(".github/workflows/ci.yml", 0)
	c.waitRunDone(run.GetId())
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_FAILURE, c.getRun(run.GetId()).GetRun().GetConclusion())
	assert.NotContains(t, c.logs.String(), "private-value")
}

// A re-run of everything resolves the reference of a called workflow afresh.
func TestCIReusableRerunAllRefreshesTheCallee(t *testing.T) {
	remote, wf := newFakeRemote(t), newFakeWorkflows(t)
	remote.set("acme/shared", ptrRef("refs/heads/main", shaMain))
	wf.set("acme/shared@"+shaMain+":.github/workflows/a.yml", ghCallee)
	wf.set("acme/shared@"+shaOther+":.github/workflows/a.yml", ghCallee)
	c, r := newReusableHarness(t, remote, wf)
	c.push("main", map[string]string{".github/workflows/ci.yml": "on: push\njobs:\n  a:\n    uses: acme/shared/.github/workflows/a.yml@main\n"}, "")
	run := c.awaitRunOf(".github/workflows/ci.yml", 0)
	first := r.await()
	assert.Contains(t, first.GetContexts(), `"workflow_sha":"`+shaMain+`"`)
	r.runJob(first, pb.CiConclusion_CI_CONCLUSION_FAILURE, nil)
	c.waitRunDone(run.GetId())

	remote.set("acme/shared", ptrRef("refs/heads/main", shaOther))
	_, err := c.root.run.RerunRun(c.ctx, connect.NewRequest(&pb.RerunRunRequest{Repo: "proj", RunId: run.GetId()}))
	require.NoError(t, err)
	again := r.await()
	assert.Contains(t, again.GetContexts(), `"workflow_sha":"`+shaOther+`"`, "the reference is resolved again")
	r.runJob(again, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, c.waitRunDone(run.GetId()).GetConclusion())
}

// A matrix of calls with max-parallel and both concurrency groups progresses
// one call at a time to the end, and the groups are free for the next run.
func TestCIReusableMaxParallelWithBothGroups(t *testing.T) {
	c, r := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
	caller := "on: push\njobs:\n  c:\n    strategy:\n      max-parallel: 1\n      matrix:\n        v: [a, b, c]\n    concurrency: caller-group\n    uses: ./.github/workflows/callee.yml\n"
	callee := strings.Replace(ghCallee, "jobs:\n", "concurrency: callee-group\njobs:\n", 1)
	c.push("main", map[string]string{".github/workflows/ci.yml": caller, ".github/workflows/callee.yml": callee}, "")
	run := c.awaitRunOf(".github/workflows/ci.yml", 0)
	for range 3 {
		j := awaitRecord(c, r, run.GetId(), "c/test")
		c.forge.CI.ReconcileConcurrency(c.ctx)
		second, err := r.fetch("")
		require.NoError(t, err)
		assert.Nil(t, second, "one call at a time")
		r.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	}
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, c.waitRunDone(run.GetId()).GetConclusion())
	c.push("main", map[string]string{"x": "second"}, "")
	next := c.awaitRunOf(".github/workflows/ci.yml", run.GetId())
	for range 3 {
		j := awaitRecord(c, r, next.GetId(), "c/test")
		r.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	}
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, c.waitRunDone(next.GetId()).GetConclusion())
}

// The job-record budget holds again after a re-run of the failed jobs.
func TestCIReusableBudgetHoldsOnRerun(t *testing.T) {
	c, _ := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
	vals := func(n int) string {
		v := make([]string, n)
		for i := range v {
			v[i] = fmt.Sprint(i)
		}
		return "[" + strings.Join(v, ",") + "]"
	}
	caller := "on: push\njobs:\n  c:\n    strategy:\n      matrix:\n        v: " + vals(33) + "\n    uses: ./.github/workflows/callee.yml\n"
	callee := "on: workflow_call\njobs:\n  j:\n    strategy:\n      matrix:\n        v: " + vals(32) + "\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n"
	c.push("main", map[string]string{".github/workflows/ci.yml": caller, ".github/workflows/callee.yml": callee}, "")
	run := c.awaitRunOf(".github/workflows/ci.yml", 0)
	refused := func() bool {
		for _, j := range c.getRun(run.GetId()).GetJobs() {
			if strings.Contains(j.GetError(), "Workflows may not contain more than 1000 jobs across all referenced files") {
				return true
			}
		}
		return false
	}
	c.waitFor("the budget refusal", refused)
	assert.LessOrEqual(t, len(c.getRun(run.GetId()).GetJobs()), 1000)
	_, _ = c.root.run.CancelRun(c.ctx, connect.NewRequest(&pb.CancelRunRequest{Repo: "proj", RunId: run.GetId()}))
	c.waitRunDone(run.GetId())
	_, err := c.root.run.RerunRun(c.ctx, connect.NewRequest(&pb.RerunRunRequest{Repo: "proj", RunId: run.GetId(), FailedOnly: true}))
	require.NoError(t, err)
	c.waitFor("the refusal again", refused)
	assert.LessOrEqual(t, len(c.getRun(run.GetId()).GetJobs()), 1000)
}
