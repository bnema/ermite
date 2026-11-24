// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

// pushedRepo is a public repository that was pushed to.
func pushedRepo(t testing.TB, name string) ports.RepoView {
	t.Helper()
	v := publicRepo(t, name)
	v.PushedAt = time.Now().Add(-time.Hour)
	return v
}

// homeCommits are n commits, newest first, with distinct ids and subjects.
func homeCommits(t testing.TB, n int) []domain.Commit {
	t.Helper()
	var out []domain.Commit
	for i := n; i >= 1; i-- {
		out = append(out, domain.Commit{
			ID:        objectID(t, fmt.Sprintf("%040x", i)),
			Summary:   fmt.Sprintf("subject number %d", i),
			Committer: domain.Signature{Time: time.Now().Add(-time.Duration(i) * time.Minute)},
		})
	}
	return out
}

// homeBrowse is a BrowseService whose figures are always there: 1234
// commits, 7 of them recent, no tag. Tests that care set their own.
func homeBrowse(t *testing.T) *mocks.BrowseService {
	t.Helper()
	b := mocks.NewBrowseService(t)
	b.EXPECT().GetTree(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, false).Return(domain.Tree{}, nil).Maybe()
	b.EXPECT().RepoStats(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(ports.RepoStats{Commits: 1234, Recent: 7}, nil).Maybe()
	return b
}

// Each row shows the figures of its repository: commits, recent commits,
// latest tag and last activity; no branch name.
func TestHomeRowFigures(t *testing.T) {
	t.Parallel()
	repo := repoName(t, "proj")
	head := objectID(t, fmt.Sprintf("%040x", 1))
	now := time.Now()
	activity := make([]int, ports.ActivityDays)
	activity[ports.ActivityDays-1] = 3
	browse := mocks.NewBrowseService(t)
	browse.EXPECT().GetTree(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, false).Return(domain.Tree{}, nil).Maybe()
	browse.EXPECT().ListCommits(mock.Anything, mock.Anything, repo, mock.Anything).Return(ports.HistoryPage{Commits: homeCommits(t, 1)}, nil)
	browse.EXPECT().RepoStats(mock.Anything, mock.Anything, repo, head, now).Return(ports.RepoStats{
		Commits: 12345, Recent: 42, LatestTag: &domain.TagInfo{Name: "v1.2.0", At: now.Add(-3 * time.Hour), CommitID: head},
		Activity: activity,
	}, nil).Once()
	srv := newTestServer(t, Config{Repos: reposServing(t, pushedRepo(t, "proj")), Browse: browse})

	view := pushedRepo(t, "proj")
	view.Topics = []string{"go", "git"}
	row := srv.homeRowOf(context.Background(), view, now, false)
	require.NotNil(t, row.Stats)
	out := renderRow(t, row)
	assert.Contains(t, out, `<li data-tip="12`+"\u202f"+`345 commits on the default branch"><span class="ico i-git-commit" aria-hidden="true"></span><b>12`+"\u202f"+`345</b><span class="sr">commits on the default branch</span></li>`)
	assert.Regexp(t, `<li class="repo-month"><figure class="repo-pulse".*</figure><b>42</b> in 30 days</li>`, out)
	assert.Contains(t, out, `<li class="repo-tag"><span class="ico i-tag" aria-hidden="true"></span> <a href="/proj/tree/`+head.String()+`" title="Latest tag v1.2.0, made 3h ago">v1.2.0</a></li>`, "by its commit: a branch may share its name")
	assert.Contains(t, out, `<span class="repo-when tip-end" data-tip="Updated `+row.ActiveAt+`">`)
	assert.NotContains(t, out, "i-branch", "the default branch is not shown")
	assert.Contains(t, out, `<ul class="repo-topics" data-fit aria-label="Topics of proj"><li>go</li><li>git</li></ul>`)
	assert.Contains(t, out, `role="img" aria-label="Activity: 3 commits in the last 90 days"`)
	assert.Contains(t, out, `<linearGradient id="hb-proj"`)
	assert.NotContains(t, out, "pulse-ring", "nothing moves")
	assert.NotContains(t, out, "90 days ago")
	assert.NotContains(t, out, "pushed")
	// The tag comes first in the facts, before the commits.
	assert.Regexp(t, `class="repo-tag".*i-git-commit`, out)

	// Without topics nor figures, the row leaves them out.
	bare := renderRow(t, HomeRow{ID: "repo-bare", Name: "bare", Href: "/bare"})
	assert.NotContains(t, bare, "repo-topics")
	assert.NotContains(t, bare, "repo-pulse")
	assert.NotContains(t, bare, "repo-tag")
	assert.NotContains(t, bare, "repo-meta", "no empty facts list")

	// Past the cap of the walk, the figures are lower bounds; without a
	// curve, the recent count stands alone.
	browse.EXPECT().RepoStats(mock.Anything, mock.Anything, repo, head, now).Return(ports.RepoStats{
		Commits: 300000, Recent: 100000, Partial: true,
	}, nil).Once()
	out = renderRow(t, srv.homeRowOf(context.Background(), pushedRepo(t, "proj"), now, false))
	assert.Contains(t, out, "<li><b>100\u202f000+</b> in 30 days</li>")
	assert.NotContains(t, out, "repo-pulse")

	// Activity only around day 40 ago: not live, but the curve is shown.
	older := make([]int, ports.ActivityDays)
	older[ports.ActivityDays-40] = 4
	browse.EXPECT().RepoStats(mock.Anything, mock.Anything, repo, head, now).Return(ports.RepoStats{
		Commits: 4, Activity: older,
	}, nil).Once()
	out = renderRow(t, srv.homeRowOf(context.Background(), pushedRepo(t, "proj"), now, false))
	assert.Regexp(t, `<li class="repo-month"><figure class="repo-pulse" data-days-commits=.*</figure><b>0</b> in 30 days</li>`, out)
	assert.NotContains(t, out, "data-live")
	assert.NotContains(t, out, "quiet")

	// A quiet repository says so instead of drawing a flat pulse.
	browse.EXPECT().RepoStats(mock.Anything, mock.Anything, repo, head, now).Return(ports.RepoStats{
		Commits: 3, Activity: make([]int, ports.ActivityDays),
	}, nil).Once()
	out = renderRow(t, srv.homeRowOf(context.Background(), pushedRepo(t, "proj"), now, false))
	assert.Contains(t, out, `<li class="repo-quiet">quiet for 90 days</li>`)
	assert.NotContains(t, out, "repo-pulse")
	assert.NotContains(t, out, "in 30 days")

	// A failed read leaves the figures out, not the row.
	browse.EXPECT().RepoStats(mock.Anything, mock.Anything, repo, head, now).Return(ports.RepoStats{}, errors.New("boom")).Once()
	row = srv.homeRowOf(context.Background(), pushedRepo(t, "proj"), now, false)
	assert.Nil(t, row.Stats)
	out = renderRow(t, row)
	assert.Contains(t, out, `id="repo-proj"`, "the row is rendered")
	assert.NotContains(t, out, "i-git-commit")
}

func TestGrouped(t *testing.T) {
	t.Parallel()
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1\u202f000", 1234567: "1\u202f234\u202f567"} {
		assert.Equal(t, want, grouped(n), n)
	}
}

func renderRow(t testing.TB, r HomeRow) string {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, HomeRowView(r).Render(context.Background(), &b))
	return b.String()
}

func mustRevision(t testing.TB, s string) domain.Revision {
	t.Helper()
	r, err := domain.ParseRevision(s)
	require.NoError(t, err)
	return r
}

// Each row reads the head of the default branch by its full name, and the
// figures of that commit; a server without CI shows no state.
func TestHomeRowReadsTheDefaultHead(t *testing.T) {
	t.Parallel()
	head := objectID(t, fmt.Sprintf("%040x", 7))
	browse := homeBrowse(t)
	browse.EXPECT().ListCommits(mock.Anything, mock.Anything, repoName(t, "proj"), ports.ListCommitsParams{
		Revision: mustRevision(t, "refs/heads/main"), PageSize: 1,
	}).Return(ports.HistoryPage{Commits: homeCommits(t, 7)[:1]}, nil).Once()
	srv := newTestServer(t, Config{Repos: reposServing(t, pushedRepo(t, "proj")), Browse: browse})

	row := srv.homeRowOf(context.Background(), pushedRepo(t, "proj"), time.Now(), false)
	require.NotNil(t, row.Stats)
	assert.Nil(t, row.CI, "a server without CI shows no state")
	browse.AssertCalled(t, "RepoStats", mock.Anything, mock.Anything, repoName(t, "proj"), head, mock.Anything)

	out := renderRow(t, row)
	assert.NotContains(t, out, `class="repo-ci"`)
}

func TestHomeRowEmptyRepository(t *testing.T) {
	t.Parallel()
	// Never pushed: nothing is read, though the creation date is known.
	view := publicRepo(t, "new")
	view.CreatedAt = time.Now().Add(-48 * time.Hour)
	srv := newTestServer(t, Config{Repos: reposServing(t, view), Browse: mocks.NewBrowseService(t), Runs: mocks.NewRunService(t)})
	row := srv.homeRowOf(context.Background(), view, time.Now(), false)
	assert.True(t, row.Empty)
	assert.Nil(t, row.Stats)
	out := renderRow(t, row)
	assert.Contains(t, out, `<div class="repo-state"><span>No commits yet</span> </div>`)
	assert.NotContains(t, out, "repo-when")
	assert.NotContains(t, out, "i-git-commit", "no figures without a commit")

	// Pushed, but the default branch is missing or empty.
	for name, res := range map[string]struct {
		page ports.HistoryPage
		err  error
	}{"missing": {err: domain.ErrNotFound}, "empty": {}} {
		browse := mocks.NewBrowseService(t)
		browse.EXPECT().ListCommits(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(res.page, res.err).Once()
		srv := newTestServer(t, Config{Repos: reposServing(t, pushedRepo(t, "new")), Browse: browse})
		row := srv.homeRowOf(context.Background(), pushedRepo(t, "new"), time.Now(), false)
		assert.True(t, row.Empty, name)
		out := renderRow(t, row)
		assert.Contains(t, out, `<span>No commits yet</span>`, name)
		assert.NotContains(t, out, "repo-when", name)
	}
}

func TestHomeRowCI(t *testing.T) {
	t.Parallel()
	repo := repoName(t, "proj")
	head := fmt.Sprintf("%040x", 2) // the newest of homeCommits(t, 2)
	check := func(run int64, status domain.CIStatus, c domain.CIConclusion) ports.CheckView {
		return ports.CheckView{CICheck: domain.CICheck{Repo: repo, Sha: head, Name: "ci / test", RunID: run, Status: status, Conclusion: c}}
	}
	tests := []struct {
		name   string
		checks []ports.CheckView
		err    error
		label  string // "" when no state is shown
		href   string
	}{
		{"passing", []ports.CheckView{check(9, domain.CICompleted, domain.CISuccess)}, nil, "passing", "/proj/actions/runs/9"},
		{"failing", []ports.CheckView{check(9, domain.CICompleted, domain.CISuccess), check(9, domain.CICompleted, domain.CIFailure)}, nil, "failing", "/proj/actions/runs/9"},
		{"running", []ports.CheckView{check(9, domain.CIInProgress, "")}, nil, "running", "/proj/actions/runs/9"},
		{"several runs link the commit", []ports.CheckView{check(9, domain.CICompleted, domain.CISuccess), check(10, domain.CICompleted, domain.CISuccess)}, nil, "passing", "/proj/commit/" + head},
		{"no check on the head", nil, nil, "", ""},
		{"the viewer cannot read CI", nil, domain.ErrNotFound, "", ""},
		{"the read fails", nil, errors.New("boom"), "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			browse := homeBrowse(t)
			browse.EXPECT().ListCommits(mock.Anything, mock.Anything, repo, mock.Anything).Return(ports.HistoryPage{Commits: homeCommits(t, 2)}, nil)
			runs := mocks.NewRunService(t)
			runs.EXPECT().ListChecks(mock.Anything, mock.Anything, repo, head).
				Return(ports.ChecksResult{Checks: tt.checks}, tt.err).Once()
			srv := newTestServer(t, Config{Repos: reposServing(t, pushedRepo(t, "proj")), Browse: browse, Runs: runs})

			row := srv.homeRowOf(context.Background(), pushedRepo(t, "proj"), time.Now(), false)
			require.NotNil(t, row.Stats, "the figures do not depend on CI")
			out := renderRow(t, row)
			if tt.label == "" {
				assert.Nil(t, row.CI)
				assert.NotContains(t, out, "repo-ci")
				return
			}
			require.NotNil(t, row.CI)
			assert.Contains(t, out, `class="repo-ci" href="`+tt.href+`"`)
			assert.Contains(t, out, `<span class="sr">CI: `+tt.label+`</span>`)
			assert.NotContains(t, out, `class="ci-label"`)
			assert.Contains(t, out, `title="`+html.EscapeString(tt.label+" · "+row.CI.Title)+`"`)
			assert.Contains(t, out, `class="ck ck-`+row.CI.Kind+`" aria-hidden="true"`)
		})
	}
}

// The private badge is for the signed-in owner only.
func TestHomeRowPrivateBadge(t *testing.T) {
	t.Parallel()
	priv := pushedRepo(t, "secret")
	priv.Visibility = domain.VisibilityPrivate
	browse := homeBrowse(t)
	browse.EXPECT().ListCommits(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.HistoryPage{}, nil).Maybe()
	srv := newTestServer(t, Config{Repos: reposServing(t, priv, pushedRepo(t, "open")), Browse: browse})

	signed := context.WithValue(context.Background(), signedInKey{}, struct{}{})
	assert.Contains(t, renderRow(t, srv.homeRowOf(signed, priv, time.Now(), false)), `<span class="repo-flag">Private</span>`)
	assert.NotContains(t, renderRow(t, srv.homeRowOf(context.Background(), priv, time.Now(), false)), "Private")
	assert.NotContains(t, renderRow(t, srv.homeRowOf(signed, pushedRepo(t, "open"), time.Now(), false)), "Private")
}

// A run update re-renders the home row, so its CI state follows the run.
func TestRunUpdatedRefreshesTheHomeRow(t *testing.T) {
	t.Parallel()
	repo := repoName(t, "proj")
	head := fmt.Sprintf("%040x", 2)
	browse := homeBrowse(t)
	browse.EXPECT().ListCommits(mock.Anything, mock.Anything, repo, mock.Anything).Return(ports.HistoryPage{Commits: homeCommits(t, 2)}, nil)
	runs := mocks.NewRunService(t)
	runs.EXPECT().ListChecks(mock.Anything, mock.Anything, repo, head).Return(ports.ChecksResult{Checks: []ports.CheckView{
		{CICheck: domain.CICheck{Repo: repo, Sha: head, RunID: 9, Status: domain.CICompleted, Conclusion: domain.CISuccess}},
	}}, nil).Once()
	srv := newTestServer(t, Config{Repos: reposServing(t, pushedRepo(t, "proj")), Browse: browse, Runs: runs})

	got, err := (&liveStream{s: srv}).render(t.Context(), ports.WatchEvent{Event: domain.RunUpdated{Run: domain.CIRun{Repo: repo, ID: 9}}})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "home-list", got[0].event)
	assert.Contains(t, got[0].html, `id="repo-proj"`)
	assert.Contains(t, got[0].html, "passing")
	assert.NotContains(t, got[0].html, "new activity", "a run is not a push")

	// A job that ended refreshes the row too; one in progress does not.
	got, err = (&liveStream{s: srv}).render(t.Context(), ports.WatchEvent{Event: domain.JobUpdated{Job: domain.CIJob{Repo: repo, Phase: domain.CIPhaseRunning}}})
	require.NoError(t, err)
	assert.Empty(t, got)
	runs.EXPECT().ListChecks(mock.Anything, mock.Anything, repo, head).Return(ports.ChecksResult{Checks: []ports.CheckView{
		{CICheck: domain.CICheck{Repo: repo, Sha: head, RunID: 9, Status: domain.CICompleted, Conclusion: domain.CIFailure}},
		{CICheck: domain.CICheck{Repo: repo, Sha: head, RunID: 9, Status: domain.CIInProgress}},
	}}, nil).Once()
	got, err = (&liveStream{s: srv}).render(t.Context(), ports.WatchEvent{Event: domain.JobUpdated{Job: domain.CIJob{Repo: repo, Phase: domain.CIPhaseEnded}}})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Contains(t, got[0].html, "failing", "a failed job shows before its run ends")
}

// A repository whose commits cannot be read keeps its row, and the page is
// served; the others are not affected.
func TestHomeSurvivesABrokenRepository(t *testing.T) {
	t.Parallel()
	browse := homeBrowse(t)
	browse.EXPECT().ListCommits(mock.Anything, mock.Anything, repoName(t, "broken"), mock.Anything).
		Return(ports.HistoryPage{}, errors.New("git exploded")).Once()
	browse.EXPECT().ListCommits(mock.Anything, mock.Anything, repoName(t, "fine"), mock.Anything).
		Return(ports.HistoryPage{Commits: homeCommits(t, 3)}, nil).Once()
	srv := newTestServer(t, Config{Repos: reposServing(t, pushedRepo(t, "broken"), pushedRepo(t, "fine")), Browse: browse})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?sort=name", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `id="repo-broken"`)
	assert.Contains(t, body, `id="repo-fine"`)
	assert.Equal(t, 1, strings.Count(body, "i-git-commit"), "only the readable repository shows its figures")
	assert.NotContains(t, body, "No commits yet", "a failed read is not an empty branch")
}

// The rows are read a few at a time, in the order of the list.
func TestHomeRowsAreBoundedAndOrdered(t *testing.T) {
	t.Parallel()
	const n = 40
	var views []ports.RepoView
	for i := range n {
		views = append(views, pushedRepo(t, fmt.Sprintf("r%02d", i)))
	}
	var running, peak atomic.Int32
	browse := homeBrowse(t)
	browse.EXPECT().ListCommits(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(context.Context, domain.Principal, domain.RepoName, ports.ListCommitsParams) (ports.HistoryPage, error) {
			cur := running.Add(1)
			for p := peak.Load(); cur > p && !peak.CompareAndSwap(p, cur); p = peak.Load() {
			}
			time.Sleep(2 * time.Millisecond)
			running.Add(-1)
			return ports.HistoryPage{Commits: homeCommits(t, 1)}, nil
		})
	srv := newTestServer(t, Config{Repos: reposServing(t, views...), Browse: browse})

	rows := srv.homeRows(context.Background(), views, time.Now())
	require.Len(t, rows, n)
	for i, r := range rows {
		assert.Equal(t, fmt.Sprintf("r%02d", i), r.Name)
		assert.NotNil(t, r.Stats)
	}
	assert.True(t, rows[0].Cursor)
	assert.LessOrEqual(t, int(peak.Load()), homeWorkers)
	assert.Greater(t, int(peak.Load()), 1, "the reads overlap")
}

// The store loads before the app script, which reads it.
func TestHomeLoadsTheStoreFirst(t *testing.T) {
	t.Parallel()
	browse := homeBrowse(t)
	browse.EXPECT().ListCommits(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.HistoryPage{Commits: homeCommits(t, 2)}, nil)
	srv := newTestServer(t, Config{Repos: reposServing(t, pushedRepo(t, "proj")), Browse: browse})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "data-layout")
	assert.NotContains(t, body, "layout-switch")
	store := strings.Index(body, "/js/store.")
	app := strings.Index(body, "/js/ermite.")
	require.NotEqual(t, -1, store)
	assert.Less(t, store, app, "the store loads before the app script")
}

func TestHomeActivityOrderAndFallback(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	a, b, c := publicRepo(t, "a"), publicRepo(t, "b"), publicRepo(t, "c")
	a.PushedAt = now.Add(-time.Hour)
	a.ActiveAt = now
	b.PushedAt = now.Add(-time.Minute)
	c.CreatedAt = now.Add(-30 * time.Minute)
	repos := []ports.RepoView{b, c, a}
	sortRepos(repos, "activity", true)
	assert.Equal(t, []ports.RepoView{a, b, c}, repos)
	sortRepos(repos, "activity", false)
	assert.Equal(t, []ports.RepoView{c, b, a}, repos)
	assert.Equal(t, rfc3339(now), homeRow(a, now, false, false).ActiveAt)
	c.PushedAt = c.CreatedAt.Add(-time.Hour)
	assert.Equal(t, c.CreatedAt, repoActiveAt(c), "fallback is the later of creation and push")
}

func TestActivityEventsRefreshTheHomeRow(t *testing.T) {
	t.Parallel()
	repo := publicRepo(t, "proj")
	srv := newTestServer(t, Config{Repos: reposServing(t, repo), Browse: homeBrowse(t)})
	mr := domain.MergeRequest{Repo: repo.Name}
	issue := domain.Issue{Repo: repo.Name}
	for _, ev := range []domain.Event{
		domain.PushEvent{Repo: repo.Name},
		domain.MergeRequestEdited{MergeRequest: mr}, domain.IssueEdited{Issue: issue},
		domain.MergeRequestOpened{MergeRequest: mr}, domain.MergeRequestMerged{MergeRequest: mr},
		domain.MergeRequestClosed{MergeRequest: mr}, domain.MergeRequestReady{MergeRequest: mr},
		domain.CommentAdded{Repo: repo.Name}, domain.IssueOpened{Issue: issue},
		domain.IssueResolved{Issue: issue}, domain.IssueClosed{Issue: issue}, domain.IssueReopened{Issue: issue}, domain.IssueCommented{Repo: repo.Name},
	} {
		t.Run(ev.EventKind(), func(t *testing.T) {
			t.Parallel()
			got, err := (&liveStream{s: srv}).home(t.Context(), ports.WatchEvent{Event: ev})
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.Equal(t, "home-list", got[0].event)
			assert.Contains(t, got[0].html, `id="repo-proj"`)
			assert.Contains(t, got[0].html, `data-activity`)
			assert.Contains(t, got[0].html, "new activity")
		})
	}
	for _, ev := range []domain.Event{domain.IssueSubmitted{Repo: repo.Name}, domain.IssuePendingRemoved{Repo: repo.Name}} {
		got, err := (&liveStream{s: srv}).home(t.Context(), ports.WatchEvent{Event: ev})
		require.NoError(t, err)
		assert.Empty(t, got)
	}
}

// The latest tag shows in the facts even when the latest release already
// names it as a badge.
func TestHomeTagAndRelease(t *testing.T) {
	t.Parallel()
	for _, release := range []*LatestRelease{nil, {Tag: "v1.1.0", Href: "/proj/releases/tag/v1.1.0"}, {Tag: "v1.2.0", Href: "/proj/releases/tag/v1.2.0"}} {
		out := renderRow(t, HomeRow{Name: "proj", Release: release, Stats: &HomeStats{Tag: "v1.2.0", TagWhen: "3h ago", Commits: "1", Recent: "0"}})
		assert.Contains(t, out, `<li class="repo-tag"><span class="ico i-tag" aria-hidden="true"></span> <span title="Latest tag v1.2.0, made 3h ago">v1.2.0</span></li>`)
		if release != nil {
			assert.Contains(t, out, `<a class="repo-flag repo-release" href="`+release.Href+`" title="Latest release: `+release.Tag+`">`+release.Tag+`</a>`)
		} else {
			assert.NotContains(t, out, "repo-release")
		}
	}
}

func TestHomeReleaseAndLicensePills(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		release domain.Release
		err     error
		visible bool
	}{
		{name: "official release", release: domain.Release{Tag: "v1.2.0", PublishedAt: fixedNow}, visible: true},
		{name: "plain tag only", err: domain.ErrNotFound},
		{name: "draft", release: domain.Release{Tag: "secret", Draft: true}},
		{name: "unpublished", release: domain.Release{Tag: "pending"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := publicRepo(t, "proj")
			repo.Visibility = domain.VisibilityPrivate
			svc := mocks.NewReleaseService(t)
			svc.EXPECT().GetRelease(mock.Anything, mock.Anything, repo.Name, ports.ReleaseRef{Latest: true}).Return(tc.release, tc.err).Once()
			srv := newTestServer(t, Config{Repos: reposServing(t, repo), Browse: mocks.NewBrowseService(t), Releases: svc})
			row := srv.homeRowOf(t.Context(), repo, fixedNow, false)
			row.Private, row.Fresh = true, true
			row.License = &LicenseBadge{ID: "MIT", Href: "/proj/blob/main/LICENSE"}
			out := renderRow(t, row)
			assert.Contains(t, out, `<li><a href="/proj/blob/main/LICENSE" title="License: MIT">MIT</a></li>`)
			assert.Regexp(t, `(?s)<ul class="repo-meta".*title="License: MIT"`, out)
			assert.Regexp(t, `(?s)<span class="repo-flag">Private</span> <span class="repo-activity" title="new activity"><span class="ico i-refresh-cw" aria-hidden="true"></span><span class="sr">new activity</span></span></h2>`, out)
			if tc.visible {
				assert.Regexp(t, `(?s)>proj</a>.*class="repo-flag repo-release".*>v1.2.0</a>.*>Private</span>`, out)
				assert.Contains(t, out, `href="/proj/releases/tag/v1.2.0"`)
			} else {
				assert.NotContains(t, out, `repo-release`)
			}
		})
	}
	out := renderRow(t, HomeRow{Name: "bare"})
	assert.NotContains(t, out, `License:`)
	assert.NotContains(t, out, `repo-release`)
}

func TestReleaseEventsRefreshTheHomeRow(t *testing.T) {
	t.Parallel()
	repo := publicRepo(t, "proj")
	rel := domain.Release{Repo: repo.Name, Tag: "v2", PublishedAt: fixedNow}
	for _, tc := range []struct {
		event  domain.Event
		latest domain.Release
		err    error
	}{
		{domain.ReleasePublished{Release: rel}, rel, nil},
		{domain.ReleaseUpdated{Release: rel}, domain.Release{Tag: "v1", PublishedAt: fixedNow}, nil},
		{domain.ReleaseDeleted{Release: rel}, domain.Release{}, domain.ErrNotFound},
		{domain.ReleaseUnpublished{Release: rel}, domain.Release{}, domain.ErrNotFound},
	} {
		t.Run(tc.event.EventKind(), func(t *testing.T) {
			t.Parallel()
			releases := mocks.NewReleaseService(t)
			releases.EXPECT().GetRelease(mock.Anything, mock.Anything, repo.Name, ports.ReleaseRef{Latest: true}).Return(tc.latest, tc.err).Once()
			srv := newTestServer(t, Config{Repos: reposServing(t, repo), Browse: homeBrowse(t), Releases: releases})
			got, err := (&liveStream{s: srv}).home(t.Context(), ports.WatchEvent{Event: tc.event})
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.Equal(t, "home-list", got[0].event)
			assert.Contains(t, got[0].html, `id="repo-proj"`)
			assert.NotContains(t, got[0].html, `data-activity`)
			assert.NotContains(t, got[0].html, `new activity`)
			if tc.err != nil {
				assert.NotContains(t, got[0].html, `repo-release`)
			} else {
				assert.Contains(t, got[0].html, `href="/proj/releases/tag/`+tc.latest.Tag+`"`)
			}
		})
	}
}

// With traffic, a quiet row shows its visitors without a pulse.
func TestHomeShowsVisitorsWithoutPulse(t *testing.T) {
	t.Parallel()
	browse := mocks.NewBrowseService(t)
	browse.EXPECT().GetTree(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, false).Return(domain.Tree{}, nil).Maybe()
	browse.EXPECT().RepoStats(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(ports.RepoStats{Commits: 3, Activity: make([]int, ports.ActivityDays)}, nil)
	browse.EXPECT().ListCommits(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.HistoryPage{Commits: homeCommits(t, 2)}, nil)
	traffic := mocks.NewTrafficService(t)
	daily := make([]uint64, ports.ActivityDays)
	daily[89] = 5
	traffic.EXPECT().RepoDaily(mock.Anything, mock.Anything, ports.ActivityDays).Return(daily, nil).Maybe()
	srv := newTestServer(t, Config{Repos: reposServing(t, pushedRepo(t, "proj")), Browse: browse, Traffic: traffic})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "repo-pulse", "no commits in 90 days")
	assert.Contains(t, body, `<li class="tip-end" data-tip="Unique visitors, last 30 days"><span class="ico i-eye" aria-hidden="true"></span><b>5</b><span class="sr">unique visitors in the last 30 days</span></li>`)
}

func TestHomeVisitors(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"enabled", "disabled", "failed"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			browse := mocks.NewBrowseService(t)
			browse.EXPECT().GetTree(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, false).Return(domain.Tree{}, nil).Maybe()
			browse.EXPECT().ListCommits(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.HistoryPage{Commits: homeCommits(t, 1)}, nil)
			activity := make([]int, ports.ActivityDays)
			activity[ports.ActivityDays-1] = 1
			browse.EXPECT().RepoStats(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.RepoStats{Commits: 1, Recent: 1, Activity: activity}, nil)
			cfg := Config{Repos: reposServing(t, pushedRepo(t, "proj")), Browse: browse}
			if mode != "disabled" {
				f := mocks.NewTrafficService(t)
				daily := make([]uint64, ports.ActivityDays)
				daily[89] = 12
				var trafficErr error
				if mode == "failed" {
					trafficErr = errors.New("traffic unavailable")
				}
				f.EXPECT().RepoDaily(mock.Anything, pushedRepo(t, "proj").Name, ports.ActivityDays).Return(daily, trafficErr).Once()
				cfg.Traffic = f
			}
			srv := newTestServer(t, cfg)
			out := renderRow(t, srv.homeRowOf(t.Context(), pushedRepo(t, "proj"), fixedNow, false))
			if mode == "enabled" {
				assert.Contains(t, out, `aria-label="Activity: 1 commit, 12 visitors in the last 90 days"`)
				assert.Contains(t, out, `data-days-visitors=`)
				assert.Contains(t, out, `<li class="tip-end" data-tip="Unique visitors, last 30 days"><span class="ico i-eye" aria-hidden="true"></span><b>12</b><span class="sr">unique visitors in the last 30 days</span></li>`)
			} else {
				assert.NotContains(t, out, "visitors")
			}
		})
	}
}
