// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

// AC6: a summary is untrusted Markdown. The renderer is the one of the READMEs:
// raw HTML is dropped and a javascript: link loses its link.
func TestSummariesAreRenderedSafely(t *testing.T) {
	t.Parallel()
	const jobID = "1-0-0"
	repo := repoName(t, "proj")
	job := ports.JobView{CIJob: domain.CIJob{
		ID: jobID, RunID: 1, Attempt: 1, RunAttempt: 1, Repo: repo, Key: "build", Name: "build <script>",
		Phase: domain.CIPhaseEnded, Conclusion: domain.CISuccess,
		Steps: []domain.CIStep{{Number: 2, Name: "step <b>two</b>", State: domain.CIStepCompleted, Conclusion: domain.CISuccess}},
	}}
	md := strings.Join([]string{
		"# Title",
		"<script>alert(1)</script>",
		"",
		"[x](javascript:alert(1))",
		"",
		"[ok](https://example.org/)",
		"",
		"<a href=\"javascript:alert(2)\" onclick=\"alert(3)\">raw</a>",
		"",
		"[frag](#title)",
	}, "\n")

	runs := mocks.NewRunService(t)
	runs.EXPECT().GetRun(mock.Anything, mock.Anything, repo, int64(1), int32(0)).Return(ports.RunDetail{
		Run:  domain.CIRun{Repo: repo, ID: 1, Attempt: 1, Number: 1, WorkflowName: "ci", Sha: strings.Repeat("a", 40), Status: domain.CICompleted, Conclusion: domain.CISuccess},
		Jobs: []ports.JobView{job}, LatestAttempt: 1,
	}, nil)
	runs.EXPECT().ListAnnotations(mock.Anything, mock.Anything, repo, int64(1), int32(1), "").Return(ports.AnnotationList{}, nil)
	runs.EXPECT().GetStepSummaries(mock.Anything, mock.Anything, repo, jobID, int32(1)).Return(ports.SummaryList{
		Summaries: []domain.CIStepSummary{{Step: 2, Markdown: md}, {Step: 7, Markdown: "second"}}, Truncated: true,
	}, nil)
	browse := mocks.NewBrowseService(t)
	browse.EXPECT().ListRefs(mock.Anything, mock.Anything, repo, mock.Anything).Return(ports.RefPage{}, nil).Maybe()
	srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "proj")), Browse: browse, Runs: runs})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/proj/actions/runs/1", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	assert.Contains(t, body, "<h1", "Markdown is rendered")
	assert.Contains(t, body, `href="https://example.org/"`)
	assert.Contains(t, body, "second")
	assert.Contains(t, body, "step &lt;b&gt;two&lt;/b&gt;", "the name of a step is text")
	assert.Contains(t, body, "step 7", "a step the job does not list is named by its number")
	assert.Contains(t, body, "build &lt;script&gt;", "the job name is text")
	assert.Contains(t, body, "more summaries than the first 20")
	assert.Contains(t, body, `href="#md-s1-0-0-2-title"`, "fragments point at the headings of their own summary")
	assert.Contains(t, body, `id="md-s1-0-0-2-title"`)

	assert.NotContains(t, body, "<script>alert")
	assert.NotContains(t, body, "javascript:")
	assert.NotContains(t, body, "onclick")
	assert.NotContains(t, body, `<a href="javascript`)
	assert.NotContains(t, body, "alert(1)</", "no script text left as markup")
	assert.Contains(t, body, ">x</", "the text of the dropped link stays")
}

func TestAnnotationViews(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("b", 40)
	jobs := map[string]jobInfo{"1-0-0": {name: "build", href: "/proj/actions/runs/1/job/1-0-0", steps: map[int32]stepInfo{
		2: {name: "Run vet", start: 4, end: 9},
		3: {name: "no lines", start: 9, end: 9},
	}}}
	list := ports.AnnotationList{Annotations: []ports.JobAnnotation{
		{JobID: "1-0-0", Annotation: domain.CIAnnotation{Level: domain.AnnotationFailure, Title: "T", Message: "m", File: "pkg/a b.go", StartLine: 4, EndLine: 6, Step: 2}},
		{JobID: "1-0-0", Annotation: domain.CIAnnotation{Level: domain.AnnotationWarning, Message: "w", File: "x.go", StartLine: 1, EndLine: 1, Step: 3}},
		{JobID: "1-0-0", Annotation: domain.CIAnnotation{Level: domain.AnnotationNotice, Message: "n", Step: 40}},
		{JobID: "gone", Annotation: domain.CIAnnotation{Level: domain.AnnotationNotice, Message: "x", Step: 1}},
		{JobID: "1-0-0", Annotation: domain.CIAnnotation{Level: domain.AnnotationNotice, Message: "p", File: "../../etc/passwd", StartLine: 2, Step: 2}},
		{JobID: "1-0-0", Annotation: domain.CIAnnotation{Level: domain.AnnotationNotice, Message: "a", File: "/etc/passwd", Step: 2}},
		{JobID: "1-0-0", Annotation: domain.CIAnnotation{Level: domain.AnnotationNotice, Message: "b", File: `dir\file.go`, Step: 2}},
		{JobID: "1-0-0", Annotation: domain.CIAnnotation{Level: domain.AnnotationNotice, Message: "c", File: "a/./b.go", Step: 2}},
	}}
	v := annotationViews("proj", sha, list, jobs)
	require.Len(t, v, 7, "an annotation of a job the page does not know is skipped")

	assert.Equal(t, "failure", v[0].Kind)
	assert.Equal(t, "error", v[0].Level)
	assert.Equal(t, "Run vet", v[0].Step)
	assert.Equal(t, "/proj/actions/runs/1/job/1-0-0#L4", v[0].StepHref)
	assert.Equal(t, "pkg/a b.go:4-6", v[0].Where)
	assert.Equal(t, "/proj/blob/"+sha+"/pkg/a%20b.go#L4-L6", v[0].WhereHref)

	assert.Equal(t, "x.go:1", v[1].Where, "an end line equal to the start is not a range")
	assert.Equal(t, "/proj/blob/"+sha+"/x.go#L1", v[1].WhereHref)
	assert.Empty(t, v[1].StepHref, "a step without lines has no anchor")

	assert.Equal(t, "step 40", v[2].Step, "a step the job does not list")
	assert.Equal(t, "notice", v[2].Level)
	assert.Empty(t, v[2].Where)

	for i, want := range []string{"../../etc/passwd:2", "/etc/passwd", `dir\file.go`, "a/./b.go"} {
		assert.Equal(t, want, v[3+i].Where)
		assert.Empty(t, v[3+i].WhereHref, "%q is not a path of the repository", want)
	}
}

func TestRepoPath(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"a", "a/b.go", "dir/with space/x", ".github/workflows/ci.yml", "a..b", "é/ü.go"} {
		assert.True(t, repoPath(ok), ok)
	}
	for _, bad := range []string{"", ".", "..", "../a", "a/..", "a/../b", "/a", "a//b", "a/./b", "a/", `a\b`, "a\x00b", "a\nb", "a\tb"} {
		assert.False(t, repoPath(bad), "%q", bad)
	}
}
