// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
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

// journalServer serves the activity page with svc: Runs and Counts once,
// the test sets the Journal it reads.
func journalServer(t *testing.T, svc *mocks.InboxService) *Server {
	t.Helper()
	expectPage(svc, inboxFixture{}, "activity")
	return newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "alpha")), Browse: homeBrowse(t), Inbox: svc,
		Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
	})
}

func sampleJournal(t *testing.T) ports.InboxJournal {
	t.Helper()
	now := time.Now()
	head, err := domain.ParseObjectID(strings.Repeat("ab", 20))
	require.NoError(t, err)
	return ports.InboxJournal{
		Entries: []domain.JournalEntry{
			{At: now.Add(-5 * time.Minute), Repo: repoName(t, "alpha"), Kind: domain.JournalMergeRequestMerged, Actor: "owner", Number: 12, Title: "add <search>"},
			{At: now.Add(-2 * time.Hour), Repo: repoName(t, "alpha"), Kind: domain.JournalDefaultBranchPush, Actor: "ci-bot", Ref: "main", Commits: 3, Commit: head, Summary: "fix the build"},
			{At: now.Add(-3 * time.Hour), Repo: repoName(t, "beta"), Kind: domain.JournalTagDeleted, Ref: "v0.1.0"},
			{At: now.Add(-4 * time.Hour), Repo: repoName(t, "beta"), Kind: domain.JournalIssueClosed, Number: 7, Title: "crash"},
			{At: now.Add(-5 * time.Hour), Repo: repoName(t, "beta"), Kind: domain.JournalReleasePublished, Ref: "v1.0.0", Title: "First"},
		},
		Unreleased: []ports.UnreleasedRepo{
			{Repo: repoName(t, "ermite-api"), Branch: "main", Distance: domain.TagDistance{Commits: 4, Tag: domain.TagInfo{Name: "v0.12.0", At: now.Add(-72 * time.Hour)}}},
			{Repo: repoName(t, "big"), Branch: "trunk", Distance: domain.TagDistance{Commits: domain.MaxTagDistance, More: true}},
		},
	}
}

func TestActivityJournalTable(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Journal(mock.Anything, mock.Anything, 24*time.Hour).Return(sampleJournal(t), nil)
	rec := inboxGet(journalServer(t, svc), activityPath, true)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	body := rec.Body.String()
	sec := body[strings.Index(body, `id="activity-journal"`):]

	assert.NotContains(t, body, `id="work-issues"`)
	assert.Contains(t, sec, `<span>Activity</span><span class="journal-count">5</span>`, "plain text, not the pill of .count")
	assert.NotContains(t, sec, `class="count"`)
	assert.Contains(t, sec, `<th>When</th><th>Repository</th><th>Type</th><th class="c-main">Detail</th><th class="c-who">Actor</th>`)
	assert.Contains(t, sec, `<tr data-kind="mr_merged">`)
	assert.Contains(t, sec, `<span class="j-kind">merged</span>`)
	assert.Contains(t, sec, `href="/alpha/merge-requests/12"><span class="j-ref">!12</span> add &lt;search&gt;</a>`, "escaped title, linked")
	assert.Contains(t, sec, `<td class="c-who mute">owner</td>`)
	assert.Contains(t, sec, "5 min ago")
	assert.Regexp(t, `<time datetime="[^"]+Z" title="\d{4}-\d\d-\d\d \d\d:\d\d UTC">5 min ago</time>`, sec)
	assert.Contains(t, sec, `href="/alpha/commit/`+strings.Repeat("ab", 20)+`"><span class="j-ref">main</span> fix the build</a>`)
	assert.Contains(t, sec, `<span class="mute num">· 3 commits</span>`)
	assert.Contains(t, sec, `<span class="j-ref">v0.1.0</span>`, "a deleted tag has no link")
	assert.NotContains(t, sec, `href="/beta/tree/v0.1.0"`)
	assert.Contains(t, sec, `href="/beta/issues/7"`)
	assert.Contains(t, sec, `<span class="j-kind">issue closed</span>`)
	assert.Contains(t, sec, `href="/beta/releases/tag/v1.0.0"`)

	// The warnings, above the table, as lines.
	assert.Less(t, strings.Index(sec, `class="unreleased"`), strings.Index(sec, `<table`))
	assert.Contains(t, sec, `<a class="u-repo" href="/ermite-api">ermite-api</a>`)
	assert.Contains(t, sec, `href="/ermite-api/commits/main">4 commits</a>`)
	assert.Contains(t, sec, `<span class="dim">since</span> <a class="u-tag" href="/ermite-api/tree/v0.12.0">v0.12.0</a>`)
	assert.Contains(t, sec, ">3d</time>")
	assert.Contains(t, sec, `<details id="unreleased-due" class="unreleased-group" data-level="due" open>`, "an id: the live swap keeps it open or closed")
	assert.Contains(t, sec, `<details id="unreleased-soon" class="unreleased-group" data-level="soon" open>`)
	assert.Contains(t, sec, `<span class="u-age dim">—</span>`, "no tag date: no <time>")
	assert.Contains(t, sec, `href="/big/commits/trunk">500+ commits</a>`)
	assert.Contains(t, sec, "since the last tag", "no tag met within the limit")
	assert.NotContains(t, sec, "card")
}

func TestActivityRangeToggle(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Journal(mock.Anything, mock.Anything, 7*24*time.Hour).Return(ports.InboxJournal{}, nil)
	rec := inboxGet(journalServer(t, svc), activityPath+"?range=7d", true)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, `<a href="/-/activity" data-nav="keep-scroll">24 h</a>`)
	assert.Contains(t, body, `<a href="/-/activity?range=7d" data-nav="keep-scroll" aria-current="page">7 days</a>`)
	assert.Contains(t, body, "no activity in the last 7 days.")
	assert.NotContains(t, body, `class="unreleased"`)
	assert.NotContains(t, body, `journal-list`)
}

func TestActivityEmptyAndBadRange(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Journal(mock.Anything, mock.Anything, 24*time.Hour).Return(ports.InboxJournal{}, nil).Once()
	srv := journalServer(t, svc)
	rec := inboxGet(srv, activityPath, true)
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), "no activity in the last 24 hours.")
	assert.Contains(t, rec.Body.String(), `<a href="/-/activity" data-nav="keep-scroll" aria-current="page">24 h</a>`)

	assert.Equal(t, 404, inboxGet(srv, activityPath+"?range=1y", true).Code)
	assert.Equal(t, 404, inboxGet(srv, activityPath+"?range=7d", false).Code, "still the owner's alone")
}

func TestActivityJournalTruncatedNote(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	j := sampleJournal(t)
	j.Truncated = true
	svc.EXPECT().Journal(mock.Anything, mock.Anything, mock.Anything).Return(j, nil)
	rec := inboxGet(journalServer(t, svc), activityPath, true)
	assert.Contains(t, rec.Body.String(), "only the latest 5 entries are shown.")
}

func TestJournalUnreleasedUrgency(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "alpha")), Browse: homeBrowse(t)})
	srv.cfg.Now = func() time.Time { return now }
	j := ports.InboxJournal{}
	for _, tc := range []struct {
		name          string
		commits, days int
	}{
		{"unknown", 12, -1}, {"zeta", 10, 31}, {"beta", 11, 31}, {"alpha", 11, 31}, {"oldest", 10, 40}, {"soon", 3, 1}, {"later", 1, 1},
	} {
		tag := domain.TagInfo{Name: "v1", At: now.Add(-time.Duration(tc.days) * 24 * time.Hour)}
		if tc.days < 0 {
			tag.At = time.Time{}
		}
		j.Unreleased = append(j.Unreleased, ports.UnreleasedRepo{Repo: repoName(t, tc.name), Branch: "main", Distance: domain.TagDistance{Commits: tc.commits, Tag: tag}})
	}
	v := srv.journalView(j, 0)
	require.Len(t, v.Unreleased, 3)
	for i, key := range []string{"due", "soon", "later"} {
		assert.Equal(t, key, v.Unreleased[i].Key)
		assert.Equal(t, i != 2, v.Unreleased[i].Open)
	}
	var names []string
	for _, row := range v.Unreleased[0].Rows {
		names = append(names, row.Repo)
	}
	assert.Equal(t, []string{"oldest", "alpha", "beta", "zeta", "unknown"}, names)
	assert.Equal(t, "unknown", j.Unreleased[0].Repo.String(), "input order is preserved")
}

// activityServer serves /-/activity with an inbox that answers only what
// each test expects: Runs, Journal and Counts once each.
func activityServer(t *testing.T) (*Server, *mocks.InboxService) {
	t.Helper()
	svc := mocks.NewInboxService(t)
	return newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "alpha")), Browse: homeBrowse(t), Inbox: svc,
		Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
	}), svc
}

func TestActivityPageForTheOwner(t *testing.T) {
	t.Parallel()
	srv, svc := activityServer(t)
	svc.EXPECT().Runs(mock.Anything, mock.Anything).Return(ports.InboxRuns{Enabled: true, Runs: []domain.CIRun{{Repo: repoName(t, "alpha"), ID: 1, Status: domain.CIQueued}}}, nil).Once()
	svc.EXPECT().Journal(mock.Anything, mock.Anything, 24*time.Hour).Return(sampleJournal(t), nil).Once()
	svc.EXPECT().Counts(mock.Anything, mock.Anything).Return(ports.OwnerCounts{Work: 2}, nil).Once()

	rec := inboxGet(srv, activityPath, true)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, "<title>Activity · ")
	assert.Contains(t, body, `<div id="view" data-view="activity"><h1 class="sr" tabindex="-1">Activity</h1>`)
	assert.Contains(t, body, `href="/-/activity" data-nav data-key="g a" aria-label="Activity" aria-current="page"`)
	assert.Contains(t, body, `aria-labelledby="activity-runs-h"`)
	assert.Contains(t, body, `id="activity-journal"`)
	assert.NotContains(t, body, `class="crumbs"`)
	assert.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
}

func TestActivityHidesRunsWithoutCI(t *testing.T) {
	t.Parallel()
	srv, svc := activityServer(t)
	svc.EXPECT().Runs(mock.Anything, mock.Anything).Return(ports.InboxRuns{}, nil).Once()
	svc.EXPECT().Journal(mock.Anything, mock.Anything, mock.Anything).Return(ports.InboxJournal{}, nil).Once()
	svc.EXPECT().Counts(mock.Anything, mock.Anything).Return(ports.OwnerCounts{}, nil).Once()

	body := inboxGet(srv, activityPath, true).Body.String()
	assert.Contains(t, body, `<section id="activity-runs" hidden>`)
	assert.NotContains(t, body, "Running")
}

func TestActivityPageIsMissingForEveryoneElse(t *testing.T) {
	t.Parallel()
	srv, _ := activityServer(t) // never asked
	missing := inboxGet(srv, "/-/nothing-here", false)
	anon := inboxGet(srv, activityPath, false)
	assert.Equal(t, 404, anon.Code)
	assert.Equal(t, missing.Body.String(), anon.Body.String())
	assert.NotContains(t, anon.Body.String(), "owner-nav")
}
