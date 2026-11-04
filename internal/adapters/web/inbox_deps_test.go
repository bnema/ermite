// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"fmt"
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
		ignore  string
	}{
		{
			name:    "idle without a merge request",
			mutate:  func(d *domain.RepoDeps) { d.Base().State, d.Base().Error, d.Base().RunID = domain.DepsIdle, "", 0 },
			status:  InboxDepsStatus{},
			primary: InboxDepsAction{Label: "Open MR", Action: base + "open"},
			ignore:  base + "ignore",
		},
		{
			name: "idle with a merge request that differs",
			mutate: func(d *domain.RepoDeps) {
				d.Base().State, d.Base().Error, d.Base().RunID = domain.DepsIdle, "", 0
				d.Base().MR, d.Base().MRVersions = 7, d.Base().Refs()[:1]
			},
			status:  InboxDepsStatus{MR: 7, MRHref: "/alpha/merge-requests/7"},
			primary: InboxDepsAction{Label: "Update MR", Action: base + "open"},
			ignore:  base + "ignore",
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
			status: InboxDepsStatus{Kind: "running", State: "running", Text: "run #3", Href: "/alpha/actions/runs/12", MR: 7, MRHref: "/alpha/merge-requests/7", Draft: true},
		},
		{
			name:    "failed with a run",
			mutate:  func(d *domain.RepoDeps) {},
			run:     run(3, domain.CICompleted),
			status:  InboxDepsStatus{Kind: "failed", State: "failed", Text: "run #3", Href: "/alpha/actions/runs/12", Title: "the run ended: failure"},
			primary: InboxDepsAction{Label: "Retry", Action: base + "open"},
			ignore:  base + "ignore",
		},
		{
			name:    "failed, the run is gone",
			mutate:  func(d *domain.RepoDeps) { d.Base().Error = "the run no longer exists" },
			status:  InboxDepsStatus{Kind: "failed", State: "failed", Title: "the run no longer exists"},
			primary: InboxDepsAction{Label: "Retry", Action: base + "open"},
			ignore:  base + "ignore",
		},
		{
			name: "failed with a merge request that holds the versions still retries",
			mutate: func(d *domain.RepoDeps) {
				d.Base().MR, d.Base().MRVersions = 7, d.Base().Refs()
			},
			status:  InboxDepsStatus{Kind: "failed", State: "failed", Title: "the run ended: failure", MR: 7, MRHref: "/alpha/merge-requests/7", Draft: true},
			primary: InboxDepsAction{Label: "Retry", Action: base + "open"},
			ignore:  base + "ignore",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := depsRec(t)
			tc.mutate(&rec)
			row := srv.depsRow(ports.InboxDeps{Rec: rec, Batch: *rec.Base(), Run: tc.run}, "")
			assert.Equal(t, tc.status, row.Status)
			assert.Equal(t, tc.primary, row.Primary)
			assert.Equal(t, tc.ignore, row.Ignore)
		})
	}
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

func TestInboxIDPartEncoding(t *testing.T) {
	seen := map[string]bool{}
	for _, name := range []string{"alpha", "alpha-beta", "alpha.beta", "alpha_2ebeta", "alpha_beta"} {
		id := inboxIDPart(name)
		assert.Regexp(t, `^[a-z0-9_-]+$`, id)
		assert.False(t, seen[id], "encoding collision for %s", name)
		seen[id] = true
	}
	assert.Equal(t, "alpha-beta", inboxIDPart("alpha-beta"))
}

func depsMajor(key domain.DepBatchKey, ups ...domain.DepUpdate) domain.DepBatch {
	return domain.DepBatch{Key: key, Updates: ups}
}

func depsUp(eco domain.DepsEcosystem, name, cur, ver string) domain.DepUpdate {
	return domain.DepUpdate{DepRef: domain.DepRef{Ecosystem: eco, Name: name, Version: ver}, Current: cur, Kind: domain.DepMajor}
}

// treeOverview: gordon has a failed base batch and two major families; lazy
// has more updates but no failure; idle has a single base batch.
func treeOverview(t testing.TB) ports.InboxOverview {
	ov := depsOverview(t)
	gordon := depsRec(t)
	gordon.Repo = repoName(t, "gordon")
	charm := depsMajor("major:gomod:github.com/charmbracelet/",
		depsUp(domain.EcosystemGoMod, "github.com/charmbracelet/bubbles", "v0.20.0", "v2.0.0"),
		depsUp(domain.EcosystemGoMod, "github.com/charmbracelet/lipgloss", "v1.0.0", "v2.0.0"))
	lego := depsMajor("major:gomod:github.com/go-acme/lego", depsUp(domain.EcosystemGoMod, "github.com/go-acme/lego", "v4.0.0", "v5.0.0"))
	gordon.Batches = append(gordon.Batches, charm, lego)
	gordon.Base().Repro = []string{"go get golang.org/x/text@v0.21.0", "go mod tidy"}
	lazy := depsRec(t)
	lazy.Repo = repoName(t, "lazy")
	lazy.Base().State, lazy.Base().Error, lazy.Base().MR, lazy.Base().MRVersions = domain.DepsIdle, "", 38, lazy.Base().Refs()
	var majors []domain.DepBatch
	for _, n := range []string{"a", "b", "c", "d"} {
		majors = append(majors, depsMajor(domain.DepBatchKey("major:npm:"+n), depsUp(domain.EcosystemNPM, n, "v1.0.0", "v2.0.0")))
	}
	lazy.Batches = append(lazy.Batches, majors...)
	idle := depsRec(t)
	idle.Repo = repoName(t, "idle")
	idle.Base().State, idle.Base().Error = domain.DepsQueued, ""
	idle.AutoOpenDisabled = true
	ov.Deps = nil
	for _, rec := range []domain.RepoDeps{idle, lazy, gordon} {
		for _, b := range rec.Batches {
			in := ports.InboxDeps{Rec: rec, Batch: b}
			if rec.Repo.String() == "idle" {
				in.QueuePos = 3
			}
			ov.Deps = append(ov.Deps, in)
		}
	}
	ov.DepsQueue = ports.DepsQueue{Running: 1, Limit: 1, Queued: 12}
	return ov
}

func depsSection(t *testing.T, path string, ov ports.InboxOverview) string {
	t.Helper()
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Overview(mock.Anything, mock.Anything).Return(ov, nil)
	rec := inboxGet(depsServer(t, svc, mocks.NewDepsService(t)), path, true)
	require.Equal(t, 200, rec.Code)
	body := rec.Body.String()
	at := strings.Index(body, `id="inbox-deps"`)
	require.GreaterOrEqual(t, at, 0)
	return body[at : at+strings.Index(body[at:], "</section>")]
}

// liOf is the markup of the element with the id, up to the next row of the
// same kind.
func liOf(t *testing.T, sec, id, next string) string {
	t.Helper()
	at := strings.Index(sec, `id="`+id+`"`)
	require.GreaterOrEqual(t, at, 0, id)
	rest := sec[at:]
	if end := strings.Index(rest[1:], next); end >= 0 {
		return rest[:end+1]
	}
	return rest
}

func TestInboxDepsTree(t *testing.T) {
	t.Parallel()
	sec := depsSection(t, "/-/inbox?tab=deps", treeOverview(t))

	// header: totals and the queue
	assert.Contains(t, sec, "9 batches · 16 updates")
	assert.Contains(t, sec, `<span class="deps-queue">queue 1/1 running, 12 waiting</span>`)
	// filter chips with their counts
	assert.Regexp(t, `href="/-/inbox\?tab=deps#inbox-deps" aria-current="page">All`, sec)
	assert.Regexp(t, `filter=failures#inbox-deps">Failures\s+<span class="count">1</span>`, sec)
	assert.Regexp(t, `filter=majors#inbox-deps">Majors\s+<span class="count">6</span>`, sec)
	assert.Regexp(t, `filter=mrs#inbox-deps">MRs\s+<span class="count">1</span>`, sec)

	// one row per repository: the failure first, then the most updates
	g, l, i := strings.Index(sec, `id="deps-gordon"`), strings.Index(sec, `id="deps-lazy"`), strings.Index(sec, `id="deps-idle"`)
	require.True(t, g > 0 && l > 0 && i > 0)
	assert.Less(t, g, l, "failures first")
	assert.Less(t, l, i, "then the most updates")
	assert.Equal(t, 3, strings.Count(sec, `<li class="deps-repo-row"`))

	gordon := liOf(t, sec, "deps-gordon", `<li class="deps-repo-row"`)
	lazy := liOf(t, sec, "deps-lazy", `<li class="deps-repo-row"`)
	idle := liOf(t, sec, "deps-idle", `<li class="deps-repo-row"`)
	// collapsed by default, open with a failure
	assert.Contains(t, gordon, `<details class="deps-main" open>`)
	assert.Contains(t, lazy, `<details class="deps-main">`)
	assert.Contains(t, idle, `<details class="deps-main">`)
	// the summary counts
	assert.Contains(t, gordon, "1 failed")
	assert.Contains(t, gordon, "3 patch/minor")
	assert.Contains(t, gordon, "2 majors")
	assert.Contains(t, lazy, "MR !38")
	assert.Contains(t, lazy, "4 majors")
	assert.Contains(t, idle, "queued #3")
	assert.Contains(t, idle, `<span class="sr">, 3 updates</span>`)

	// the base batch first, then the majors group, failure-free majors by title
	base := strings.Index(gordon, `id="deps-gordon-base"`)
	group := strings.Index(gordon, `<h3 class="deps-group-h">Majors</h3>`)
	charm := strings.Index(gordon, "github.com/charmbracelet/*")
	lego := strings.Index(gordon, ">github.com/go-acme/lego<")
	require.True(t, base > 0 && group > 0 && charm > 0 && lego > 0)
	assert.Less(t, base, group)
	assert.Less(t, group, charm)
	assert.Less(t, charm, lego)
	assert.Contains(t, gordon, `<span class="deps-pkgs dim">bubbles, lipgloss</span>`)
	assert.Contains(t, gordon, `<span class="deps-target">→ v2</span>`)
	assert.Contains(t, gordon, `<span class="deps-target">→ v5</span>`)
	assert.Contains(t, gordon, ">Open MR<")
	// a family offers to split each of its packages; a lone package does not
	assert.Contains(t, gordon, ">Split bubbles<")
	assert.Contains(t, gordon, ">Split lipgloss<")
	legoRow := liOf(t, gordon, strings.TrimPrefix(regexp.MustCompile(`id="(deps-gordon-major-gomod-github-com-go-acme-lego-[0-9a-f]+)"`).FindStringSubmatch(gordon)[1], ""), `<li class="deps-row`)
	assert.NotContains(t, legoRow, ">Split")

	// the repository actions
	assert.Contains(t, gordon, `action="/-/inbox/deps/gordon/recheck"`)
	assert.Contains(t, gordon, `id="deps-gordon-recheck"`)
	assert.Contains(t, gordon, `aria-label="More actions for gordon"`)
	assert.Regexp(t, `action="/-/inbox/deps/gordon/auto"[^>]*><input type="hidden" name="on" value="0">\s*<button type="submit" class="btn">Auto-open off`, gordon)
	assert.Regexp(t, `name="on" value="1">\s*<button type="submit" class="btn">Auto-open on`, idle)
	assert.Contains(t, gordon, `action="/-/inbox/deps/gordon/snooze"`)
	// a queued batch has no button
	assert.NotContains(t, liOf(t, idle, "deps-idle-base", "</li>"), `class="btn primary"`)

	// no summary holds a control
	for _, sum := range regexp.MustCompile(`(?s)<summary[^>]*>.*?</summary>`).FindAllString(sec, -1) {
		if strings.Contains(sum, "⋯") {
			continue
		}
		for _, tag := range []string{"<form", "<button", "<input", "<a "} {
			assert.NotContains(t, sum, tag)
		}
	}
	// language-agnostic: no ecosystem on screen (the ids and hidden fields
	// are not shown)
	visible := regexp.MustCompile(`<input[^>]*>| id="[^"]*"`).ReplaceAllString(sec, "")
	for _, banned := range []string{"gomod", "github-actions", "ecosystem"} {
		assert.NotContains(t, visible, banned)
	}
}

func TestInboxDepsTreeFilterAndSort(t *testing.T) {
	t.Parallel()
	sec := depsSection(t, "/-/inbox?tab=deps&filter=failures", treeOverview(t))
	assert.Contains(t, sec, `id="deps-gordon"`)
	assert.NotContains(t, sec, `id="deps-lazy"`)
	assert.NotContains(t, sec, "charmbracelet", "only the failed batches")
	assert.Contains(t, sec, `action="/-/inbox/deps/gordon/open?filter=failures"`, "the actions keep the view")
	assert.Contains(t, sec, `sort=name&amp;filter=failures#inbox-deps">name</a>`)

	sec = depsSection(t, "/-/inbox?tab=deps&filter=mrs", treeOverview(t))
	assert.Contains(t, sec, `id="deps-lazy"`)
	assert.NotContains(t, sec, `id="deps-gordon"`)

	sec = depsSection(t, "/-/inbox?tab=deps&sort=name", treeOverview(t))
	g, i, l := strings.Index(sec, `id="deps-gordon"`), strings.Index(sec, `id="deps-idle"`), strings.Index(sec, `id="deps-lazy"`)
	assert.True(t, g < i && i < l, "sort=name orders by repository name")
	assert.Contains(t, sec, `action="/-/inbox/deps/idle/snooze?sort=name"`)

	ov := treeOverview(t)
	ov.Deps = ov.Deps[:1]
	sec = depsSection(t, "/-/inbox?tab=deps&filter=majors", ov)
	assert.Contains(t, sec, "no batch matches this filter.")
	assert.Contains(t, sec, "Dependency updates")
}

func TestInboxDepsSplitOut(t *testing.T) {
	t.Parallel()
	srv := depsServer(t, mocks.NewInboxService(t), mocks.NewDepsService(t))
	rec := depsRec(t)
	rec.SplitOut = []domain.DepName{{Ecosystem: domain.EcosystemGoMod, Name: "github.com/charmbracelet/bubbles"}}
	b := depsMajor("major:gomod:github.com/charmbracelet/bubbles", depsUp(domain.EcosystemGoMod, "github.com/charmbracelet/bubbles", "v0.20.0", "v2.0.0"))
	row := srv.depsRow(ports.InboxDeps{Rec: rec, Batch: b}, "")
	assert.Equal(t, "github.com/charmbracelet/bubbles", row.Title)
	assert.Equal(t, "v2", row.Target)
	require.Len(t, row.Splits, 1)
	assert.Equal(t, InboxDepsSplit{Label: "Join family", Action: "/-/inbox/deps/alpha/split", Ecosystem: "gomod", Name: "github.com/charmbracelet/bubbles"}, row.Splits[0])
	html := renderToString(t, inboxDepsRow(row))
	assert.Regexp(t, `name="split" value="0">\s*<button type="submit" class="btn">Join family`, html)
}

func TestInboxDepsBatchStates(t *testing.T) {
	t.Parallel()
	srv := depsServer(t, mocks.NewInboxService(t), mocks.NewDepsService(t))
	for _, tc := range []struct {
		name   string
		mutate func(b *domain.DepBatch)
		pos    int
		status InboxDepsStatus
		label  string
	}{
		{"queued", func(b *domain.DepBatch) { b.State = domain.DepsQueued }, 4, InboxDepsStatus{Kind: "queued", State: "queued #4"}, ""},
		{"drafting", func(b *domain.DepBatch) { b.State = domain.DepsQueued }, 1, InboxDepsStatus{Kind: "running", State: "drafting"}, ""},
		{"draft waits for a slot", func(b *domain.DepBatch) { b.State, b.MR = domain.DepsDrafted, 41 }, 2,
			InboxDepsStatus{Kind: "queued", State: "queued #2", MR: 41, MRHref: "/alpha/merge-requests/41", Draft: true}, ""},
		{"adopted", func(b *domain.DepBatch) { b.State, b.MR, b.Adopted = domain.DepsIdle, 41, true }, 0,
			InboxDepsStatus{MR: 41, MRHref: "/alpha/merge-requests/41", Adopted: true}, "View MR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := depsRec(t)
			b := rec.Batches[0]
			b.Error = ""
			tc.mutate(&b)
			row := srv.depsRow(ports.InboxDeps{Rec: rec, Batch: b, QueuePos: tc.pos}, "")
			assert.Equal(t, tc.status, row.Status)
			assert.Equal(t, tc.label, row.Primary.Label)
		})
	}
}

func TestInboxDepsFailureBlock(t *testing.T) {
	reason := "dependency command failed (exit 1)\ngo: requires go >= 1.26\n<script>alert(1)</script>\n"
	ov := treeOverview(t)
	for i := range ov.Deps {
		if ov.Deps[i].Rec.Repo.String() == "gordon" && ov.Deps[i].Batch.Key == "" {
			ov.Deps[i].Batch.Error = reason
			ov.Deps[i].Run = depsRun(3, domain.CICompleted)
		}
	}
	sec := depsSection(t, "/-/inbox?tab=deps", ov)
	gordon := liOf(t, sec, "deps-gordon-base", `<h3 class="deps-group-h">`)
	assert.Contains(t, gordon, `<div class="deps-failure" role="region" aria-label="Failure of Patch &amp; minor">`)
	// the full text, newlines kept, escaped
	assert.Contains(t, gordon, `<pre class="deps-error">`+html.EscapeString(reason)+`</pre>`)
	assert.NotContains(t, sec, "<script>alert(1)</script>")
	assert.Contains(t, gordon, "Reproduce locally:")
	assert.Contains(t, gordon, `<pre class="deps-repro">go get golang.org/x/text@v0.21.0`+"\n"+`go mod tidy</pre>`)
	assert.Contains(t, gordon, `<a id="deps-gordon-base-log" href="/gordon/actions/runs/12">View log · run #3</a>`)
	assert.Contains(t, gordon, ">Retry<")
	assert.Contains(t, gordon, ">Ignore<")
	// no truncated red line any more
	assert.NotContains(t, sec, "deps-error-tail")
	// the other repositories have no failure block
	assert.NotContains(t, liOf(t, sec, "deps-lazy", `<li class="deps-repo-row"`), "deps-failure")
}

func TestInboxDepsRepoActions(t *testing.T) {
	t.Parallel()
	alpha := repoName(t, "alpha")
	post := func(srv *Server, path string, form url.Values, hx bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", testPublicURL)
		if hx {
			req.Header.Set("HX-Request", "true")
		}
		req.AddCookie(&http.Cookie{Name: CookieName, Value: "tok"})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	bubbles := domain.DepName{Ecosystem: domain.EcosystemGoMod, Name: "github.com/charmbracelet/bubbles"}
	for _, tc := range []struct {
		name, path string
		form       url.Values
		expect     func(d *mocks.DepsService)
	}{
		{"recheck", "/-/inbox/deps/alpha/recheck", nil, func(d *mocks.DepsService) {
			d.EXPECT().Check(mock.Anything, mock.Anything, alpha).Return(domain.RepoDeps{}, nil).Once()
		}},
		{"auto off", "/-/inbox/deps/alpha/auto", url.Values{"on": {"0"}}, func(d *mocks.DepsService) {
			d.EXPECT().SetAutoOpen(mock.Anything, mock.Anything, alpha, false).Return(domain.RepoDeps{}, nil).Once()
		}},
		{"auto on", "/-/inbox/deps/alpha/auto", url.Values{"on": {"1"}}, func(d *mocks.DepsService) {
			d.EXPECT().SetAutoOpen(mock.Anything, mock.Anything, alpha, true).Return(domain.RepoDeps{}, nil).Once()
		}},
		{"split", "/-/inbox/deps/alpha/split", url.Values{"ecosystem": {"gomod"}, "name": {bubbles.Name}, "split": {"1"}}, func(d *mocks.DepsService) {
			d.EXPECT().Split(mock.Anything, mock.Anything, alpha, bubbles, true).Return(domain.RepoDeps{}, nil).Once()
		}},
		{"join", "/-/inbox/deps/alpha/split?filter=majors", url.Values{"ecosystem": {"gomod"}, "name": {bubbles.Name}, "split": {"0"}}, func(d *mocks.DepsService) {
			d.EXPECT().Split(mock.Anything, mock.Anything, alpha, bubbles, false).Return(domain.RepoDeps{}, nil).Once()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps := mocks.NewDepsService(t)
			tc.expect(deps)
			rec := post(depsServer(t, mocks.NewInboxService(t), deps), tc.path, tc.form, false)
			assert.Equal(t, 303, rec.Code)
			want := "/-/inbox?tab=deps"
			if strings.Contains(tc.path, "filter=majors") {
				want += "&filter=majors"
			}
			assert.Equal(t, want, rec.Header().Get("Location"))
		})
	}
	for _, tc := range []struct {
		name, path string
		form       url.Values
	}{
		{"auto without a flag", "/-/inbox/deps/alpha/auto", url.Values{"on": {"yes"}}},
		{"split without a name", "/-/inbox/deps/alpha/split", url.Values{"ecosystem": {"gomod"}, "split": {"1"}}},
		{"split without a flag", "/-/inbox/deps/alpha/split", url.Values{"ecosystem": {"gomod"}, "name": {"x"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := post(depsServer(t, mocks.NewInboxService(t), mocks.NewDepsService(t)), tc.path, tc.form, false)
			assert.NotEqual(t, 303, rec.Code, "the service is not called (the mock has no expectation)")
		})
	}
	t.Run("everyone else", func(t *testing.T) {
		t.Parallel()
		srv := depsServer(t, mocks.NewInboxService(t), mocks.NewDepsService(t))
		for _, op := range []string{"recheck", "auto", "split"} {
			req := httptest.NewRequest(http.MethodPost, "/-/inbox/deps/alpha/"+op, nil)
			req.Header.Set("Origin", testPublicURL)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			assert.Equal(t, 403, rec.Code, op)
			// past the gate, the handler hides the action from a non-owner
			r := httptest.NewRequest(http.MethodPost, "/-/inbox/deps/alpha/"+op, nil)
			r.SetPathValue("repo", "alpha")
			_, err := srv.depsAction(depsRecheck)(r.Context(), r)
			assert.ErrorIs(t, err, errNotFound)
		}
	})
	t.Run("htmx swaps the repository row, open", func(t *testing.T) {
		t.Parallel()
		inbox := mocks.NewInboxService(t)
		inbox.EXPECT().Overview(mock.Anything, mock.Anything).Return(depsOverview(t), nil).Once()
		deps := mocks.NewDepsService(t)
		deps.EXPECT().Check(mock.Anything, mock.Anything, alpha).Return(domain.RepoDeps{}, nil).Once()
		rec := post(depsServer(t, inbox, deps), "/-/inbox/deps/alpha/recheck", nil, true)
		assert.Equal(t, 200, rec.Code)
		body := rec.Body.String()
		assert.Contains(t, body, `hx-target="#deps-alpha" hx-swap="outerMorph"`)
		assert.Contains(t, body, `<details class="deps-main" open>`)
		assert.NotContains(t, body, "<html")
	})
}

func TestInboxAsyncDeps(t *testing.T) {
	for _, op := range []string{"open", "ignore", "snooze"} {
		for _, gone := range []bool{false, true} {
			t.Run(op+"/gone="+boolText(gone), func(t *testing.T) {
				inbox := mocks.NewInboxService(t)
				deps := mocks.NewDepsService(t)
				after := depsOverview(t)
				after.Deps = append(after.Deps, ports.InboxDeps{Rec: after.Deps[0].Rec, Batch: depsMajor("major:npm:other", depsUp(domain.EcosystemNPM, "other", "v1.0.0", "v2.0.0"))})
				after.Deps[0].Batch.State = domain.DepsApplying
				if gone {
					after.Deps = nil
				}
				inbox.EXPECT().Overview(mock.Anything, mock.Anything).Return(after, nil).Once()
				switch op {
				case "open":
					deps.EXPECT().Open(mock.Anything, mock.Anything, repoName(t, "alpha"), domain.DepBatchKey(""), mock.Anything).Return(domain.RepoDeps{}, nil).Once()
				case "ignore":
					deps.EXPECT().Ignore(mock.Anything, mock.Anything, repoName(t, "alpha"), domain.DepBatchKey(""), mock.Anything).Return(domain.RepoDeps{}, nil).Once()
				case "snooze":
					deps.EXPECT().Snooze(mock.Anything, mock.Anything, repoName(t, "alpha")).Return(domain.RepoDeps{}, nil).Once()
				}
				srv := depsServer(t, inbox, deps)
				req := httptest.NewRequest(http.MethodPost, "/-/inbox/deps/alpha/"+op+"?sort=name", strings.NewReader(url.Values{"batch": {""}}.Encode()))
				req.Header.Set("Origin", testPublicURL)
				req.Header.Set("HX-Request", "true")
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.AddCookie(&http.Cookie{Name: CookieName, Value: "tok"})
				w := httptest.NewRecorder()
				srv.ServeHTTP(w, req)
				assert.Equal(t, http.StatusOK, w.Code)
				assert.Empty(t, w.Header().Get("Location"))
				assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
				body := w.Body.String()
				if gone {
					assert.Contains(t, body, `hx-target="#deps-alpha" hx-swap="delete"`)
				} else {
					assert.Contains(t, body, `hx-target="#deps-alpha" hx-swap="outerMorph"`)
					assert.Contains(t, body, "applying")
					assert.Contains(t, body, `id="deps-alpha-base"`)
					assert.Contains(t, body, `/deps/alpha/open?sort=name`, "the swapped row keeps the order")
				}
				assert.NotContains(t, body, `id="inbox-content"`)
			})
		}
	}
}

func TestInboxDottedRepoDepsIDs(t *testing.T) {
	for _, key := range []domain.DepBatchKey{"", "major:npm:react"} {
		t.Run(string(key), func(t *testing.T) {
			srv := depsServer(t, mocks.NewInboxService(t), mocks.NewDepsService(t))
			ov := depsOverview(t)
			ov.Deps[0].Rec.Repo = repoName(t, "alpha.beta")
			ov.Deps[0].Batch.Key = key
			repos := srv.depsRepos(ov.Deps, depsQuery{})
			require.Len(t, repos, 1)
			assert.Equal(t, "deps-alpha_2ebeta", repos[0].ID)
			html := renderToString(t, inboxDepsRepo(repos[0]))
			for _, id := range regexp.MustCompile(`id="([^"]+)"`).FindAllStringSubmatch(html, -1) {
				assert.Regexp(t, `^[a-zA-Z0-9_-]+$`, id[1])
			}
			assert.Contains(t, html, `/deps/alpha.beta/`)
		})
	}
}

// An adopted batch is in human hands: whatever its state, the row offers no
// action Bernard would refuse, only the link to the merge request.
func TestInboxDepsAdoptedWins(t *testing.T) {
	t.Parallel()
	srv := depsServer(t, mocks.NewInboxService(t), mocks.NewDepsService(t))
	for _, st := range []domain.DepsState{domain.DepsIdle, domain.DepsFailed, domain.DepsApplying, domain.DepsQueued, domain.DepsDrafted} {
		for _, outdated := range []bool{false, true} {
			t.Run(fmt.Sprintf("state=%d/outdated=%v", st, outdated), func(t *testing.T) {
				rec := depsRec(t)
				b := rec.Batches[0]
				b.State, b.MR, b.Adopted = st, 1, true
				b.MRVersions = b.Refs()
				if outdated {
					b.MRVersions = b.MRVersions[:1]
				}
				row := srv.depsRow(ports.InboxDeps{Rec: rec, Batch: b}, "")
				assert.Equal(t, InboxDepsAction{Label: "View MR", Href: "/alpha/merge-requests/1"}, row.Primary)
				assert.Empty(t, row.Ignore)
				assert.True(t, row.Status.Adopted)
				assert.False(t, row.Status.Draft)
			})
		}
	}
	// failed and adopted: the failure stays, with a word on who holds it
	rec := depsRec(t)
	b := rec.Batches[0]
	b.MR, b.Adopted = 1, true
	html := renderToString(t, inboxDepsRow(srv.depsRow(ports.InboxDeps{Rec: rec, Batch: b, Run: depsRun(3, domain.CICompleted)}, "")))
	assert.Contains(t, html, `class="deps-failure"`)
	assert.Contains(t, html, "the run ended: failure")
	assert.Contains(t, html, "changed by hand")
	assert.Contains(t, html, `>adopted</span>`)
	assert.Contains(t, html, `class="btn primary" href="/alpha/merge-requests/1">View MR</a>`)
	assert.NotContains(t, html, ">Retry<")
	assert.NotContains(t, html, "/deps/alpha/open")
	assert.NotContains(t, html, "/deps/alpha/ignore")
}

// The repository name is said once: the summary, plus an icon link to the
// repository outside of it (a link inside a summary breaks the toggle for
// keyboard and screen reader users).
func TestInboxDepsRepoLinkOnce(t *testing.T) {
	t.Parallel()
	sec := depsSection(t, "/-/inbox?tab=deps", depsOverview(t))
	row := liOf(t, sec, "deps-alpha", `<li class="deps-repo-row"`)
	assert.NotContains(t, row, "deps-repolink")
	assert.Equal(t, 1, strings.Count(row, ">alpha<"), "the name appears once")
	assert.Contains(t, row, `<a id="deps-alpha-repo" class="btn icon" href="/alpha" aria-label="Open repository alpha"`)
	sum := regexp.MustCompile(`(?s)<summary id="deps-alpha-sum">.*?</summary>`).FindString(row)
	assert.NotContains(t, sum, "<a ")
}

// An action brings the header back with the new check time and counts.
func TestInboxDepsActionRefreshesTheHeader(t *testing.T) {
	t.Parallel()
	ov := depsOverview(t)
	ov.DepsCheckedAt = time.Date(2026, 3, 5, 6, 0, 0, 0, time.UTC)
	inbox := mocks.NewInboxService(t)
	inbox.EXPECT().Overview(mock.Anything, mock.Anything).Return(ov, nil).Once()
	deps := mocks.NewDepsService(t)
	deps.EXPECT().Check(mock.Anything, mock.Anything, repoName(t, "alpha")).Return(domain.RepoDeps{}, nil).Once()
	req := httptest.NewRequest(http.MethodPost, "/-/inbox/deps/alpha/recheck", nil)
	req.Header.Set("Origin", testPublicURL)
	req.Header.Set("HX-Request", "true")
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "tok"})
	w := httptest.NewRecorder()
	depsServer(t, inbox, deps).ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `<hx-partial hx-target="#inbox-deps-h" hx-swap="outerMorph"><h2 id="inbox-deps-h"`)
	assert.Contains(t, body, `<time datetime="2026-03-05T06:00:00Z">2026-03-05 06:00</time>`)
	assert.Contains(t, body, "1 batch · 3 updates")
}
