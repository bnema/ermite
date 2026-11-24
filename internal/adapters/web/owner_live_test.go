package web

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// ownerLiveServer is a server whose inbox answers only what the test
// expects, with deps and traffic configured.
func ownerLiveServer(t *testing.T, svc *mocks.InboxService) *Server {
	t.Helper()
	return newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "alpha")), Browse: homeBrowse(t), Inbox: svc,
		Deps: mocks.NewDepsService(t), Traffic: mocks.NewTrafficService(t),
		Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
	})
}

// Each owner view has its stream, the owner's alone; a view the server does
// not have, an unknown view or range is the unknown-topic answer.
func TestOwnerStreamPerView(t *testing.T) {
	t.Parallel()
	f := &watchProbe{release: make(chan struct{})}
	svc := mocks.NewInboxService(t)
	cfg := Config{
		Repos: reposServing(t, publicRepo(t, "x")), Browse: mocks.NewBrowseService(t), Events: f.service(t), Inbox: svc,
		Heartbeat: time.Hour, Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
	}
	bare := httptest.NewServer(newTestServer(t, cfg))
	t.Cleanup(bare.Close)
	cfg.Deps, cfg.Traffic = mocks.NewDepsService(t), mocks.NewTrafficService(t)
	full := httptest.NewServer(newTestServer(t, cfg))
	t.Cleanup(full.Close)
	owner := map[string]string{"Cookie": CookieName + "=tok"}

	unknown, _ := open(t, full, "topic=nope&view=inbox", nil)
	want, _ := io.ReadAll(unknown.Body)
	for name, tc := range map[string]struct {
		ts    *httptest.Server
		query string
		hdr   map[string]string
	}{
		"anonymous work":                {full, "view=work", nil},
		"anonymous deps":                {full, "view=deps", nil},
		"bad view":                      {full, "view=summary", owner},
		"no view":                       {full, "", owner},
		"bad activity range":            {full, "view=activity&range=1y", owner},
		"bad insights range":            {full, "view=insights&range=1y", owner},
		"deps unconfigured":             {bare, "view=deps", owner},
		"insights unconfigured":         {bare, "view=insights", owner},
		"insights unconfigured, ranged": {bare, "view=insights&range=30d", owner},
	} {
		resp, _ := open(t, tc.ts, "topic=owner&"+tc.query, tc.hdr)
		body, _ := io.ReadAll(resp.Body)
		assert.Equal(t, unknown.StatusCode, resp.StatusCode, name)
		assert.Equal(t, string(want), string(body), name)
	}
	assert.Zero(t, f.opened.Load(), "no watch is opened for them")

	for _, q := range []string{"view=inbox", "view=work", "view=activity&range=7d", "view=deps&sort=name", "view=insights&range=30d"} {
		resp, _ := open(t, full, "topic=owner&"+q, owner)
		assert.Equal(t, http.StatusOK, resp.StatusCode, q)
	}
	for _, q := range []string{"view=inbox", "view=work", "view=activity"} {
		resp, _ := open(t, bare, "topic=owner&"+q, owner)
		assert.Equal(t, http.StatusOK, resp.StatusCode, q)
	}
}

// A view re-renders only its live sections for the events of its set, and
// the navigation for the events that can change its counts, read once.
func TestOwnerLiveSets(t *testing.T) {
	t.Parallel()
	alpha := repoName(t, "alpha")
	queued := domain.RunUpdated{Run: domain.CIRun{Repo: alpha, Status: domain.CIQueued}}
	running := domain.RunUpdated{Run: domain.CIRun{Repo: alpha, Status: domain.CIInProgress}}
	done := domain.RunUpdated{Run: domain.CIRun{Repo: alpha, Status: domain.CICompleted}}
	sections := map[string][]string{
		"inbox":    {"attention"},
		"work":     {"work-mrs", "work-issues"},
		"activity": {"activity-runs", "activity-journal"},
		"deps":     {"deps"},
		"insights": nil,
	}
	all := []string{"attention", "work-mrs", "work-issues", "activity-runs", "activity-journal", "deps", "insights"}
	for _, tc := range []struct {
		name string
		ev   domain.Event
		in   []string // the views whose sections the event re-renders
		nav  bool
	}{
		{"issue submitted", domain.IssueSubmitted{Repo: alpha}, []string{"inbox"}, true},
		{"pending removed", domain.IssuePendingRemoved{Repo: alpha}, []string{"inbox"}, true},
		{"issue opened", domain.IssueOpened{Issue: domain.Issue{Repo: alpha}}, []string{"work", "activity"}, true},
		{"issue edited", domain.IssueEdited{Issue: domain.Issue{Repo: alpha}}, []string{"work"}, false},
		{"issue resolved", domain.IssueResolved{Issue: domain.Issue{Repo: alpha}}, []string{"work", "activity"}, true},
		{"issue closed", domain.IssueClosed{Issue: domain.Issue{Repo: alpha}}, []string{"work", "activity"}, true},
		{"issue reopened", domain.IssueReopened{Issue: domain.Issue{Repo: alpha}}, []string{"work"}, true},
		{"issue commented", domain.IssueCommented{}, nil, false},
		{"mr opened", domain.MergeRequestOpened{}, []string{"inbox", "work", "activity"}, true},
		{"mr merged", domain.MergeRequestMerged{}, []string{"inbox", "work", "activity"}, true},
		{"mr closed", domain.MergeRequestClosed{}, []string{"inbox", "work", "activity"}, true},
		{"mr edited", domain.MergeRequestEdited{}, []string{"inbox", "work"}, true},
		{"mr ready", domain.MergeRequestReady{}, []string{"inbox", "work"}, true},
		{"comment", domain.CommentAdded{}, nil, false},
		{"mergeability", domain.MergeRequestMergeability{}, nil, false},
		{"push", domain.PushEvent{Repo: alpha}, nil, false}, // never on the owner topic: its JournalUpdated is
		{"run queued", queued, []string{"inbox", "work", "activity", "deps"}, true},
		{"run in progress", running, []string{"work", "activity", "deps"}, false},
		{"run completed", done, []string{"inbox", "work", "activity", "deps"}, true},
		{"job", domain.JobUpdated{}, nil, false},
		{"journal", domain.JournalUpdated{Repo: alpha}, []string{"inbox", "activity"}, true},
		{"release published", domain.ReleasePublished{Release: domain.Release{Repo: alpha}}, []string{"inbox", "activity"}, true},
		{"release updated", domain.ReleaseUpdated{}, nil, false},
		{"deps found", domain.DependencyUpdatesFound{Repo: alpha}, []string{"inbox", "deps"}, true},
		{"deps changed", domain.DependencyUpdatesChanged{Repo: alpha}, []string{"inbox", "deps"}, true},
		{"repo updated", domain.RepoUpdated{Repo: alpha}, []string{"inbox", "work", "activity", "deps"}, true},
		{"repo deleted", domain.RepoDeleted{Repo: alpha}, []string{"inbox", "work", "activity", "deps"}, true},
	} {
		for view, ids := range sections {
			t.Run(tc.name+"/"+view, func(t *testing.T) {
				t.Parallel()
				in := false
				for _, v := range tc.in {
					in = in || v == view
				}
				svc := mocks.NewInboxService(t)
				counts := ports.OwnerCounts{Attention: 3, Work: 4}
				if view == "inbox" && (in || tc.nav) {
					// One read gives the rows and the nav counts.
					svc.EXPECT().Attention(mock.Anything, mock.Anything).Return(ports.OwnerAttention{Counts: counts}, nil).Once()
				}
				if in {
					switch view {
					case "work":
						svc.EXPECT().Work(mock.Anything, mock.Anything).Return(ports.InboxWork{}, nil).Once()
					case "activity":
						svc.EXPECT().Runs(mock.Anything, mock.Anything).Return(ports.InboxRuns{}, nil).Once()
						svc.EXPECT().Journal(mock.Anything, mock.Anything, 24*time.Hour).Return(ports.InboxJournal{}, nil).Once()
					case "deps":
						svc.EXPECT().Deps(mock.Anything, mock.Anything).Return(ports.InboxDepsView{}, nil).Once()
					}
				}
				if tc.nav && view != "inbox" {
					svc.EXPECT().Counts(mock.Anything, mock.Anything).Return(counts, nil).Once()
				}
				ls := &liveStream{s: ownerLiveServer(t, svc), forOwner: true, view: view}
				frags, err := ls.render(withOwner(t.Context()), ports.WatchEvent{ID: "b-1", Event: tc.ev})
				require.NoError(t, err)
				want := in && len(ids) > 0
				if !want && !tc.nav {
					assert.Empty(t, frags)
					return
				}
				require.Len(t, frags, 1)
				assert.Equal(t, "owner", frags[0].event)
				html := frags[0].html
				for _, id := range all {
					shown := false
					if want {
						for _, w := range ids {
							shown = shown || w == id
						}
					}
					assert.Equal(t, shown, strings.Contains(html, `id="`+id+`"`), id)
				}
				assert.Equal(t, tc.nav, strings.Contains(html, `id="owner-nav"`), "owner-nav")
				if tc.nav {
					assert.Contains(t, html, `aria-current="page"`, "the view is current in the nav")
					assert.NotContains(t, html, "hx-swap-oob", "the live swap replaces it by id")
				}
			})
		}
	}
}

// A stream that is not the owner topic, or a visitor who is not signed in,
// never reads the inbox, whatever its view.
func TestOwnerLiveNeedsTheOwnerTopic(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	srv := ownerLiveServer(t, svc)
	ev := ports.WatchEvent{Event: domain.IssueSubmitted{Repo: repoName(t, "alpha")}}
	frags, err := (&liveStream{s: srv, forOwner: true, view: "work"}).render(t.Context(), ev)
	require.NoError(t, err)
	assert.Empty(t, frags, "anonymous")
	home, err := (&liveStream{s: srv, view: "work"}).render(withOwner(t.Context()), ports.WatchEvent{Event: domain.MergeRequestEdited{}})
	require.NoError(t, err)
	for _, f := range home {
		assert.NotEqual(t, "owner", f.event, "a home stream with view=work is not the owner's")
	}
	frags, err = (&liveStream{s: srv, forOwner: true, view: "work"}).owner(t.Context(), ev)
	require.NoError(t, err)
	assert.Empty(t, frags)
}

// The insights view has no live section: only the navigation follows.
func TestInboxTrafficStream(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Counts(mock.Anything, mock.Anything).Return(ports.OwnerCounts{Attention: 1}, nil).Once()
	srv := inboxServer(t, svc)
	srv.cfg.Traffic = mocks.NewTrafficService(t)
	q := url.Values{"view": {"insights"}, "range": {"30d"}}
	assert.True(t, srv.ownerStreamOK(withOwner(t.Context()), q))
	assert.False(t, srv.ownerStreamOK(t.Context(), q))
	ls := &liveStream{s: srv, forOwner: true, view: "insights", rng: "30d"}
	none, err := ls.render(withOwner(t.Context()), ports.WatchEvent{Event: domain.IssueEdited{}})
	require.NoError(t, err)
	assert.Empty(t, none, "no traffic refresh")
	frags, err := ls.render(withOwner(t.Context()), ports.WatchEvent{Event: domain.IssueSubmitted{Repo: repoName(t, "alpha")}})
	require.NoError(t, err)
	require.Len(t, frags, 1)
	assert.Contains(t, frags[0].html, `id="owner-nav"`)
	assert.NotContains(t, frags[0].html, `id="insights"`)
}

// The owner's stream takes the range of the page: an unknown one is the
// unknown-topic answer (as the page is a 404) and opens no watch; a known one
// is the range of the journal its fragments render.
func TestInboxJournalStreamRange(t *testing.T) {
	t.Parallel()
	f := &watchProbe{release: make(chan struct{}), push: make(chan ports.WatchEvent)}
	svc := mocks.NewInboxService(t)
	expectInbox(svc, inboxFixture{})
	svc.EXPECT().Journal(mock.Anything, mock.Anything, 7*24*time.Hour).Return(sampleJournal(t), nil).Once()
	srv := newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "x")), Browse: mocks.NewBrowseService(t), Events: f.service(t), Inbox: svc,
		Heartbeat: time.Hour, Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	owner := map[string]string{"Cookie": CookieName + "=tok"}

	unknown, _ := open(t, ts, "topic=nope&view=activity", nil)
	want, _ := io.ReadAll(unknown.Body)
	bad, _ := open(t, ts, "topic=owner&view=activity&range=1y", owner)
	body, _ := io.ReadAll(bad.Body)
	assert.Equal(t, unknown.StatusCode, bad.StatusCode)
	assert.Equal(t, string(want), string(body), "the unknown-topic response")
	assert.Zero(t, f.opened.Load(), "no watch is opened")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+activityPath+"?range=1y", nil)
	require.NoError(t, err)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "tok"})
	page, err := ts.Client().Do(req)
	require.NoError(t, err)
	_ = page.Body.Close()
	assert.Equal(t, http.StatusNotFound, page.StatusCode, "the page")

	resp, _ := open(t, ts, "topic=owner&view=activity&range=7d", owner)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	rd := bufio.NewReader(resp.Body)
	f.push <- ports.WatchEvent{ID: "b-1", Event: domain.JournalUpdated{Repo: repoName(t, "alpha")}}
	var journal string
	for journal == "" {
		line, err := rd.ReadString('\n')
		require.NoError(t, err)
		if strings.HasPrefix(line, "data:") && strings.Contains(line, `id="activity-journal"`) {
			journal = line
		}
	}
	assert.Contains(t, journal, `aria-current="page">7 days</a>`, "rendered for the range of the stream")
}

// The journal signal, a push and a published release refresh the activity
// on the range of the stream.
func TestInboxJournalLive(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Journal(mock.Anything, mock.Anything, 7*24*time.Hour).Return(sampleJournal(t), nil)
	expectInbox(svc, inboxFixture{})
	srv := inboxServer(t, svc)
	ls := &liveStream{s: srv, forOwner: true, view: "activity", rng: "7d"}
	for _, ev := range []domain.Event{
		domain.JournalUpdated{Repo: repoName(t, "alpha")},
		domain.ReleasePublished{Release: domain.Release{Repo: repoName(t, "beta")}},
	} {
		frags, err := ls.render(withOwner(t.Context()), ports.WatchEvent{ID: "b-1", Event: ev})
		require.NoError(t, err)
		require.Len(t, frags, 1, "%T", ev)
		assert.Contains(t, frags[0].html, `id="activity-runs"`)
		assert.Contains(t, frags[0].html, `id="activity-journal"`)
		assert.Contains(t, frags[0].html, "add &lt;search&gt;")
		assert.Contains(t, frags[0].html, `aria-current="page">7 days</a>`)
	}
	none, err := ls.render(withOwner(t.Context()), ports.WatchEvent{Event: domain.CommentAdded{}})
	require.NoError(t, err)
	assert.Empty(t, none)
	assert.True(t, liveEvent(ports.WatchEvent{Event: domain.JournalUpdated{}}))
}

func TestInboxDepsLiveFragment(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	expectInbox(svc, depsOverview(t))
	srv := depsServer(t, svc, mocks.NewDepsService(t))
	ls := &liveStream{s: srv, forOwner: true, view: "deps", sort: "name"}
	frags, err := ls.render(withOwner(t.Context()), ports.WatchEvent{ID: "b-1", Event: domain.DependencyUpdatesFound{Repo: repoName(t, "alpha"), Count: 2}})
	require.NoError(t, err)
	require.Len(t, frags, 1)
	assert.Contains(t, frags[0].html, `id="deps"`)
	assert.Contains(t, frags[0].html, "golang.org/x/text")
	assert.Contains(t, frags[0].html, `<a href="/-/deps?sort=name" data-nav="keep-scroll" aria-current="page">name</a>`, "rendered with the sort of the stream")
	assert.True(t, liveEvent(ports.WatchEvent{Event: domain.DependencyUpdatesFound{}}))
}

func TestInboxDepsChangedRefreshesTheLiveView(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	expectInbox(svc, depsOverview(t))
	ls := &liveStream{s: depsServer(t, svc, mocks.NewDepsService(t)), forOwner: true, view: "deps"}
	ev := ports.WatchEvent{ID: "b-2", Event: domain.DependencyUpdatesChanged{Repo: repoName(t, "alpha")}}
	assert.True(t, liveEvent(ev))
	frags, err := ls.render(withOwner(t.Context()), ev)
	require.NoError(t, err)
	require.Len(t, frags, 1)
	assert.Contains(t, frags[0].html, `id="deps"`)
}

// Every owner page follows its own stream, with the query it shows.
func TestOwnerPagesAreLive(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	expectInbox(svc, inboxFixture{})
	svc.EXPECT().Journal(mock.Anything, mock.Anything, mock.Anything).Return(ports.InboxJournal{}, nil).Maybe()
	f := mocks.NewTrafficService(t)
	f.EXPECT().Forge(mock.Anything, mock.Anything).Return(ports.TrafficForge{}, nil).Maybe()
	srv := newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "alpha")), Browse: homeBrowse(t), Inbox: svc, Deps: mocks.NewDepsService(t), Traffic: f,
		Events: (&watchProbe{release: make(chan struct{})}).service(t), Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
	})
	for path, want := range map[string]string{
		inboxPath:                         "/-/events?topic=owner&amp;view=inbox",
		workPath:                          "/-/events?topic=owner&amp;view=work",
		activityPath + "?range=7d":        "/-/events?topic=owner&amp;view=activity&amp;range=7d",
		depsPath + "?sort=name&filter=go": "/-/events?topic=owner&amp;view=deps&amp;",
		insightsPath:                      "/-/events?topic=owner&amp;view=insights&amp;range=",
	} {
		rec := inboxGet(srv, path, true)
		require.Equal(t, http.StatusOK, rec.Code, path)
		assert.Contains(t, rec.Body.String(), want, path)
	}
}

// The owner's home keeps today's rows and adds the owner navigation for the
// events that can change its counts, read once.
func TestOwnerHomeFollowsTheNav(t *testing.T) {
	t.Parallel()
	repo := repoName(t, "proj")
	browse := homeBrowse(t)
	browse.EXPECT().ListCommits(mock.Anything, mock.Anything, repo, mock.Anything).Return(ports.HistoryPage{Commits: homeCommits(t, 2)}, nil)
	svc := mocks.NewInboxService(t)
	srv := newTestServer(t, Config{Repos: reposServing(t, pushedRepo(t, "proj")), Browse: browse, Inbox: svc})
	ls := &liveStream{s: srv, homeNav: true}

	svc.EXPECT().Counts(mock.Anything, mock.Anything).Return(ports.OwnerCounts{Attention: 1, Work: 2}, nil).Once()
	got, err := ls.render(withOwner(t.Context()), ports.WatchEvent{Event: domain.IssueSubmitted{Repo: repo}})
	require.NoError(t, err)
	require.Len(t, got, 1, "only the navigation")
	assert.Equal(t, "owner", got[0].event)
	assert.Contains(t, got[0].html, `id="owner-nav"`)
	assert.Contains(t, got[0].html, `aria-label="Repositories" aria-current="page"`)

	got, err = ls.render(withOwner(t.Context()), ports.WatchEvent{Event: domain.PushEvent{Repo: repo}})
	require.NoError(t, err)
	require.Len(t, got, 1, "the row; the counts follow the JournalUpdated of the push")
	assert.Equal(t, "home-list", got[0].event)
	assert.Contains(t, got[0].html, `id="repo-proj"`)

	svc.EXPECT().Counts(mock.Anything, mock.Anything).Return(ports.OwnerCounts{}, nil).Once()
	got, err = ls.render(withOwner(t.Context()), ports.WatchEvent{Event: domain.JournalUpdated{Repo: repo}})
	require.NoError(t, err)
	require.Len(t, got, 1, "only the navigation")
	assert.Equal(t, "owner", got[0].event)
	assert.Contains(t, got[0].html, `id="owner-nav"`)

	svc.EXPECT().Counts(mock.Anything, mock.Anything).Return(ports.OwnerCounts{}, nil).Once()
	got, err = ls.render(withOwner(t.Context()), ports.WatchEvent{Event: domain.DependencyUpdatesFound{Repo: repo}})
	require.NoError(t, err)
	require.Len(t, got, 1, "no home row for an owner-only event")
	assert.Equal(t, "owner", got[0].event)

	for _, ev := range []domain.Event{
		domain.RunUpdated{Run: domain.CIRun{Repo: repo, ID: 9, Status: domain.CIInProgress}},
	} {
		got, err = ls.render(withOwner(t.Context()), ports.WatchEvent{Event: ev})
		require.NoError(t, err)
		for _, f := range got {
			assert.NotEqual(t, "owner", f.event, "%T: no navigation", ev)
		}
	}
	svc.AssertNumberOfCalls(t, "Counts", 3)
}

// Only the owner's home watches the owner topic; an anonymous home never
// sends the owner navigation.
func TestHomeStreamTopics(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		hdr  map[string]string
		want []string
	}{
		"anonymous": {nil, []string{"home"}},
		"owner":     {map[string]string{"Cookie": CookieName + "=tok"}, []string{"home", "owner"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := &watchProbe{release: make(chan struct{}), events: []ports.WatchEvent{
				{ID: "b-1", Event: domain.IssueSubmitted{Repo: repoName(t, "x")}},
				{Gap: true},
			}}
			svc := mocks.NewInboxService(t)
			svc.EXPECT().Counts(mock.Anything, mock.Anything).Return(ports.OwnerCounts{Attention: 1}, nil).Maybe()
			srv := newTestServer(t, Config{
				Repos: reposServing(t, publicRepo(t, "x")), Browse: mocks.NewBrowseService(t), Events: f.service(t), Inbox: svc,
				Heartbeat: time.Hour, Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
			})
			ts := httptest.NewServer(srv)
			t.Cleanup(ts.Close)
			resp, _ := open(t, ts, "topic=home", tc.hdr)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			sc := bufio.NewScanner(resp.Body)
			sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
			var all strings.Builder
			for sc.Scan() {
				all.WriteString(sc.Text() + "\n")
				if sc.Text() == "data: gap" {
					break
				}
			}
			assert.Equal(t, tc.want, f.topics.Load())
			assert.Equal(t, name == "owner", strings.Contains(all.String(), `id="owner-nav"`))
			assert.Equal(t, name == "owner", strings.Contains(all.String(), "event: owner"))
		})
	}
}

// A form too large on an owner action names that action and returns to its
// page, even for a repository whose name contains "issues".
func TestOwnerFormTooLargeBack(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "issues-x")), Browse: homeBrowse(t)})
	for path, want := range map[string]string{
		depsPath + "/issues-x/open":             "a dependency action",
		moderationPath + "/issues-x/id/approve": "a moderation action",
		"/issues-x/issues/1/close":              "an issue",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("a="+strings.Repeat("b", maxFormBytes+1)))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetPathValue("repo", "issues-x")
		rec := httptest.NewRecorder()
		require.False(t, srv.form(rec, req), path)
		assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, path)
		assert.Contains(t, rec.Body.String(), "larger than "+want+" can be", path)
	}
}
