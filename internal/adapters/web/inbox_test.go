// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bufio"
	"context"
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
		MergeRequests: []ports.InboxMergeRequest{{MergeRequest: domain.MergeRequest{
			Repo: repoName(t, "alpha"), Number: 4, Title: mrTitle, State: domain.MRStateOpen, UpdatedAt: at,
		}}},
		Issues: []domain.Issue{{Repo: repoName(t, "beta"), Number: 7, Title: isTitle, State: domain.IssueStateOpen, UpdatedAt: at}},
		Repos:  []domain.RepoName{repoName(t, "alpha"), repoName(t, "beta")},
	}
	ov.Counts = ports.InboxCounts{MergeRequests: 1, Issues: 1}
	return ov
}

func inboxServer(t *testing.T, inbox ports.InboxService) *Server {
	t.Helper()
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
	svc.EXPECT().Overview(mock.Anything, mock.Anything, domain.RepoName{}).Return(inboxOverview(t), nil)
	srv := inboxServer(t, svc)

	rec := inboxGet(srv, "/-/inbox", true)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, "<strong>1</strong> merge requests · <strong>1</strong> issues")
	assert.Contains(t, body, "Open merge requests")
	assert.Contains(t, body, "Open issues")
	assert.Contains(t, body, `href="/alpha/merge-requests/4"`)
	assert.Contains(t, body, "fix &lt;b&gt;search&lt;/b&gt;")
	assert.Contains(t, body, `href="/beta/issues/7"`)
	assert.Contains(t, body, "#7")
	assert.Contains(t, body, "2h ago")
	assert.Contains(t, body, `href="/-/inbox?repo=beta"`, "the filter")
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

func TestInboxFilterAndEmptyStates(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	ov := inboxOverview(t)
	filtered := ov
	filtered.MergeRequests, filtered.Issues = nil, nil
	svc.EXPECT().Overview(mock.Anything, mock.Anything, repoName(t, "gamma")).Return(filtered, nil)
	srv := inboxServer(t, svc)

	rec := inboxGet(srv, "/-/inbox?repo=gamma", true)
	require.Equal(t, 200, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "no open merge requests.")
	assert.Contains(t, body, "no open issues.")
	assert.Contains(t, body, `href="/-/inbox"`, "the link back to all")

	assert.Equal(t, 404, inboxGet(srv, "/-/inbox?repo=Bad%20Name", true).Code, "an invalid name is a missing page")
}

func TestInboxButtonOnHomeIsForTheOwnerOnly(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	ov := inboxOverview(t)
	ov.Counts = ports.InboxCounts{MergeRequests: 3, Issues: 2}
	svc.EXPECT().Overview(mock.Anything, mock.Anything, domain.RepoName{}).Return(ov, nil).Once() // the owner's home only
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
	svc.EXPECT().Overview(mock.Anything, mock.Anything, domain.RepoName{}).Return(inboxOverview(t), nil)
	srv := inboxServer(t, svc)
	ls := &liveStream{s: srv, view: "inbox"}
	frags, err := ls.render(withOwner(t.Context()), ports.WatchEvent{ID: "b-1", Event: domain.IssueOpened{Issue: domain.Issue{Repo: repoName(t, "beta"), Number: 7}}})
	require.NoError(t, err)
	require.Len(t, frags, 4)
	assert.Contains(t, frags[0].html, `id="inbox-counts"`)
	assert.Contains(t, frags[1].html, `id="inbox-repos"`)
	assert.Contains(t, frags[2].html, `id="inbox-mrs"`)
	assert.Contains(t, frags[3].html, `id="inbox-issues"`)

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
	svc.EXPECT().Overview(mock.Anything, mock.Anything, domain.RepoName{}).Return(inboxOverview(t), nil).Maybe()
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
		"repo deleted":      domain.RepoDeleted{Repo: repoName(t, "beta")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			after := ports.InboxOverview{Repos: []domain.RepoName{repoName(t, "alpha")}}
			svc := mocks.NewInboxService(t)
			svc.EXPECT().Overview(mock.Anything, mock.Anything, domain.RepoName{}).Return(after, nil)
			srv := inboxServer(t, svc)
			ls := &liveStream{s: srv, view: "inbox"}
			frags, err := ls.render(withOwner(t.Context()), ports.WatchEvent{ID: "b-1", Event: ev})
			require.NoError(t, err)
			require.Len(t, frags, 4)
			all := ""
			for _, f := range frags {
				all += f.html
			}
			assert.Contains(t, all, `id="inbox-issues"`)
			assert.NotContains(t, all, "crash on start", "the issue is gone")
			assert.NotContains(t, all, "/beta", "the repository is gone from the filter")
		})
	}
}
