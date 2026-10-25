// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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

func depsRec(t testing.TB) domain.RepoDeps {
	return domain.RepoDeps{
		Repo: repoName(t, "alpha"), State: domain.DepsFailed, Error: "the run ended: failure", RunID: 12,
		CheckedAt: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
		Updates: []domain.DepUpdate{
			{DepRef: domain.DepRef{Ecosystem: domain.EcosystemGoMod, Directory: "tools", Name: "golang.org/x/text", Version: "v0.21.0"}, Current: "v0.20.0", Kind: domain.DepMinor, New: true},
			{DepRef: domain.DepRef{Ecosystem: domain.EcosystemActions, Name: "actions/checkout", Version: "v5"}, Current: "v4", Kind: domain.DepMajor},
			{DepRef: domain.DepRef{Ecosystem: domain.EcosystemGoMod, Name: "example.com/p", Version: "v1.0.1"}, Current: "v1.0.0", Kind: domain.DepPatch},
		},
	}
}

func depsRun(number int64, status domain.CIStatus) *domain.CIRun {
	return &domain.CIRun{ID: 12, Number: number, Status: status}
}

func depsOverview(t testing.TB) ports.InboxOverview {
	rec := depsRec(t)
	ov := inboxOverview(t)
	ov.Deps = []ports.InboxDeps{{Rec: rec, Run: depsRun(3, domain.CICompleted)}}
	ov.DepsCheckedAt = rec.CheckedAt
	ov.Counts.Deps = 1
	return ov
}

func depsServer(t *testing.T, inbox ports.InboxService, deps ports.DepsService) *Server {
	t.Helper()
	emptyJournal(inbox)
	return newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "alpha")), Browse: homeBrowse(t), Inbox: inbox, Deps: deps,
		Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
	})
}

func TestInboxDepsTable(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Overview(mock.Anything, mock.Anything, domain.RepoName{}).Return(depsOverview(t), nil)
	srv := depsServer(t, svc, mocks.NewDepsService(t))

	rec := inboxGet(srv, "/-/inbox", true)
	require.Equal(t, 200, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "<strong>1</strong> dependency updates")
	assert.Less(t, strings.Index(body, `id="inbox-runs"`), strings.Index(body, `id="inbox-deps"`))
	assert.Less(t, strings.Index(body, `id="inbox-deps"`), strings.Index(body, `id="inbox-mrs"`))
	sec := body[strings.Index(body, `id="inbox-deps"`):strings.Index(body, `id="inbox-mrs"`)]

	assert.Contains(t, sec, " data-keep-open")
	assert.Contains(t, sec, "Dependency updates")
	assert.Contains(t, sec, `checked <time datetime="2026-03-04T05:06:07Z">2026-03-04</time>`)
	assert.Contains(t, sec, "1 repo · 3 updates")
	assert.Contains(t, sec, `<li class="deps-row" id="deps-alpha">`)
	assert.Contains(t, sec, "1 major · 1 minor · 1 patch")
	assert.Contains(t, sec, "new 1")
	assert.Contains(t, sec, `aria-label="Updates of alpha"`)
	assert.Contains(t, sec, `aria-label="More actions for alpha"`)
	for _, want := range []string{"golang.org/x/text", "v0.20.0 → v0.21.0", ">minor<", ">major<", ">patch<", ">tools<"} {
		assert.Contains(t, sec, want)
	}
	assert.Contains(t, sec, `<a href="/alpha/actions/runs/12" title="the run ended: failure">run #3</a>`)
	assert.Contains(t, sec, ">Retry<")
	assert.Contains(t, sec, `action="/-/inbox/deps/alpha/open"`)
	for _, a := range []string{"ignore", "snooze"} {
		assert.Contains(t, sec, `action="/-/inbox/deps/alpha/`+a+`"`)
	}
	assert.Contains(t, sec, `name="version" value="v0.21.0"`)
	assert.Contains(t, sec, `name="directory" value="tools"`)
	assert.Contains(t, sec, `name="ecosystem" value="github-actions"`, "the forms carry the ecosystem")

	// language-agnostic, compact: no ecosystem on screen, no log or error text
	visible := regexp.MustCompile(`<input[^>]*>`).ReplaceAllString(sec, "")
	for _, banned := range []string{"gomod", "go mod", "github-actions", "ecosystem", "pending-list", `class="pending"`, "p-body"} {
		assert.NotContains(t, visible, banned)
	}
	assert.NotContains(t, sec, "FAILED")
	assert.NotContains(t, sec, "APPLYING")
}

func TestInboxDepsSummaryHasNoInteractiveContent(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Overview(mock.Anything, mock.Anything, domain.RepoName{}).Return(depsOverview(t), nil)
	body := inboxGet(depsServer(t, svc, mocks.NewDepsService(t)), "/-/inbox", true).Body.String()
	sec := body[strings.Index(body, `id="inbox-deps"`):strings.Index(body, `id="inbox-mrs"`)]
	summaries := regexp.MustCompile(`(?s)<summary[^>]*>.*?</summary>`).FindAllString(sec, -1)
	require.Len(t, summaries, 2, "the row and its menu")
	for _, s := range summaries {
		for _, tag := range []string{"<form", "<button", "<input", "<select"} {
			assert.NotContains(t, s, tag)
		}
	}
	assert.NotContains(t, summaries[0], "<a ", "the summary of the row holds no link")
	assert.Contains(t, summaries[1], "⋯")
	// the menu is a details.menu, closed
	assert.Contains(t, sec, `<details class="menu deps-menu">`)
	assert.NotContains(t, sec, "<details open")
}

func TestInboxDepsRowMapping(t *testing.T) {
	t.Parallel()
	srv := depsServer(t, mocks.NewInboxService(t), mocks.NewDepsService(t))
	alpha := repoName(t, "alpha")
	const base = "/-/inbox/deps/alpha/"
	run := func(n int64, st domain.CIStatus) *domain.CIRun { return depsRun(n, st) }

	tests := []struct {
		name   string
		mutate func(d *domain.RepoDeps)
		run    *domain.CIRun
		filter domain.RepoName

		status  InboxDepsStatus
		primary InboxDepsAction
		menu    [2]string // Ignore, Snooze
	}{
		{
			name:    "idle without a merge request",
			mutate:  func(d *domain.RepoDeps) { d.State, d.Error, d.RunID = domain.DepsIdle, "", 0 },
			status:  InboxDepsStatus{},
			primary: InboxDepsAction{Label: "Open MR", Action: base + "open"},
			menu:    [2]string{base + "ignore", base + "snooze"},
		},
		{
			name: "idle with a merge request that differs",
			mutate: func(d *domain.RepoDeps) {
				d.State, d.Error, d.RunID = domain.DepsIdle, "", 0
				d.MR, d.MRVersions = 7, d.Refs()[:1]
			},
			status:  InboxDepsStatus{MR: 7, MRHref: "/alpha/merge-requests/7"},
			primary: InboxDepsAction{Label: "Update MR", Action: base + "open"},
			menu:    [2]string{base + "ignore", base + "snooze"},
		},
		{
			name: "idle with a merge request up to date",
			mutate: func(d *domain.RepoDeps) {
				d.State, d.Error = domain.DepsIdle, ""
				d.MR, d.MRVersions = 7, d.Refs()
			},
			run:     run(3, domain.CICompleted),
			status:  InboxDepsStatus{MR: 7, MRHref: "/alpha/merge-requests/7"},
			primary: InboxDepsAction{Label: "View MR", Href: "/alpha/merge-requests/7"},
			menu:    [2]string{"", base + "snooze"},
		},
		{
			name:   "applying, run queued",
			mutate: func(d *domain.RepoDeps) { d.State, d.Error = domain.DepsApplying, "" },
			run:    run(3, domain.CIQueued),
			status: InboxDepsStatus{Kind: "queued", State: "queued", Text: "run #3", Href: "/alpha/actions/runs/12"},
		},
		{
			name:   "applying, run held back by a concurrency group",
			mutate: func(d *domain.RepoDeps) { d.State, d.Error = domain.DepsApplying, "" },
			run:    run(3, domain.CIPending),
			status: InboxDepsStatus{Kind: "queued", State: "queued", Text: "run #3", Href: "/alpha/actions/runs/12"},
		},
		{
			name:   "applying, the run is not readable",
			mutate: func(d *domain.RepoDeps) { d.State, d.Error = domain.DepsApplying, "" },
			status: InboxDepsStatus{Kind: "running", State: "applying"},
		},
		{
			name:   "applying, the run ended and the record is not settled yet",
			mutate: func(d *domain.RepoDeps) { d.State, d.Error = domain.DepsApplying, "" },
			run:    run(3, domain.CICompleted),
			status: InboxDepsStatus{Kind: "running", State: "applying", Text: "run #3", Href: "/alpha/actions/runs/12"},
		},
		{
			name:   "applying, run in progress",
			mutate: func(d *domain.RepoDeps) { d.State, d.Error = domain.DepsApplying, "" },
			run:    run(3, domain.CIInProgress),
			status: InboxDepsStatus{Kind: "running", State: "running", Text: "run #3", Href: "/alpha/actions/runs/12"},
		},
		{
			name: "applying with a merge request open keeps the link to it",
			mutate: func(d *domain.RepoDeps) {
				d.State, d.Error = domain.DepsApplying, ""
				d.MR, d.MRVersions = 7, d.Refs()
			},
			run:    run(3, domain.CIInProgress),
			status: InboxDepsStatus{Kind: "running", State: "running", Text: "run #3", Href: "/alpha/actions/runs/12", MR: 7, MRHref: "/alpha/merge-requests/7"},
		},
		{
			name:    "failed with a run",
			mutate:  func(d *domain.RepoDeps) {},
			run:     run(3, domain.CICompleted),
			status:  InboxDepsStatus{Kind: "failed", State: "failed", Text: "run #3", Href: "/alpha/actions/runs/12", Title: "the run ended: failure"},
			primary: InboxDepsAction{Label: "Retry", Action: base + "open"},
			menu:    [2]string{base + "ignore", base + "snooze"},
		},
		{
			name:    "failed, the run is gone",
			mutate:  func(d *domain.RepoDeps) { d.Error = "the run no longer exists" },
			status:  InboxDepsStatus{Kind: "failed", State: "failed", Title: "the run no longer exists"},
			primary: InboxDepsAction{Label: "Retry", Action: base + "open"},
			menu:    [2]string{base + "ignore", base + "snooze"},
		},
		{
			name: "failed with a merge request that holds the versions still retries",
			mutate: func(d *domain.RepoDeps) {
				d.MR, d.MRVersions = 7, d.Refs()
			},
			status:  InboxDepsStatus{Kind: "failed", State: "failed", Title: "the run ended: failure", MR: 7, MRHref: "/alpha/merge-requests/7"},
			primary: InboxDepsAction{Label: "Retry", Action: base + "open"},
			menu:    [2]string{base + "ignore", base + "snooze"},
		},
		{
			name:    "the filter is kept on the targets",
			mutate:  func(d *domain.RepoDeps) { d.State, d.Error, d.RunID = domain.DepsIdle, "", 0 },
			filter:  alpha,
			primary: InboxDepsAction{Label: "Open MR", Action: base + "open?repo=alpha"},
			menu:    [2]string{base + "ignore?repo=alpha", base + "snooze?repo=alpha"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := depsRec(t)
			tc.mutate(&rec)
			row := srv.depsRow(ports.InboxDeps{Rec: rec, Run: tc.run}, tc.filter)
			assert.Equal(t, tc.status, row.Status)
			assert.Equal(t, tc.primary, row.Primary)
			assert.Equal(t, tc.menu, [2]string{row.Ignore, row.Snooze})
		})
	}
}

// Whatever the state, the row names the repository and summarises its updates.
func TestInboxDepsRowSummarisesTheUpdates(t *testing.T) {
	t.Parallel()
	srv := depsServer(t, mocks.NewInboxService(t), mocks.NewDepsService(t))
	row := srv.depsRow(ports.InboxDeps{Rec: depsRec(t), Run: depsRun(3, domain.CICompleted)}, domain.RepoName{})
	assert.Equal(t, "deps-alpha", row.ID)
	assert.Equal(t, "alpha", row.Repo)
	assert.Equal(t, 3, row.Count)
	assert.Equal(t, 1, row.New)
	assert.Equal(t, "1 major · 1 minor · 1 patch", row.Breakdown)
	assert.Len(t, row.Updates, 3)
}

func TestDepsBreakdownOmitsZeroParts(t *testing.T) {
	t.Parallel()
	up := func(k domain.DepKind, n int) (out []domain.DepUpdate) {
		for range n {
			out = append(out, domain.DepUpdate{Kind: k})
		}
		return out
	}
	assert.Equal(t, "", depsBreakdown(nil))
	assert.Equal(t, "3 patch", depsBreakdown(up(domain.DepPatch, 3)))
	assert.Equal(t, "1 major · 3 patch", depsBreakdown(append(up(domain.DepPatch, 3), up(domain.DepMajor, 1)...)))
	assert.Equal(t, "2 major · 1 minor · 3 patch", depsBreakdown(append(append(up(domain.DepPatch, 3), up(domain.DepMinor, 1)...), up(domain.DepMajor, 2)...)))
}

func TestInboxDepsWithoutDepsServiceHasNoButtons(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Overview(mock.Anything, mock.Anything, domain.RepoName{}).Return(depsOverview(t), nil)
	srv := newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "alpha")), Browse: homeBrowse(t), Inbox: svc,
		Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
	})
	body := inboxGet(srv, "/-/inbox", true).Body.String()
	sec := body[strings.Index(body, `id="inbox-deps"`):strings.Index(body, `id="inbox-mrs"`)]
	assert.NotContains(t, sec, "<form")
	assert.NotContains(t, sec, "More actions")
}

func TestInboxDepsEmptyFragmentIsHidden(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Overview(mock.Anything, mock.Anything, domain.RepoName{}).Return(inboxOverview(t), nil)
	body := inboxGet(depsServer(t, svc, mocks.NewDepsService(t)), "/-/inbox", true).Body.String()
	assert.Regexp(t, `<section id="inbox-deps" data-keep-open hidden>`, body)
	assert.NotContains(t, body, "Dependency updates<")
}

func TestInboxDepsPageIsMissingForEveryoneElse(t *testing.T) {
	t.Parallel()
	srv := depsServer(t, mocks.NewInboxService(t), mocks.NewDepsService(t))
	assert.Equal(t, 404, inboxGet(srv, "/-/inbox", false).Code)

	req := httptest.NewRequest(http.MethodPost, "/-/inbox/deps/alpha/ignore", nil)
	req.Header.Set("Origin", testPublicURL)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	assert.Equal(t, 403, rec.Code, "the gate refuses a POST without a session, as for the other inbox actions")
}

func TestInboxDepsActions(t *testing.T) {
	t.Parallel()
	alpha := repoName(t, "alpha")
	post := func(srv *Server, path string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", testPublicURL)
		req.AddCookie(&http.Cookie{Name: CookieName, Value: "tok"})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	form := url.Values{
		"ecosystem": {"gomod", "github-actions"}, "directory": {"tools", ""},
		"name": {"golang.org/x/text", "actions/checkout"}, "version": {"v0.21.0", "v5"},
	}
	want := []domain.DepRef{
		{Ecosystem: domain.EcosystemGoMod, Directory: "tools", Name: "golang.org/x/text", Version: "v0.21.0"},
		{Ecosystem: domain.EcosystemActions, Name: "actions/checkout", Version: "v5"},
	}
	t.Run("ignore", func(t *testing.T) {
		t.Parallel()
		deps := mocks.NewDepsService(t)
		deps.EXPECT().Ignore(mock.Anything, mock.Anything, alpha, want).Return(domain.RepoDeps{}, nil).Once()
		rec := post(depsServer(t, mocks.NewInboxService(t), deps), "/-/inbox/deps/alpha/ignore?repo=alpha", form)
		assert.Equal(t, 303, rec.Code)
		assert.Equal(t, "/-/inbox?repo=alpha", rec.Header().Get("Location"))
	})
	t.Run("open", func(t *testing.T) {
		t.Parallel()
		deps := mocks.NewDepsService(t)
		deps.EXPECT().Open(mock.Anything, mock.Anything, alpha, want).Return(domain.RepoDeps{}, nil).Once()
		rec := post(depsServer(t, mocks.NewInboxService(t), deps), "/-/inbox/deps/alpha/open", form)
		assert.Equal(t, 303, rec.Code)
		assert.Equal(t, "/-/inbox", rec.Header().Get("Location"))
	})
	t.Run("snooze", func(t *testing.T) {
		t.Parallel()
		deps := mocks.NewDepsService(t)
		deps.EXPECT().Snooze(mock.Anything, mock.Anything, alpha).Return(domain.RepoDeps{}, nil).Once()
		rec := post(depsServer(t, mocks.NewInboxService(t), deps), "/-/inbox/deps/alpha/snooze", nil)
		assert.Equal(t, 303, rec.Code)
	})
	t.Run("stale", func(t *testing.T) {
		t.Parallel()
		deps := mocks.NewDepsService(t)
		deps.EXPECT().Ignore(mock.Anything, mock.Anything, alpha, want).Return(domain.RepoDeps{}, domain.ErrPreconditionFailed).Once()
		rec := post(depsServer(t, mocks.NewInboxService(t), deps), "/-/inbox/deps/alpha/ignore", form)
		assert.NotEqual(t, 303, rec.Code)
	})
	t.Run("malformed", func(t *testing.T) {
		t.Parallel()
		rec := post(depsServer(t, mocks.NewInboxService(t), mocks.NewDepsService(t)), "/-/inbox/deps/alpha/ignore", url.Values{"name": {"x"}})
		assert.NotEqual(t, 303, rec.Code)
	})
}

func TestInboxDepsLiveFragment(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Overview(mock.Anything, mock.Anything, domain.RepoName{}).Return(depsOverview(t), nil)
	srv := depsServer(t, svc, mocks.NewDepsService(t))
	ls := &liveStream{s: srv, view: "inbox"}
	frags, err := ls.render(withOwner(t.Context()), ports.WatchEvent{ID: "b-1", Event: domain.DependencyUpdatesFound{Repo: repoName(t, "alpha"), Count: 2}})
	require.NoError(t, err)
	all := ""
	for _, f := range frags {
		all += f.html
	}
	assert.Contains(t, all, "golang.org/x/text")
	assert.True(t, liveEvent(ports.WatchEvent{Event: domain.DependencyUpdatesFound{}}))
}

func TestInboxDepsChangedRefreshesTheLiveView(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Overview(mock.Anything, mock.Anything, domain.RepoName{}).Return(depsOverview(t), nil)
	ls := &liveStream{s: depsServer(t, svc, mocks.NewDepsService(t)), view: "inbox"}
	ev := ports.WatchEvent{ID: "b-2", Event: domain.DependencyUpdatesChanged{Repo: repoName(t, "alpha")}}
	assert.True(t, liveEvent(ev))
	frags, err := ls.render(withOwner(t.Context()), ev)
	require.NoError(t, err)
	assert.NotEmpty(t, frags)
}
