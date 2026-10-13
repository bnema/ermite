// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
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

// CI-2 results (phase E2): annotations and step summaries, from the runner's
// calls to the API, the pages and the CLI.

const resultsWorkflow = `name: ci
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hello
  lint:
    runs-on: ubuntu-latest
    steps:
      - run: echo lint
`

func ann(level pb.AnnotationLevel, step int32, msg string) *pb.Annotation {
	return &pb.Annotation{Level: level, Message: msg, StepNumber: step}
}

func (r *fakeRunner) update(j *pb.Job, steps []*pb.StepUpdate, anns ...*pb.Annotation) error {
	_, err := r.jobAPI(j).runner.UpdateJob(r.c.ctx, connect.NewRequest(&pb.UpdateJobRequest{JobId: j.GetJobId(), Steps: steps, Annotations: anns}))
	return err
}

func (r *fakeRunner) summary(j *pb.Job, step int32, md string) error {
	_, err := r.jobAPI(j).runner.UploadSummary(r.c.ctx, connect.NewRequest(&pb.UploadSummaryRequest{JobId: j.GetJobId(), StepNumber: step, Markdown: md}))
	return err
}

func (r *fakeRunner) heartbeat(j *pb.Job) {
	r.c.t.Helper()
	_, err := r.jobAPI(j).runner.Heartbeat(r.c.ctx, connect.NewRequest(&pb.HeartbeatRequest{JobId: j.GetJobId()}))
	require.NoError(r.c.t, err)
}

func (c *ciHarness) annotations(runID int64, jobID string) *pb.ListAnnotationsResponse {
	c.t.Helper()
	res, err := c.root.run.ListAnnotations(c.ctx, connect.NewRequest(&pb.ListAnnotationsRequest{Repo: c.repo, RunId: runID, JobId: jobID}))
	require.NoError(c.t, err)
	return res.Msg
}

func (c *ciHarness) summaries(jobID string) *pb.GetStepSummariesResponse {
	c.t.Helper()
	res, err := c.root.run.GetStepSummaries(c.ctx, connect.NewRequest(&pb.GetStepSummariesRequest{Repo: c.repo, JobId: jobID}))
	require.NoError(c.t, err)
	return res.Msg
}

func code(err error) connect.Code { return connect.CodeOf(err) }

func TestCIAnnotationsAndSummariesOverTheAPI(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/ci.yml": resultsWorkflow}, "")
	c.waitFor("a run", func() bool { return len(c.runs()) == 1 })
	runID := c.runs()[0].GetId()

	ver, err := c.root.server.GetVersion(c.ctx, connect.NewRequest(&pb.GetVersionRequest{}))
	require.NoError(t, err)
	assert.Contains(t, ver.Msg.GetFeatures(), "ci.annotations")
	assert.Contains(t, ver.Msg.GetFeatures(), "ci.summaries")

	a, b := runner.await(), runner.await()
	runner.heartbeat(a)
	runner.heartbeat(b)
	start := now()
	running := []*pb.StepUpdate{{Number: 1, Name: "Set up job", State: pb.StepState_STEP_STATE_IN_PROGRESS, StartedAt: start}}
	require.NoError(t, runner.update(a, running))
	done := &pb.StepUpdate{Number: 1, Name: "Set up job", State: pb.StepState_STEP_STATE_COMPLETED, Conclusion: pb.CiConclusion_CI_CONCLUSION_SUCCESS, StartedAt: start, CompletedAt: now()}

	notice, warning, failure := pb.AnnotationLevel_ANNOTATION_LEVEL_NOTICE, pb.AnnotationLevel_ANNOTATION_LEVEL_WARNING, pb.AnnotationLevel_ANNOTATION_LEVEL_FAILURE
	t.Run("an annotation of a step that UpdateJob never named is stored", func(t *testing.T) {
		full := &pb.Annotation{Level: failure, Title: "Vet", Message: "unused variable", File: "main.go", StartLine: 3, EndLine: 4, StartColumn: 2, EndColumn: 8, StepNumber: 7}
		require.NoError(t, runner.update(a, nil, full, ann(notice, 7, "second")))
		got := c.annotations(runID, a.GetJobId())
		require.Len(t, got.GetAnnotations(), 2)
		assert.Equal(t, a.GetJobId(), got.GetAnnotations()[0].GetJobId())
		assert.EqualValues(t, full.GetStartLine(), got.GetAnnotations()[0].GetAnnotation().GetStartLine())
		assert.Equal(t, "Vet", got.GetAnnotations()[0].GetAnnotation().GetTitle())
		assert.Equal(t, "second", got.GetAnnotations()[1].GetAnnotation().GetMessage(), "in the order the step produced them")
		assert.False(t, got.GetTruncated())
	})

	t.Run("replacement per step, a retry changes nothing, a step absent is kept", func(t *testing.T) {
		require.NoError(t, runner.update(a, nil, ann(warning, 3, "three")))
		require.NoError(t, runner.update(a, nil, ann(warning, 3, "three")))
		require.NoError(t, runner.update(a, nil, ann(notice, 7, "only one now")))
		got := c.annotations(runID, a.GetJobId()).GetAnnotations()
		require.Len(t, got, 2, "step 3 is kept, step 7 holds one annotation")
		assert.EqualValues(t, 3, got[0].GetAnnotation().GetStepNumber(), "ordered by step")
		assert.Equal(t, "only one now", got[1].GetAnnotation().GetMessage())
	})

	t.Run("a COMPLETED step can still have its annotations replaced", func(t *testing.T) {
		require.NoError(t, runner.update(a, []*pb.StepUpdate{done}, ann(notice, 1, "before")))
		require.NoError(t, runner.update(a, nil, ann(failure, 1, "after")))
		require.NoError(t, runner.update(a, []*pb.StepUpdate{done}), "the same step again changes nothing")
		got := c.annotations(runID, a.GetJobId()).GetAnnotations()
		assert.Equal(t, "after", got[0].GetAnnotation().GetMessage())
		assert.Equal(t, failure, got[0].GetAnnotation().GetLevel())
		// The rule that a completed step cannot change still holds for the step.
		changed := &pb.StepUpdate{Number: 1, Name: "Set up job", State: pb.StepState_STEP_STATE_COMPLETED, Conclusion: pb.CiConclusion_CI_CONCLUSION_FAILURE, StartedAt: start, CompletedAt: done.GetCompletedAt()}
		assert.Equal(t, connect.CodeFailedPrecondition, code(runner.update(a, []*pb.StepUpdate{changed})))
	})

	t.Run("an empty list for a step is not a request: only a step present is replaced", func(t *testing.T) {
		before := len(c.annotations(runID, a.GetJobId()).GetAnnotations())
		require.NoError(t, runner.update(a, nil))
		assert.Len(t, c.annotations(runID, a.GetJobId()).GetAnnotations(), before)
	})

	t.Run("the limits, and a call applied whole or not at all", func(t *testing.T) {
		before := c.annotations(runID, a.GetJobId()).GetAnnotations()
		many := func(level pb.AnnotationLevel, n int) []*pb.Annotation {
			var out []*pb.Annotation
			for range n {
				out = append(out, ann(level, 9, "m"))
			}
			return out
		}
		require.NoError(t, runner.update(a, nil, many(warning, 10)...), "ten of a level is allowed")
		require.NoError(t, runner.update(a, nil, append(many(warning, 10), many(failure, 10)...)...), "the limit is per level")
		bad := map[string][]*pb.Annotation{
			"eleven of a level":  many(notice, 11),
			"no level":           {{Message: "m", StepNumber: 1}},
			"no message":         {ann(notice, 1, "")},
			"message too long":   {ann(notice, 1, strings.Repeat("é", 4097))},
			"title too long":     {{Level: notice, Message: "m", Title: strings.Repeat("t", 256), StepNumber: 1}},
			"file too long":      {{Level: notice, Message: "m", File: strings.Repeat("f", 1025), StepNumber: 1}},
			"step zero":          {ann(notice, 0, "m")},
			"step beyond 1000":   {ann(notice, 1001, "m")},
			"end before start":   {{Level: notice, Message: "m", StartLine: 5, EndLine: 4, StepNumber: 1}},
			"a good one and bad": {ann(notice, 1, "good"), ann(notice, 1, "")},
		}
		for name, anns := range bad {
			assert.Equal(t, connect.CodeInvalidArgument, code(runner.update(a, nil, anns...)), name)
		}
		// Characters, not bytes: 4096 two-byte characters pass.
		require.NoError(t, runner.update(a, nil, ann(notice, 11, strings.Repeat("é", 4096))))
		require.NoError(t, runner.update(a, nil, &pb.Annotation{Level: notice, Message: "m", Title: strings.Repeat("t", 255), File: strings.Repeat("f", 1024), StepNumber: 11}))
		// An invalid call stored nothing: steps 1, 3 and 7 are as they were.
		after := c.annotations(runID, a.GetJobId()).GetAnnotations()
		require.GreaterOrEqual(t, len(after), len(before))
		var step1 []string
		for _, x := range after {
			if x.GetAnnotation().GetStepNumber() == 1 {
				step1 = append(step1, x.GetAnnotation().GetMessage())
			}
		}
		assert.Equal(t, []string{"after"}, step1)
		// A step of a bad call that is valid is not applied either.
		assert.Equal(t, connect.CodeInvalidArgument, code(runner.update(a, []*pb.StepUpdate{{Number: 5, State: pb.StepState_STEP_STATE_IN_PROGRESS, StartedAt: now()}}, ann(notice, 1, ""))))
		for _, s := range c.getRun(runID).GetJobs()[0].GetSteps() {
			assert.NotEqual(t, int32(5), s.GetNumber())
		}
	})

	t.Run("summaries: stored, replaced, removed, bounded", func(t *testing.T) {
		require.NoError(t, runner.summary(a, 2, "# one"))
		require.NoError(t, runner.summary(a, 2, "# one"))
		require.NoError(t, runner.summary(a, 2, "# two"))
		require.NoError(t, runner.summary(a, 12, "later"))
		got := c.summaries(a.GetJobId())
		require.Len(t, got.GetSummaries(), 2)
		assert.EqualValues(t, 2, got.GetSummaries()[0].GetStepNumber())
		assert.Equal(t, "# two", got.GetSummaries()[0].GetMarkdown())
		assert.EqualValues(t, 12, got.GetSummaries()[1].GetStepNumber(), "numeric order")
		require.NoError(t, runner.summary(a, 12, ""))
		assert.Len(t, c.summaries(a.GetJobId()).GetSummaries(), 1, "an empty markdown removes it")

		assert.Equal(t, connect.CodeInvalidArgument, code(runner.summary(a, 0, "x")))
		assert.Equal(t, connect.CodeInvalidArgument, code(runner.summary(a, 1001, "x")))
		assert.Equal(t, connect.CodeInvalidArgument, code(runner.summary(a, 2, strings.Repeat("x", 1<<20+1))))
		require.NoError(t, runner.summary(a, 3, strings.Repeat("x", 1<<20)), "exactly 1 MiB is accepted")
		assert.Len(t, c.summaries(a.GetJobId()).GetSummaries(), 2)
		assert.Equal(t, "# two", c.summaries(a.GetJobId()).GetSummaries()[0].GetMarkdown(), "an invalid call changes nothing")
	})

	t.Run("at most 20 summaries per job, with truncated", func(t *testing.T) {
		for i := int32(1); i <= 22; i++ {
			require.NoError(t, runner.summary(b, i, "s"))
		}
		got := c.summaries(b.GetJobId())
		assert.Len(t, got.GetSummaries(), 20)
		assert.True(t, got.GetTruncated())
		assert.EqualValues(t, 20, got.GetSummaries()[19].GetStepNumber())
	})

	t.Run("the token of another job is refused", func(t *testing.T) {
		other := runner.jobAPI(b).runner
		_, err := other.UpdateJob(c.ctx, connect.NewRequest(&pb.UpdateJobRequest{JobId: a.GetJobId(), Annotations: []*pb.Annotation{ann(notice, 1, "x")}}))
		assert.Equal(t, connect.CodePermissionDenied, code(err))
		_, err = other.UploadSummary(c.ctx, connect.NewRequest(&pb.UploadSummaryRequest{JobId: a.GetJobId(), StepNumber: 1, Markdown: "x"}))
		assert.Equal(t, connect.CodePermissionDenied, code(err))
		_, err = other.CompleteJob(c.ctx, connect.NewRequest(&pb.CompleteJobRequest{JobId: a.GetJobId(), Conclusion: pb.CiConclusion_CI_CONCLUSION_SUCCESS, Annotations: []*pb.Annotation{ann(notice, 1, "x")}}))
		assert.Equal(t, connect.CodePermissionDenied, code(err))
		// No credential, a runner token and a stored token are not job tokens either.
		for _, secret := range []string{"", runner.secret, adminToken} {
			cl := c.api(secret).runner
			_, err = cl.UploadSummary(c.ctx, connect.NewRequest(&pb.UploadSummaryRequest{JobId: a.GetJobId(), StepNumber: 1, Markdown: "x"}))
			assert.Equal(t, connect.CodeUnauthenticated, code(err), secret)
		}
		assert.Equal(t, "# two", c.summaries(a.GetJobId()).GetSummaries()[0].GetMarkdown())
	})

	t.Run("CompleteJob applies annotations with the steps and the outcome", func(t *testing.T) {
		steps := []*pb.StepUpdate{
			done,
			{
				Number: 2, Name: "Run make", State: pb.StepState_STEP_STATE_COMPLETED, Conclusion: pb.CiConclusion_CI_CONCLUSION_SUCCESS,
				Outcome: pb.CiConclusion_CI_CONCLUSION_FAILURE, StartedAt: start, CompletedAt: now(),
			},
		}
		_, err := runner.jobAPI(a).runner.CompleteJob(c.ctx, connect.NewRequest(&pb.CompleteJobRequest{
			JobId: a.GetJobId(), Conclusion: pb.CiConclusion_CI_CONCLUSION_SUCCESS, Steps: steps,
			Annotations: []*pb.Annotation{ann(warning, 2, "from complete"), ann(notice, 1, "step one, replaced")},
		}))
		require.NoError(t, err)
		got := c.annotations(runID, a.GetJobId()).GetAnnotations()
		var msgs []string
		for _, x := range got {
			if s := x.GetAnnotation().GetStepNumber(); s == 1 || s == 2 {
				msgs = append(msgs, x.GetAnnotation().GetMessage())
			}
		}
		assert.Equal(t, []string{"step one, replaced", "from complete"}, msgs)

		job := c.getRun(runID).GetJobs()[0]
		assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, job.GetSteps()[1].GetConclusion())
		assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_FAILURE, job.GetSteps()[1].GetOutcome(), "continue-on-error: failed, then succeeded")
		assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, job.GetSteps()[0].GetOutcome(), "a runner that sent none: read as the conclusion")
	})

	t.Run("a job that ended rejects annotations and summaries; a retry of CompleteJob changes nothing", func(t *testing.T) {
		before := len(c.annotations(runID, a.GetJobId()).GetAnnotations())
		err := runner.update(a, nil, ann(notice, 1, "too late"))
		require.Equal(t, connect.CodeFailedPrecondition, code(err))
		assert.NotEmpty(t, errDetails(err), "with a JobEnded detail")
		err = runner.summary(a, 1, "too late")
		assert.Equal(t, connect.CodeFailedPrecondition, code(err))
		assert.NotEmpty(t, errDetails(err))
		res, err := runner.jobAPI(a).runner.CompleteJob(c.ctx, connect.NewRequest(&pb.CompleteJobRequest{
			JobId: a.GetJobId(), Conclusion: pb.CiConclusion_CI_CONCLUSION_SUCCESS, Annotations: []*pb.Annotation{ann(notice, 1, "ignored")},
		}))
		require.NoError(t, err)
		assert.False(t, res.Msg.GetApplied())
		assert.Len(t, c.annotations(runID, a.GetJobId()).GetAnnotations(), before)
		for _, x := range c.annotations(runID, a.GetJobId()).GetAnnotations() {
			assert.NotEqual(t, "too late", x.GetAnnotation().GetMessage())
			assert.NotEqual(t, "ignored", x.GetAnnotation().GetMessage())
		}
		assert.Equal(t, "# two", c.summaries(a.GetJobId()).GetSummaries()[0].GetMarkdown(), "still readable after the end")
	})

	t.Run("reading: the run, one job, a job that is not of the run, access", func(t *testing.T) {
		all := c.annotations(runID, "")
		jobs := map[string]int{}
		for _, x := range all.GetAnnotations() {
			jobs[x.GetJobId()]++
		}
		assert.Len(t, jobs, 1, "job b has none")
		assert.Empty(t, c.annotations(runID, b.GetJobId()).GetAnnotations())

		_, err := c.root.run.ListAnnotations(c.ctx, connect.NewRequest(&pb.ListAnnotationsRequest{Repo: c.repo, RunId: runID, JobId: "99-0-0"}))
		assert.Equal(t, connect.CodeNotFound, code(err))
		_, err = c.root.run.ListAnnotations(c.ctx, connect.NewRequest(&pb.ListAnnotationsRequest{Repo: c.repo, RunId: runID, Attempt: 2}))
		assert.Equal(t, connect.CodeNotFound, code(err))
		_, err = c.root.run.ListAnnotations(c.ctx, connect.NewRequest(&pb.ListAnnotationsRequest{Repo: c.repo, RunId: 99}))
		assert.Equal(t, connect.CodeNotFound, code(err))
		_, err = c.root.run.GetStepSummaries(c.ctx, connect.NewRequest(&pb.GetStepSummariesRequest{Repo: c.repo, JobId: "99-0-0"}))
		assert.Equal(t, connect.CodeNotFound, code(err))
		_, err = c.root.run.GetStepSummaries(c.ctx, connect.NewRequest(&pb.GetStepSummariesRequest{Repo: c.repo, JobId: a.GetJobId(), Attempt: 3}))
		assert.Equal(t, connect.CodeNotFound, code(err))

		// The repository is private: an anonymous caller sees nothing of it, a
		// reader sees everything, as for GetRun.
		anon := c.api("").run
		_, err = anon.ListAnnotations(c.ctx, connect.NewRequest(&pb.ListAnnotationsRequest{Repo: c.repo, RunId: runID}))
		assert.Equal(t, connect.CodeNotFound, code(err))
		_, err = anon.GetStepSummaries(c.ctx, connect.NewRequest(&pb.GetStepSummariesRequest{Repo: c.repo, JobId: a.GetJobId()}))
		assert.Equal(t, connect.CodeNotFound, code(err))
		reader := c.api(c.token(scopes(domain.ScopeRepoRead))).run
		_, err = reader.ListAnnotations(c.ctx, connect.NewRequest(&pb.ListAnnotationsRequest{Repo: c.repo, RunId: runID}))
		assert.NoError(t, err)
		_, err = reader.GetStepSummaries(c.ctx, connect.NewRequest(&pb.GetStepSummariesRequest{Repo: c.repo, JobId: a.GetJobId()}))
		assert.NoError(t, err)
	})

	t.Run("the summaries of an ended job stay readable", func(t *testing.T) {
		_, err := runner.jobAPI(b).runner.CompleteJob(c.ctx, connect.NewRequest(&pb.CompleteJobRequest{JobId: b.GetJobId(), Conclusion: pb.CiConclusion_CI_CONCLUSION_FAILURE}))
		require.NoError(t, err)
		assert.Len(t, c.summaries(b.GetJobId()).GetSummaries(), 20)
	})
}

// errDetails returns the Connect error details of err.
func errDetails(err error) []*connect.ErrorDetail {
	var ce *connect.Error
	if connect.CodeOf(err) == connect.CodeUnknown {
		return nil
	}
	if e, ok := err.(*connect.Error); ok {
		ce = e
	}
	if ce == nil {
		return nil
	}
	return ce.Details()
}

// The pages: the Annotations box of the run, the annotations at their step on
// the job page, the summaries rendered under the run, with nothing of the
// workflow's markup reaching the page as markup.
func TestWebAnnotationsAndSummaries(t *testing.T) {
	ci := newCIHarness(t, nil)
	b := browser{ci.harness}
	runner := ci.registerRunner("alpha", "ubuntu-latest")
	_, err := ci.root.repo.UpdateRepo(ci.ctx, connect.NewRequest(&pb.UpdateRepoRequest{
		Repo: "proj", Visibility: pb.Visibility_VISIBILITY_PUBLIC, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"visibility"}},
	}))
	require.NoError(t, err)
	sha := ci.push("main", map[string]string{".github/workflows/ci.yml": strings.Split(resultsWorkflow, "  lint:")[0], "src/main.go": "package main\n"}, "")
	ci.waitFor("the run", func() bool { return len(ci.runs()) == 1 })

	job := runner.await()
	runner.heartbeat(job)
	start := now()
	steps := []*pb.StepUpdate{
		{Number: 1, Name: "Set up job", State: pb.StepState_STEP_STATE_COMPLETED, Conclusion: pb.CiConclusion_CI_CONCLUSION_SUCCESS, StartedAt: start, CompletedAt: now()},
		{Number: 2, Name: "Run <b>vet</b>", State: pb.StepState_STEP_STATE_COMPLETED, Conclusion: pb.CiConclusion_CI_CONCLUSION_SUCCESS, StartedAt: start, CompletedAt: now()},
	}
	require.NoError(t, runner.update(job, steps))
	_, err = runner.jobAPI(job).runner.AppendLog(ci.ctx, connect.NewRequest(&pb.AppendLogRequest{JobId: job.GetJobId(), Step: 2, Lines: []*pb.LogLine{{Time: now(), Text: "vet"}}}))
	require.NoError(t, err)
	evil := `<img src=x onerror=alert(1)>`
	require.NoError(t, runner.update(job, nil,
		&pb.Annotation{Level: pb.AnnotationLevel_ANNOTATION_LEVEL_FAILURE, Title: "Vet <i>failed</i>", Message: "unused " + evil, File: "src/main.go", StartLine: 1, EndLine: 3, StepNumber: 2},
		&pb.Annotation{Level: pb.AnnotationLevel_ANNOTATION_LEVEL_WARNING, Message: "outside", File: "../../etc/passwd", StartLine: 1, StepNumber: 2},
		&pb.Annotation{Level: pb.AnnotationLevel_ANNOTATION_LEVEL_NOTICE, Message: "plain notice", StepNumber: 1},
	))
	require.NoError(t, runner.summary(job, 2, strings.Join([]string{
		"# Results",
		"",
		"<script>alert('summary')</script>",
		"",
		"[click](javascript:alert(1)) and [ok](https://example.org/x)",
		"",
		"![pixel](https://evil.example/p.png)",
		"",
		"<img src=x onerror=alert(2)>",
		"",
		"| a | b |", "|---|---|", "| 1 | 2 |",
	}, "\n")))
	_, err = runner.jobAPI(job).runner.CompleteJob(ci.ctx, connect.NewRequest(&pb.CompleteJobRequest{
		JobId: job.GetJobId(), Conclusion: pb.CiConclusion_CI_CONCLUSION_SUCCESS, Steps: steps,
	}))
	require.NoError(t, err)
	ci.waitFor("the run completes", func() bool { return ci.runs()[0].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })

	run := b.get("/proj/actions/runs/1", nil)
	require.Equal(t, 200, run.status, run.body)
	assert.Contains(t, run.body, "Annotations")
	assert.Contains(t, run.body, `aria-label="error"`, "the level has a symbol and a word")
	assert.Contains(t, run.body, `aria-label="warning"`)
	assert.Contains(t, run.body, "Vet &lt;i&gt;failed&lt;/i&gt;", "a title is text")
	assert.Contains(t, run.body, "unused &lt;img src=x onerror=alert(1)&gt;")
	assert.NotContains(t, run.body, evil)
	assert.Contains(t, run.body, "Run &lt;b&gt;vet&lt;/b&gt;", "the step name is text")
	assert.Contains(t, run.body, `href="/proj/blob/`+sha+`/src/main.go#L1-L3"`, "file:line links to the file at the commit")
	assert.Contains(t, run.body, "src/main.go:1-3")
	assert.Contains(t, run.body, "../../etc/passwd:1")
	assert.NotContains(t, run.body, `href="/proj/blob/`+sha+`/../`, "a path that leaves the repository is not a link")
	assert.NotContains(t, run.body, `etc/passwd:1</a>`)
	assert.Contains(t, run.body, `href="/proj/actions/runs/1/job/`+job.GetJobId()+`"`, "the job of the annotation")

	// The summary is rendered by the README renderer: markup dropped.
	assert.Contains(t, run.body, "<h1", "Markdown is rendered")
	assert.Contains(t, run.body, "Results")
	assert.Contains(t, run.body, "<table>")
	assert.Contains(t, run.body, `href="https://example.org/x"`)
	assert.Contains(t, run.body, `rel="nofollow noopener noreferrer"`)
	assert.NotContains(t, run.body, "alert('summary')")
	assert.NotContains(t, run.body, "<script>alert")
	assert.NotContains(t, run.body, "javascript:")
	assert.NotContains(t, run.body, "onerror=alert(2)")
	assert.NotContains(t, run.body, `<img src=x`)
	assert.NotContains(t, run.body, "evil.example/p.png", "images are never loaded")
	assert.Contains(t, run.body, `<span class="alt">[pixel]</span>`)
	assert.Contains(t, run.body, `click`, "the text of a dropped link stays")

	// The job page: each annotation at its step, the steps linking to the log.
	jobPage := b.get("/proj/actions/runs/1/job/"+job.GetJobId(), nil)
	require.Equal(t, 200, jobPage.status, jobPage.body)
	assert.Contains(t, jobPage.body, `id="job-annotations"`)
	assert.Contains(t, jobPage.body, "plain notice")
	assert.Contains(t, jobPage.body, "unused &lt;img src=x onerror=alert(1)&gt;")
	assert.NotContains(t, jobPage.body, evil)
	assert.Less(t, strings.Index(jobPage.body, "plain notice"), strings.Index(jobPage.body, "unused &lt;img"), "by step")

}

// The job page follows the annotations live: a report of the runner replaces the
// annotations element, and the stream still never carries the log.
func TestWebJobAnnotationsAreLive(t *testing.T) {
	ci := newCIHarness(t, nil)
	runner := ci.registerRunner("alpha", "ubuntu-latest")
	_, err := ci.root.repo.UpdateRepo(ci.ctx, connect.NewRequest(&pb.UpdateRepoRequest{
		Repo: "proj", Visibility: pb.Visibility_VISIBILITY_PUBLIC, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"visibility"}},
	}))
	require.NoError(t, err)
	ci.push("main", map[string]string{".github/workflows/ci.yml": strings.Split(resultsWorkflow, "  lint:")[0]}, "")
	ci.waitFor("the run", func() bool { return len(ci.runs()) == 1 })
	job := runner.await()
	stream := ci.sse("topic=repo:proj&view=job&job="+job.GetJobId(), "")
	runner.heartbeat(job)
	require.NoError(t, runner.update(job, []*pb.StepUpdate{{Number: 1, Name: "Set up job", State: pb.StepState_STEP_STATE_IN_PROGRESS, StartedAt: now()}},
		ann(pb.AnnotationLevel_ANNOTATION_LEVEL_WARNING, 1, "live <b>warning</b>")))
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-stream.events:
			if ev.name == "ci-job" && strings.Contains(ev.data, `id="job-annotations"`) && strings.Contains(ev.data, "live &lt;b&gt;warning&lt;/b&gt;") {
				assert.NotContains(t, ev.data, "<b>warning</b>")
				return
			}
		case <-deadline:
			t.Fatal("the annotation did not reach the stream of the job")
		}
	}
}

// A run that is not over has no summary block yet, and a run without result has
// neither box.
func TestWebRunWithoutAnnotationsOrSummaries(t *testing.T) {
	ci := newCIHarness(t, nil)
	b := browser{ci.harness}
	runner := ci.registerRunner("alpha", "ubuntu-latest")
	_, err := ci.root.repo.UpdateRepo(ci.ctx, connect.NewRequest(&pb.UpdateRepoRequest{
		Repo: "proj", Visibility: pb.Visibility_VISIBILITY_PUBLIC, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"visibility"}},
	}))
	require.NoError(t, err)
	ci.push("main", map[string]string{".github/workflows/ci.yml": strings.Split(resultsWorkflow, "  lint:")[0]}, "")
	ci.waitFor("the run", func() bool { return len(ci.runs()) == 1 })
	queued := b.get("/proj/actions/runs/1", nil)
	require.Equal(t, 200, queued.status)
	assert.NotContains(t, queued.body, "Annotations")

	job := runner.await()
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "hello")
	done := b.get("/proj/actions/runs/1", nil)
	require.Equal(t, 200, done.status)
	assert.NotContains(t, done.body, "Annotations")
	assert.NotContains(t, done.body, `class="summaries"`)
	page := b.get("/proj/actions/runs/1/job/"+job.GetJobId(), nil)
	require.Equal(t, 200, page.status)
	assert.Contains(t, page.body, `id="job-annotations"`, "the slot the live updates fill")
	assert.NotContains(t, page.body, "Annotations <")
}

// `run view` prints the ANNOTATIONS section and `--summary` the Markdown.
func TestCLIRunViewAnnotationsAndSummary(t *testing.T) {
	ci := newCIHarness(t, nil)
	h := &cliHarness{harness: ci.harness, url: ci.srv.URL}
	runner := ci.registerRunner("alpha", "ubuntu-latest")
	ci.push("main", map[string]string{".github/workflows/ci.yml": resultsWorkflow}, "")
	ci.waitFor("the run", func() bool { return len(ci.runs()) == 1 })

	// A run without annotation prints none.
	a, b := runner.await(), runner.await()
	runner.heartbeat(a)
	runner.heartbeat(b)
	plain := h.cli(t, adminToken, "run", "view", "proj", "1")
	require.NoError(t, plain.err, plain.stderr)
	assert.NotContains(t, plain.stdout, "ANNOTATIONS")
	none := h.cli(t, adminToken, "run", "view", "proj", "1", "--summary")
	require.NoError(t, none.err, none.stderr)
	assert.Contains(t, none.stderr, "no job summary")

	start := now()
	steps := []*pb.StepUpdate{
		{Number: 1, Name: "Set up job", State: pb.StepState_STEP_STATE_COMPLETED, Conclusion: pb.CiConclusion_CI_CONCLUSION_SUCCESS, StartedAt: start, CompletedAt: now()},
		{Number: 2, Name: "Run make", State: pb.StepState_STEP_STATE_COMPLETED, Conclusion: pb.CiConclusion_CI_CONCLUSION_SUCCESS, Outcome: pb.CiConclusion_CI_CONCLUSION_FAILURE, StartedAt: start, CompletedAt: now()},
	}
	require.NoError(t, runner.update(a, steps,
		&pb.Annotation{Level: pb.AnnotationLevel_ANNOTATION_LEVEL_FAILURE, Title: "Vet", Message: "unused\x1b[31m variable", File: "main.go", StartLine: 3, EndLine: 5, StepNumber: 2},
		&pb.Annotation{Level: pb.AnnotationLevel_ANNOTATION_LEVEL_WARNING, Message: "careful", StepNumber: 2},
		&pb.Annotation{Level: pb.AnnotationLevel_ANNOTATION_LEVEL_NOTICE, Message: "fyi", File: "README.md", StepNumber: 1},
	))
	require.NoError(t, runner.summary(a, 2, "# Hello\n\nfrom a\x1b[2J step\n"))
	require.NoError(t, runner.summary(b, 2, "from b"))

	view := h.cli(t, adminToken, "run", "view", "proj", "1")
	require.NoError(t, view.err, view.stderr)
	assert.Contains(t, view.stdout, "\nANNOTATIONS\n")
	assert.Contains(t, view.stdout, "LEVEL")
	for _, want := range []string{"error", "Vet: unused", "main.go:3-5", "warning", "careful", "notice", "fyi", "README.md", "build"} {
		assert.Contains(t, view.stdout, want)
	}
	assert.NotContains(t, view.stdout, "\x1b", "control characters of the workflow never reach the terminal")
	assert.Less(t, strings.Index(view.stdout, "Set up job"), strings.Index(view.stdout, "ANNOTATIONS"), "after the jobs")
	// Ordered by job, step, production order: notice (step 1) before the two of step 2.
	assert.Less(t, strings.Index(view.stdout, "fyi"), strings.Index(view.stdout, "Vet: unused"))
	assert.Less(t, strings.Index(view.stdout, "Vet: unused"), strings.Index(view.stdout, "careful"))

	js := h.cli(t, adminToken, "run", "view", "proj", "1", "--json")
	require.NoError(t, js.err, js.stderr)
	d := decode[jsonRunDetail](t, js.stdout)
	require.Len(t, d.Annotations, 3)
	assert.Equal(t, jsonAnnotation{
		Job: "build", JobID: a.GetJobId(), Step: 1, Level: "notice", Message: "fyi", File: "README.md",
	}, d.Annotations[0])
	assert.Equal(t, "failure", d.Annotations[1].Level, "the API's name in JSON")
	assert.EqualValues(t, 5, d.Annotations[1].EndLine)
	assert.False(t, d.AnnotationsTruncated)
	assert.Equal(t, "failure", d.Jobs[0].Steps[1].Outcome)
	assert.Equal(t, "success", d.Jobs[0].Steps[1].Conclusion)
	assert.Equal(t, "success", d.Jobs[0].Steps[0].Outcome, "none stored: the conclusion")

	// --summary: raw Markdown, a header per job and step when there are several.
	sum := h.cli(t, adminToken, "run", "view", "proj", "1", "--summary")
	require.NoError(t, sum.err, sum.stderr)
	assert.Contains(t, sum.stdout, "== build: Run make ==\n# Hello\n")
	assert.Contains(t, sum.stdout, "== lint: step 2 ==\nfrom b\n")
	assert.NotContains(t, sum.stdout, "\x1b")
	one := h.cli(t, adminToken, "run", "view", "proj", "1", "--summary", "--job", "lint")
	require.NoError(t, one.err, one.stderr)
	assert.Equal(t, "from b\n", one.stdout, "a single summary is printed as it is")
	assert.Error(t, h.cli(t, adminToken, "run", "view", "proj", "1", "--summary", "--log").err)
	assert.Error(t, h.cli(t, adminToken, "run", "view", "proj", "1", "--summary", "--json").err)
	assert.Error(t, h.cli(t, adminToken, "run", "view", "proj", "1", "--summary", "--job", "nope").err)
}
