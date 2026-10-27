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

func inboxOverview(t testing.TB) ports.InboxOverview {
	t.Helper()
	mrTitle, err := domain.ParseMRTitle("fix <b>search</b>")
	require.NoError(t, err)
	isTitle, err := domain.ParseIssueTitle("crash on start")
	require.NoError(t, err)
	at := time.Now().Add(-2 * time.Hour)
	ov := ports.InboxOverview{
		MergeRequests: []domain.MergeRequest{{
			Repo: repoName(t, "alpha"), Number: 4, Title: mrTitle, State: domain.MRStateOpen, UpdatedAt: at,
		}},
		Issues: []domain.Issue{{Repo: repoName(t, "beta"), Number: 7, Title: isTitle, State: domain.IssueStateOpen, UpdatedAt: at}},
	}
	ov.Counts = ports.InboxCounts{MergeRequests: 1, Issues: 1}
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
	svc.EXPECT().Overview(mock.Anything, mock.Anything).Return(inboxOverview(t), nil)
	srv := inboxServer(t, svc)

	rec := inboxGet(srv, "/-/inbox", true)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, "Open Issues &amp; Merge Requests (2)")
	assert.Contains(t, body, "Open merge requests")
	assert.Contains(t, body, "Open issues")
	assert.Contains(t, body, `href="/alpha/merge-requests/4"`)
	assert.Contains(t, body, "fix &lt;b&gt;search&lt;/b&gt;")
	assert.Contains(t, body, `href="/beta/issues/7"`)
	assert.Contains(t, body, "#7")
	assert.Contains(t, body, "2h ago")
	assert.Contains(t, body, `href="/-/inbox?tab=deps"`, "the dependency tab")
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

func TestInboxButtonOnHomeIsForTheOwnerOnly(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	ov := inboxOverview(t)
	ov.Counts = ports.InboxCounts{MergeRequests: 3, Issues: 2}
	svc.EXPECT().Overview(mock.Anything, mock.Anything).Return(ov, nil).Once() // the owner's home only
	srv := inboxServer(t, svc)

	anon := inboxGet(srv, "/", false)
	require.Equal(t, 200, anon.Code)
	assert.NotContains(t, anon.Body.String(), "/-/inbox")
	assert.NotContains(t, anon.Body.String(), "Inbox")

	owner := inboxGet(srv, "/", true)
	require.Equal(t, 200, owner.Code)
	assert.Contains(t, owner.Body.String(), `href="/-/inbox"`)
	assert.Contains(t, owner.Body.String(), `<span class="count">5</span>`)
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
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Overview(mock.Anything, mock.Anything).Return(inboxOverview(t), nil)
	srv := inboxServer(t, svc)
	ls := &liveStream{s: srv, view: "inbox"}
	frags, err := ls.render(withOwner(t.Context()), ports.WatchEvent{ID: "b-1", Event: domain.IssueOpened{Issue: domain.Issue{Repo: repoName(t, "beta"), Number: 7}}})
	require.NoError(t, err)
	require.Len(t, frags, 1)
	assert.Contains(t, frags[0].html, `id="inbox-content"`)
	assert.Contains(t, frags[0].html, `id="inbox-mrs"`)
	assert.Contains(t, frags[0].html, `id="inbox-issues"`)
	assert.NotContains(t, frags[0].html, `id="inbox-pending"`)

	resolved, err := ls.render(withOwner(t.Context()), ports.WatchEvent{Event: domain.IssueResolved{Issue: domain.Issue{Repo: repoName(t, "beta"), Number: 7}}})
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	assert.Contains(t, resolved[0].html, `id="inbox-issues"`)

	none, err := ls.render(withOwner(t.Context()), ports.WatchEvent{Event: domain.PushEvent{Repo: repoName(t, "beta")}})
	require.NoError(t, err)
	assert.Empty(t, none, "a push is not inbox news")
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
	svc.EXPECT().Overview(mock.Anything, mock.Anything).Return(inboxOverview(t), nil).Maybe()
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
		return ports.WatchEvent{ID: id, Event: domain.IssueOpened{Issue: domain.Issue{Repo: repoName(t, "beta"), Number: 7}}}
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

// A repository event refreshes the inbox: issues turned off or a deletion
// remove its items from the fragments.
func TestInboxRefreshesOnRepoEvents(t *testing.T) {
	t.Parallel()
	for name, ev := range map[string]domain.Event{
		"issues turned off": domain.RepoUpdated{Repo: repoName(t, "beta")},
		"run updated":       domain.RunUpdated{},
		"repo deleted":      domain.RepoDeleted{Repo: repoName(t, "beta")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			after := ports.InboxOverview{}
			svc := mocks.NewInboxService(t)
			svc.EXPECT().Overview(mock.Anything, mock.Anything).Return(after, nil)
			srv := inboxServer(t, svc)
			ls := &liveStream{s: srv, view: "inbox"}
			frags, err := ls.render(withOwner(t.Context()), ports.WatchEvent{ID: "b-1", Event: ev})
			require.NoError(t, err)
			require.Len(t, frags, 1)
			all := ""
			for _, f := range frags {
				all += f.html
			}
			assert.Contains(t, all, `id="inbox-issues"`)
			assert.NotContains(t, all, "crash on start", "the issue is gone")
			assert.NotContains(t, all, "/beta", "the repository is gone from the lists")
		})
	}
}

func inboxOverviewWithHead(t testing.TB, head string) ports.InboxOverview {
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

func TestInboxMRRowsShowTheCIIcon(t *testing.T) {
	t.Parallel()
	head := fmt.Sprintf("%040x", 7)
	repo := repoName(t, "alpha")
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Overview(mock.Anything, mock.Anything).Return(inboxOverviewWithHead(t, head), nil).Twice()
	runs := mocks.NewRunService(t)
	runs.EXPECT().ListChecks(mock.Anything, mock.Anything, repo, head).Return(ports.ChecksResult{Checks: []ports.CheckView{
		{CICheck: domain.CICheck{Repo: repo, Sha: head, RunID: 9, Status: domain.CICompleted, Conclusion: domain.CISuccess}},
	}}, nil).Twice()
	srv := inboxChecksServer(t, svc, runs)

	rec := inboxGet(srv, "/-/inbox", true)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `class="ck-sum"`)
	assert.Contains(t, rec.Body.String(), `href="/alpha/actions"`)

	frags, err := (&liveStream{s: srv, view: "inbox"}).render(withOwner(t.Context()), ports.WatchEvent{ID: "b-1", Event: domain.IssueOpened{Issue: domain.Issue{Repo: repoName(t, "beta"), Number: 7}}})
	require.NoError(t, err)
	require.Len(t, frags, 1)
	assert.Contains(t, frags[0].html, `id="inbox-mrs"`)
	assert.Contains(t, frags[0].html, `class="ck-sum"`)
	assert.NotContains(t, frags[0].html[strings.Index(frags[0].html, `id="inbox-issues"`):], "ck-sum", "issue rows have no CI icon")
}

func TestInboxZeroHeadReadsNoChecks(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Overview(mock.Anything, mock.Anything).Return(inboxOverviewWithHead(t, ""), nil)
	runs := mocks.NewRunService(t) // any ListChecks call fails the test
	srv := inboxChecksServer(t, svc, runs)

	rec := inboxGet(srv, "/-/inbox", true)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "ck-sum")
}

func TestInboxReadsChecksOncePerHead(t *testing.T) {
	t.Parallel()
	head := fmt.Sprintf("%040x", 7)
	repo := repoName(t, "alpha")
	ov := inboxOverviewWithHead(t, head)
	second := ov.MergeRequests[0]
	second.Number = 5
	ov.MergeRequests = append(ov.MergeRequests, second)
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Overview(mock.Anything, mock.Anything).Return(ov, nil)
	runs := mocks.NewRunService(t)
	runs.EXPECT().ListChecks(mock.Anything, mock.Anything, repo, head).Return(ports.ChecksResult{}, nil).Once()
	srv := inboxChecksServer(t, svc, runs)

	require.Equal(t, 200, inboxGet(srv, "/-/inbox", true).Code)
}

// Every tab keeps all counts, but renders only its own sections, including on
// live refresh. Plain hrefs make selection work without JavaScript.
func TestInboxTabs(t *testing.T) {
	for _, tc := range []struct {
		query, tab string
		sections   []string
	}{
		{"", "work", []string{"mrs", "issues"}},
		{"?tab=work", "work", []string{"mrs", "issues"}},
		{"?tab=unknown", "work", []string{"mrs", "issues"}},
		{"?tab=deps", "deps", []string{"deps"}},
		{"?tab=activity", "activity", []string{"runs", "journal"}},
		{"?tab=moderation", "moderation", []string{"pending"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			ov := depsOverview(t)
			ov.Counts.Pending = 2
			ov.RunsEnabled = true
			ov.Runs = []domain.CIRun{{Repo: repoName(t, "alpha"), ID: 1, Status: domain.CIQueued}}
			svc := mocks.NewInboxService(t)
			svc.EXPECT().Overview(mock.Anything, mock.Anything).Return(ov, nil)
			svc.EXPECT().Journal(mock.Anything, mock.Anything, mock.Anything).Return(sampleJournal(t), nil)
			srv := inboxServer(t, svc)
			body := inboxGet(srv, inboxPath+tc.query, true).Body.String()
			for _, label := range []string{"Deps Updates (3)", "Open Issues &amp; Merge Requests (2)", "Activity (8)", "Awaiting Moderation (2)"} {
				assert.Contains(t, body, label)
			}
			assert.Contains(t, body, `href="`+inboxHref(tc.tab, 0)+`" aria-current="page"`)
			for _, section := range []string{"mrs", "issues", "deps", "runs", "journal", "pending"} {
				want := false
				for _, selected := range tc.sections {
					want = want || section == selected
				}
				assert.Equal(t, want, strings.Contains(body, `id="inbox-`+section+`"`), section)
			}
			assert.NotContains(t, body, `id="inbox-repos"`)
			assert.NotContains(t, body, `id="inbox-counts"`)
			assert.NotContains(t, body, `?repo=`)
			ls := &liveStream{s: srv, view: "inbox", tab: tc.tab}
			frags, err := ls.render(withOwner(t.Context()), ports.WatchEvent{Event: domain.JournalUpdated{}})
			require.NoError(t, err)
			require.Len(t, frags, 1)
			assert.Contains(t, frags[0].html, `id="inbox-content"`)
			for _, section := range []string{"mrs", "issues", "deps", "runs", "journal", "pending"} {
				assert.Equal(t, strings.Contains(body, `id="inbox-`+section+`"`), strings.Contains(frags[0].html, `id="inbox-`+section+`"`), section)
			}
		})
	}
}
