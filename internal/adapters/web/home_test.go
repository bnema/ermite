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
	head := objectID(t, fmt.Sprintf("%040x", 2))
	now := time.Now()
	browse := mocks.NewBrowseService(t)
	browse.EXPECT().GetTree(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, false).Return(domain.Tree{}, nil).Maybe()
	browse.EXPECT().ListCommits(mock.Anything, mock.Anything, repo, mock.Anything).Return(ports.HistoryPage{Commits: homeCommits(t, 2)}, nil)
	browse.EXPECT().RepoStats(mock.Anything, mock.Anything, repo, head, now).Return(ports.RepoStats{
		Commits: 12345, Recent: 42, LatestTag: &domain.TagInfo{Name: "v1.2.0", At: now.Add(-3 * time.Hour), CommitID: head},
		Activity: make([]int, ports.ActivityDays),
	}, nil).Once()
	srv := newTestServer(t, Config{Repos: reposServing(t, pushedRepo(t, "proj")), Browse: browse})

	view := pushedRepo(t, "proj")
	view.Topics = []string{"go", "git"}
	row := srv.homeRowOf(context.Background(), view, now, false)
	require.NotNil(t, row.Stats)
	out := renderRow(t, row)
	assert.Contains(t, out, "<dt>commits</dt><dd>12\u202f345</dd>")
	assert.Contains(t, out, "<dt>last 30 days</dt><dd>42</dd>")
	assert.Contains(t, out, `<a href="/proj/tree/`+head.String()+`" title="v1.2.0 · made 3h ago">v1.2.0</a>`, "by its commit: a branch may share its name")
	assert.Contains(t, out, "<dt>last activity</dt>")
	assert.NotContains(t, out, "i-branch", "the default branch is not shown")
	assert.Contains(t, out, `<ul class="repo-topics" aria-label="Topics of proj" data-fit><li>go</li><li>git</li></ul>`)
	assert.Contains(t, out, `role="img" aria-label="Activity: 0 commits in the last 90 days"`)
	assert.Contains(t, out, `<linearGradient id="hb-proj"`)
	assert.NotContains(t, out, "pulse-ring", "nothing moves")
	assert.NotContains(t, out, "90 days ago")
	assert.NotContains(t, out, "pushed")
	// The tag comes last, after the last activity.
	assert.Regexp(t, `<dt>last activity</dt>.*<dt>last tag</dt>`, out)

	// Without topics nor figures, the row keeps their room: every card has the
	// same height.
	bare := renderRow(t, HomeRow{ID: "repo-bare", Name: "bare", Href: "/bare"})
	assert.Contains(t, bare, `<div class="repo-topics" aria-hidden="true"></div>`)
	assert.Contains(t, bare, `<div class="repo-pulse" aria-hidden="true"></div>`)
	assert.NotContains(t, bare, "last tag")

	// Past the cap of the walk, the figures are lower bounds.
	browse.EXPECT().RepoStats(mock.Anything, mock.Anything, repo, head, now).Return(ports.RepoStats{
		Commits: 300000, Recent: 100000, Activity: make([]int, ports.ActivityDays), Partial: true,
	}, nil).Once()
	out = renderRow(t, srv.homeRowOf(context.Background(), pushedRepo(t, "proj"), now, false))
	assert.Contains(t, out, "<dt>last 30 days</dt><dd>100\u202f000+</dd>")
	assert.Contains(t, out, `aria-label="Activity: at least 0 commits in the last 90 days"`)

	// A failed read leaves the figures out, not the row.
	browse.EXPECT().RepoStats(mock.Anything, mock.Anything, repo, head, now).Return(ports.RepoStats{}, errors.New("boom")).Once()
	row = srv.homeRowOf(context.Background(), pushedRepo(t, "proj"), now, false)
	assert.Nil(t, row.Stats)
	assert.Len(t, row.Commits, 2)
	assert.NotContains(t, renderRow(t, row), "<dt>commits</dt>")
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

// Each row lists at most five commits of the default branch, read by the full
// name of the branch.
func TestHomeRowShowsFiveCommits(t *testing.T) {
	t.Parallel()
	browse := homeBrowse(t)
	browse.EXPECT().ListCommits(mock.Anything, mock.Anything, repoName(t, "proj"), ports.ListCommitsParams{
		Revision: mustRevision(t, "refs/heads/main"), PageSize: homeRecent,
	}).Return(ports.HistoryPage{Commits: homeCommits(t, 7)}, nil).Once()
	srv := newTestServer(t, Config{Repos: reposServing(t, pushedRepo(t, "proj")), Browse: browse})

	row := srv.homeRowOf(context.Background(), pushedRepo(t, "proj"), time.Now(), false)
	require.Len(t, row.Commits, homeRecent)
	assert.Equal(t, "subject number 7", row.Commits[0].Subject)
	assert.False(t, row.NoCommits)
	assert.Nil(t, row.CI, "a server without CI shows no state")

	out := renderRow(t, row)
	assert.Equal(t, homeRecent, strings.Count(out, `class="rc-sha"`))
	assert.Contains(t, out, `class="repo-commits" data-full`, "a full list fades its last commit")
	assert.Contains(t, out, `href="/proj/commit/`+fmt.Sprintf("%040x", 7)+`"`)
	assert.Contains(t, out, `aria-label="Recent commits of proj"`)
	assert.NotContains(t, out, `class="repo-ci"`)
}

func TestHomeRowEmptyRepository(t *testing.T) {
	t.Parallel()
	// Never pushed: nothing is read.
	srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "new")), Browse: mocks.NewBrowseService(t), Runs: mocks.NewRunService(t)})
	row := srv.homeRowOf(context.Background(), publicRepo(t, "new"), time.Now(), false)
	assert.True(t, row.NoCommits)
	out := renderRow(t, row)
	assert.Contains(t, out, "no commits yet")
	assert.Contains(t, out, `<dt>last activity</dt><dd class="mute">never</dd>`)
	assert.NotContains(t, out, "<dt>commits</dt>", "no figures without a commit")
	assert.NotContains(t, out, "repo-commits\" aria-label")
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
			require.Len(t, row.Commits, 2, "the commits do not depend on CI")
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
	assert.Contains(t, renderRow(t, srv.homeRowOf(signed, priv, time.Now(), false)), `<span class="pill">private</span>`)
	assert.NotContains(t, renderRow(t, srv.homeRowOf(context.Background(), priv, time.Now(), false)), "private")
	assert.NotContains(t, renderRow(t, srv.homeRowOf(signed, pushedRepo(t, "open"), time.Now(), false)), "private")
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
	assert.Equal(t, 3, strings.Count(body, `class="rc-sha"`), "only the readable repository lists commits")
	assert.NotContains(t, body, "no commits yet", "a failed read is not an empty branch")
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
		assert.Len(t, r.Commits, 1)
	}
	assert.True(t, rows[0].Cursor)
	assert.LessOrEqual(t, int(peak.Load()), homeWorkers)
	assert.Greater(t, int(peak.Load()), 1, "the reads overlap")
}

// The page starts in the cards layout (a script applies the stored choice) and
// offers the three layouts, "cards" pressed; the store loads before the app.
func TestHomeOffersTheLayouts(t *testing.T) {
	t.Parallel()
	browse := homeBrowse(t)
	browse.EXPECT().ListCommits(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.HistoryPage{Commits: homeCommits(t, 2)}, nil)
	srv := newTestServer(t, Config{Repos: reposServing(t, pushedRepo(t, "proj")), Browse: browse})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `<div class="home" data-layout="cards">`)
	assert.Contains(t, body, `<div class="layout-switch" role="group" aria-label="Layout" data-js>`)
	for layout, pressed := range map[string]string{"cards": "true", "split": "false", "compact": "false"} {
		assert.Contains(t, body, `data-set-layout="`+layout+`" aria-pressed="`+pressed+`"`)
		assert.Contains(t, body, `class="ico i-`+layout+`"`)
	}
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
		domain.MergeRequestOpened{MergeRequest: mr}, domain.MergeRequestMerged{MergeRequest: mr},
		domain.MergeRequestClosed{MergeRequest: mr}, domain.MergeRequestReady{MergeRequest: mr},
		domain.CommentAdded{Repo: repo.Name}, domain.IssueOpened{Issue: issue},
		domain.IssueClosed{Issue: issue}, domain.IssueReopened{Issue: issue}, domain.IssueCommented{Repo: repo.Name},
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
			assert.Contains(t, out, `class="pill repo-license" href="/proj/blob/main/LICENSE" title="License: MIT">MIT</a>`)
			assert.Regexp(t, `(?s)>private</span>.*repo-license.*class="repo-activity" title="new activity"><span class="ico i-refresh-cw" aria-hidden="true"></span><span class="sr">new activity</span></span></h2>`, out)
			if tc.visible {
				assert.Regexp(t, `(?s)>proj</a>.*class="pill repo-release".*>v1.2.0</a>.*>private</span>`, out)
				assert.Contains(t, out, `href="/proj/releases/tag/v1.2.0"`)
			} else {
				assert.NotContains(t, out, `repo-release`)
			}
		})
	}
	out := renderRow(t, HomeRow{Name: "bare"})
	assert.NotContains(t, out, `repo-license`)
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
