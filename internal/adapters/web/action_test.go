// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a-h/templ"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

func TestAction(t *testing.T) {
	for _, tt := range []struct {
		name     string
		hx       bool
		fragment templ.Component
		err      error
		status   int
		contains string
	}{
		{name: "redirect", status: http.StatusSeeOther},
		{name: "fragment", hx: true, fragment: TitleOOB("updated fragment"), status: http.StatusOK, contains: "updated fragment"},
		{name: "no fragment", hx: true, status: http.StatusNoContent},
		{name: "htmx not found", hx: true, err: domain.ErrNotFound, status: http.StatusNotFound, contains: `id="action-status"`},
		{name: "page not found", err: domain.ErrNotFound, status: http.StatusNotFound, contains: "<html"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t, Config{Repos: reposServing(t), Browse: mocks.NewBrowseService(t)})
			r := httptest.NewRequest(http.MethodPost, "/demo/action", strings.NewReader("value=bound"))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tt.hx {
				r.Header.Set("HX-Request", "true")
			}
			w := httptest.NewRecorder()
			s.action(func(*http.Request) string { return "/back" }, func(ctx context.Context, r *http.Request) (ActionResult, error) {
				assert.Equal(t, "bound", r.PostForm.Get("value"))
				_, bounded := ctx.Deadline()
				assert.True(t, bounded)
				return ActionResult{Redirect: "/result", Fragment: tt.fragment}, tt.err
			})(w, r)
			assert.Equal(t, tt.status, w.Code)
			assert.Contains(t, w.Body.String(), tt.contains)
			assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			if tt.status == http.StatusSeeOther {
				assert.Equal(t, "/result", w.Header().Get("Location"))
			}
			if tt.status == http.StatusNoContent {
				assert.Empty(t, w.Body.String())
			}
			if tt.fragment != nil {
				assert.Contains(t, w.Body.String(), `id="action-status"`)
			}
			if !tt.hx && tt.err != nil {
				assert.NotContains(t, w.Body.String(), `hx-swap-oob="true"`)
			}
		})
	}
}

func TestPresentationFailuresPreserveSuccessfulAction(t *testing.T) {
	for _, kind := range []string{"issue comment", "MR comment", "moderation", "deps", "CI"} {
		t.Run(kind, func(t *testing.T) {
			failure := errors.New("private read failure")
			var s *Server
			var h ActionFunc
			path, back := "/demo/merge-requests/3/comments", "/demo/merge-requests/3"
			switch kind {
			case "issue comment":
				rig := newIssueRig(t, domain.IssuesOwner, false)
				s = rig.srv
				rig.issues.EXPECT().AddComment(mock.Anything, mock.Anything, repoName(t, "proj"), int64(3), "hello").Return(domain.IssueComment{ID: 7}, nil).Once()
				rig.issues.EXPECT().Get(mock.Anything, mock.Anything, repoName(t, "proj"), int64(3)).Return(ports.IssueDetail{}, failure).Once()
				h = s.issueComment
				path = "/proj/issues/3/comments"
				back = "/proj/issues/3"
			case "MR comment":
				var svc *mocks.MergeRequestService
				s, svc = asyncMergeServer(t, domain.MRStateOpen)
				svc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Unset()
				svc.EXPECT().AddComment(mock.Anything, mock.Anything, mock.Anything, int64(3), mock.Anything).Return(domain.Comment{ID: 7}, nil).Once()
				svc.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything, int64(3), mock.Anything).Return(ports.MergeRequestDetail{}, failure).Once()
				h = s.mrComment
			case "moderation", "deps":
				inbox := mocks.NewInboxService(t)
				inbox.EXPECT().Overview(mock.Anything, mock.Anything).Return(ports.InboxOverview{}, failure).Once()
				if kind == "moderation" {
					mod := mocks.NewIssueModerationService(t)
					mod.EXPECT().Reject(mock.Anything, mock.Anything, mock.Anything, mock.Anything, false).Return(nil).Once()
					s = moderationServer(t, inbox, mod)
					h = s.pendingReject(false)
					path = "/alpha/pending"
					back = pendingBack(nil)
				} else {
					deps := mocks.NewDepsService(t)
					deps.EXPECT().Open(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(domain.RepoDeps{}, nil).Once()
					s = depsServer(t, inbox, deps)
					h = s.depsAction(depsOpen)
					path = "/alpha/deps"
					back = "/-/inbox?tab=deps"
				}
			case "CI":
				runs := mocks.NewRunService(t)
				runs.EXPECT().CancelRun(mock.Anything, mock.Anything, mock.Anything, int64(3)).Return(domain.CIRun{}, nil).Once()
				runs.EXPECT().GetRun(mock.Anything, mock.Anything, mock.Anything, int64(3), int32(0)).Return(ports.RunDetail{}, failure).Once()
				browse := mocks.NewBrowseService(t)
				browse.EXPECT().ListRefs(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.RefPage{}, nil).Once()
				s = newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "demo")), Browse: browse, Runs: runs})
				h = s.runCancel
				back = "/demo/actions/runs/3"
			}
			r := autoMergeRequest(path, "text=hello&thread=mr-comment-thread")
			if kind == "issue comment" {
				r.SetPathValue("repo", "proj")
			}
			if kind == "moderation" || kind == "deps" {
				r.SetPathValue("repo", "alpha")
			}
			if kind == "moderation" {
				r.SetPathValue("id", testPendingID)
			} else {
				r.SetPathValue("id", "3")
			}
			r = r.WithContext(withOwner(r.Context()))
			r.Header.Set("HX-Request", "true")
			w := httptest.NewRecorder()
			s.action(func(*http.Request) string { return back }, h)(w, r)
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Contains(t, w.Body.String(), "saved, but the page could not refresh")
			assert.Contains(t, w.Body.String(), `href="`+strings.ReplaceAll(back, "&", "&amp;")+`"`)
			assert.NotContains(t, w.Body.String(), "private read failure")
		})
	}
}

func TestActionFailureAndGateBackLinks(t *testing.T) {
	s := newTestServer(t, Config{Repos: reposServing(t), Browse: mocks.NewBrowseService(t)})
	for path, back := range map[string]string{
		"/demo/issues/3/comments":                "/demo/issues/3",
		"/demo/merge-requests/3/comments/7/edit": "/demo/merge-requests/3",
		"/demo/actions/runs/8/rerun-failed":      "/demo/actions/runs/8",
		"/-/inbox/pending/demo/abc/ban":          "/-/inbox?tab=moderation",
		"/-/inbox/deps/demo/open":                "/-/inbox?tab=deps",
	} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, path, nil)
			r.Header.Set("HX-Request", "true")
			w := httptest.NewRecorder()
			s.denyMergeGate(w, r, "your session has expired")
			assert.Equal(t, http.StatusForbidden, w.Code)
			assert.Contains(t, w.Body.String(), `href="`+back+`"`)
			w = httptest.NewRecorder()
			s.actionFailed(r.Context(), w, r, errors.New("private"), back)
			assert.Equal(t, http.StatusInternalServerError, w.Code)
			assert.Contains(t, w.Body.String(), `href="`+back+`"`)
			assert.Contains(t, w.Body.String(), "the action could not be completed")
			assert.NotContains(t, w.Body.String(), "the merge")
		})
	}
}
