// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bnema.dev/zerowrap"
	"context"
	"errors"
	"net/http"
	"strings"

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

// actionRefreshFailed reports a presentation failure without denying a committed write.
func (s *Server) actionRefreshFailed(ctx context.Context, r *http.Request, result ActionResult, back string, err error) (ActionResult, error) {
	id := requestID()
	log := zerowrap.FromCtx(ctx)
	log.Error().Err(err).Str("request_id", id).Str("path", logPath(r.URL.Path)).Msg("web action refresh failed")
	result.Fragment = ActionStatus("saved, but the page could not refresh. reload to see the result. reference: "+id, back)
	return result, nil
}

// actionBack also works at the session gate, before routing binds path values.
func actionBack(r *http.Request) string {
	path := r.URL.Path
	if strings.HasPrefix(path, inboxPath+"/pending/") {
		return pendingBack(r)
	}
	if strings.HasPrefix(path, inboxPath+"/deps/") {
		return depsBack(r)
	}
	if strings.HasPrefix(path, passkeysPath+"/") {
		return passkeysPath
	}
	for _, section := range []string{"/merge-requests", "/issues", "/actions/runs/"} {
		if at := strings.Index(path, section); at > 0 {
			tail := path[at+len(section):]
			if section == "/actions/runs/" {
				id, _, _ := strings.Cut(tail, "/")
				return path[:at] + section + id
			}
			tail = strings.TrimPrefix(tail, "/")
			n, _, _ := strings.Cut(tail, "/")
			if n == "" {
				return path[:at] + section
			}
			return path[:at] + section + "/" + n
		}
	}
	return "/"
}
