// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
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
	emptyJournal(inbox)
	return newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "alpha")), Browse: homeBrowse(t), Inbox: inbox, Moderation: mod,
		Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
	})
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
		rec := post(moderationServer(t, mocks.NewInboxService(t), mod), "/-/moderation/alpha/"+testPendingID+"/approve", site)
		assert.Equal(t, 303, rec.Code)
		assert.Equal(t, "/-/inbox", rec.Header().Get("Location"))
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	})
	t.Run("reject", func(t *testing.T) {
		t.Parallel()
		mod := mocks.NewIssueModerationService(t)
		mod.EXPECT().Reject(mock.Anything, mock.Anything, alpha, id, false).Return(nil).Once()
		rec := post(moderationServer(t, mocks.NewInboxService(t), mod), "/-/moderation/alpha/"+testPendingID+"/reject", site)
		assert.Equal(t, 303, rec.Code)
		assert.Equal(t, "/-/inbox", rec.Header().Get("Location"))
	})
	t.Run("ban", func(t *testing.T) {
		t.Parallel()
		mod := mocks.NewIssueModerationService(t)
		mod.EXPECT().Reject(mock.Anything, mock.Anything, alpha, id, true).Return(nil).Once()
		rec := post(moderationServer(t, mocks.NewInboxService(t), mod), "/-/moderation/alpha/"+testPendingID+"/ban", site)
		assert.Equal(t, 303, rec.Code)
	})
	t.Run("gone", func(t *testing.T) {
		t.Parallel()
		mod := mocks.NewIssueModerationService(t)
		mod.EXPECT().Approve(mock.Anything, mock.Anything, alpha, id).Return(domain.Issue{}, domain.ErrNotFound).Once()
		rec := post(moderationServer(t, mocks.NewInboxService(t), mod), "/-/moderation/alpha/"+testPendingID+"/approve", site)
		assert.Equal(t, 404, rec.Code)
	})
	t.Run("a malformed id is not found and calls nothing", func(t *testing.T) {
		t.Parallel()
		rec := post(moderationServer(t, mocks.NewInboxService(t), mocks.NewIssueModerationService(t)), "/-/moderation/alpha/nope/approve", site)
		assert.Equal(t, 404, rec.Code)
	})
	t.Run("the gate: no origin, no session", func(t *testing.T) {
		t.Parallel()
		srv := moderationServer(t, mocks.NewInboxService(t), mocks.NewIssueModerationService(t))
		assert.Equal(t, 403, post(srv, "/-/moderation/alpha/"+testPendingID+"/approve", nil).Code)
		req := httptest.NewRequest(http.MethodPost, "/-/moderation/alpha/"+testPendingID+"/approve", nil)
		req.Header.Set("Origin", testPublicURL)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		assert.Equal(t, 403, rec.Code, "anonymous")
	})
}

func TestInboxAsyncModeration(t *testing.T) {
	for _, op := range []string{"approve", "reject", "ban"} {
		t.Run(op, func(t *testing.T) {
			inbox := mocks.NewInboxService(t)
			ov := inboxOverview(t)
			ov.Pending = []domain.PendingIssue{testPendingIssue(t), testPendingIssue(t), testPendingIssue(t)} // what is left after the action
			expectPage(inbox, ov, "inbox")
			mod := mocks.NewIssueModerationService(t)
			id, err := domain.ParsePendingIssueID(testPendingID)
			require.NoError(t, err)
			if op == "approve" {
				mod.EXPECT().Approve(mock.Anything, mock.Anything, repoName(t, "alpha"), id).Return(domain.Issue{}, nil).Once()
			} else {
				mod.EXPECT().Reject(mock.Anything, mock.Anything, repoName(t, "alpha"), id, op == "ban").Return(nil).Once()
			}
			srv := moderationServer(t, inbox, mod)
			req := httptest.NewRequest(http.MethodPost, "/-/moderation/alpha/"+testPendingID+"/"+op, nil)
			req.Header.Set("Origin", testPublicURL)
			req.Header.Set("HX-Request", "true")
			req.AddCookie(&http.Cookie{Name: CookieName, Value: "tok"})
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			assert.Empty(t, w.Header().Get("Location"))
			body := w.Body.String()
			assert.Contains(t, body, `<hx-partial hx-target="#attention" hx-swap="outerMorph"><section id="attention"`)
			assert.Equal(t, 3, strings.Count(body, `data-kind="moderation"`), "the list read after the action")
			assert.NotContains(t, body, `id="att-moderation-alpha-`+testPendingID+`"`)
			assert.Equal(t, 1, strings.Count(body, "data-cursor"), "the cursor on the first row left")
			nav := w.Body.String()[strings.Index(w.Body.String(), `<nav id="owner-nav"`):]
			assert.Contains(t, nav, `aria-label="Owner" hx-swap-oob="outerHTML"`)
			assert.Contains(t, nav, `aria-label="Inbox (3)" aria-current="page"`)
			assert.Contains(t, nav, `<span class="count hot">3</span>`, "the count after the action")
			assert.Contains(t, w.Body.String(), `id="action-status"`)
			assert.NotContains(t, w.Body.String(), "<html")
		})
	}
}

func TestInboxModerationEnhancedForms(t *testing.T) {
	p := testPendingIssue(t)
	srv := moderationServer(t, mocks.NewInboxService(t), mocks.NewIssueModerationService(t))
	row := srv.pendingRow(p)
	html := renderToString(t, InboxAttention([]AttentionRow{{ID: row.ID, Kind: "moderation", Title: row.Title, Pending: &row}}))
	assert.Contains(t, html, `id="att-moderation-alpha-`+p.ID.String()+`"`)
	assert.Contains(t, html, `hx-post="`+row.Approve+`"`)
	assert.Contains(t, html, `hx-post="`+row.Reject+`"`)
	assert.Contains(t, html, `hx-post="`+row.Ban+`"`)
	assert.Contains(t, html, `hx-confirm="Reject and ban this author?"`)
}

func TestInboxDottedRepoPendingIDs(t *testing.T) {
	inbox := mocks.NewInboxService(t)
	expectPage(inbox, inboxFixture{}, "inbox")
	srv := moderationServer(t, inbox, mocks.NewIssueModerationService(t))
	p := testPendingIssue(t)
	p.Repo = repoName(t, "alpha.beta")
	p.ID, _ = domain.ParsePendingIssueID(testPendingID)
	row := srv.pendingRow(p)
	require.Equal(t, "att-moderation-alpha_2ebeta-"+testPendingID, row.ID)
	html := renderToString(t, InboxAttention([]AttentionRow{{ID: row.ID, Kind: "moderation", Title: row.Title, Pending: &row}}))
	assert.Contains(t, html, `id="`+row.ID+`"`)
	assert.Contains(t, html, `/-/moderation/alpha.beta/`)
	r := httptest.NewRequest(http.MethodPost, "/-/moderation/alpha.beta/"+testPendingID+"/approve", nil)
	r.SetPathValue("repo", "alpha.beta")
	r.SetPathValue("id", testPendingID)
	r.Header.Set("HX-Request", "true")
	result, err := srv.pendingResult(t.Context(), r)
	require.NoError(t, err)
	fragment := renderToString(t, result.Fragment)
	assert.Contains(t, fragment, "nothing awaits you.", "the last row moderated")
	assert.NotContains(t, fragment, `id="`+row.ID+`"`)
}

// A failed read of the list after a done moderation says the action was
// saved and the page could not refresh.
func TestModerationRefreshFails(t *testing.T) {
	inbox := mocks.NewInboxService(t)
	inbox.EXPECT().Attention(mock.Anything, mock.Anything).Return(ports.OwnerAttention{}, errors.New("down")).Once()
	mod := mocks.NewIssueModerationService(t)
	mod.EXPECT().Reject(mock.Anything, mock.Anything, mock.Anything, mock.Anything, false).Return(nil).Once()
	srv := moderationServer(t, inbox, mod)
	req := httptest.NewRequest(http.MethodPost, "/-/moderation/alpha/"+testPendingID+"/reject", nil)
	req.Header.Set("Origin", testPublicURL)
	req.Header.Set("HX-Request", "true")
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "tok"})
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "saved, but the page could not refresh")
	assert.NotContains(t, w.Body.String(), "down")
}
