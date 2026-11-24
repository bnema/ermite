// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

// workServer serves /-/work with an inbox that answers only what the page
// reads; each test expects Work and Counts once and nothing else.
func workServer(t *testing.T, runs ports.RunService) (*Server, *mocks.InboxService) {
	t.Helper()
	svc := mocks.NewInboxService(t)
	srv := newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "alpha")), Browse: homeBrowse(t), Inbox: svc, Runs: runs,
		Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
	})
	return srv, svc
}

func TestWorkPageForTheOwner(t *testing.T) {
	t.Parallel()
	ov := inboxOverview(t)
	srv, svc := workServer(t, nil)
	svc.EXPECT().Work(mock.Anything, mock.Anything).Return(ports.InboxWork{MergeRequests: ov.MergeRequests, Issues: ov.Issues}, nil).Once()
	svc.EXPECT().Counts(mock.Anything, mock.Anything).Return(ports.OwnerCounts{Attention: 1, Work: 2}, nil).Once()

	rec := inboxGet(srv, workPath, true)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, "<title>Work · ")
	assert.Contains(t, body, `<div id="view" data-view="work"><h1 class="sr" tabindex="-1">Work</h1>`)
	assert.Contains(t, body, `href="/-/work" data-nav data-key="g w" aria-label="Work (2)" aria-current="page"`)
	assert.Contains(t, body, `href="/alpha/merge-requests/4"`)
	assert.Contains(t, body, `href="/beta/issues/7"`)
	assert.Contains(t, body, `<tr data-cursor data-state="open">`, "the first row has the cursor")
	assert.NotContains(t, body, `class="crumbs"`)
	assert.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
}

func TestWorkPageIsMissingForEveryoneElse(t *testing.T) {
	t.Parallel()
	srv, _ := workServer(t, nil) // never asked
	missing := inboxGet(srv, "/-/nothing-here", false)
	anon := inboxGet(srv, workPath, false)
	assert.Equal(t, 404, anon.Code)
	assert.Equal(t, missing.Body.String(), anon.Body.String())
	assert.NotContains(t, anon.Body.String(), "owner-nav")
}

func TestWorkReadsChecksOncePerHead(t *testing.T) {
	t.Parallel()
	head := fmt.Sprintf("%040x", 7)
	repo := repoName(t, "alpha")
	ov := inboxOverviewWithHead(t, head)
	second := ov.MergeRequests[0]
	second.Number = 5
	ov.MergeRequests = append(ov.MergeRequests, second)
	runs := mocks.NewRunService(t)
	runs.EXPECT().ListChecks(mock.Anything, mock.Anything, repo, head).Return(ports.ChecksResult{}, nil).Once()
	srv, svc := workServer(t, runs)
	svc.EXPECT().Work(mock.Anything, mock.Anything).Return(ports.InboxWork{MergeRequests: ov.MergeRequests, Issues: ov.Issues}, nil).Once()
	svc.EXPECT().Counts(mock.Anything, mock.Anything).Return(ports.OwnerCounts{}, nil).Once()

	require.Equal(t, 200, inboxGet(srv, workPath, true).Code)
}
