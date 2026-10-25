// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/clientlimit"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

// S6: every page that streams a log through the use case names the visitor, so
// that the limit of log streams per client applies to the pages too, behind a
// trusted proxy by the address it forwards.
func TestLogPagesPassTheClientKey(t *testing.T) {
	t.Parallel()
	const jobID = "1-0-0"
	repo := repoName(t, "proj")
	job := ports.JobView{CIJob: domain.CIJob{
		ID: jobID, RunID: 1, Attempt: 1, RunAttempt: 1, Repo: repo, Key: "build", Name: "build",
		Phase: domain.CIPhaseEnded, Conclusion: domain.CISuccess,
	}, Lines: 1}
	want := clientlimit.Key("198.51.100.7")

	runs := mocks.NewRunService(t)
	runs.EXPECT().GetJob(mock.Anything, mock.Anything, repo, jobID, int32(0)).Return(job, nil).Maybe()
	runs.EXPECT().GetRun(mock.Anything, mock.Anything, repo, int64(1), int32(0)).Return(ports.RunDetail{
		Run: domain.CIRun{Repo: repo, ID: 1, Attempt: 1}, LatestAttempt: 1,
	}, nil).Maybe()
	runs.EXPECT().ListAnnotations(mock.Anything, mock.Anything, repo, int64(1), int32(0), jobID).Return(ports.AnnotationList{}, nil).Maybe()
	var seen []string
	runs.EXPECT().GetJobLog(mock.Anything, mock.Anything, repo, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, _ domain.Principal, _ domain.RepoName, p ports.GetJobLogParams, send func(ports.LogBatch) error) error {
			seen = append(seen, p.Client)
			return send(ports.LogBatch{Lines: []ports.JobLogLine{{Step: 1, CILogLine: domain.CILogLine{Time: time.Unix(0, 0), Text: "x"}}}})
		})

	browse := mocks.NewBrowseService(t)
	browse.EXPECT().ListRefs(mock.Anything, mock.Anything, repo, mock.Anything).Return(ports.RefPage{}, nil).Maybe()
	srv := newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "proj")), Browse: browse, Runs: runs,
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
	})
	for _, path := range []string{
		"/proj/actions/runs/1/job/" + jobID,
		"/proj/actions/runs/1/job/" + jobID + "/raw",
		"/proj/actions/runs/1/job/" + jobID + "/stream",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "192.0.2.1:4000"
		req.Header.Set("X-Forwarded-For", "198.51.100.7")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, path)
	}
	require.Len(t, seen, 3)
	for _, c := range seen {
		assert.Equal(t, want, c)
	}
}

// M4: one helper shapes the lines of the page, the raw feed and the live feed.
func TestAppendLogLine(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 12, 10, 11, 12, 0, time.UTC)
	line := func(text string, cont bool) ports.JobLogLine {
		return ports.JobLogLine{Step: 2, CILogLine: domain.CILogLine{Time: at, Text: text, Continued: cont}}
	}
	var out []LogLineView
	out = appendLogLine(out, 10, line("##[group]Run make", false))
	out = appendLogLine(out, 11, line("plain <b>", false))
	out = appendLogLine(out, 12, line("##[error]Process completed with exit code 1.", false))
	out = appendLogLine(out, 13, line("##[endgroup]", false))
	require.Len(t, out, 4)
	assert.True(t, out[0].Group)
	assert.Equal(t, "Run make", out[0].Text, "the marker is structure, not text")
	assert.EqualValues(t, 10, out[0].N)
	assert.Equal(t, "10:11:12", out[0].Time)
	assert.EqualValues(t, 2, out[0].Step)
	assert.False(t, out[1].Group || out[1].Error || out[1].End)
	assert.True(t, out[2].Error)
	assert.Equal(t, "Process completed with exit code 1.", out[2].Text)
	assert.True(t, out[3].End)

	// A cut line comes back as one line, with the number of its first piece.
	var cut []LogLineView
	cut = appendLogLine(cut, 20, line("first half, ", false))
	cut = appendLogLine(cut, 21, line("second half", true))
	require.Len(t, cut, 1)
	assert.Equal(t, "first half, second half", cut[0].Text)
	assert.EqualValues(t, 20, cut[0].N)

	// A piece that opens a batch has no line before it here: it stays, marked,
	// for the page to join to the line it already holds.
	open := appendLogLine(nil, 30, line("rest", true))
	require.Len(t, open, 1)
	assert.True(t, open[0].Continued)

	// Invisible characters of the runner's text are shown, never applied.
	hidden := appendLogLine(nil, 40, line("a\u202eb", false))
	assert.NotContains(t, hidden[0].Text, "\u202e")
}

// A dependency run is started again from the Inbox: its page offers Cancel while
// it is going and no re-run once it ended, where a workflow run offers both.
func TestRunPageOffersNoRerunOnDependencyRuns(t *testing.T) {
	t.Parallel()
	repo := publicRepo(t, "proj")
	for _, tc := range []struct {
		name       string
		event      string
		status     domain.CIStatus
		wantCancel bool
		wantRerun  bool
	}{
		{"dependency run going", domain.CIEventDependencies, domain.CIInProgress, true, false},
		{"dependency run failed", domain.CIEventDependencies, domain.CICompleted, false, false},
		{"workflow run failed", "push", domain.CICompleted, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runs := mocks.NewRunService(t)
			runs.EXPECT().GetRun(mock.Anything, mock.Anything, repo.Name, int64(1), int32(0)).Return(ports.RunDetail{
				Run:           domain.CIRun{ID: 1, Repo: repo.Name, Attempt: 1, Event: tc.event, Status: tc.status},
				LatestAttempt: 1,
				Jobs:          []ports.JobView{{CIJob: domain.CIJob{Conclusion: domain.CIFailure}}},
			}, nil)
			runs.EXPECT().ListAnnotations(mock.Anything, mock.Anything, repo.Name, int64(1), int32(1), "").Return(ports.AnnotationList{}, nil)
			s := &Server{cfg: Config{Runs: runs, Now: time.Now}}
			p, err := s.runPage(withOwner(t.Context()), &repoData{repo: repo}, 1, 0)
			require.NoError(t, err)
			assert.Equal(t, tc.wantCancel, p.CancelAction != "")
			assert.Equal(t, tc.wantRerun, p.RerunAction != "")
			assert.Equal(t, tc.wantRerun, p.RerunFailedAction != "")
			assert.Equal(t, tc.wantRerun, p.CanRerunFailed)
		})
	}
}
