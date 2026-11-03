// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"html"
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
		Repo:      repoName(t, "alpha"),
		CheckedAt: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC), Batches: []domain.DepBatch{{State: domain.DepsFailed, Error: "the run ended: failure", RunID: 12,
			Updates: []domain.DepUpdate{
				{DepRef: domain.DepRef{Ecosystem: domain.EcosystemGoMod, Directory: "tools", Name: "golang.org/x/text", Version: "v0.21.0"}, Current: "v0.20.0", Kind: domain.DepMinor, New: true},
				{DepRef: domain.DepRef{Ecosystem: domain.EcosystemActions, Name: "actions/checkout", Version: "v5"}, Current: "v4", Kind: domain.DepMajor},
				{DepRef: domain.DepRef{Ecosystem: domain.EcosystemGoMod, Name: "example.com/p", Version: "v1.0.1"}, Current: "v1.0.0", Kind: domain.DepPatch},
			}}}}
}

func depsRun(number int64, status domain.CIStatus) *domain.CIRun {
	return &domain.CIRun{ID: 12, Number: number, Status: status}
}

func depsOverview(t testing.TB) ports.InboxOverview {
	rec := depsRec(t)
	ov := inboxOverview(t)
	ov.Deps = []ports.InboxDeps{{Rec: rec, Batch: *rec.Base(), Run: depsRun(3, domain.CICompleted)}}
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
	svc.EXPECT().Overview(mock.Anything, mock.Anything).Return(depsOverview(t), nil)
	srv := depsServer(t, svc, mocks.NewDepsService(t))

	rec := inboxGet(srv, "/-/inbox?tab=deps", true)
	require.Equal(t, 200, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Deps Updates (3)")
	assert.NotContains(t, body, `id="inbox-runs"`)
	depsAt := strings.Index(body, `id="inbox-deps"`)
	require.GreaterOrEqual(t, depsAt, 0)
	sec := body[depsAt:]

	assert.Contains(t, sec, " data-keep-open")
	assert.Contains(t, sec, "Dependency updates")
	assert.Contains(t, sec, `checked <time datetime="2026-03-04T05:06:07Z">2026-03-04</time>`)
	assert.Contains(t, sec, "1 batch · 3 updates")
	assert.Contains(t, sec, `<li class="deps-row" id="deps-alpha">`)
	assert.Contains(t, sec, "1 major · 1 minor · 1 patch")
	assert.Contains(t, sec, "new 1")
	assert.Contains(t, sec, `aria-label="Updates of alpha"`)
	assert.Contains(t, sec, `aria-label="More actions for alpha"`)
	for _, want := range []string{"golang.org/x/text", "v0.20.0 → v0.21.0", ">minor<", ">major<", ">patch<", ">tools<"} {
		assert.Contains(t, sec, want)
	}
	assert.Contains(t, sec, `<a href="/alpha/actions/runs/12">run #3</a>`)
	assert.Contains(t, sec, ">Retry<")
	// stable ids keep the focus on the same control through a live update
	for _, id := range []string{"deps-alpha-sum", "deps-alpha-act", "deps-alpha-more", "deps-alpha-log"} {
		assert.Contains(t, sec, `id="`+id+`"`)
	}
	// the screen reader hears words, not the icon
	assert.Contains(t, sec, `<span class="count">3</span><span class="sr"> updates</span>`)
	assert.Contains(t, sec, `>the run ended: failure</span>`)
	assert.Contains(t, sec, `<span aria-hidden="true"><span class="ck ck-bad"`)
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

func TestInboxDepsSortsByUpdates(t *testing.T) {
	t.Parallel()
	ov := depsOverview(t)
	// aaa: 2 base updates and a major batch of 2 (4 in total); zzz: 3 base
	// updates, the largest single row.
	aaa := depsRec(t)
	aaa.Repo = repoName(t, "aaa")
	base := aaa.Batches[0]
	base.Updates = base.Updates[:2]
	major := domain.DepBatch{Key: "npm/web/react@v19", Updates: []domain.DepUpdate{
		{DepRef: domain.DepRef{Ecosystem: domain.EcosystemNPM, Directory: "web", Name: "react", Version: "v19.0.0"}, Current: "v18.3.1", Kind: domain.DepMajor},
		{DepRef: domain.DepRef{Ecosystem: domain.EcosystemNPM, Directory: "web", Name: "react-dom", Version: "v19.0.0"}, Current: "v18.3.1", Kind: domain.DepMajor},
	}}
	aaa.Batches = []domain.DepBatch{base, major}
	zzz := depsRec(t)
	zzz.Repo = repoName(t, "zzz")
	ov.Deps = []ports.InboxDeps{{Rec: zzz, Batch: zzz.Batches[0]}, {Rec: aaa, Batch: base}, {Rec: aaa, Batch: major}}
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Overview(mock.Anything, mock.Anything).Return(ov, nil)
	srv := depsServer(t, svc, mocks.NewDepsService(t))

	var majorAt int
	order := func(path string) (first, second int, body string) {
		body = inboxGet(srv, path, true).Body.String()
		majorAt = strings.Index(body, `id="deps-aaa-`)
		return strings.Index(body, `id="deps-aaa"`), strings.Index(body, `id="deps-zzz"`), body
	}
	a, z, body := order("/-/inbox?tab=deps")
	assert.Less(t, a, z, "the repository with the most updates across its batches comes first")
	assert.Less(t, a, majorAt, "the batches of a repository stay together")
	assert.Less(t, majorAt, z, "the batches of a repository stay together")
	assert.Contains(t, body, `<a href="/-/inbox?tab=deps#inbox-deps" aria-current="page">most updates</a>`)
	assert.Contains(t, body, `<a href="/-/inbox?tab=deps&amp;sort=name#inbox-deps">name</a>`)

	a, z, body = order("/-/inbox?tab=deps&sort=name")
	assert.Less(t, a, z, "sort=name orders by repository name")
	assert.Contains(t, body, `sort=name#inbox-deps" aria-current="page">name</a>`)
	assert.Contains(t, body, `action="/-/inbox/deps/aaa/snooze?sort=name"`, "the actions keep the order")
	assert.Regexp(t, `action="/-/inbox/deps/aaa/open\?sort=name"[^>]*><input type="hidden" name="batch" value="npm/web/react@v19">`, body[majorAt:], "the batch forms keep the order")
}

func TestInboxDepsSummaryHasNoInteractiveContent(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Overview(mock.Anything, mock.Anything).Return(depsOverview(t), nil)
	body := inboxGet(depsServer(t, svc, mocks.NewDepsService(t)), "/-/inbox?tab=deps", true).Body.String()
	sec := body[strings.Index(body, `id="inbox-deps"`):]
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
	const base = "/-/inbox/deps/alpha/"
	run := func(n int64, st domain.CIStatus) *domain.CIRun { return depsRun(n, st) }

	tests := []struct {
		name   string
		mutate func(d *domain.RepoDeps)
		run    *domain.CIRun

		status  InboxDepsStatus
		primary InboxDepsAction
		menu    [2]string // Ignore, Snooze
	}{
		{
			name:    "idle without a merge request",
			mutate:  func(d *domain.RepoDeps) { d.Base().State, d.Base().Error, d.Base().RunID = domain.DepsIdle, "", 0 },
			status:  InboxDepsStatus{},
			primary: InboxDepsAction{Label: "Open MR", Action: base + "open"},
			menu:    [2]string{base + "ignore", base + "snooze"},
		},
		{
			name: "idle with a merge request that differs",
			mutate: func(d *domain.RepoDeps) {
				d.Base().State, d.Base().Error, d.Base().RunID = domain.DepsIdle, "", 0
				d.Base().MR, d.Base().MRVersions = 7, d.Base().Refs()[:1]
			},
			status:  InboxDepsStatus{MR: 7, MRHref: "/alpha/merge-requests/7"},
			primary: InboxDepsAction{Label: "Update MR", Action: base + "open"},
			menu:    [2]string{base + "ignore", base + "snooze"},
		},
		{
			name: "idle with a merge request up to date",
			mutate: func(d *domain.RepoDeps) {
				d.Base().State, d.Base().Error = domain.DepsIdle, ""
				d.Base().MR, d.Base().MRVersions = 7, d.Base().Refs()
			},
			run:     run(3, domain.CICompleted),
			status:  InboxDepsStatus{MR: 7, MRHref: "/alpha/merge-requests/7"},
			primary: InboxDepsAction{Label: "View MR", Href: "/alpha/merge-requests/7"},
			menu:    [2]string{"", base + "snooze"},
		},
		{
			name:   "applying, run queued",
			mutate: func(d *domain.RepoDeps) { d.Base().State, d.Base().Error = domain.DepsApplying, "" },
			run:    run(3, domain.CIQueued),
			status: InboxDepsStatus{Kind: "queued", State: "queued", Text: "run #3", Href: "/alpha/actions/runs/12"},
		},
		{
			name:   "applying, run held back by a concurrency group",
			mutate: func(d *domain.RepoDeps) { d.Base().State, d.Base().Error = domain.DepsApplying, "" },
			run:    run(3, domain.CIPending),
			status: InboxDepsStatus{Kind: "queued", State: "queued", Text: "run #3", Href: "/alpha/actions/runs/12"},
		},
		{
			name:   "applying, the run is not readable",
			mutate: func(d *domain.RepoDeps) { d.Base().State, d.Base().Error = domain.DepsApplying, "" },
			status: InboxDepsStatus{Kind: "running", State: "applying"},
		},
		{
			name:   "applying, the run ended and the record is not settled yet",
			mutate: func(d *domain.RepoDeps) { d.Base().State, d.Base().Error = domain.DepsApplying, "" },
			run:    run(3, domain.CICompleted),
			status: InboxDepsStatus{Kind: "running", State: "applying", Text: "run #3", Href: "/alpha/actions/runs/12"},
		},
		{
			name:   "applying, run in progress",
			mutate: func(d *domain.RepoDeps) { d.Base().State, d.Base().Error = domain.DepsApplying, "" },
			run:    run(3, domain.CIInProgress),
			status: InboxDepsStatus{Kind: "running", State: "running", Text: "run #3", Href: "/alpha/actions/runs/12"},
		},
		{
			name: "applying with a merge request open keeps the link to it",
			mutate: func(d *domain.RepoDeps) {
				d.Base().State, d.Base().Error = domain.DepsApplying, ""
				d.Base().MR, d.Base().MRVersions = 7, d.Base().Refs()
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
			mutate:  func(d *domain.RepoDeps) { d.Base().Error = "the run no longer exists" },
			status:  InboxDepsStatus{Kind: "failed", State: "failed", Title: "the run no longer exists"},
			primary: InboxDepsAction{Label: "Retry", Action: base + "open"},
			menu:    [2]string{base + "ignore", base + "snooze"},
		},
		{
			name: "failed with a merge request that holds the versions still retries",
			mutate: func(d *domain.RepoDeps) {
				d.Base().MR, d.Base().MRVersions = 7, d.Base().Refs()
			},
			status:  InboxDepsStatus{Kind: "failed", State: "failed", Title: "the run ended: failure", MR: 7, MRHref: "/alpha/merge-requests/7"},
			primary: InboxDepsAction{Label: "Retry", Action: base + "open"},
			menu:    [2]string{base + "ignore", base + "snooze"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := depsRec(t)
			tc.mutate(&rec)
			row := srv.depsRow(ports.InboxDeps{Rec: rec, Batch: *rec.Base(), Run: tc.run})
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
	row := srv.depsRow(ports.InboxDeps{Rec: depsRec(t), Batch: depsRec(t).Batches[0], Run: depsRun(3, domain.CICompleted)})
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
	emptyJournal(svc)
	svc.EXPECT().Overview(mock.Anything, mock.Anything).Return(depsOverview(t), nil)
	srv := newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "alpha")), Browse: homeBrowse(t), Inbox: svc,
		Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
	})
	body := inboxGet(srv, "/-/inbox?tab=deps", true).Body.String()
	sec := body[strings.Index(body, `id="inbox-deps"`):]
	assert.NotContains(t, sec, "<form")
	assert.NotContains(t, sec, "More actions")
}

func TestInboxDepsEmptyFragmentIsHidden(t *testing.T) {
	t.Parallel()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Overview(mock.Anything, mock.Anything).Return(inboxOverview(t), nil)
	body := inboxGet(depsServer(t, svc, mocks.NewDepsService(t)), "/-/inbox?tab=deps", true).Body.String()
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
		deps.EXPECT().Ignore(mock.Anything, mock.Anything, alpha, domain.DepBatchKey(""), want).Return(domain.RepoDeps{}, nil).Once()
		rec := post(depsServer(t, mocks.NewInboxService(t), deps), "/-/inbox/deps/alpha/ignore", form)
		assert.Equal(t, 303, rec.Code)
		assert.Equal(t, "/-/inbox?tab=deps", rec.Header().Get("Location"))
	})
	t.Run("open", func(t *testing.T) {
		t.Parallel()
		majorForm := url.Values{}
		for k, v := range form {
			majorForm[k] = v
		}
		majorForm.Set("batch", "npm/web/react@v19")
		deps := mocks.NewDepsService(t)
		deps.EXPECT().Open(mock.Anything, mock.Anything, alpha, domain.DepBatchKey("npm/web/react@v19"), want).Return(domain.RepoDeps{}, nil).Once()
		rec := post(depsServer(t, mocks.NewInboxService(t), deps), "/-/inbox/deps/alpha/open", majorForm)
		assert.Equal(t, 303, rec.Code)
		assert.Equal(t, "/-/inbox?tab=deps", rec.Header().Get("Location"))
	})
	t.Run("snooze", func(t *testing.T) {
		t.Parallel()
		deps := mocks.NewDepsService(t)
		deps.EXPECT().Snooze(mock.Anything, mock.Anything, alpha).Return(domain.RepoDeps{}, nil).Once()
		rec := post(depsServer(t, mocks.NewInboxService(t), deps), "/-/inbox/deps/alpha/snooze?sort=name", nil)
		assert.Equal(t, 303, rec.Code)
		assert.Equal(t, "/-/inbox?tab=deps&sort=name", rec.Header().Get("Location"), "the order survives the action")
	})
	t.Run("stale", func(t *testing.T) {
		t.Parallel()
		deps := mocks.NewDepsService(t)
		deps.EXPECT().Ignore(mock.Anything, mock.Anything, alpha, domain.DepBatchKey(""), want).Return(domain.RepoDeps{}, domain.ErrPreconditionFailed).Once()
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
	svc.EXPECT().Overview(mock.Anything, mock.Anything).Return(depsOverview(t), nil)
	srv := depsServer(t, svc, mocks.NewDepsService(t))
	ls := &liveStream{s: srv, view: "inbox", tab: "deps"}
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
	svc.EXPECT().Overview(mock.Anything, mock.Anything).Return(depsOverview(t), nil)
	ls := &liveStream{s: depsServer(t, svc, mocks.NewDepsService(t)), view: "inbox", tab: "deps"}
	ev := ports.WatchEvent{ID: "b-2", Event: domain.DependencyUpdatesChanged{Repo: repoName(t, "alpha")}}
	assert.True(t, liveEvent(ev))
	frags, err := ls.render(withOwner(t.Context()), ev)
	require.NoError(t, err)
	assert.NotEmpty(t, frags)
}

func TestInboxDepsFailureExcerpt(t *testing.T) {
	for _, reason := range []string{"dependency command failed", "dependency command failed (exit 1)\ngo: requires go >= 1.26\n<script>alert(1)</script>\n"} {
		t.Run(reason, func(t *testing.T) {
			svc := mocks.NewInboxService(t)
			ov := depsOverview(t)
			ov.Deps[0].Batch.Error = reason
			svc.EXPECT().Overview(mock.Anything, mock.Anything).Return(ov, nil)
			srv := depsServer(t, svc, mocks.NewDepsService(t))
			body := inboxGet(srv, "/-/inbox?tab=deps", true).Body.String()
			assert.Contains(t, body, `<pre>`+html.EscapeString(reason)+`</pre>`)
			assert.Contains(t, body, `title="`+html.EscapeString(reason)+`">`+html.EscapeString(depsErrorTail(reason))+`</span>`)
			assert.NotContains(t, body, "<script>alert(1)</script>")
			assert.Contains(t, body, `<a href="/alpha/actions/runs/12">run #3</a>`)
			if strings.Contains(reason, "<script>") {
				start := strings.Index(body, `<li class="deps-row"`)
				row := body[start:]
				t.Log(row[:strings.Index(row, `</li>`)+len(`</li>`)])
			}
		})
	}
}

func TestInboxMajorBatchRow(t *testing.T) {
	srv := depsServer(t, mocks.NewInboxService(t), mocks.NewDepsService(t))
	rec := depsRec(t)
	batch := domain.DepBatch{Key: "npm/web/@scope/pkg@v2", MR: 9, Updates: []domain.DepUpdate{{DepRef: domain.DepRef{Name: "@scope/pkg", Version: "v2.1.0"}}}}
	row := srv.depsRow(ports.InboxDeps{Rec: rec, Batch: batch})
	assert.Equal(t, "@scope/pkg → v2", row.Title)
	assert.Equal(t, string(batch.Key), row.Batch)
	assert.True(t, row.Outdated)
	assert.NotEqual(t, "deps-alpha", row.ID)
	var buf strings.Builder
	require.NoError(t, depsForm(row, row.Primary.Action, "Open", "btn", true, "").Render(t.Context(), &buf))
	assert.Contains(t, buf.String(), `name="batch" value="npm/web/@scope/pkg@v2"`)
}

func TestInboxAsyncDeps(t *testing.T) {
	for _, op := range []string{"open", "ignore", "snooze"} {
		for _, key := range []domain.DepBatchKey{"", "npm/web/react@v19"} {
			t.Run(op+"/"+string(key), func(t *testing.T) {
				inbox := mocks.NewInboxService(t)
				deps := mocks.NewDepsService(t)
				before := depsOverview(t)
				before.Deps[0].Batch.Key = key
				before.Deps = append(before.Deps, ports.InboxDeps{Rec: before.Deps[0].Rec, Batch: domain.DepBatch{Key: "npm/web/other@v2"}})
				after := before
				if op == "open" {
					after.Deps = append([]ports.InboxDeps(nil), before.Deps...)
					after.Deps[0].Batch.State = domain.DepsApplying
				} else {
					after.Deps = nil
				}
				if op == "snooze" {
					inbox.EXPECT().Overview(mock.Anything, mock.Anything).Return(before, nil).Once()
				}
				inbox.EXPECT().Overview(mock.Anything, mock.Anything).Return(after, nil).Once()
				switch op {
				case "open":
					deps.EXPECT().Open(mock.Anything, mock.Anything, repoName(t, "alpha"), key, mock.Anything).Return(domain.RepoDeps{}, nil).Once()
				case "ignore":
					deps.EXPECT().Ignore(mock.Anything, mock.Anything, repoName(t, "alpha"), key, mock.Anything).Return(domain.RepoDeps{}, nil).Once()
				case "snooze":
					deps.EXPECT().Snooze(mock.Anything, mock.Anything, repoName(t, "alpha")).Return(domain.RepoDeps{}, nil).Once()
				}
				srv := depsServer(t, inbox, deps)
				form := url.Values{"batch": {string(key)}}
				req := httptest.NewRequest(http.MethodPost, "/-/inbox/deps/alpha/"+op+"?sort=name", strings.NewReader(form.Encode()))
				req.Header.Set("Origin", testPublicURL)
				req.Header.Set("HX-Request", "true")
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.AddCookie(&http.Cookie{Name: CookieName, Value: "tok"})
				w := httptest.NewRecorder()
				srv.ServeHTTP(w, req)
				assert.Equal(t, http.StatusOK, w.Code)
				assert.Empty(t, w.Header().Get("Location"))
				assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
				id := srv.depsRow(before.Deps[0]).ID
				body := w.Body.String()
				if op == "open" {
					assert.Contains(t, body, `hx-target="#`+id+`" hx-swap="outerMorph"`)
					assert.Contains(t, body, "applying")
					assert.NotContains(t, body, `hx-target="#`+srv.depsRow(before.Deps[1]).ID+`"`)
				} else {
					assert.Contains(t, body, `hx-target="#`+id+`" hx-swap="delete"`)
					if op == "snooze" {
						assert.Contains(t, body, `hx-target="#`+srv.depsRow(before.Deps[1]).ID+`" hx-swap="delete"`)
					}
				}
				assert.NotContains(t, body, "<html")
				assert.NotContains(t, body, `id="inbox-content"`)
			})
		}
	}
}
