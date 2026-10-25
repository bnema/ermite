// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

func depsOverview(t testing.TB) ports.InboxOverview {
	ov := inboxOverview(t)
	ov.Deps = []domain.RepoDeps{{
		Repo: repoName(t, "alpha"), State: domain.DepsFailed, Error: "go mod tidy failed",
		Updates: []domain.DepUpdate{
			{DepRef: domain.DepRef{Ecosystem: domain.EcosystemGoMod, Directory: "tools", Name: "golang.org/x/text", Version: "v0.21.0"}, Current: "v0.20.0", Kind: domain.DepMinor, New: true},
			{DepRef: domain.DepRef{Ecosystem: domain.EcosystemActions, Name: "actions/checkout", Version: "v5"}, Current: "v4", Kind: domain.DepMajor},
		},
	}}
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

func TestInboxDepsCard(t *testing.T) {
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
	assert.Contains(t, body, "golang.org/x/text")
	assert.Contains(t, body, "v0.20.0 → v0.21.0")
	assert.Contains(t, body, "(minor)")
	assert.Contains(t, body, "(major)")
	assert.Contains(t, body, ">new<")
	assert.Contains(t, body, ">tools<")
	assert.Contains(t, body, "FAILED")
	assert.Contains(t, body, "go mod tidy failed")
	for _, a := range []string{"open", "ignore", "snooze"} {
		assert.Contains(t, body, `action="/-/inbox/deps/alpha/`+a+`"`)
	}
	assert.Contains(t, body, ">Open MR<")
	assert.Contains(t, body, `name="version" value="v0.21.0"`)
	assert.Contains(t, body, `name="directory" value="tools"`)
	assert.Contains(t, body, `name="ecosystem" value="github-actions"`)
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

func TestInboxDepsCardUpdateMRWhenOpen(t *testing.T) {
	t.Parallel()
	ov := depsOverview(t)
	ov.Deps[0].MR = 7
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Overview(mock.Anything, mock.Anything, domain.RepoName{}).Return(ov, nil)
	srv := depsServer(t, svc, mocks.NewDepsService(t))

	body := inboxGet(srv, "/-/inbox", true).Body.String()
	assert.Contains(t, body, ">Update MR<")
	assert.NotContains(t, body, ">Open MR<")
	assert.Contains(t, body, `action="/-/inbox/deps/alpha/open"`)
}

func depsMRBody(t *testing.T, mutate func(d *domain.RepoDeps)) string {
	t.Helper()
	ov := depsOverview(t)
	mutate(&ov.Deps[0])
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Overview(mock.Anything, mock.Anything, domain.RepoName{}).Return(ov, nil)
	return inboxGet(depsServer(t, svc, mocks.NewDepsService(t)), "/-/inbox", true).Body.String()
}

func TestInboxDepsCardWithoutMR(t *testing.T) {
	t.Parallel()
	body := depsMRBody(t, func(d *domain.RepoDeps) {})
	assert.Contains(t, body, ">Open MR<")
	assert.NotContains(t, body, "View MR")
	assert.NotContains(t, body, "MR !")
}

func TestInboxDepsCardMRUpToDate(t *testing.T) {
	t.Parallel()
	body := depsMRBody(t, func(d *domain.RepoDeps) {
		d.State, d.Error = domain.DepsIdle, ""
		d.MR, d.MRVersions = 7, d.Refs()
	})
	assert.Contains(t, body, `href="/alpha/merge-requests/7"`)
	assert.Contains(t, body, "MR !7")
	assert.Contains(t, body, "View MR")
	assert.Contains(t, body, "2 updates in the merge request")
	assert.NotContains(t, body, "Update MR")
	assert.NotContains(t, body, `action="/-/inbox/deps/alpha/ignore"`)
	assert.NotContains(t, body, `action="/-/inbox/deps/alpha/open"`)
	assert.Contains(t, body, `action="/-/inbox/deps/alpha/snooze"`)
}

func TestInboxDepsCardMRUpToDateSingular(t *testing.T) {
	t.Parallel()
	body := depsMRBody(t, func(d *domain.RepoDeps) {
		d.State, d.Error = domain.DepsIdle, ""
		d.Updates = d.Updates[:1]
		d.MR, d.MRVersions = 7, d.Refs()
	})
	assert.Contains(t, body, "1 update in the merge request")
}

func TestInboxDepsCardMRDiffers(t *testing.T) {
	t.Parallel()
	body := depsMRBody(t, func(d *domain.RepoDeps) {
		d.State, d.Error = domain.DepsIdle, ""
		d.MR, d.MRVersions = 7, d.Refs()[:1]
	})
	assert.Contains(t, body, `href="/alpha/merge-requests/7"`)
	assert.Contains(t, body, ">Update MR<")
	assert.Contains(t, body, `action="/-/inbox/deps/alpha/ignore"`)
	assert.NotContains(t, body, "updates in the merge request")
}

func TestInboxDepsCardMRFailedStaysFull(t *testing.T) {
	t.Parallel()
	body := depsMRBody(t, func(d *domain.RepoDeps) {
		d.MR, d.MRVersions = 7, d.Refs()
	})
	assert.Contains(t, body, "go mod tidy failed")
	assert.Contains(t, body, ">Update MR<")
	assert.Contains(t, body, `href="/alpha/merge-requests/7"`)
}
