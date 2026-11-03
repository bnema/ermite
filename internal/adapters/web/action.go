// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"

	"github.com/a-h/templ"
)

// ActionResult describes the progressive response to a form action.
// Redirect is required; Fragment is optional and only rendered for htmx.
type ActionResult struct {
	Redirect string
	Fragment templ.Component
}

type ActionFunc func(context.Context, *http.Request) (ActionResult, error)

// action bounds form input and execution, and maps failures consistently.
func (s *Server) action(back func(*http.Request) string, h ActionFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		if !s.form(w, r) {
			return
		}
		res, err := h(ctx, r)
		var re *rerender
		if errors.As(err, &re) && s.rerenderMR(ctx, w, r, re) {
			return
		}
		if err != nil {
			s.actionFailed(ctx, w, r, err, back(r))
			return
		}
		if isHX(r) {
			w.Header().Set("Cache-Control", "no-store")
			if res.Fragment == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			s.write(ctx, w, r, http.StatusOK, actionSuccess(res.Fragment))
			return
		}
		seeOther(w, r, res.Redirect)
	}
}

func isHX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }
