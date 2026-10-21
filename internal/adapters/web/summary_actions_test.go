// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

func testRuns(repo domain.RepoName, n int) []domain.CIRun {
	var runs []domain.CIRun
	for i := n; i >= 1; i-- {
		runs = append(runs, domain.CIRun{
			Repo: repo, ID: int64(i), Number: int64(i), Attempt: 1, WorkflowName: "ci", Ref: "refs/heads/main",
			Sha: strings.Repeat("a", 40), Status: domain.CICompleted, Conclusion: domain.CISuccess,
			CreatedAt: time.Now().Add(-time.Duration(i) * time.Minute),
		})
	}
	return runs
}

func servingRuns(t *testing.T, repo domain.RepoName, runs []domain.CIRun, err error) (*Server, *mocks.RunService) {
	t.Helper()
	m := mocks.NewRunService(t)
	m.EXPECT().ListRuns(mock.Anything, mock.Anything, repo, ports.ListRunsParams{PageSize: recentRuns}).
		Return(ports.RunPage{Runs: runs}, err).Maybe()
	return newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, repo.String())), Browse: mocks.NewBrowseService(t), Runs: m}), m
}

func renderRail(t *testing.T, p SummaryPage) string {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, summaryRail(p).Render(t.Context(), &b))
	return b.String()
}

func TestSummaryRailRecentActions(t *testing.T) {
	t.Parallel()
	repo := repoName(t, "proj")
	srv, _ := servingRuns(t, repo, testRuns(repo, 5), nil)

	p := SummaryPage{Actions: srv.recentActions(t.Context(), repo, time.Now())}
	require.NotNil(t, p.Actions)
	out := renderRail(t, p)
	assert.Contains(t, out, `id="recent-actions"`)
	assert.Contains(t, out, "Recent actions")
	assert.Contains(t, out, `href="/proj/actions"`, "view all")
	assert.Equal(t, 5, strings.Count(out, `class="rr-name"`))
	assert.Contains(t, out, `href="/proj/actions/runs/5"`)
	assert.Contains(t, out, `aria-label="success"`, "the status icon")
	assert.Contains(t, out, `href="/proj/tree/main"`, "the ref")
	assert.Contains(t, out, "#5")
	assert.NotContains(t, out, `id="recent-actions" aria-labelledby="r-actions" hidden`)
}

func TestSummaryRailRecentActionsHidden(t *testing.T) {
	t.Parallel()
	repo := repoName(t, "proj")

	// The viewer cannot read CI: no block at all, not even to receive events.
	srv, _ := servingRuns(t, repo, nil, domain.ErrNotFound)
	assert.Nil(t, srv.recentActions(t.Context(), repo, time.Now()))
	assert.NotContains(t, renderRail(t, SummaryPage{}), "recent-actions")

	// A failure of the read does not break the page.
	srv, _ = servingRuns(t, repo, nil, errors.New("boom"))
	assert.Nil(t, srv.recentActions(t.Context(), repo, time.Now()))

	// No CI on this server.
	noCI := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "proj")), Browse: mocks.NewBrowseService(t)})
	assert.Nil(t, noCI.recentActions(t.Context(), repo, time.Now()))

	// No run yet: the block is there for the first run, but hidden.
	srv, _ = servingRuns(t, repo, nil, nil)
	a := srv.recentActions(t.Context(), repo, time.Now())
	require.NotNil(t, a)
	assert.Contains(t, renderRail(t, SummaryPage{Actions: a}), `id="recent-actions" aria-labelledby="r-actions" hidden`)
}

func TestRunUpdatedReplacesRecentActionsOnSummary(t *testing.T) {
	t.Parallel()
	repo := repoName(t, "proj")
	runs := testRuns(repo, 3)
	runs[0].Status, runs[0].Conclusion = domain.CIInProgress, ""
	srv, _ := servingRuns(t, repo, runs, nil)

	ev := ports.WatchEvent{Event: domain.RunUpdated{Run: runs[0]}}
	ls := &liveStream{s: srv, repo: "proj", view: "summary"}
	got, err := ls.render(t.Context(), ev)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "ci-run", got[0].event)
	assert.True(t, strings.HasPrefix(got[0].html, `<section id="recent-actions"`), got[0].html)
	assert.Equal(t, 3, strings.Count(got[0].html, `class="rr-name"`))
	assert.Contains(t, got[0].html, `aria-label="running"`)

	// The other pages do not get the block.
	for _, view := range []string{"refs", "commits", "mrs"} {
		got, err = (&liveStream{s: srv, repo: "proj", view: view}).render(t.Context(), ev)
		require.NoError(t, err, view)
		assert.Empty(t, got, view)
	}
}

func TestRunUpdatedOnSummaryWhenCIIsNotReadable(t *testing.T) {
	t.Parallel()
	repo := repoName(t, "proj")
	srv, _ := servingRuns(t, repo, nil, domain.ErrNotFound)

	ls := &liveStream{s: srv, repo: "proj", view: "summary"}
	got, err := ls.render(t.Context(), ports.WatchEvent{Event: domain.RunUpdated{Run: testRuns(repo, 1)[0]}})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Contains(t, got[0].html, "hidden", "the block empties and hides, no run leaks")
	assert.NotContains(t, got[0].html, "rr-name")
}

func TestMRChecksLiveMergeControls(t *testing.T) {
	t.Parallel()
	repo := repoName(t, "proj")
	head := strings.Repeat("a", domain.ObjectIDLen)
	for _, kind := range []string{"bad", "running", "ok", ""} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			runs := mocks.NewRunService(t)
			var checks []ports.CheckView
			if kind != "" {
				check := domain.CICheck{Name: "ci", Status: domain.CICompleted, Conclusion: domain.CISuccess}
				if kind == "bad" {
					check.Conclusion = domain.CIFailure
				}
				if kind == "running" {
					check.Status, check.Conclusion = domain.CIQueued, ""
				}
				checks = []ports.CheckView{{CICheck: check}}
			}
			runs.EXPECT().ListChecks(mock.Anything, mock.Anything, repo, head).Return(ports.ChecksResult{Checks: checks}, nil)
			srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "proj")), Browse: mocks.NewBrowseService(t), Runs: runs})

			// A visitor's stream never carries the owner's merge form.
			visitor, err := (&liveStream{s: srv, repo: "proj", head: head, view: "mr"}).mrChecks(t.Context())
			require.NoError(t, err)
			require.Len(t, visitor, 2)
			for _, f := range visitor {
				assert.NotContains(t, f.html, `mr-merge-checks`)
			}

			owner := context.WithValue(t.Context(), signedInKey{}, struct{}{})
			ls := &liveStream{s: srv, repo: "proj", head: head, view: "mr"}
			got, err := ls.mrChecks(owner)
			require.NoError(t, err)
			require.Len(t, got, 3)
			assert.Contains(t, got[2].html, `data-keep-checks`, "a swap keeps the ticked box")
			assert.Equal(t, "ci-run", got[2].event)
			assert.Contains(t, got[2].html, `id="mr-merge-checks"`)
			assert.Equal(t, renderToString(t, MRMergeChecks(checksSummary(repo, head, checks))), got[2].html)
			if kind == "bad" || kind == "running" {
				assert.Contains(t, got[2].html, `name="ignore_checks"`)
			} else {
				assert.NotContains(t, got[2].html, `name="ignore_checks"`)
			}
			// An unrelated job event with identical checks must not reset the checkbox.
			got, err = ls.mrChecks(owner)
			require.NoError(t, err)
			require.Len(t, got, 2)
		})
	}
}
