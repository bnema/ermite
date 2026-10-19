// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
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

const testPendingID = "aaaaaaaaaaaaaaaaaaaaaaaaaa"

func testPendingIssue(t testing.TB) domain.PendingIssue {
	t.Helper()
	id, err := domain.NewPendingIssueID()
	require.NoError(t, err)
	title, err := domain.ParseIssueTitle("<script>alert(1)</script> crash")
	require.NoError(t, err)
	body, err := domain.ParseIssueBody("**not bold** [x](javascript:alert(1)) <img src=x onerror=y>\n" + strings.Repeat("long line ", 60))
	require.NoError(t, err)
	return domain.PendingIssue{ID: id, Repo: repoName(t, "alpha"), Title: title, Body: body, Author: "Eve <i>", CreatedAt: time.Now().Add(-3 * time.Hour)}
}

func moderationServer(t *testing.T, inbox ports.InboxService, mod ports.IssueModerationService) *Server {
	t.Helper()
	return newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "alpha")), Browse: homeBrowse(t), Inbox: inbox, Moderation: mod,
		Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
	})
}

func TestInboxShowsPendingIssuesAsPlainTextFirst(t *testing.T) {
	t.Parallel()
	p := testPendingIssue(t)
	ov := inboxOverview(t)
	ov.Pending = []domain.PendingIssue{p, {ID: p.ID, Repo: repoName(t, "beta"), Title: p.Title, CreatedAt: time.Now()}}
	ov.Counts.Pending = 2
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Overview(mock.Anything, mock.Anything, domain.RepoName{}).Return(ov, nil)
	srv := moderationServer(t, svc, mocks.NewIssueModerationService(t))

	rec := inboxGet(srv, "/-/inbox", true)
	require.Equal(t, 200, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "<strong>2</strong> awaiting moderation")
	assert.Less(t, strings.Index(body, `id="inbox-pending"`), strings.Index(body, `id="inbox-mrs"`), "first section")
	assert.Contains(t, body, "&lt;script&gt;alert(1)&lt;/script&gt; crash")
	assert.NotContains(t, body, "<script>alert")
	assert.Contains(t, body, "**not bold**", "no Markdown")
	assert.NotContains(t, body, "<strong>not bold")
	assert.NotContains(t, body, `href="javascript`)
	assert.NotContains(t, body, "<img")
	assert.Contains(t, body, "Eve &lt;i&gt;")
	assert.Contains(t, body, ">anonymous<", "no label")
	assert.Contains(t, body, "…</p>", "a preview of about 300 characters")
	assert.Contains(t, body, `action="/-/inbox/pending/alpha/`+p.ID.String()+`/approve"`)
	assert.Contains(t, body, `/reject"`)
	assert.Contains(t, body, `/ban"`)
	assert.Contains(t, body, "Reject and ban")
}

func TestInboxPendingKeepsTheFilterInItsActions(t *testing.T) {
	t.Parallel()
	p := testPendingIssue(t)
	ov := inboxOverview(t)
	ov.Pending = []domain.PendingIssue{p}
	ov.Counts.Pending = 1
	svc := mocks.NewInboxService(t)
	svc.EXPECT().Overview(mock.Anything, mock.Anything, repoName(t, "alpha")).Return(ov, nil)
	srv := moderationServer(t, svc, mocks.NewIssueModerationService(t))
	rec := inboxGet(srv, "/-/inbox?repo=alpha", true)
	assert.Contains(t, rec.Body.String(), `/approve?repo=alpha"`)
}

func TestInboxModerationActions(t *testing.T) {
	t.Parallel()
	post := func(srv *Server, path string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		req.AddCookie(&http.Cookie{Name: CookieName, Value: "tok"})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	id, err := domain.ParsePendingIssueID(testPendingID)
	require.NoError(t, err)
	alpha := repoName(t, "alpha")
	site := map[string]string{"Origin": testPublicURL}

	t.Run("approve", func(t *testing.T) {
		t.Parallel()
		mod := mocks.NewIssueModerationService(t)
		mod.EXPECT().Approve(mock.Anything, mock.Anything, alpha, id).Return(domain.Issue{}, nil).Once()
		rec := post(moderationServer(t, mocks.NewInboxService(t), mod), "/-/inbox/pending/alpha/"+testPendingID+"/approve?repo=alpha", site)
		assert.Equal(t, 303, rec.Code)
		assert.Equal(t, "/-/inbox?repo=alpha", rec.Header().Get("Location"))
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	})
	t.Run("reject", func(t *testing.T) {
		t.Parallel()
		mod := mocks.NewIssueModerationService(t)
		mod.EXPECT().Reject(mock.Anything, mock.Anything, alpha, id, false).Return(nil).Once()
		rec := post(moderationServer(t, mocks.NewInboxService(t), mod), "/-/inbox/pending/alpha/"+testPendingID+"/reject", site)
		assert.Equal(t, 303, rec.Code)
		assert.Equal(t, "/-/inbox", rec.Header().Get("Location"))
	})
	t.Run("ban", func(t *testing.T) {
		t.Parallel()
		mod := mocks.NewIssueModerationService(t)
		mod.EXPECT().Reject(mock.Anything, mock.Anything, alpha, id, true).Return(nil).Once()
		rec := post(moderationServer(t, mocks.NewInboxService(t), mod), "/-/inbox/pending/alpha/"+testPendingID+"/ban", site)
		assert.Equal(t, 303, rec.Code)
	})
	t.Run("gone", func(t *testing.T) {
		t.Parallel()
		mod := mocks.NewIssueModerationService(t)
		mod.EXPECT().Approve(mock.Anything, mock.Anything, alpha, id).Return(domain.Issue{}, domain.ErrNotFound).Once()
		rec := post(moderationServer(t, mocks.NewInboxService(t), mod), "/-/inbox/pending/alpha/"+testPendingID+"/approve", site)
		assert.Equal(t, 404, rec.Code)
	})
	t.Run("a malformed id is not found and calls nothing", func(t *testing.T) {
		t.Parallel()
		rec := post(moderationServer(t, mocks.NewInboxService(t), mocks.NewIssueModerationService(t)), "/-/inbox/pending/alpha/nope/approve", site)
		assert.Equal(t, 404, rec.Code)
	})
	t.Run("the gate: no origin, no session", func(t *testing.T) {
		t.Parallel()
		srv := moderationServer(t, mocks.NewInboxService(t), mocks.NewIssueModerationService(t))
		assert.Equal(t, 403, post(srv, "/-/inbox/pending/alpha/"+testPendingID+"/approve", nil).Code)
		req := httptest.NewRequest(http.MethodPost, "/-/inbox/pending/alpha/"+testPendingID+"/approve", nil)
		req.Header.Set("Origin", testPublicURL)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		assert.Equal(t, 403, rec.Code, "anonymous")
	})
}
