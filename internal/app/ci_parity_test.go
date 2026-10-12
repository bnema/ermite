// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
)

// conformance is testdata/conformance/ctx-job-runner.contexts.json: the
// contexts github.com gave to the ctx-job-runner scenario of ermite-conformance
// (see its _source).
type conformance struct {
	GithubKeyOrder []string       `json:"_github_key_order"`
	JobKeyOrder    []string       `json:"_job_key_order"`
	RunnerOwned    []string       `json:"_runner_owned_github_keys"`
	Github         map[string]any `json:"github"`
	Job            map[string]any `json:"job"`
	Matrix         any            `json:"matrix"`
	Strategy       map[string]any `json:"strategy"`
}

func loadConformance(t *testing.T) conformance {
	t.Helper()
	b, err := os.ReadFile("testdata/conformance/ctx-job-runner.contexts.json")
	require.NoError(t, err)
	var c conformance
	require.NoError(t, json.Unmarshal(b, &c))
	return c
}

// numericPlaceholders are the placeholders of the normaliser of
// ermite-conformance for values that are numbers (or times) on github.com: ids,
// times and the repository size. In the raw bundle of the same scenario id,
// created_at, pushed_at and size are JSON numbers; the fixture only has the
// placeholder, a string. They are matched by "number or string".
var numericPlaceholders = []string{"<id:", "<time>", "<size>", "<actor_id:", "<repository_id:", "<repository_owner_id:", "<check_run_id:"}

// shape is the type of each value of an object. A placeholder of a number
// has the type "scalar".
func shape(m map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if s, ok := v.(string); ok && slices.ContainsFunc(numericPlaceholders, func(p string) bool { return strings.HasPrefix(s, p) }) {
			out[k] = "scalar"
			continue
		}
		switch v.(type) {
		case nil:
			out[k] = "null"
		case string:
			out[k] = "string"
		case float64:
			out[k] = "number"
		case bool:
			out[k] = "bool"
		case []any:
			out[k] = "array"
		case map[string]any:
			out[k] = "object"
		}
	}
	return out
}

// orderedKeys returns the keys of a JSON object in document order.
func orderedKeys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	tok, err := dec.Token()
	require.NoError(t, err)
	require.Equal(t, json.Delim('{'), tok)
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		require.NoError(t, err)
		keys = append(keys, k.(string))
		var skip json.RawMessage
		require.NoError(t, dec.Decode(&skip))
	}
	return keys
}

// sameShape compares the shape of a recorded object with ours: a "scalar"
// (numeric placeholder) accepts a number or a string. The description is a
// property of the recorded repository, which ermite's test repository lacks.
func sameShape(t *testing.T, want, got map[string]any, what string) {
	t.Helper()
	ws, gs := shape(want), shape(got)
	for k, wt := range ws {
		if k == "description" {
			continue
		}
		if wt == "scalar" {
			assert.Contains(t, []string{"number", "string"}, gs[k], "%s.%s", what, k)
			continue
		}
		assert.Equal(t, wt, gs[k], "%s.%s", what, k)
	}
}

// The github and job contexts of a push have the keys, the key order and the
// value types of the ones github.com recorded for the same scenario shape
// (ermite-conformance, ctx-job-runner), apart from the keys the runner adds.
func TestCIContextsMatchTheGitHubRecording(t *testing.T) {
	want := loadConformance(t)
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/ci.yml": workflowWith("branches: [main]")}, "")
	job := runner.await()
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(job.GetContexts()), &raw))
	var ctxs map[string]map[string]any
	require.NoError(t, json.Unmarshal([]byte(job.GetContexts()), &ctxs))
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "x")

	t.Run("github", func(t *testing.T) {
		gh := ctxs["github"]
		// The runner owns some keys and adds job and token; the server sends the rest.
		serverKeys := slices.DeleteFunc(slices.Clone(want.GithubKeyOrder), func(k string) bool { return slices.Contains(want.RunnerOwned, k) })
		assert.Equal(t, serverKeys, orderedKeys(t, raw["github"]), "key set and order")
		serverGithub := map[string]any{}
		for k, v := range want.Github {
			if !slices.Contains(want.RunnerOwned, k) {
				serverGithub[k] = v
			}
		}
		sameShape(t, serverGithub, gh, "github")
		assert.Equal(t, "Actions", gh["secret_source"])
		assert.Equal(t, "10", gh["artifact_cache_size_limit"])
		assert.Equal(t, "private", gh["repository_visibility"], "from the repository")
		assert.Equal(t, "owner", gh["actor"])
		assert.Equal(t, "owner", gh["repository_owner"])
		assert.Regexp(t, `^[1-9][0-9]*$`, gh["actor_id"])
		assert.Regexp(t, `^[1-9][0-9]*$`, gh["repository_id"])
		assert.Regexp(t, `^[1-9][0-9]*$`, gh["repository_owner_id"])
		assert.Equal(t, gh["actor_id"], gh["repository_owner_id"], "the owner pushed")
		assert.Equal(t, "git://example.test/owner/proj.git", gh["repositoryUrl"], "git://<host>/<owner>/<repo>.git, as GitHub's")
	})

	t.Run("event", func(t *testing.T) {
		ev := ctxs["github"]["event"].(map[string]any)
		wev := want.Github["event"].(map[string]any)
		assert.ElementsMatch(t, keysSlice(wev), keysSlice(ev), "event keys")
		sameShape(t, wev, ev, "event")
		repo, wrepo := ev["repository"].(map[string]any), wev["repository"].(map[string]any)
		assert.Len(t, wrepo, 83, "the recording has 83 repository keys")
		assert.ElementsMatch(t, keysSlice(wrepo), keysSlice(repo), "repository keys")
		sameShape(t, wrepo, repo, "event.repository")
		assert.ElementsMatch(t, keysSlice(wrepo["owner"].(map[string]any)), keysSlice(repo["owner"].(map[string]any)), "repository.owner keys")
		sender, wsender := ev["sender"].(map[string]any), wev["sender"].(map[string]any)
		assert.Len(t, wsender, 19)
		assert.ElementsMatch(t, keysSlice(wsender), keysSlice(sender), "sender keys")
		sameShape(t, wsender, sender, "event.sender")
		assert.ElementsMatch(t, keysSlice(wev["pusher"].(map[string]any)), keysSlice(ev["pusher"].(map[string]any)))
		for _, got := range []map[string]any{ev["head_commit"].(map[string]any), ev["commits"].([]any)[0].(map[string]any)} {
			wc := wev["head_commit"].(map[string]any)
			assert.ElementsMatch(t, keysSlice(wc), keysSlice(got), "commit keys: no added/removed/modified/username")
			assert.ElementsMatch(t, keysSlice(wc["author"].(map[string]any)), keysSlice(got["author"].(map[string]any)))
			assert.ElementsMatch(t, keysSlice(wc["committer"].(map[string]any)), keysSlice(got["committer"].(map[string]any)))
			assert.True(t, strings.HasPrefix(got["url"].(string), publicOrigin+"/owner/proj/commit/"), got["url"])
		}
		assert.Equal(t, "proj", repo["name"])
		assert.Equal(t, "owner/proj", repo["full_name"])
		assert.Equal(t, true, repo["private"])
		assert.Equal(t, "private", repo["visibility"])
		assert.Equal(t, "main", repo["default_branch"])
		assert.Equal(t, ctxs["github"]["repository_id"], jsonNumber(repo["id"]), "repository_id is repository.id")
		assert.Equal(t, ctxs["github"]["actor_id"], jsonNumber(sender["id"]))
		assert.Equal(t, ctxs["github"]["repository_owner_id"], jsonNumber(repo["owner"].(map[string]any)["id"]))
	})

	t.Run("job", func(t *testing.T) {
		j := ctxs["job"]
		// The runner adds status first; the server sends the rest, in GitHub's order.
		serverKeys := slices.DeleteFunc(slices.Clone(want.JobKeyOrder), func(k string) bool { return k == "status" })
		assert.Equal(t, serverKeys, orderedKeys(t, raw["job"]))
		serverJob := map[string]any{}
		for k, v := range want.Job {
			if k != "status" {
				serverJob[k] = v
			}
		}
		sameShape(t, serverJob, j, "job")
		assert.Equal(t, ".github/workflows/ci.yml", j["workflow_file_path"])
		assert.Equal(t, "owner/proj", j["workflow_repository"])
		assert.Equal(t, "owner/proj/.github/workflows/ci.yml@refs/heads/main", j["workflow_ref"])
		assert.Equal(t, ctxs["github"]["sha"], j["workflow_sha"])
		assert.Equal(t, ctxs["github"]["workflow_ref"], j["workflow_ref"])
	})

	t.Run("matrix is null without a matrix", func(t *testing.T) {
		assert.Equal(t, "null", string(raw["matrix"]))
		assert.Nil(t, want.Matrix)
		assert.ElementsMatch(t, keysSlice(want.Strategy), keysSlice(ctxs["strategy"]))
	})
}

func keysSlice(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func jsonNumber(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// The run title is the evaluated run-name, else the head commit message.
func TestCIRunTitle(t *testing.T) {
	c := newCIHarness(t, nil)
	c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/named.yml": `name: Named
run-name: "run ${{ github.event_name }} on ${{ github.ref_type }} by ${{ github.actor == '' && 'nobody' || 'someone' }}"
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`, ".github/workflows/plain.yml": workflowWith("branches: [main]")}, "the head commit\n\nbody")
	c.waitFor("two runs", func() bool { return len(c.runs()) == 2 })
	titles := map[string]string{}
	for _, r := range c.runs() {
		titles[r.GetWorkflowPath()] = r.GetTitle()
	}
	assert.Equal(t, map[string]string{
		".github/workflows/named.yml": "run push on branch by someone",
		".github/workflows/plain.yml": "the head commit",
	}, titles)
}

// An over-256 matrix is an invalid workflow, like on github.com
// (matrix-over-256): a failed run with no job.
func TestCIMatrixOver256IsAnInvalidWorkflow(t *testing.T) {
	c := newCIHarness(t, nil)
	c.registerRunner("alpha", "ubuntu-latest")
	var a, b []string
	for i := 1; i <= 17; i++ {
		a = append(a, itoa(i))
	}
	for i := 1; i <= 16; i++ {
		b = append(b, itoa(i))
	}
	c.push("main", map[string]string{".github/workflows/m.yml": `on: push
jobs:
  too-many:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        a: [` + strings.Join(a, ", ") + `]
        b: [` + strings.Join(b, ", ") + `]
    steps:
      - run: echo
`}, "")
	c.waitFor("the run fails", func() bool { return len(c.runs()) == 1 && c.runs()[0].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })
	run := c.getRun(c.runs()[0].GetId())
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_FAILURE, run.GetRun().GetConclusion())
	assert.Empty(t, run.GetJobs(), "no job")
	assert.Contains(t, run.GetRun().GetError(), "Strategy for job 'too-many' produced 272 configurations which exceeds the maximum of 256 configurations")
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

// A step with continue-on-error is no longer refused; the runner runs it.
func TestCIStepContinueOnErrorIsPlanned(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/s.yml": `on: push
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - continue-on-error: ${{ github.event_name == 'push' }}
        run: exit 1
`}, "")
	job := runner.await()
	assert.Equal(t, "j", job.GetJobKey())
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "x")
}

// A step timeout stays refused as long as the runner has none.
func TestCIStepTimeoutIsStillRefused(t *testing.T) {
	c := newCIHarness(t, nil)
	c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/s.yml": `on: push
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - timeout-minutes: 1
        run: echo
`}, "")
	c.waitFor("the run fails", func() bool { return len(c.runs()) == 1 && c.runs()[0].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })
	assert.Equal(t, "not supported yet: timeout-minutes on a step", c.getRun(c.runs()[0].GetId()).GetJobs()[0].GetError())
}

// A failed job with continue-on-error does not fail the run, keeps its own
// failure conclusion, and shows as success to the jobs that need it (recorded:
// if-job-level-fail-effects, where the dependent job printed result=success).
func TestCIJobContinueOnError(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/c.yml": `on: push
jobs:
  soft:
    runs-on: ubuntu-latest
    continue-on-error: ${{ github.event_name == 'push' }}
    steps:
      - run: exit 5
  after:
    needs: soft
    runs-on: ubuntu-latest
    steps:
      - run: echo "result=${{ needs.soft.result }}"
`}, "")
	soft := runner.await()
	assert.Equal(t, "soft", soft.GetJobKey())
	runner.runJob(soft, pb.CiConclusion_CI_CONCLUSION_FAILURE, nil, "x")
	after := runner.await()
	assert.Equal(t, "after", after.GetJobKey(), "a needed job that failed softly does not skip its dependents")
	assert.Contains(t, after.GetContexts(), `"result":"success"`)
	runner.runJob(after, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "x")
	c.waitFor("the run ends", func() bool { return c.runs()[0].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, c.runs()[0].GetConclusion(), "the run does not fail")
	byKey := map[string]pb.CiConclusion{}
	for _, j := range c.getRun(c.runs()[0].GetId()).GetJobs() {
		byKey[j.GetName()] = j.GetConclusion()
	}
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_FAILURE, byKey["soft"], "the job itself failed")
}

// A matrix leg with continue-on-error that fails does not cancel its siblings
// (fail-fast), and does not fail the run; a leg without it still does.
func TestCIJobContinueOnErrorLegDoesNotTriggerFailFast(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/m.yml": `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    continue-on-error: ${{ matrix.experimental }}
    strategy:
      matrix:
        experimental: [true, false]
    steps:
      - run: echo ${{ matrix.experimental }}
`}, "")
	a, b := runner.await(), runner.await()
	soft, hard := a, b
	if strings.Contains(b.GetMatrix(), "true") {
		soft, hard = b, a
	}
	apiHard := runner.jobAPI(hard)
	_, err := apiHard.runner.Heartbeat(c.ctx, connect.NewRequest(&pb.HeartbeatRequest{JobId: hard.GetJobId()}))
	require.NoError(t, err)
	runner.runJob(soft, pb.CiConclusion_CI_CONCLUSION_FAILURE, nil, "x")
	// The sibling must not have been asked to stop.
	watch, err := apiHard.runner.WatchJob(c.ctx, connect.NewRequest(&pb.WatchJobRequest{JobId: hard.GetJobId()}))
	require.NoError(t, err)
	done := make(chan *pb.CancelReason, 1)
	go func() {
		for watch.Receive() {
			if m := watch.Msg().GetCancel(); m != nil {
				r := m.GetReason()
				done <- &r
				return
			}
		}
		done <- nil
	}()
	runner.runJob(hard, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "x")
	c.waitFor("the run ends", func() bool { return c.runs()[0].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, c.runs()[0].GetConclusion())
	if r := <-done; r != nil {
		t.Fatalf("the sibling of a continue-on-error leg was cancelled: %v", *r)
	}
}

// GitHub-shaped URLs, /<owner>/<repo>/..., redirect to ermite's pages, only for
// the owner of the instance and only where there is no page of its own.
func TestWebRedirectsOwnerURLs(t *testing.T) {
	c := newCIHarness(t, nil)
	b := browser{c.harness}
	c.push("main", map[string]string{"README": "x\n"}, "")
	sha := strings.TrimSpace(c.mustGit(c.wt, "rev-parse", "HEAD"))
	for path, to := range map[string]string{
		"/owner/proj":                   "/proj",
		"/owner/proj/commit/" + sha:     "/proj/commit/" + sha,
		"/owner/proj/compare/a...b?x=1": "/proj/compare/a...b?x=1",
		"/owner/proj.git/info/refs":     "/proj.git/info/refs",
	} {
		res := b.get(path, nil)
		assert.Equal(t, http.StatusPermanentRedirect, res.status, path)
		assert.Equal(t, to, res.hdr.Get("Location"), path)
	}
	assert.Equal(t, http.StatusNotFound, b.get("/other/proj/commit/"+sha, nil).status, "not the owner")
	assert.Equal(t, http.StatusNotFound, b.get("/owner", nil).status, "a bare owner is no page")
	assert.Equal(t, http.StatusNotFound, b.get("/owner/Bad Name", nil).status)
	post := b.do("POST", "/owner/proj", map[string]string{"Origin": publicOrigin}, "")
	assert.Equal(t, http.StatusForbidden, post.status, "a write is stopped by the gate, never redirected")
	assert.Empty(t, post.hdr.Get("Location"))
}

// github.job is empty while a job is evaluated, so a name that reads it loses
// it (recorded: ctx-github-names), but the job gets its key in the contexts.
func TestCIJobNameReadsAnEmptyGithubJob(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/n.yml": `on: push
jobs:
  first-job:
    name: Display name of ${{ github.job }}
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`}, "")
	job := runner.await()
	assert.Contains(t, job.GetContexts(), `"job":"first-job"`)
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "x")
	c.waitFor("the run ends", func() bool { return c.runs()[0].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })
	assert.Equal(t, "Display name of", c.getRun(c.runs()[0].GetId()).GetJobs()[0].GetName())
}

// A tag push carries, like a created branch, the commits the default branch
// lacks, newest as head_commit, and the compare URL of several commits is
// "<first>^...<last>" (recorded: push-tag, skip-directive-tag).
func TestCITagEventCarriesTheNewCommits(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/rel.yml": workflowWith("tags: ['v*']")}, "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "rel")
	c.writeFile(c.wt, "a.txt", []byte("a\n")) // commits "add a.txt"
	first := strings.TrimSpace(c.mustGit(c.wt, "rev-parse", "HEAD"))
	c.writeFile(c.wt, "b.txt", []byte("b\n")) // commits "add b.txt"
	last := strings.TrimSpace(c.mustGit(c.wt, "rev-parse", "HEAD"))
	c.mustGit(c.wt, "tag", "v1")
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), "v1")

	job := runner.await()
	var ctxs map[string]map[string]any
	require.NoError(t, json.Unmarshal([]byte(job.GetContexts()), &ctxs))
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "x")
	ev := ctxs["github"]["event"].(map[string]any)
	var msgs []string
	for _, cm := range ev["commits"].([]any) {
		msgs = append(msgs, cm.(map[string]any)["message"].(string))
	}
	assert.Equal(t, []string{"add a.txt", "add b.txt"}, msgs)
	assert.Equal(t, "add b.txt", ev["head_commit"].(map[string]any)["message"])
	assert.Equal(t, publicOrigin+"/owner/proj/compare/"+first[:12]+"^..."+last[:12], ev["compare"])
	assert.Equal(t, "tag", ctxs["github"]["ref_type"])
	assert.Equal(t, true, ev["created"])
}
