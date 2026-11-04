// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestMRCommentActions(t *testing.T) {
	for _, op := range []string{"create", "edit", "delete", "resolve"} {
		for _, hx := range []bool{false, true} {
			for _, line := range []bool{false, true} {
				t.Run(op+"/hx="+boolText(hx)+"/line="+boolText(line), func(t *testing.T) {
					s, svc := asyncMergeServer(t, domain.MRStateOpen)
					text, err := domain.ParsePlainText("hello <script>alert(1)</script>")
					require.NoError(t, err)
					c := domain.Comment{ID: 7, Text: text}
					values := url.Values{"text": {text.String()}, "thread": {"mr-comment-thread"}, "resolved": {"true"}}
					if line {
						c.Path = "a.go"
						c.Side = domain.SideNew
						c.Line = 1
						values.Set("path", "a.go")
						values.Set("line", "1")
						values.Set("side", "new")
						values.Set("thread", "mr-line-thread-s")
					}
					var h ActionFunc
					path := "/demo/merge-requests/3/comments"
					switch op {
					case "create":
						h = s.mrComment
						svc.EXPECT().AddComment(mock.Anything, mock.Anything, mock.Anything, int64(3), mock.Anything).Return(c, nil).Once()
					case "edit":
						h = s.mrEditComment
						path += "/7/edit"
						svc.EXPECT().EditComment(mock.Anything, mock.Anything, mock.Anything, int64(3), int64(7), text.String()).Return(c, nil).Once()
					case "delete":
						h = s.mrDeleteComment
						path += "/7/delete"
						svc.EXPECT().DeleteComment(mock.Anything, mock.Anything, mock.Anything, int64(3), int64(7)).Return(c, nil).Once()
					case "resolve":
						h = s.mrResolve
						path += "/7/resolve"
						c.Resolved = true
						svc.EXPECT().ResolveComment(mock.Anything, mock.Anything, mock.Anything, int64(3), int64(7), true).Return(c, nil).Once()
					}
					if hx {
						svc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Unset()
						comments := []domain.Comment{c}
						if op == "delete" {
							comments = nil
						}
						svc.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything, int64(3), mock.Anything).Return(ports.MergeRequestDetail{Comments: comments}, nil).Once()
					}
					r := autoMergeRequest(path, values.Encode())
					r.SetPathValue("id", "7")
					if hx {
						r.Header.Set("HX-Request", "true")
					}
					w := httptest.NewRecorder()
					s.action(mrHrefFromPath, h)(w, r)
					if !hx {
						assert.Equal(t, http.StatusSeeOther, w.Code)
						assert.Equal(t, "/demo/merge-requests/3#c7", w.Header().Get("Location"))
						return
					}
					assert.Equal(t, http.StatusOK, w.Code)
					body := w.Body.String()
					assert.Contains(t, body, "<hx-partial")
					assert.Contains(t, body, `id="mr-comment-count"`)
					assert.NotContains(t, body, "<html")
					assert.NotContains(t, body, "<script>")
					if op == "delete" {
						assert.Contains(t, body, `hx-target="#c7" hx-swap="delete"`)
					} else {
						assert.Contains(t, body, "hello")
					}
					if op == "create" {
						target := "mr-comment-thread"
						if line {
							target = "mr-line-thread"
						}
						assert.Contains(t, body, `hx-target="#`+target+`" hx-swap="beforeend"`)
						if line {
							assert.Contains(t, body, `hx-target="#form-cf-line-s" hx-swap="delete"`)
						} else {
							assert.Contains(t, body, `hx-target="#form-cf-general" hx-swap="outerHTML"`)
							assert.Contains(t, body, `></textarea>`)
						}
					} else if op != "delete" {
						assert.Contains(t, body, `hx-target="#c7" hx-swap="outerMorph"`)
					}
					if line && op != "create" {
						assert.Contains(t, body, `hx-target="#c7-s"`+map[bool]string{true: ` hx-swap="delete"`, false: ""}[op == "delete"])
					}
				})
			}
		}
	}
}

func TestMRCommentInvalidThread(t *testing.T) {
	for _, thread := range []string{"", "#bad", "x,y", strings.Repeat("a", 65), "other-container"} {
		t.Run(thread, func(t *testing.T) {
			s, _ := asyncMergeServer(t, domain.MRStateOpen)
			r := autoMergeRequest("/demo/merge-requests/3/comments", url.Values{"thread": {thread}}.Encode())
			r.Header.Set("HX-Request", "true")
			w := httptest.NewRecorder()
			s.action(mrHrefFromPath, s.mrComment)(w, r)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Body.String(), `id="action-status"`)
		})
	}
}

func TestMRReplyAndDeletedThread(t *testing.T) {
	for _, line := range []bool{false, true} {
		for _, remove := range []bool{false, true} {
			t.Run("line="+boolText(line)+"/delete="+boolText(remove), func(t *testing.T) {
				s, svc := asyncMergeServer(t, domain.MRStateOpen)
				text, err := domain.ParsePlainText("reply & text")
				require.NoError(t, err)
				root := domain.Comment{ID: 1, Deleted: remove}
				if line {
					root.Path = "a.go"
					root.Side = domain.SideNew
					root.Line = 1
				}
				reply := domain.Comment{ID: 7, ReplyTo: 1, Text: text}
				svc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Unset()
				svc.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything, int64(3), mock.Anything).Return(ports.MergeRequestDetail{Comments: []domain.Comment{root, reply}}, nil).Once()
				path := "/demo/merge-requests/3/comments"
				h := s.mrComment
				values := url.Values{"text": {text.String()}, "reply_to": {"1"}, "thread": {"replies-1"}, "from": {"25"}}
				if remove {
					path += "/1/delete"
					h = s.mrDeleteComment
					svc.EXPECT().DeleteComment(mock.Anything, mock.Anything, mock.Anything, int64(3), int64(1)).Return(root, nil).Once()
				} else {
					svc.EXPECT().AddComment(mock.Anything, mock.Anything, mock.Anything, int64(3), mock.Anything).Return(reply, nil).Once()
				}
				r := autoMergeRequest(path, values.Encode())
				r.SetPathValue("id", "1")
				r.Header.Set("HX-Request", "true")
				w := httptest.NewRecorder()
				s.action(mrHrefFromPath, h)(w, r)
				assert.Equal(t, http.StatusOK, w.Code)
				body := w.Body.String()
				if remove {
					assert.Contains(t, body, `hx-target="#c1" hx-swap="outerMorph"`)
					assert.Contains(t, body, "deleted note")
					assert.Contains(t, body, `id="c7"`)
				} else {
					assert.Contains(t, body, `hx-target="#replies-1" hx-swap="beforeend"`)
					assert.Contains(t, body, `hx-target="#form-reply-1" hx-swap="outerHTML"`)
					assert.Contains(t, body, `name="from" value="25"`)
					assert.Contains(t, body, "reply &amp; text")
					if line {
						assert.Contains(t, body, `hx-target="#replies-1-s"`)
					}
				}
			})
		}
	}
}

func TestCommentFormResetAndIssueEnhancement(t *testing.T) {
	f := CommentForm{ID: "cf-general", Action: "/demo/merge-requests/3/comments", Title: "Comment"}
	html := renderToString(t, commentForm(f, ""))
	assert.Contains(t, html, `hx-post="/demo/merge-requests/3/comments"`)
	assert.Contains(t, html, `name="thread" value="mr-comment-thread"`)
	assert.Contains(t, html, `id="form-cf-general"`)
	// An empty textarea's value and defaultValue are both empty: hasDraft stays false.
	assert.Contains(t, html, `placeholder="Markdown, 8000 characters at most"></textarea>`)
	f.Issue = true
	html = renderToString(t, commentForm(f, ""))
	assert.Contains(t, html, `hx-post="/demo/merge-requests/3/comments"`)
	assert.NotContains(t, html, `name="thread"`)
}

func TestInlineRangeDisclosure(t *testing.T) {
	for _, start := range []string{"", "152"} {
		body := renderToString(t, commentForm(CommentForm{ID: "cf-line", Path: "a.go", Line: "153", StartLine: start}, ""))
		assert.Contains(t, body, `<summary>Extend to a range…</summary>`)
		assert.Contains(t, body, `name="start_line" min="1" value="`+start+`"`)
		if start == "" {
			assert.Contains(t, body, `<details class="f-range-d">`)
		} else {
			assert.Contains(t, body, `<details class="f-range-d" open>`)
		}
	}
}

func TestUnfoldFollowsNoteMetadata(t *testing.T) {
	body := renderToString(t, commentView(CommentView{ID: "1", Resolved: true, Edited: true, Where: "a.go:1", Range: "lines 1–2"}, ""))
	assert.Less(t, strings.Index(body, `>edited</span>`), strings.Index(body, `class="c-unfold"`))
}
