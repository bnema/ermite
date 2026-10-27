// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
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

func journalServer(t *testing.T, svc *mocks.InboxService) *Server {
	t.Helper()
	svc.EXPECT().Overview(mock.Anything, mock.Anything).Return(ports.InboxOverview{}, nil).Maybe()
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

func TestInboxJournalTable(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Journal(mock.Anything, mock.Anything, 24*time.Hour).Return(sampleJournal(t), nil)
	rec := inboxGet(journalServer(t, svc), "/-/inbox?tab=activity", true)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	body := rec.Body.String()
	sec := body[strings.Index(body, `id="inbox-journal"`):]

	assert.NotContains(t, body, `id="inbox-issues"`)
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
	assert.Contains(t, sec, "3d ago")
	assert.Contains(t, sec, `href="/big/commits/trunk">500+ commits</a>`)
	assert.Contains(t, sec, "since the last tag", "no tag met within the limit")
	assert.NotContains(t, sec, "card")
}

func TestInboxJournalRangeToggleKeepsTheActivityTab(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Journal(mock.Anything, mock.Anything, 7*24*time.Hour).Return(ports.InboxJournal{}, nil)
	rec := inboxGet(journalServer(t, svc), "/-/inbox?tab=activity&range=7d", true)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, `<a href="/-/inbox?tab=activity#inbox-journal">24 h</a>`)
	assert.Contains(t, body, `<a href="/-/inbox?range=7d&amp;tab=activity#inbox-journal" aria-current="page">7 days</a>`)
	assert.Contains(t, body, `href="/-/inbox?range=7d&amp;tab=activity"`, "the tab keeps the range")
	assert.Contains(t, body, "no activity in the last 7 days.")
	assert.NotContains(t, body, `class="unreleased"`)
	assert.NotContains(t, body, `journal-list`)
}

func TestInboxJournalEmptyAndBadRange(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Journal(mock.Anything, mock.Anything, 24*time.Hour).Return(ports.InboxJournal{}, nil).Once()
	srv := journalServer(t, svc)
	rec := inboxGet(srv, "/-/inbox?tab=activity", true)
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), "no activity in the last 24 hours.")
	assert.Contains(t, rec.Body.String(), `<a href="/-/inbox?tab=activity#inbox-journal" aria-current="page">24 h</a>`)

	assert.Equal(t, 404, inboxGet(srv, "/-/inbox?range=1y", true).Code)
	assert.Equal(t, 404, inboxGet(srv, "/-/inbox?range=7d", false).Code, "still the owner's alone")
}

func TestInboxJournalTruncatedNote(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	j := sampleJournal(t)
	j.Truncated = true
	svc.EXPECT().Journal(mock.Anything, mock.Anything, mock.Anything).Return(j, nil)
	rec := inboxGet(journalServer(t, svc), "/-/inbox?tab=activity", true)
	assert.Contains(t, rec.Body.String(), "only the latest 5 entries are shown.")
}

// The owner's stream takes the range of the page: an unknown one is the
// unknown-topic answer (as the page is a 404) and opens no watch; a known one
// is the range of the journal its fragments render.
func TestInboxJournalStreamRange(t *testing.T) {
	t.Parallel()
	f := &watchProbe{release: make(chan struct{}), push: make(chan ports.WatchEvent)}
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Overview(mock.Anything, mock.Anything).Return(ports.InboxOverview{}, nil).Maybe()
	svc.EXPECT().Journal(mock.Anything, mock.Anything, 7*24*time.Hour).Return(sampleJournal(t), nil).Once()
	srv := newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "x")), Browse: mocks.NewBrowseService(t), Events: f.service(t), Inbox: svc,
		Heartbeat: time.Hour, Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	owner := map[string]string{"Cookie": CookieName + "=tok"}

	unknown, _ := open(t, ts, "topic=nope&view=inbox", nil)
	want, _ := io.ReadAll(unknown.Body)
	bad, _ := open(t, ts, "topic=owner&view=inbox&range=1y", owner)
	body, _ := io.ReadAll(bad.Body)
	assert.Equal(t, unknown.StatusCode, bad.StatusCode)
	assert.Equal(t, string(want), string(body), "the unknown-topic response")
	assert.Zero(t, f.opened.Load(), "no watch is opened")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/-/inbox?range=1y", nil)
	require.NoError(t, err)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "tok"})
	page, err := ts.Client().Do(req)
	require.NoError(t, err)
	_ = page.Body.Close()
	assert.Equal(t, http.StatusNotFound, page.StatusCode, "the page")

	resp, _ := open(t, ts, "topic=owner&view=inbox&tab=activity&range=7d", owner)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	rd := bufio.NewReader(resp.Body)
	f.push <- ports.WatchEvent{ID: "b-1", Event: domain.JournalUpdated{Repo: repoName(t, "alpha")}}
	var journal string
	for journal == "" {
		line, err := rd.ReadString('\n')
		require.NoError(t, err)
		if strings.HasPrefix(line, "data:") && strings.Contains(line, `id="inbox-journal"`) {
			journal = line
		}
	}
	assert.Contains(t, journal, `aria-current="page">7 days</a>`, "rendered for the range of the stream")
}

// The journal signal of a push and a published release refresh the inbox,
// on the range of the stream; a push itself does not.
func TestInboxJournalLive(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Journal(mock.Anything, mock.Anything, 7*24*time.Hour).Return(sampleJournal(t), nil)
	srv := journalServer(t, svc)
	ls := &liveStream{s: srv, view: "inbox", tab: "activity", rng: "7d"}
	for _, ev := range []domain.Event{
		domain.JournalUpdated{Repo: repoName(t, "alpha")},
		domain.ReleasePublished{Release: domain.Release{Repo: repoName(t, "beta")}},
	} {
		frags, err := ls.render(withOwner(t.Context()), ports.WatchEvent{ID: "b-1", Event: ev})
		require.NoError(t, err)
		require.Len(t, frags, 1, "%T", ev)
		assert.Contains(t, frags[0].html, `id="inbox-journal"`)
		assert.Contains(t, frags[0].html, "add &lt;search&gt;")
		assert.Contains(t, frags[0].html, `aria-current="page">7 days</a>`)
	}
	none, err := ls.render(withOwner(t.Context()), ports.WatchEvent{Event: domain.PushEvent{Repo: repoName(t, "alpha")}})
	require.NoError(t, err)
	assert.Empty(t, none)
	assert.True(t, liveEvent(ports.WatchEvent{Event: domain.JournalUpdated{}}))
}
