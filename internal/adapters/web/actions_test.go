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
