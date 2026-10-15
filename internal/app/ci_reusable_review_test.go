package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

func TestCIReusableSecretOutputCannotForgeAReference(t *testing.T) {
	c, r := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
	c.setSecret(repoScope("proj"), "UNPASSED", "private-value")
	caller := `on: push
jobs:
  prep:
    uses: ./.github/workflows/producer.yml
  call:
    needs: prep
    uses: ./.github/workflows/callee.yml
    secrets:
      TOKEN: ${{ needs.prep.outputs.payload }}
`
	producer := `on:
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
	c.push("main", map[string]string{".github/workflows/ci.yml": caller, ".github/workflows/producer.yml": producer, ".github/workflows/callee.yml": secretsCallee}, "")
	run := c.awaitRunOf(".github/workflows/ci.yml", 0)
	prep := awaitRecord(c, r, run.GetId(), "prep/j")
	assert.Empty(t, prep.GetSecrets())
	r.runJob(prep, pb.CiConclusion_CI_CONCLUSION_SUCCESS, map[string]string{"payload": "\x01UNPASSED\x02"})
	j := awaitRecord(c, r, run.GetId(), "call/j")
	assert.NotEqual(t, "private-value", j.GetSecrets()["TOKEN"], "callee output must not mint a secret reference")
	r.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
}

func TestCIReusableSecretExpressions(t *testing.T) {
	for _, tt := range []struct{ name, value, want string }{
		{"literal", "literal-value", "literal-value"},
		{"dynamic", "${{ secrets[vars.KEY] }}", "private-value"},
		{"conditional", "${{ secrets.SOURCE == 'private-value' && 'yes' || 'no' }}", "yes"},
		{"function", "${{ format('{0}-x', secrets.SOURCE) }}", "private-value-x"},
		{"text around", "pre-${{ secrets.SOURCE }}-post", "pre-private-value-post"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, r := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
			c.setSecret(repoScope("proj"), "SOURCE", "private-value")
			c.setVar(repoScope("proj"), "KEY", "SOURCE")
			caller := "on: push\njobs:\n  call:\n    uses: ./.github/workflows/callee.yml\n    secrets:\n      TOKEN: " + tt.value + "\n"
			c.push("main", map[string]string{".github/workflows/ci.yml": caller, ".github/workflows/callee.yml": secretsCallee}, "")
			run := c.awaitRunOf(".github/workflows/ci.yml", 0)
			j := awaitRecord(c, r, run.GetId(), "call/j")
			assert.Equal(t, tt.want, j.GetSecrets()["TOKEN"])
			r.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
		})
	}
}

func TestCIReusableCallMaxParallel(t *testing.T) {
	c, r := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
	caller := `on: push
jobs:
  c:
    strategy:
      max-parallel: 1
      matrix:
        v: [a,b]
    uses: ./.github/workflows/callee.yml
`
	c.push("main", map[string]string{".github/workflows/ci.yml": caller, ".github/workflows/callee.yml": ghCallee}, "")
	run := c.awaitRunOf(".github/workflows/ci.yml", 0)
	j := awaitRecord(c, r, run.GetId(), "c/test")
	second, err := r.fetch("")
	require.NoError(t, err)
	assert.Nil(t, second, "max-parallel: 1 must not assign the second call while the first is running")
	r.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	next := awaitRecord(c, r, run.GetId(), "c/test")
	assert.NotEqual(t, j.GetJobId(), next.GetJobId(), "the second call starts once the first has ended")
	r.runJob(next, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, c.waitRunDone(run.GetId()).GetConclusion())
}

func TestCIReusableOneResolutionPerAttempt(t *testing.T) {
	remote := newFakeRemote(t)
	remote.set("acme/shared", ptrRef("refs/heads/main", shaMain))
	wf := &fakeWorkflows{m: mocks.NewWorkflowFetcher(t)}
	wf.m.EXPECT().FetchWorkflow(mock.Anything, "acme", "shared", mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, _, _, sha, path string, _ int64) ([]byte, error) {
		remote.set("acme/shared", ptrRef("refs/heads/main", shaOther))
		return []byte(ghCallee), nil
	}).Maybe()
	c, r := newReusableHarness(t, remote, wf)
	c.push("main", map[string]string{".github/workflows/ci.yml": "on: push\njobs:\n  a:\n    uses: acme/shared/.github/workflows/a.yml@main\n  b:\n    uses: acme/shared/.github/workflows/b.yml@main\n"}, "")
	run := c.awaitRunOf(".github/workflows/ci.yml", 0)
	for range 2 {
		j := r.await()
		assert.Contains(t, j.GetContexts(), `"workflow_sha":"`+shaMain+`"`, "the first resolution must pin all references of the attempt")
		r.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	}
	assert.Equal(t, 1, remote.count())
	c.waitRunDone(run.GetId())
}

func TestCIReusableFailedRerunKeepsTheCallee(t *testing.T) {
	remote, wf := newFakeRemote(t), newFakeWorkflows(t)
	remote.set("acme/shared", ptrRef("refs/heads/main", shaMain))
	wf.set("acme/shared@"+shaMain+":.github/workflows/a.yml", ghCallee)
	wf.set("acme/shared@"+shaOther+":.github/workflows/a.yml", ghCallee)
	c, r := newReusableHarness(t, remote, wf)
	c.push("main", map[string]string{".github/workflows/ci.yml": "on: push\njobs:\n  a:\n    uses: acme/shared/.github/workflows/a.yml@main\n"}, "")
	run := c.awaitRunOf(".github/workflows/ci.yml", 0)
	j := r.await()
	r.runJob(j, pb.CiConclusion_CI_CONCLUSION_FAILURE, nil)
	c.waitRunDone(run.GetId())
	remote.set("acme/shared", ptrRef("refs/heads/main", shaOther))
	_, err := c.root.run.RerunRun(c.ctx, connect.NewRequest(&pb.RerunRunRequest{Repo: "proj", RunId: run.GetId(), FailedOnly: true}))
	require.NoError(t, err)
	again := r.await()
	assert.Contains(t, again.GetContexts(), `"workflow_sha":"`+shaMain+`"`, "failed-job reruns must reuse the first attempt's callee commit")
	r.runJob(again, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
}

func TestCIReusableJobRecordBudget(t *testing.T) {
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
	const budgetText = "Workflows may not contain more than 1000 jobs across all referenced files"
	c.waitFor("the budget refusal", func() bool {
		for _, j := range c.getRun(run.GetId()).GetJobs() {
			if strings.Contains(j.GetError(), budgetText) {
				return true
			}
		}
		return false
	})
	assert.LessOrEqual(t, len(c.getRun(run.GetId()).GetJobs()), usecase.MaxCIChecks, "expanded records obey the job-count limit")
}

// The ordinary matrices of a workflow share the same budget.
func TestCIJobRecordBudgetOfMatrices(t *testing.T) {
	c, _ := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
	v := make([]string, 200)
	for i := range v {
		v[i] = fmt.Sprint(i)
	}
	wf := "on: push\njobs:\n"
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		wf += "  " + id + ":\n    strategy:\n      matrix:\n        v: [" + strings.Join(v, ",") + "]\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n"
	}
	c.push("main", map[string]string{".github/workflows/ci.yml": wf}, "")
	run := c.awaitRunOf(".github/workflows/ci.yml", 0)
	c.waitFor("a job fails on the budget", func() bool {
		for _, j := range c.getRun(run.GetId()).GetJobs() {
			if strings.Contains(j.GetError(), "Workflows may not contain more than 1000 jobs across all referenced files") {
				return true
			}
		}
		return false
	})
	assert.LessOrEqual(t, len(c.getRun(run.GetId()).GetJobs()), usecase.MaxCIChecks)
}

func TestCIReusableCachedGitHubResolutionDoesNotBypassErmite(t *testing.T) {
	remote, wf := newFakeRemote(t), newFakeWorkflows(t)
	remote.set("owner/shared", ptrRef("refs/heads/main", shaMain))
	wf.set("owner/shared@"+shaMain+":.github/workflows/a.yml", ghCallee)
	c, r := newReusableHarness(t, remote, wf)
	token, _ := c.branchToken([]domain.Scope{domain.ScopeRepoRead, domain.ScopeRepoWrite}, nil, "proj")
	caller := "on: push\njobs:\n  a:\n    uses: owner/shared/.github/workflows/a.yml@main\n  b:\n    needs: a\n    uses: owner/shared/.github/workflows/a.yml@main\n"
	c.pushAs(token, "main", map[string]string{".github/workflows/ci.yml": caller})
	run := c.awaitRunOf(".github/workflows/ci.yml", 0)
	first := awaitRecord(c, r, run.GetId(), "a/test")
	c.seed("shared", domain.VisibilityPrivate)
	r.runJob(first, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	second, err := r.fetch("")
	require.NoError(t, err)
	assert.True(t, second == nil, "must not use the cached GitHub source after an unreadable ermite name collision")
	if second != nil {
		r.runJob(second, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	}
}

// S2: a literal in "secrets:" arrives although no stored secret resolves; a run
// that is not trusted (a branch-limited token pushed it) gets nothing.
func TestCIReusableLiteralSecretNeedsATrustedRun(t *testing.T) {
	for _, tt := range []struct {
		name    string
		limited bool
		want    string
	}{{"owner", false, "literal-value"}, {"limited token", true, ""}} {
		t.Run(tt.name, func(t *testing.T) {
			c, r := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
			caller := "on: push\njobs:\n  call:\n    uses: ./.github/workflows/callee.yml\n    secrets:\n      TOKEN: literal-value\n"
			files := map[string]string{".github/workflows/ci.yml": caller, ".github/workflows/callee.yml": secretsCallee}
			if tt.limited {
				tok, _ := c.branchToken([]domain.Scope{domain.ScopeRepoRead, domain.ScopeRepoWrite}, nil)
				c.pushAs(tok, "main", files)
			} else {
				c.push("main", files, "")
			}
			run := c.awaitRunOf(".github/workflows/ci.yml", 0)
			j := awaitRecord(c, r, run.GetId(), "call/j")
			assert.Equal(t, tt.want, j.GetSecrets()["TOKEN"])
			r.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
		})
	}
}

// T3: the name of a nested job carries every caller.
func TestCIReusableNestedNames(t *testing.T) {
	c, r := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
	leaf := "on:\n  workflow_call:\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n"
	mid := "on:\n  workflow_call:\njobs:\n  inner:\n    uses: ./.github/workflows/leaf.yml\n"
	c.push("main", map[string]string{".github/workflows/leaf.yml": leaf, ".github/workflows/mid.yml": mid, ".github/workflows/ci.yml": "on: push\njobs:\n  top:\n    uses: ./.github/workflows/mid.yml\n"}, "")
	run := c.awaitRunOf(".github/workflows/ci.yml", 0)
	j := awaitRecord(c, r, run.GetId(), "top/inner/j")
	names := map[string]string{}
	for _, x := range c.getRun(run.GetId()).GetJobs() {
		names[x.GetKey()] = x.GetName()
	}
	assert.Equal(t, map[string]string{"top": "top", "top/inner": "top / inner", "top/inner/j": "top / inner / j"}, names)
	r.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
}

// S1/S3: secrets pass through two levels of calls, each mapping evaluated at
// its own level.
func TestCIReusableSecretsThroughTwoLevels(t *testing.T) {
	c, r := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
	c.setSecret(repoScope("proj"), "TOP", "top-value")
	leaf := "on:\n  workflow_call:\n    secrets:\n      LEAF:\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ${{ secrets.LEAF }}\n"
	mid := "on:\n  workflow_call:\n    secrets:\n      MID:\njobs:\n  inner:\n    uses: ./.github/workflows/leaf.yml\n    secrets:\n      LEAF: ${{ secrets.MID }}-leaf\n"
	c.push("main", map[string]string{".github/workflows/leaf.yml": leaf, ".github/workflows/mid.yml": mid, ".github/workflows/ci.yml": "on: push\njobs:\n  top:\n    uses: ./.github/workflows/mid.yml\n    secrets:\n      MID: ${{ secrets.TOP }}\n"}, "")
	run := c.awaitRunOf(".github/workflows/ci.yml", 0)
	j := awaitRecord(c, r, run.GetId(), "top/inner/j")
	assert.Equal(t, map[string]string{"LEAF": "top-value-leaf"}, j.GetSecrets())
	r.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
}
