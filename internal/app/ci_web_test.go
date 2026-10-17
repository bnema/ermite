// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
)

// AC5, AC6: the pages of CI: the list of runs, a run, a job with its log (page,
// raw and stream), the owner's buttons, and the checks on the commit, the
// branch list, the commit list and the merge request.
func TestWebActionsPages(t *testing.T) {
	ci := newCIHarness(t, nil)
	c := &cliHarness{harness: ci.harness, url: ci.srv.URL}
	b := browser{ci.harness}
	runner := ci.registerRunner("alpha", "ubuntu-latest")
	_, err := ci.root.repo.UpdateRepo(ci.ctx, connect.NewRequest(&pb.UpdateRepoRequest{
		Repo: "proj", Visibility: pb.Visibility_VISIBILITY_PUBLIC, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"visibility"}},
	}))
	require.NoError(t, err)
	ci.push("main", map[string]string{"README": "x\n"}, "")
	sha := ci.push("feat", map[string]string{".github/workflows/ci.yml": ciWorkflow}, "add ci")
	ci.waitFor("the run", func() bool { return len(ci.runs()) == 1 })

	// Queued: the list shows it, with a word for its state.
	list := b.get("/proj/actions", nil)
	require.Equal(t, 200, list.status, list.body)
	assert.Contains(t, list.body, "queued")
	assert.Contains(t, list.body, `href="/proj/actions/runs/1"`)
	assert.Contains(t, list.body, `aria-label="queued"`)
	assert.Contains(t, list.body, "/-/events?topic=repo%3Aproj&amp;view=actions", "the page follows the repository stream")
	assert.Contains(t, list.body, `aria-current="page">Actions <kbd`, "the tab")

	// The MR page says "waiting for a runner" only when no runner matches.
	mr, err := ci.root.mr.CreateMergeRequest(ci.ctx, connect.NewRequest(&pb.CreateMergeRequestRequest{Repo: "proj", Title: "Add CI", SourceBranch: "feat"}))
	require.NoError(t, err)
	mrPage := b.get("/proj/merge-requests/1", nil)
	require.Equal(t, 200, mrPage.status)
	assert.Contains(t, mrPage.body, `id="mr-checks"`)
	assert.Contains(t, mrPage.body, "build")
	assert.Contains(t, mrPage.body, "a failing or unfinished check blocks the merge", "the rule shown is the one that applies")
	assert.NotContains(t, mrPage.body, "waiting for a runner", "a runner matches")
	_ = mr

	job := runner.await()
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "##[group]Run echo hello", "hello <b>bold</b>", "##[endgroup]")
	next := runner.await()
	runner.runJob(next, pb.CiConclusion_CI_CONCLUSION_FAILURE, nil, "##[error]Process completed with exit code 1.")
	ci.waitFor("the run completes", func() bool { return ci.runs()[0].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })

	run := b.get("/proj/actions/runs/1", nil)
	require.Equal(t, 200, run.status, run.body)
	assert.Contains(t, run.body, "build")
	assert.Contains(t, run.body, "test")
	assert.Contains(t, run.body, "failure")
	assert.Contains(t, run.body, "on alpha (vm)", "the runner and its isolation are named")
	assert.NotContains(t, run.body, `action="/proj/actions/runs/1/rerun"`, "no button for a visitor")

	jobPage := b.get("/proj/actions/runs/1/job/"+job.GetJobId(), nil)
	require.Equal(t, 200, jobPage.status, jobPage.body)
	assert.Contains(t, jobPage.body, "Run echo hello")
	assert.Contains(t, jobPage.body, "hello &lt;b&gt;bold&lt;/b&gt;", "log text is escaped, never markup")
	assert.NotContains(t, jobPage.body, "<b>bold</b>")
	assert.NotContains(t, jobPage.body, "##[group]", "markers are shown as structure")
	assert.Contains(t, jobPage.body, `class="lg group"`)
	failed := b.get("/proj/actions/runs/1/job/"+next.GetJobId(), nil)
	assert.Contains(t, failed.body, `class="lg error"`)

	raw := b.get("/proj/actions/runs/1/job/"+job.GetJobId()+"/raw", nil)
	require.Equal(t, 200, raw.status)
	assert.Equal(t, "text/plain; charset=utf-8", raw.hdr.Get("Content-Type"))
	assert.Equal(t, "nosniff", raw.hdr.Get("X-Content-Type-Options"))
	assert.Contains(t, raw.hdr.Get("Content-Security-Policy"), "sandbox")
	assert.Contains(t, raw.body, "hello <b>bold</b>", "raw is raw text")
	assert.Regexp(t, `(?m)^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d+Z ##\[group\]Run echo hello$`, raw.body)

	// The stream of a finished job sends its lines then ends.
	stream := b.get("/proj/actions/runs/1/job/"+job.GetJobId()+"/stream?from=1", nil)
	require.Equal(t, 200, stream.status)
	assert.Equal(t, "text/event-stream", stream.hdr.Get("Content-Type"))
	assert.Contains(t, stream.body, "event: ci-log")
	assert.Contains(t, stream.body, "event: ci-end")
	assert.NotContains(t, stream.body, "Run echo hello", "from=1 skips the first line")

	// Checks next to the commit, the commits list and the branch list.
	commit := b.get("/proj/commit/"+sha, nil)
	require.Equal(t, 200, commit.status)
	assert.Contains(t, commit.body, "checks failed")
	assert.Contains(t, b.get("/proj/commits/feat", nil).body, "checks failed")
	assert.Contains(t, b.get("/proj/refs", nil).body, "checks failed")

	// Private repository: nothing to anonymous.
	_, err = ci.root.repo.UpdateRepo(ci.ctx, connect.NewRequest(&pb.UpdateRepoRequest{
		Repo: "proj", Visibility: pb.Visibility_VISIBILITY_PRIVATE, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"visibility"}},
	}))
	require.NoError(t, err)
	for _, p := range []string{"/proj/actions", "/proj/actions/runs/1", "/proj/actions/runs/1/job/" + job.GetJobId(), "/proj/actions/runs/1/job/" + job.GetJobId() + "/raw", "/proj/actions/runs/1/job/" + job.GetJobId() + "/stream"} {
		assert.Equal(t, 404, b.get(p, nil).status, p)
	}

	// The owner sees the buttons and re-runs the failed job.
	token := c.signIn(t)
	owner := b.get("/proj/actions/runs/1", as(token))
	require.Equal(t, 200, owner.status)
	assert.Contains(t, owner.body, `action="/proj/actions/runs/1/rerun"`)
	assert.Contains(t, owner.body, `action="/proj/actions/runs/1/rerun-failed"`)
	assert.NotContains(t, owner.body, `action="/proj/actions/runs/1/cancel"`)
	// A POST needs the owner's session and the origin.
	assert.Equal(t, 403, b.do("POST", "/proj/actions/runs/1/rerun-failed", map[string]string{"Origin": publicOrigin}, "").status, "anonymous")
	res := b.form("/proj/actions/runs/1/rerun-failed", token, url.Values{})
	require.Equal(t, 303, res.status, res.body)
	ci.waitFor("attempt 2 is queued", func() bool { return ci.runs()[0].GetAttempt() == 2 })
	again := b.get("/proj/actions/runs/1?attempt=1", as(token))
	assert.Contains(t, again.body, "attempt 1")
	assert.NotContains(t, again.body, `action="/proj/actions/runs/1/rerun"`, "an earlier attempt has no button")

	cancel := b.form("/proj/actions/runs/1/cancel", token, url.Values{})
	require.Equal(t, 303, cancel.status, cancel.body)
	ci.waitFor("cancelled", func() bool {
		return ci.runs()[0].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED && ci.runs()[0].GetConclusion() == pb.CiConclusion_CI_CONCLUSION_CANCELLED
	})
	assert.Equal(t, 409, b.form("/proj/actions/runs/1/cancel", token, url.Values{}).status, "a completed run cannot be cancelled")
}

// The live updates of the pages: the run page gets the body of the run when a
// job changes, the job page its state, never a line of the log, and the page
// of a merge request the checks of the head it shows (and only of that head).
func TestWebActionsLiveFragments(t *testing.T) {
	ci := newCIHarness(t, nil)
	c := &cliHarness{harness: ci.harness, url: ci.srv.URL}
	runner := ci.registerRunner("alpha", "ubuntu-latest")
	_, err := ci.root.repo.UpdateRepo(ci.ctx, connect.NewRequest(&pb.UpdateRepoRequest{
		Repo: "proj", Visibility: pb.Visibility_VISIBILITY_PUBLIC, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"visibility"}},
	}))
	require.NoError(t, err)
	ci.push("main", map[string]string{".github/workflows/ci.yml": strings.Split(ciWorkflow, "  test:")[0]}, "")
	ci.waitFor("the run", func() bool { return len(ci.runs()) == 1 })
	token := c.signIn(t)

	run := ci.sse("topic=repo:proj&view=run&n=1", "")
	list := ci.sse("topic=repo:proj&view=actions", "")
	job := runner.await()
	jobStream := ci.sse("topic=repo:proj&view=job&job="+job.GetJobId(), "")
	sha := ci.runs()[0].GetSha()
	mrPage := ci.sse("topic=repo:proj&view=mr&n=1&ref=main&head="+sha, "")
	otherHead := ci.sse("topic=repo:proj&view=mr&n=2&ref=x&head="+strings.Repeat("0", 40), "")
	_ = token

	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "a secret value in the log")
	deadline := time.After(10 * time.Second)
	var sawRun, sawList, sawJob, sawChecks bool
	for !sawRun || !sawList || !sawJob || !sawChecks {
		select {
		case ev := <-run.events:
			sawRun = sawRun || (ev.name == "ci-run" && strings.Contains(ev.data, `id="run-body"`))
			assert.NotContains(t, ev.data, "a secret value in the log")
		case ev := <-list.events:
			sawList = sawList || (ev.name == "ci-run" && strings.Contains(ev.data, `id="run-1"`))
		case ev := <-jobStream.events:
			sawJob = sawJob || (ev.name == "ci-job" && strings.Contains(ev.data, `id="job-head"`))
			assert.NotContains(t, ev.data, "a secret value in the log", "the stream of states never carries the log")
		case ev := <-mrPage.events:
			sawChecks = sawChecks || (ev.name == "ci-run" && strings.Contains(ev.data, `id="mr-checks"`) && strings.Contains(ev.data, "success"))
		case ev := <-otherHead.events:
			assert.NotEqual(t, "ci-run", ev.name, "the checks of another head are not sent")
		case <-deadline:
			t.Fatalf("fragments: run %v, list %v, job %v, checks %v", sawRun, sawList, sawJob, sawChecks)
		}
	}
	_ = domain.CISuccess
}
