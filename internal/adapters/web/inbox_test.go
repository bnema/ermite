// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bufio"
	"context"
	"fmt"
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

// inboxFixture is what the inbox tests serve: every section at once.
// expectInbox answers each focused read of InboxService from it.
type inboxFixture struct {
	DepsQueue     ports.DepsQueue
	Deps          []ports.InboxDeps
	DepsCheckedAt time.Time
	Runs          []domain.CIRun
	RunsEnabled   bool
	MergeRequests []domain.MergeRequest
	Issues        []domain.Issue
	Pending       []domain.PendingIssue
}

// expectInbox lets svc answer every focused read from ov, as many times as
// asked.
func expectInbox(svc *mocks.InboxService, ov inboxFixture) {
	svc.EXPECT().Work(mock.Anything, mock.Anything).Return(ports.InboxWork{MergeRequests: ov.MergeRequests, Issues: ov.Issues}, nil).Maybe()
	svc.EXPECT().Runs(mock.Anything, mock.Anything).Return(ports.InboxRuns{Enabled: ov.RunsEnabled, Runs: ov.Runs}, nil).Maybe()
	svc.EXPECT().Deps(mock.Anything, mock.Anything).Return(ports.InboxDepsView{Queue: ov.DepsQueue, Deps: ov.Deps, CheckedAt: ov.DepsCheckedAt}, nil).Maybe()
	var items []ports.AttentionItem
	for i := range ov.Pending {
		items = append(items, ports.AttentionItem{Kind: ports.AttentionModeration, Repo: ov.Pending[i].Repo, Pending: &ov.Pending[i]})
	}
	counts := ports.OwnerCounts{Attention: len(items), Work: len(ov.MergeRequests) + len(ov.Issues)}
	svc.EXPECT().Attention(mock.Anything, mock.Anything).Return(ports.OwnerAttention{Items: items, Counts: counts}, nil).Maybe()
	svc.EXPECT().Counts(mock.Anything, mock.Anything).Return(counts, nil).Maybe()
}

func inboxOverview(t testing.TB) inboxFixture {
	t.Helper()
	mrTitle, err := domain.ParseMRTitle("fix <b>search</b>")
	require.NoError(t, err)
	isTitle, err := domain.ParseIssueTitle("crash on start")
	require.NoError(t, err)
	at := time.Now().Add(-2 * time.Hour)
	ov := inboxFixture{
		MergeRequests: []domain.MergeRequest{{
			Repo: repoName(t, "alpha"), Number: 4, Title: mrTitle, State: domain.MRStateOpen, UpdatedAt: at,
		}},
		Issues: []domain.Issue{{Repo: repoName(t, "beta"), Number: 7, Title: isTitle, State: domain.IssueStateOpen, UpdatedAt: at}},
	}
	return ov
}

// emptyJournal lets the inbox read an empty journal: the tests of the other
// sections do not look at it.
func emptyJournal(inbox ports.InboxService) {
	if m, ok := inbox.(*mocks.InboxService); ok {
		m.EXPECT().Journal(mock.Anything, mock.Anything, mock.Anything).Return(ports.InboxJournal{}, nil).Maybe()
	}
}

func inboxServer(t *testing.T, inbox ports.InboxService) *Server {
	t.Helper()
	emptyJournal(inbox)
	return newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "alpha")), Browse: homeBrowse(t), Inbox: inbox,
		Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
	})
}

func inboxGet(srv *Server, path string, owner bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if owner {
		req.AddCookie(&http.Cookie{Name: CookieName, Value: "tok"})
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestInboxPageForTheOwner(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	pending := testPendingIssue(t)
	svc.EXPECT().Attention(mock.Anything, mock.Anything).Return(ports.OwnerAttention{Items: []ports.AttentionItem{
		{Kind: ports.AttentionModeration, Repo: pending.Repo, Pending: &pending},
		{Kind: ports.AttentionRelease, Repo: repoName(t, "beta")},
	}, Counts: ports.OwnerCounts{Attention: 2, Work: 5}}, nil).Once() // no Counts call: the nav uses these
	srv := newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "alpha")), Browse: homeBrowse(t), Inbox: svc,
		Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
	})

	rec := inboxGet(srv, "/-/inbox?tab=deps", true)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, "<title>Inbox · ")
	assert.Contains(t, body, `<div id="view" data-view="inbox"><h1 class="sr" tabindex="-1">Inbox</h1>`)
	assert.Contains(t, body, `href="/-/inbox" data-nav data-key="g i" aria-label="Inbox (2)" aria-current="page"`)
	assert.Contains(t, body, `id="attention"`)
	assert.Contains(t, body, `id="att-moderation-alpha-`+pending.ID.String()+`"`)
	for _, gone := range []string{"tab=", `id="inbox-mrs"`, `id="inbox-deps"`, `id="inbox-journal"`, `class="crumbs"`, `aria-label="Inbox sections"`} {
		assert.NotContains(t, body, gone, "?tab= is ignored")
	}
	assert.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
}

func TestInboxPageIsMissingForEveryoneElse(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t) // never asked
	srv := inboxServer(t, svc)
	missing := inboxGet(srv, "/-/nothing-here", false)
	require.Equal(t, 404, missing.Code)

	anon := inboxGet(srv, "/-/inbox", false)
	assert.Equal(t, 404, anon.Code)
	assert.Equal(t, missing.Body.String(), anon.Body.String(), "the same page as an unknown one")
	assert.NotContains(t, anon.Body.String(), "Open merge requests")

	// A bad cookie is anonymous as well.
	req := httptest.NewRequest(http.MethodGet, "/-/inbox", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "other"})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	assert.Equal(t, 404, rec.Code)
}

func TestInboxNotMountedWithoutTheService(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "alpha")), Browse: homeBrowse(t), Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{}})
	assert.Equal(t, 404, inboxGet(srv, "/-/inbox", true).Code)
}

func TestOwnerNavOnHomeIsForTheOwnerOnly(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Counts(mock.Anything, mock.Anything).Return(ports.OwnerCounts{Attention: 2, Work: 5}, nil).Once() // the owner's home only
	srv := inboxServer(t, svc)

	anon := inboxGet(srv, "/", false)
	require.Equal(t, 200, anon.Code)
	assert.NotContains(t, anon.Body.String(), "/-/inbox")
	assert.NotContains(t, anon.Body.String(), "Inbox")
	assert.NotContains(t, anon.Body.String(), "owner-nav")
	assert.Contains(t, anon.Body.String(), `<div id="view" data-view="home">`)

	owner := inboxGet(srv, "/", true)
	require.Equal(t, 200, owner.Code)
	body := owner.Body.String()
	assert.Contains(t, body, `id="owner-nav"`)
	assert.Contains(t, body, `href="/" data-nav data-key="g h" aria-label="Repositories" aria-current="page"`)
	assert.Contains(t, body, `href="/-/inbox"`)
	assert.Contains(t, body, `<span class="count hot">2</span>`)
	assert.Contains(t, body, `<span class="count">5</span>`)
	assert.NotContains(t, body, "inbox-btn")
}

func TestInboxStreamIsTheOwnersAlone(t *testing.T) {
	t.Parallel()
	f := &watchProbe{release: make(chan struct{})}
	svc := mocks.NewInboxService(t)
	srv := newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "x")), Browse: mocks.NewBrowseService(t), Events: f.service(t), Inbox: svc,
		Heartbeat: time.Hour, Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	unknown, _ := open(t, ts, "topic=nope&view=inbox", nil)
	want, _ := io.ReadAll(unknown.Body)
	for name, hdr := range map[string]map[string]string{
		"anonymous":     nil,
		"another token": {"Cookie": CookieName + "=other"},
	} {
		resp, _ := open(t, ts, "topic=owner&view=inbox", hdr)
		body, _ := io.ReadAll(resp.Body)
		assert.Equal(t, unknown.StatusCode, resp.StatusCode, name)
		assert.Equal(t, string(want), string(body), name)
		assert.Equal(t, unknown.Header.Get("Cache-Control"), resp.Header.Get("Cache-Control"), name)
	}
	assert.Zero(t, f.opened.Load(), "no watch is opened for them")

	resp, _ := open(t, ts, "topic=owner&view=inbox", map[string]string{"Cookie": CookieName + "=tok"})
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
}

func TestInboxLiveFragments(t *testing.T) {
	t.Parallel()
	ov := inboxOverview(t)
	ov.Pending = []domain.PendingIssue{testPendingIssue(t)}
	svc := mocks.NewInboxService(t)
	expectInbox(svc, ov)
	srv := inboxServer(t, svc)
	ls := &liveStream{s: srv, forOwner: true, view: "inbox"}
	frags, err := ls.render(withOwner(t.Context()), ports.WatchEvent{ID: "b-1", Event: domain.IssueSubmitted{Repo: repoName(t, "alpha")}})
	require.NoError(t, err)
	require.Len(t, frags, 1)
	assert.Contains(t, frags[0].html, `id="attention"`)
	assert.Contains(t, frags[0].html, `id="att-moderation-alpha-`)
	assert.NotContains(t, frags[0].html, `id="inbox-mrs"`)

	removed, err := ls.render(withOwner(t.Context()), ports.WatchEvent{Event: domain.IssuePendingRemoved{Repo: repoName(t, "alpha")}})
	require.NoError(t, err)
	require.Len(t, removed, 1)

	for _, ev := range []domain.Event{domain.IssueEdited{Issue: domain.Issue{Repo: repoName(t, "beta"), Number: 7}}, domain.CommentAdded{}, domain.JobUpdated{}} {
		none, err := ls.render(withOwner(t.Context()), ports.WatchEvent{Event: ev})
		require.NoError(t, err)
		assert.Empty(t, none, "%T is not inbox news", ev)
	}
	pushed, err := ls.render(withOwner(t.Context()), ports.WatchEvent{Event: domain.PushEvent{Repo: repoName(t, "beta")}})
	require.NoError(t, err)
	require.Len(t, pushed, 1)
	assert.Equal(t, "owner", pushed[0].event)
	assert.Contains(t, pushed[0].html, `id="attention"`)
	assert.Contains(t, pushed[0].html, `id="owner-nav"`)
}

// withOwner is a context as the session middleware leaves it for the owner.
func withOwner(ctx context.Context) context.Context {
	return domain.ContextWithPrincipal(context.WithValue(ctx, signedInKey{}, struct{}{}), domain.Owner())
}

// A bearer token is not a web session: the inbox answers it as it answers
// anyone else, with the unknown-page and unknown-topic responses.
func TestInboxRefusesABearerToken(t *testing.T) {
	t.Parallel()
	f := &watchProbe{release: make(chan struct{})}
	svc := mocks.NewInboxService(t) // never asked
	srv := newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "x")), Browse: mocks.NewBrowseService(t), Events: f.service(t), Inbox: svc,
		Heartbeat: time.Hour, Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
	})
	bearer := map[string]string{"Authorization": "Bearer tok"}

	// The page.
	missing := inboxGet(srv, "/-/nothing-here", false)
	req := httptest.NewRequest(http.MethodGet, "/-/inbox", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	assert.Equal(t, missing.Code, rec.Code)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, missing.Body.String(), rec.Body.String(), "the unknown-page response")

	// The stream.
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	unknown, _ := open(t, ts, "topic=nope&view=inbox", nil)
	want, _ := io.ReadAll(unknown.Body)
	resp, _ := open(t, ts, "topic=owner&view=inbox", bearer)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, unknown.StatusCode, resp.StatusCode)
	assert.Equal(t, string(want), string(body), "the unknown-topic response")
	assert.Zero(t, f.opened.Load(), "no watch is opened")
}

// The owner's inbox stream ends once the session is invalid: the next event is
// not sent.
func TestInboxStreamEndsWhenTheSessionEnds(t *testing.T) {
	t.Parallel()
	f := &watchProbe{release: make(chan struct{}), push: make(chan ports.WatchEvent)}
	sess, ended := newRevocable(t, "tok")
	svc := mocks.NewInboxService(t)
	expectInbox(svc, inboxOverview(t))
	emptyJournal(svc)
	srv := newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "x")), Browse: mocks.NewBrowseService(t), Events: f.service(t), Inbox: svc,
		Heartbeat: time.Hour, Sessions: sess, Limiter: &countingLimiter{},
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	resp, cancel := open(t, ts, "topic=owner&view=inbox", map[string]string{"Cookie": CookieName + "=tok"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	defer time.AfterFunc(5*time.Second, cancel).Stop()
	rd := bufio.NewReader(resp.Body)
	line := func() string {
		s, err := rd.ReadString('\n')
		if err != nil {
			return "EOF"
		}
		return strings.TrimRight(s, "\n")
	}
	require.Equal(t, ": connected", line())
	line()
	ev := func(id string) ports.WatchEvent {
		return ports.WatchEvent{ID: id, Event: domain.IssueSubmitted{Repo: repoName(t, "beta")}}
	}
	f.push <- ev("b-1")
	var seen []string
	for l := line(); !strings.HasPrefix(l, "data:"); l = line() {
		seen = append(seen, l)
		require.NotEqual(t, "EOF", l)
	}
	assert.Contains(t, seen, "id: b-1")

	ended.Store(true)
	select {
	case f.push <- ev("b-2"):
	case <-time.After(5 * time.Second):
		t.Fatal("the event was not delivered to the web")
	}
	rest, _ := io.ReadAll(rd)
	assert.NotContains(t, string(rest), "b-2", "nothing is sent after the session ended")
	require.Eventually(t, func() bool { return f.closed.Load() == 1 }, 5*time.Second, time.Millisecond, "the watch is closed")
}

// A repository event refreshes the inbox: a deletion removes its pending
// issues.
func TestInboxRefreshesOnRepoEvents(t *testing.T) {
	t.Parallel()
	for name, ev := range map[string]domain.Event{
		"repo updated": domain.RepoUpdated{Repo: repoName(t, "alpha")},
		"repo deleted": domain.RepoDeleted{Repo: repoName(t, "alpha")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc := mocks.NewInboxService(t)
			expectInbox(svc, inboxFixture{})
			srv := inboxServer(t, svc)
			ls := &liveStream{s: srv, forOwner: true, view: "inbox"}
			frags, err := ls.render(withOwner(t.Context()), ports.WatchEvent{ID: "b-1", Event: ev})
			require.NoError(t, err)
			require.Len(t, frags, 1)
			assert.Contains(t, frags[0].html, `id="attention"`)
			assert.Contains(t, frags[0].html, "nothing awaits you.")
		})
	}
}

func inboxOverviewWithHead(t testing.TB, head string) inboxFixture {
	t.Helper()
	ov := inboxOverview(t)
	if head != "" {
		id, err := domain.ParseObjectID(head)
		require.NoError(t, err)
		ov.MergeRequests[0].HeadID = id
	}
	return ov
}

func inboxChecksServer(t *testing.T, svc ports.InboxService, runs ports.RunService) *Server {
	t.Helper()
	emptyJournal(svc)
	return newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "alpha")), Browse: homeBrowse(t), Inbox: svc, Runs: runs,
		Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
	})
}

func TestWorkMRRowsShowTheCIIcon(t *testing.T) {
	t.Parallel()
	head := fmt.Sprintf("%040x", 7)
	repo := repoName(t, "alpha")
	svc := mocks.NewInboxService(t)
	expectInbox(svc, inboxOverviewWithHead(t, head))
	runs := mocks.NewRunService(t)
	runs.EXPECT().ListChecks(mock.Anything, mock.Anything, repo, head).Return(ports.ChecksResult{Checks: []ports.CheckView{
		{CICheck: domain.CICheck{Repo: repo, Sha: head, RunID: 9, Status: domain.CICompleted, Conclusion: domain.CISuccess}},
	}}, nil).Once()
	srv := inboxChecksServer(t, svc, runs)

	rec := inboxGet(srv, workPath, true)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, `class="ck-sum"`)
	assert.Contains(t, body, `href="/alpha/actions"`)
	assert.NotContains(t, body[strings.Index(body, `id="inbox-issues"`):], "ck-sum", "issue rows have no CI icon")
}

func TestWorkZeroHeadReadsNoChecks(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	expectInbox(svc, inboxOverviewWithHead(t, ""))
	runs := mocks.NewRunService(t) // any ListChecks call fails the test
	srv := inboxChecksServer(t, svc, runs)

	rec := inboxGet(srv, workPath, true)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "ck-sum")
}
