// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a-h/templ"
	"github.com/stretchr/testify/assert"

	"bnema.dev/ermite/internal/domain"
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
