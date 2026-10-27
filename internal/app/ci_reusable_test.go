// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

// fakeWorkflows stands in for github.com's file download: no test reaches the
// network. It answers from a table of "owner/repo@sha:path" to file content.
type fakeWorkflows struct {
	m     *mocks.WorkflowFetcher
	mu    sync.Mutex
	files map[string]string
	calls []string
}

func newFakeWorkflows(t *testing.T) *fakeWorkflows {
	t.Helper()
	f := &fakeWorkflows{m: mocks.NewWorkflowFetcher(t), files: map[string]string{}}
	f.m.EXPECT().FetchWorkflow(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, owner, repo, sha, path string, _ int64) ([]byte, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			key := owner + "/" + repo + "@" + sha + ":" + path
			f.calls = append(f.calls, key)
			if s, ok := f.files[key]; ok {
				return []byte(s), nil
			}
			return nil, domain.ErrNotFound
		}).Maybe()
	return f
}

func (f *fakeWorkflows) set(key, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[key] = content
}

func newReusableHarness(t *testing.T, remote *fakeRemote, wf *fakeWorkflows) (*ciHarness, *fakeRunner) {
	t.Helper()
	h := newHarnessWith(t, harnessOpts{env: map[string]string{"ERMITE_SECRETS_KEY": secretsKeyA}}, func(o *options) {
		o.remoteLister, o.workflowFetcher = remote.m, wf.m
		o.ci = func(cfg *usecase.CIConfig) { cfg.Tick, cfg.PollTimeout = 20*time.Millisecond, 100*time.Millisecond }
	})
	c := &ciHarness{harness: h, root: h.api(adminToken), repo: "proj"}
	_, err := h.forge.Repos.Create(h.ctx, domain.Root(), ports.CreateRepoParams{Name: h.name("proj"), Visibility: domain.VisibilityPrivate})
	require.NoError(t, err)
	c.wt = h.workTree("main")
	return c, c.registerRunner("alpha", "ubuntu-latest")
}

func jobByKey(c *ciHarness, runID int64, key string) *pb.RunJob {
	for _, j := range c.getRun(runID).GetJobs() {
		if j.GetKey() == key {
			return j
		}
	}
	return nil
}

// awaitRecord fetches jobs until the one of the record with the key comes. The
// runner is given the id of the job in its own workflow as the job key, so the
// record is told apart by its job id.
func awaitRecord(c *ciHarness, r *fakeRunner, runID int64, key string) *pb.Job {
	c.t.Helper()
	for range 20 {
		j := r.await()
		for _, rec := range c.getRun(runID).GetJobs() {
			if rec.GetKey() == key && rec.GetId() == j.GetJobId() {
				return j
			}
		}
	}
	c.t.Fatalf("no job %s of run %d", key, runID)
	return nil
}

func (c *ciHarness) waitRunDone(runID int64) *pb.Run {
	c.t.Helper()
	c.waitFor("the run completes", func() bool {
		return c.getRun(runID).GetRun().GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED
	})
	return c.getRun(runID).GetRun()
}

const localCaller = `name: caller
on: push
jobs:
  prep:
    runs-on: ubuntu-latest
    outputs:
      who: ${{ steps.s.outputs.who }}
    steps:
      - id: s
        run: echo who=world >> $GITHUB_OUTPUT
  call:
    needs: prep
    uses: ./.github/workflows/callee.yml
    with:
      name: ${{ needs.prep.outputs.who }}
      count: 3
    secrets: inherit
  after:
    needs: call
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ needs.call.outputs.greeting }} ${{ needs.call.outputs.total }}
`

const localCallee = `name: callee
on:
  workflow_call:
    inputs:
      name:
        type: string
        required: true
      count:
        type: number
        default: 1
      loud:
        type: boolean
        default: false
    outputs:
      greeting:
        value: ${{ jobs.hello.outputs.msg }}
      total:
        value: ${{ inputs.count }}
jobs:
  hello:
    runs-on: ubuntu-latest
    outputs:
      msg: ${{ steps.m.outputs.msg }}
    steps:
      - id: m
        run: echo "msg=hello ${{ inputs.name }} ${{ secrets.TOKEN }} ${{ inputs.loud }}" >> $GITHUB_OUTPUT
  second:
    needs: hello
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ needs.hello.outputs.msg }}
`

// AC6: a local callee with inputs, secrets: inherit and outputs consumed by a
// later job of the caller; the callee's jobs are keyed and named after the call.
func TestCIReusableLocalCalleeWithInputsInheritAndOutputs(t *testing.T) {
	c, runner := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
	c.setSecret(repoScope("proj"), "TOKEN", "s3cr3t")
	c.setSecret(repoScope("proj"), "OTHER", "not-sent")
	c.push("main", map[string]string{".github/workflows/caller.yml": localCaller, ".github/workflows/callee.yml": localCallee}, "")

	var run *pb.Run
	c.waitFor("the run exists", func() bool {
		for _, r := range c.runs() {
			if r.GetWorkflowPath() == ".github/workflows/caller.yml" {
				run = r
				return true
			}
		}
		return false
	})
	prep := awaitRecord(c, runner, run.GetId(), "prep")
	runner.runJob(prep, pb.CiConclusion_CI_CONCLUSION_SUCCESS, map[string]string{"who": "world"})

	hello := awaitRecord(c, runner, run.GetId(), "call/hello")
	// The runner is given the called file and the callee's id of the job.
	assert.Equal(t, ".github/workflows/callee.yml", hello.GetWorkflowPath())
	assert.Contains(t, hello.GetWorkflowYaml(), "name: callee")
	assert.Equal(t, "s3cr3t", hello.GetSecrets()["TOKEN"], "inherit passes the caller's secrets")
	assert.NotContains(t, hello.GetSecrets(), "OTHER", "only the secrets the callee reads")
	assert.Contains(t, hello.GetContexts(), `"inputs":{"name":"world","count":3,"loud":false}`)
	assert.Contains(t, hello.GetContexts(), `"workflow_file_path":".github/workflows/callee.yml"`)
	assert.Equal(t, "call/hello", jobByKey(c, run.GetId(), "call/hello").GetKey())
	assert.Equal(t, "call / hello", jobByKey(c, run.GetId(), "call/hello").GetName())
	assert.Equal(t, []string{"call/hello"}, jobByKey(c, run.GetId(), "call/second").GetNeeds())
	runner.runJob(hello, pb.CiConclusion_CI_CONCLUSION_SUCCESS, map[string]string{"msg": "hello world s3cr3t false"})

	second := awaitRecord(c, runner, run.GetId(), "call/second")
	assert.Contains(t, second.GetContexts(), `"msg":"hello world s3cr3t false"`)
	runner.runJob(second, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)

	after := awaitRecord(c, runner, run.GetId(), "after")
	assert.Contains(t, after.GetContexts(), `"greeting":"hello world s3cr3t false"`, "outputs mapped from the callee's jobs")
	assert.Contains(t, after.GetContexts(), `"total":"3"`)
	runner.runJob(after, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)

	done := c.waitRunDone(run.GetId())
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, done.GetConclusion())
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, jobByKey(c, run.GetId(), "call").GetConclusion())
}

const secretsCallee = `on:
  workflow_call:
    secrets:
      TOKEN:
        required: true
      OPTIONAL:
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ secrets.TOKEN }} ${{ secrets.OPTIONAL }}
`

// D2: secrets are passed only when listed or inherited, renamed by the mapping.
func TestCIReusableSecretsAreNotPassedUnlessExplicitOrInherited(t *testing.T) {
	c, runner := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
	c.setSecret(repoScope("proj"), "MY_TOKEN", "mapped-value")
	c.setSecret(repoScope("proj"), "OPTIONAL", "caller-optional")
	files := map[string]string{".github/workflows/callee.yml": secretsCallee}
	caller := func(secrets string) string {
		return "on: push\njobs:\n  c:\n    uses: ./.github/workflows/callee.yml\n" + secrets
	}

	// Explicit: TOKEN from MY_TOKEN; OPTIONAL is not passed although the caller has one.
	files[".github/workflows/explicit.yml"] = caller("    secrets:\n      TOKEN: ${{ secrets.MY_TOKEN }}\n")
	// Nothing: the callee requires TOKEN, so the call is refused.
	files[".github/workflows/none.yml"] = caller("")
	c.push("main", files, "")
	var explicit, none *pb.Run
	c.waitFor("two runs", func() bool {
		for _, r := range c.runs() {
			switch r.GetWorkflowPath() {
			case ".github/workflows/explicit.yml":
				explicit = r
			case ".github/workflows/none.yml":
				none = r
			}
		}
		return explicit != nil && none != nil
	})
	j := awaitRecord(c, runner, explicit.GetId(), "c/j")
	assert.Equal(t, map[string]string{"TOKEN": "mapped-value"}, j.GetSecrets())
	runner.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)

	done := c.waitRunDone(none.GetId())
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_FAILURE, done.GetConclusion())
	assert.Contains(t, jobByKey(c, none.GetId(), "c").GetError(), "Secret TOKEN is required, but not provided while calling.")
}

// The callee must declare workflow_call; with: is checked against its inputs.
func TestCIReusableCallErrors(t *testing.T) {
	c, _ := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
	callee := "on:\n  workflow_call:\n    inputs:\n      n:\n        type: number\n        required: true\n      flag:\n        type: boolean\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n"
	caller := func(with string) string {
		return "on: push\njobs:\n  c:\n    uses: ./.github/workflows/callee.yml\n" + with
	}
	files := map[string]string{
		".github/workflows/callee.yml":  callee,
		".github/workflows/nocall.yml":  "on: push\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n",
		".github/workflows/missing.yml": "on: push\njobs:\n  c:\n    uses: ./.github/workflows/nocall.yml\n",
		".github/workflows/noreq.yml":   caller("    with:\n      flag: true\n"),
		".github/workflows/badtype.yml": caller("    with:\n      n: abc\n"),
		".github/workflows/unknown.yml": caller("    with:\n      n: 1\n      zzz: 2\n"),
		".github/workflows/gone.yml":    "on: push\njobs:\n  c:\n    uses: ./.github/workflows/nothere.yml\n",
		".github/workflows/ver.yml":     "on: push\njobs:\n  c:\n    uses: ./.github/workflows/callee.yml@main\n",
	}
	c.push("main", files, "")
	want := map[string]string{
		".github/workflows/missing.yml": "workflow_call key is not defined in the referenced workflow.",
		".github/workflows/noreq.yml":   "Input n is required, but not provided while calling.",
		".github/workflows/badtype.yml": "Unexpected value 'abc'",
		".github/workflows/unknown.yml": "Invalid input, zzz is not defined in the referenced workflow.",
		".github/workflows/gone.yml":    "Unable to find reusable workflow",
		".github/workflows/ver.yml":     "cannot specify version when calling local workflows",
	}
	runs := map[string]*pb.Run{}
	c.waitFor("the runs exist", func() bool {
		for _, r := range c.runs() {
			runs[r.GetWorkflowPath()] = r
		}
		for p := range want {
			if runs[p] == nil {
				return false
			}
		}
		return true
	})
	for path, text := range want {
		c.waitRunDone(runs[path].GetId())
		j := jobByKey(c, runs[path].GetId(), "c")
		require.NotNil(t, j, path)
		assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_FAILURE, j.GetConclusion(), path)
		assert.Contains(t, j.GetError(), text, path)
	}
	assert.Contains(t, jobByKey(c, runs[".github/workflows/noreq.yml"].GetId(), "c").GetError(), "In .github/workflows/noreq.yml (Line: 4, Col: 11): Error from called workflow")
}

// AC6: depth 5 fails with the documented error; depth 4 runs.
func TestCIReusableDepth(t *testing.T) {
	c, runner := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
	files := map[string]string{}
	for i := 1; i <= 5; i++ {
		body := "on:\n  workflow_call:\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo leaf\n"
		if i < 5 {
			body = "on:\n  workflow_call:\njobs:\n  next:\n    uses: ./.github/workflows/l" + string(rune('0'+i+1)) + ".yml\n"
		}
		files[".github/workflows/l"+string(rune('0'+i))+".yml"] = body
	}
	files[".github/workflows/top4.yml"] = "on: push\njobs:\n  a:\n    uses: ./.github/workflows/l2.yml\n"
	files[".github/workflows/top5.yml"] = "on: push\njobs:\n  a:\n    uses: ./.github/workflows/l1.yml\n"
	delete(files, ".github/workflows/l5.yml")
	files[".github/workflows/l5.yml"] = "on:\n  workflow_call:\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo leaf\n"
	c.push("main", files, "")
	var top4, top5 *pb.Run
	c.waitFor("both runs", func() bool {
		for _, r := range c.runs() {
			switch r.GetWorkflowPath() {
			case ".github/workflows/top4.yml":
				top4 = r
			case ".github/workflows/top5.yml":
				top5 = r
			}
		}
		return top4 != nil && top5 != nil
	})
	// top4: a(1) -> l2(2) -> l3(3) -> l4(4) -> l5 (leaf): the leaf runs at depth 4.
	leaf := awaitRecord(c, runner, top4.GetId(), "a/next/next/next/j")
	runner.runJob(leaf, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, c.waitRunDone(top4.GetId()).GetConclusion())

	// top5: one more level: l1 -> ... -> l5 holds a call at depth 5.
	c.waitRunDone(top5.GetId())
	var failed *pb.RunJob
	for _, j := range c.getRun(top5.GetId()).GetJobs() {
		if j.GetConclusion() == pb.CiConclusion_CI_CONCLUSION_FAILURE && strings.HasSuffix(j.GetKey(), "/next") {
			failed = j
		}
	}
	require.NotNil(t, failed, "the call that is too deep fails")
	assert.Contains(t, failed.GetError(), "Nested reusable workflow depth exceeded 4.")
}

const ghCallee = `on:
  workflow_call:
    inputs:
      go-version:
        type: string
        default: stable
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ inputs.go-version }}
`

// AC6: a github.com callee, resolved with the CI-2 ref resolution and fetched
// at the resolved commit through the fetcher port (a fake here).
func TestCIReusableFromGitHub(t *testing.T) {
	remote, wf := newFakeRemote(t), newFakeWorkflows(t)
	remote.set("acme/gh-actions", ptrRef("refs/heads/main", shaMain))
	wf.set("acme/gh-actions@"+shaMain+":.github/workflows/go-ci.yml", ghCallee)
	c, runner := newReusableHarness(t, remote, wf)
	c.push("main", map[string]string{".github/workflows/ci.yml": "on: push\njobs:\n  ci:\n    uses: acme/gh-actions/.github/workflows/go-ci.yml@main\n    with:\n      go-version: '1.27'\n"}, "")
	var r *pb.Run
	c.waitFor("the run", func() bool {
		if rs := c.runs(); len(rs) > 0 {
			r = rs[0]
		}
		return r != nil
	})
	j := awaitRecord(c, runner, r.GetId(), "ci/test")
	assert.Contains(t, j.GetContexts(), `"inputs":{"go-version":"1.27"}`)
	assert.Contains(t, j.GetContexts(), `"workflow_ref":"acme/gh-actions/.github/workflows/go-ci.yml@main"`)
	assert.Contains(t, j.GetContexts(), `"workflow_sha":"`+shaMain+`"`)
	runner.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, c.waitRunDone(r.GetId()).GetConclusion())
	assert.True(t, remote.asked("acme/gh-actions"))
	wf.mu.Lock()
	assert.Equal(t, []string{"acme/gh-actions@" + shaMain + ":.github/workflows/go-ci.yml"}, wf.calls)
	wf.mu.Unlock()
}

// AC6: an owner repository of the server wins over github.com.
func TestCIReusableFromAnErmiteRepository(t *testing.T) {
	remote, wf := newFakeRemote(t), newFakeWorkflows(t)
	remote.set("owner/shared", ptrRef("refs/heads/main", shaMain)) // shadowed by ermite
	c, runner := newReusableHarness(t, remote, wf)
	c.seed("shared", domain.VisibilityPrivate)
	wt := c.workTree("main")
	c.writeFile(wt, ".github/workflows/lib.yml", []byte(ghCallee))
	c.mustGit(wt, "push", "-q", c.urlAuth(adminToken, "/shared.git"), "main")

	c.push("main", map[string]string{".github/workflows/ci.yml": "on: push\njobs:\n  lib:\n    uses: owner/shared/.github/workflows/lib.yml@main\n"}, "")
	var r *pb.Run
	c.waitFor("the run", func() bool {
		if rs := c.runs(); len(rs) > 0 {
			r = rs[0]
		}
		return r != nil
	})
	j := awaitRecord(c, runner, r.GetId(), "lib/test")
	assert.Contains(t, j.GetContexts(), `"inputs":{"go-version":"stable"}`, "the default")
	runner.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	c.waitRunDone(r.GetId())
	assert.False(t, remote.asked("owner/shared"))
	wf.mu.Lock()
	assert.Empty(t, wf.calls, "github.com is not asked for an ermite repository")
	wf.mu.Unlock()
}

// A matrix on the calling job expands the callee once per leg.
func TestCIReusableMatrixCaller(t *testing.T) {
	c, runner := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
	callee := "on:\n  workflow_call:\n    inputs:\n      v:\n        type: string\n        required: true\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ${{ inputs.v }}\n"
	caller := "on: push\njobs:\n  c:\n    strategy:\n      matrix:\n        v: [a, b]\n    uses: ./.github/workflows/callee.yml\n    with:\n      v: ${{ matrix.v }}\n"
	c.push("main", map[string]string{".github/workflows/callee.yml": callee, ".github/workflows/ci.yml": caller}, "")
	var r *pb.Run
	c.waitFor("the run", func() bool {
		for _, x := range c.runs() {
			if x.GetWorkflowPath() == ".github/workflows/ci.yml" {
				r = x
			}
		}
		return r != nil
	})
	got := map[string]bool{}
	for range 2 {
		j := awaitRecord(c, runner, r.GetId(), "c/j")
		switch {
		case strings.Contains(j.GetContexts(), `"inputs":{"v":"a"}`):
			got["a"] = true
		case strings.Contains(j.GetContexts(), `"inputs":{"v":"b"}`):
			got["b"] = true
		}
		runner.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	}
	assert.Equal(t, map[string]bool{"a": true, "b": true}, got)
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, c.waitRunDone(r.GetId()).GetConclusion())
	names := map[string]bool{}
	for _, j := range c.getRun(r.GetId()).GetJobs() {
		names[j.GetName()] = true
	}
	assert.True(t, names["c (a) / j"] && names["c (b) / j"], "%v", names)
}

// D3: the callee can only lower the caller's permissions; a request above is
// refused with the runner's text, and a lower one is kept.
func TestCIReusablePermissionsOnlyLowered(t *testing.T) {
	c, runner := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
	callee := func(perm string) string {
		return "on:\n  workflow_call:\n" + perm + "jobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n"
	}
	caller := func(perm, callee string) string {
		return "on: push\n" + perm + "jobs:\n  c:\n    uses: ./.github/workflows/" + callee + ".yml\n"
	}
	c.push("main", map[string]string{
		".github/workflows/lower.yml":  callee("permissions:\n  contents: read\n"),
		".github/workflows/higher.yml": callee("permissions:\n  contents: write\n"),
		".github/workflows/plain.yml":  callee(""),
		".github/workflows/a.yml":      caller("permissions:\n  contents: write\n  pull-requests: write\n", "lower"),
		".github/workflows/b.yml":      caller("permissions:\n  contents: read\n", "higher"),
		".github/workflows/p.yml":      caller("permissions:\n  contents: write\n", "plain"),
	}, "")
	runs := map[string]*pb.Run{}
	c.waitFor("three runs", func() bool {
		for _, r := range c.runs() {
			runs[r.GetWorkflowPath()] = r
		}
		return runs[".github/workflows/a.yml"] != nil && runs[".github/workflows/b.yml"] != nil && runs[".github/workflows/p.yml"] != nil
	})
	lower := awaitRecord(c, runner, runs[".github/workflows/a.yml"].GetId(), "c/j")
	assert.Equal(t, "read", lower.GetPermissions().GetLevels()["contents"], "the callee lowers")
	assert.NotContains(t, lower.GetPermissions().GetLevels(), "pull-requests")
	runner.runJob(lower, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)

	plain := awaitRecord(c, runner, runs[".github/workflows/p.yml"].GetId(), "c/j")
	assert.Equal(t, "write", plain.GetPermissions().GetLevels()["contents"], "a callee without permissions keeps the caller's")
	runner.runJob(plain, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)

	c.waitRunDone(runs[".github/workflows/b.yml"].GetId())
	var errText string
	for _, j := range c.getRun(runs[".github/workflows/b.yml"].GetId()).GetJobs() {
		if j.GetKey() == "c/j" {
			errText = j.GetError()
		}
	}
	assert.Contains(t, errText, "is requesting 'contents: write', but is only allowed 'contents: read'.")
}

// D4: the job-level concurrency of a callee's job applies.
func TestCIReusableCalleeJobConcurrency(t *testing.T) {
	c, runner := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
	disabled := false
	_, err := c.root.repo.UpdateRepo(c.ctx, connect.NewRequest(&pb.UpdateRepoRequest{Repo: c.repo, CiAutoCancel: &disabled}))
	require.NoError(t, err)
	callee := "on:\n  workflow_call:\njobs:\n  j:\n    runs-on: ubuntu-latest\n    concurrency:\n      group: callee-job-group\n    steps:\n      - run: echo\n"
	caller := "on: push\njobs:\n  c:\n    uses: ./.github/workflows/callee.yml\n"
	c.push("main", map[string]string{".github/workflows/callee.yml": callee, ".github/workflows/ci.yml": caller}, "")
	r1 := c.awaitRunOf(".github/workflows/ci.yml", 0)
	j1 := awaitRecord(c, runner, r1.GetId(), "c/j")
	assert.Equal(t, "callee-job-group", jobByKey(c, r1.GetId(), "c/j").GetConcurrencyGroup())
	c.push("main", map[string]string{"x.txt": "1"}, "second")
	r2 := c.awaitRunOf(".github/workflows/ci.yml", r1.GetId())
	c.waitFor("the second run's job is pending on the group", func() bool {
		j := jobByKey(c, r2.GetId(), "c/j")
		return j != nil && j.GetStatus() == pb.CiStatus_CI_STATUS_PENDING
	})
	runner.runJob(j1, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	j2 := awaitRecord(c, runner, r2.GetId(), "c/j")
	runner.runJob(j2, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, c.waitRunDone(r2.GetId()).GetConclusion())
}

// awaitRunOf waits for the newest run of a workflow file whose id is above after.
func (c *ciHarness) awaitRunOf(path string, after int64) *pb.Run {
	c.t.Helper()
	var r *pb.Run
	c.waitFor("a run of "+path, func() bool {
		for _, x := range c.runs() {
			if x.GetWorkflowPath() == path && x.GetId() > after {
				r = x
				return true
			}
		}
		return false
	})
	return r
}

const concCallee = `on:
  workflow_call:
    inputs:
      env:
        type: string
        default: prod
%s
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - run: echo
`

func concCaller(top string) string {
	return "on: push\njobs:\n  c:\n" + top + "    uses: ./.github/workflows/callee.yml\n    with:\n      env: ${{ github.ref_name }}\n"
}

// D4: the concurrency of the calling job, and the workflow-level concurrency of
// the called workflow (evaluated with the callee's inputs), hold the whole call:
// its jobs are not assignable until the call has the group, and the group is
// freed when the call ends. Both groups are tested in turn.
func TestCIReusableCallConcurrency(t *testing.T) {
	for _, tt := range []struct{ name, caller, callee, group string }{
		{"calling job", "    concurrency:\n      group: call-group\n", "", "call-group"},
		{"called workflow", "", "concurrency: deploy-${{ inputs.env }}\n", "deploy-main"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, runner := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
			c.disableAutoCancel()
			callee := strings.Replace(concCallee, "%s\n", tt.callee, 1)
			files := map[string]string{".github/workflows/callee.yml": callee, ".github/workflows/ci.yml": concCaller(tt.caller)}
			c.push("main", files, "")
			r1 := c.awaitRunOf(".github/workflows/ci.yml", 0)
			j1 := awaitRecord(c, runner, r1.GetId(), "c/j")

			c.push("main", map[string]string{"x.txt": "1"}, "second")
			r2 := c.awaitRunOf(".github/workflows/ci.yml", r1.GetId())
			c.waitFor("the second call waits for the group", func() bool {
				call := jobByKey(c, r2.GetId(), "c")
				return call != nil && call.GetStatus() == pb.CiStatus_CI_STATUS_PENDING
			})
			// Its jobs exist but are not assignable; they show the group.
			j, err := runner.fetch("")
			require.NoError(t, err)
			assert.Nil(t, j, "no job of the second call is given while the first holds the group")

			runner.runJob(j1, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
			j2 := awaitRecord(c, runner, r2.GetId(), "c/j")
			runner.runJob(j2, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
			assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, c.waitRunDone(r2.GetId()).GetConclusion())

			// Freed with the call: a third call takes it at once.
			c.push("main", map[string]string{"y.txt": "1"}, "third")
			r3 := c.awaitRunOf(".github/workflows/ci.yml", r2.GetId())
			j3 := awaitRecord(c, runner, r3.GetId(), "c/j")
			runner.runJob(j3, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
			c.waitRunDone(r3.GetId())
		})
	}
}

// D4: cancel-in-progress on a call cancels the running call: its running job is
// asked to stop, the call ends cancelled with GitHub's annotation, and the
// waiting call takes the group.
func TestCIReusableCallCancelInProgress(t *testing.T) {
	for _, tt := range []struct{ name, caller, callee string }{
		{"calling job", "    concurrency:\n      group: Cip\n      cancel-in-progress: true\n", ""},
		{"called workflow", "", "concurrency:\n  group: Cip\n  cancel-in-progress: true\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, runner := newReusableHarness(t, newFakeRemote(t), newFakeWorkflows(t))
			c.disableAutoCancel()
			callee := strings.Replace(concCallee, "%s\n", tt.callee, 1)
			c.push("main", map[string]string{".github/workflows/callee.yml": callee, ".github/workflows/ci.yml": concCaller(tt.caller)}, "")
			r1 := c.awaitRunOf(".github/workflows/ci.yml", 0)
			j1 := awaitRecord(c, runner, r1.GetId(), "c/j")
			watch, err := runner.jobAPI(j1).runner.WatchJob(c.ctx, connect.NewRequest(&pb.WatchJobRequest{JobId: j1.GetJobId()}))
			require.NoError(t, err)

			c.push("main", map[string]string{"x.txt": "1"}, "second")
			r2 := c.awaitRunOf(".github/workflows/ci.yml", r1.GetId())
			var cancel *pb.CancelRequest
			for watch.Receive() {
				if m := watch.Msg().GetCancel(); m != nil {
					cancel = m
					break
				}
			}
			require.NotNil(t, cancel, "the running job of the call is asked to stop")
			assert.Contains(t, cancel.GetMessage(), "higher priority waiting request for 'Cip'")
			assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_CANCELLED, c.cancelledCall(r1), "the call ends cancelled")

			_, err = runner.jobAPI(j1).runner.CompleteJob(c.ctx, connect.NewRequest(&pb.CompleteJobRequest{
				JobId: j1.GetJobId(), Conclusion: pb.CiConclusion_CI_CONCLUSION_CANCELLED,
			}))
			require.NoError(t, err)
			j2 := awaitRecord(c, runner, r2.GetId(), "c/j")
			runner.runJob(j2, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
			assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, c.waitRunDone(r2.GetId()).GetConclusion())
			assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_CANCELLED, c.waitRunDone(r1.GetId()).GetConclusion())
		})
	}
}

// cancelledCall waits for the call record "c" of a run to end and returns its conclusion.
func (c *ciHarness) cancelledCall(r *pb.Run) pb.CiConclusion {
	c.t.Helper()
	c.waitFor("the call ends", func() bool {
		j := jobByKey(c, r.GetId(), "c")
		return j != nil && j.GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED
	})
	return jobByKey(c, r.GetId(), "c").GetConclusion()
}

// A private repository of the server that the actor cannot read is not a
// source of reusable workflows: the call fails as for a repository that does
// not exist, and a reader gets it.
func TestCIReusableFromAPrivateErmiteRepository(t *testing.T) {
	for _, tt := range []struct {
		name  string
		token func(c *ciHarness) (string, string)
		reads bool
	}{
		{"an owner token", nil, true},
		{"a token that lists the caller only", func(c *ciHarness) (string, string) {
			return c.branchToken([]domain.Scope{domain.ScopeRepoRead, domain.ScopeRepoWrite}, nil, "proj")
		}, false},
		{"a token that lists both", func(c *ciHarness) (string, string) {
			return c.branchToken([]domain.Scope{domain.ScopeRepoRead, domain.ScopeRepoWrite}, nil, "proj", "shared")
		}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			remote := newFakeRemote(t)
			remote.set("owner/shared", ptrRef("refs/heads/main", shaMain))
			wf := newFakeWorkflows(t)
			c, runner := newReusableHarness(t, remote, wf)
			c.seed("shared", domain.VisibilityPrivate)
			wt := c.workTree("main")
			c.writeFile(wt, ".github/workflows/lib.yml", []byte(ghCallee))
			c.mustGit(wt, "push", "-q", c.urlAuth(adminToken, "/shared.git"), "main")
			secret := adminToken
			if tt.token != nil {
				secret, _ = tt.token(c)
			}
			c.pushAs(secret, "main", map[string]string{".github/workflows/ci.yml": "on: push\njobs:\n  lib:\n    uses: owner/shared/.github/workflows/lib.yml@main\n"})
			r := c.awaitRunOf(".github/workflows/ci.yml", 0)
			if tt.reads {
				j := awaitRecord(c, runner, r.GetId(), "lib/test")
				runner.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
				c.waitRunDone(r.GetId())
				return
			}
			c.waitRunDone(r.GetId())
			j := jobByKey(c, r.GetId(), "lib")
			require.NotNil(t, j)
			assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_FAILURE, j.GetConclusion())
			assert.Equal(t, "Unable to resolve action `owner/shared@main`, repository not found", j.GetError(),
				"the text of a repository that is not there, nothing about the private one")
			assert.False(t, remote.asked("owner/shared"), "never falls through to github.com")
			wf.mu.Lock()
			assert.Empty(t, wf.calls)
			wf.mu.Unlock()
		})
	}
}
