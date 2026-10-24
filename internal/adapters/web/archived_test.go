// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestArchivedBadges(t *testing.T) {
	v := publicRepo(t, "proj")
	v.Archived = true
	row := homeRow(v, time.Now(), false, false)
	assert.Contains(t, renderToString(t, HomeRowView(row)), `class="pill">Archived</span>`)
	assert.Contains(t, renderToString(t, Header(Layout{Archived: true})), `class="pill">Archived</span>`)
	assert.NotContains(t, renderToString(t, Header(Layout{})), ">Archived<")
}

func archivedIssueRig(t *testing.T) *issueRig {
	t.Helper()
	r := newIssueRig(t, domain.IssuesOwner, false)
	v := issueRepoView(t, domain.IssuesOwner)
	v.Archived = true
	r.srv.cfg.Repos = reposServing(t, v)
	return r
}

func TestArchivedIssueControls(t *testing.T) {
	for _, state := range []domain.IssueState{domain.IssueStateOpen, domain.IssueStateClosed} {
		t.Run(string(state), func(t *testing.T) {
			r := archivedIssueRig(t)
			r.issues.EXPECT().Get(mock.Anything, mock.Anything, repoName(t, "proj"), int64(1)).Return(ports.IssueDetail{Issue: testIssue(t, 1, state, "existing", "")}, nil)
			rec := r.do("GET", "/proj/issues/1", true, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			body := rec.Body.String()
			assert.Contains(t, body, ">Archived<")
			assert.NotContains(t, body, "comment on the issue")
			assert.NotContains(t, body, "Reopen issue")
			assert.Equal(t, state == domain.IssueStateOpen, strings.Contains(body, "Close issue"))
		})
	}
	r := archivedIssueRig(t)
	r.issues.EXPECT().List(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.IssuePage{}, nil)
	rec := r.do("GET", "/proj/issues", true, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), ">New issue<")
	assert.Equal(t, http.StatusNotFound, r.do("GET", "/proj/issues/new", true, nil).Code)
}

func TestArchivedMRControls(t *testing.T) {
	r := archivedIssueRig(t)
	r.mrs.EXPECT().Get(mock.Anything, mock.Anything, repoName(t, "proj"), int64(1), mock.Anything).Return(ports.MergeRequestDetail{MergeRequest: domain.MergeRequest{Repo: repoName(t, "proj"), Number: 1, State: domain.MRStateOpen, Source: "feat", Target: "main"}}, nil)
	rec := r.do("GET", "/proj/merge-requests/1", true, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ">Archived<")
	assert.NotContains(t, rec.Body.String(), `id="merge-menu"`)
	assert.NotContains(t, rec.Body.String(), "comment on the merge request")
	assert.Contains(t, rec.Body.String(), `action="/proj/merge-requests/1/close"`)
}

func TestArchivedRunControls(t *testing.T) {
	v := publicRepo(t, "proj")
	v.Archived = true
	runs := mocks.NewRunService(t)
	runs.EXPECT().GetRun(mock.Anything, mock.Anything, v.Name, int64(1), int32(0)).Return(ports.RunDetail{Run: domain.CIRun{ID: 1, Repo: v.Name, Attempt: 1, Status: domain.CICompleted}, LatestAttempt: 1, Jobs: []ports.JobView{{CIJob: domain.CIJob{Conclusion: domain.CIFailure}}}}, nil)
	runs.EXPECT().ListAnnotations(mock.Anything, mock.Anything, v.Name, int64(1), int32(1), "").Return(ports.AnnotationList{}, nil)
	s := &Server{cfg: Config{Runs: runs, Now: time.Now}}
	p, err := s.runPage(withOwner(t.Context()), &repoData{repo: v}, 1, 0)
	require.NoError(t, err)
	assert.Empty(t, p.RerunAction)
	assert.Empty(t, p.RerunFailedAction)
	assert.False(t, p.CanRerunFailed)
}

func TestArchivedMRListHidesCreation(t *testing.T) {
	r := archivedIssueRig(t)
	r.mrs.EXPECT().List(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.MergeRequestPage{}, nil)
	rec := r.do("GET", "/proj/merge-requests", true, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), ">Open one<")
	assert.NotContains(t, rec.Body.String(), "ermite mr create")
}
