// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/bnema/zerowrap"

	pb "bnema.dev/ermite-api/gen/ermite"
)

// The writes of the merge request pages. Each is a POST that passed the gate
// (an Origin of this site and the owner's session), calls the API as the
// owner, and answers 303 to the page that shows the result, with
// "Cache-Control: no-store". A request the API refuses is answered with a
// state page that says why, in the words of the API for the rules the owner
// can fix (a text rule, a moved branch, a full merge request), and without
// detail for anything else.

// maxFormBytes bounds the body of a form: a description of 8,000 characters is
// at most 32,000 bytes.
const maxFormBytes = 128 << 10

// form reads the form of a POST, bounded.
func (s *Server) form(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		s.reject(r.Context(), w, r, http.StatusRequestEntityTooLarge, "form too large", "the form is larger than a merge request can be.", mrBase(r.PathValue("repo")))
		return false
	}
	return true
}

// seeOther redirects to a page, never cached.
func seeOther(w http.ResponseWriter, r *http.Request, to string) {
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// mrAction wraps a POST handler: it bounds the API calls, reads the form and
// maps the error of the API to a page.
func (s *Server) mrAction(h func(ctx context.Context, r *http.Request) (to string, back string, err error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		if !s.form(w, r) {
			return
		}
		to, back, err := h(ctx, r)
		if err != nil {
			s.actionFailed(ctx, w, r, err, back)
			return
		}
		seeOther(w, r, to)
	}
}

// actionFailed answers a refused action.
func (s *Server) actionFailed(ctx context.Context, w http.ResponseWriter, r *http.Request, err error, back string) {
	msg := ""
	var ce *connect.Error
	if errors.As(err, &ce) {
		msg = ce.Message()
	}
	switch code := connect.CodeOf(err); {
	case code == connect.CodeNotFound:
		w.Header().Set("Cache-Control", "no-store")
		s.write(ctx, w, r, http.StatusNotFound, s.notFoundPage())
	case code == connect.CodeInvalidArgument:
		s.reject(ctx, w, r, http.StatusBadRequest, "not accepted", rule(msg, "the server refused what was sent."), back)
	case code == connect.CodeFailedPrecondition, code == connect.CodeAborted:
		s.reject(ctx, w, r, http.StatusConflict, "not possible now", rule(msg, "the merge request is not in a state that allows this."), back)
	case code == connect.CodeAlreadyExists:
		s.reject(ctx, w, r, http.StatusConflict, "already there", rule(msg, "this already exists."), back)
	case code == connect.CodeResourceExhausted:
		s.reject(ctx, w, r, http.StatusConflict, "limit reached", rule(msg, "a limit of the merge request is reached."), back)
	case code == connect.CodePermissionDenied, code == connect.CodeUnauthenticated:
		s.reject(ctx, w, r, http.StatusForbidden, "forbidden", "the session is not allowed to do this.", back)
	case errors.Is(err, context.Canceled), code == connect.CodeCanceled:
		// The browser is gone.
	default:
		id := requestID()
		log := zerowrap.FromCtx(ctx)
		log.Error().Err(err).Str("request_id", id).Str("path", logPath(r.URL.Path)).Msg("web action failed")
		w.Header().Set("Cache-Control", "no-store")
		s.write(ctx, w, r, http.StatusInternalServerError, s.errorPage(id))
	}
}

// rule is a message of the API fit to show: the invisible characters marked,
// bounded, and a fallback for an empty one.
func rule(msg, fallback string) string {
	msg = strings.TrimSpace(visibleText(msg))
	if msg == "" {
		return fallback
	}
	return cutRunes(msg, 400)
}

// reject writes the page of a refused action, with the way back.
func (s *Server) reject(ctx context.Context, w http.ResponseWriter, r *http.Request, status int, title, line, back string) {
	if back == "" {
		back = "/"
	}
	l := s.stateLayout(title, itoa(status)+" · "+title, []Crumb{{Label: "-"}, {Label: title}})
	w.Header().Set("Cache-Control", "no-store")
	s.write(ctx, w, r, status, State(l, StatePage{
		Code: itoa(status), Title: title,
		Lines: []string{line, "nothing was changed by this request."},
		Back:  []Link{{Label: "back", Href: back}, {Label: "home", Href: "/"}},
	}))
}

// number reads the merge request number of the path.
func number(r *http.Request) (int64, error) { return mrNumber(r) }

// mrCreate is POST /<repo>/merge-requests: it opens a merge request.
func (s *Server) mrCreate(ctx context.Context, r *http.Request) (string, string, error) {
	repo := r.PathValue("repo")
	source := r.PostFormValue("source")
	back := repoHref(repo, "compare", source)
	res, err := s.cfg.MergeRequests.CreateMergeRequest(ctx, connect.NewRequest(&pb.CreateMergeRequestRequest{
		Repo: repo, Title: r.PostFormValue("title"), Description: normalizeNewlines(r.PostFormValue("description")),
		SourceBranch: source, TargetBranch: r.PostFormValue("target"),
	}))
	if err != nil {
		return "", back, err
	}
	return mrHref(repo, res.Msg.GetMergeRequest().GetNumber()), back, nil
}

// mrComment is POST /<repo>/merge-requests/<n>/comments: a general comment, or
// one on a line (path, line and side).
func (s *Server) mrComment(ctx context.Context, r *http.Request) (string, string, error) {
	repo := r.PathValue("repo")
	n, err := number(r)
	if err != nil {
		return "", "", connect.NewError(connect.CodeNotFound, err)
	}
	back := mrHref(repo, n)
	req := &pb.AddCommentRequest{Repo: repo, Number: n, Text: normalizeNewlines(r.PostFormValue("text")), Path: r.PostFormValue("path")}
	if req.Path != "" || r.PostFormValue("line") != "" || r.PostFormValue("side") != "" {
		line, err := strconv.ParseInt(r.PostFormValue("line"), 10, 32)
		if err != nil {
			return "", back, connect.NewError(connect.CodeInvalidArgument, errors.New("line is not a number"))
		}
		req.Line = int32(line)
		switch r.PostFormValue("side") {
		case "new":
			req.Side = pb.DiffSide_DIFF_SIDE_NEW
		case "old":
			req.Side = pb.DiffSide_DIFF_SIDE_OLD
		default:
			return "", back, connect.NewError(connect.CodeInvalidArgument, errors.New("side must be new or old"))
		}
	}
	res, err := s.cfg.MergeRequests.AddComment(ctx, connect.NewRequest(req))
	if err != nil {
		return "", back, err
	}
	return back + "#c" + res.Msg.GetComment().GetId(), back, nil
}

// mrResolve is POST /<repo>/merge-requests/<n>/comments/<id>/resolve.
//
//hexcheck:ignore suspicious-business-logic-in-adapter reads two form fields into an API call; the state of the comment is decided by the API
func (s *Server) mrResolve(ctx context.Context, r *http.Request) (string, string, error) {
	repo := r.PathValue("repo")
	n, err := number(r)
	if err != nil {
		return "", "", connect.NewError(connect.CodeNotFound, err)
	}
	back := mrHref(repo, n)
	resolved, err := formBool(r.PostFormValue("resolved"))
	if err != nil {
		return "", back, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("resolved: %w", err))
	}
	res, err := s.cfg.MergeRequests.ResolveComment(ctx, connect.NewRequest(&pb.ResolveCommentRequest{
		Repo: repo, Number: n, CommentId: r.PathValue("id"), Resolved: resolved,
	}))
	if err != nil {
		return "", back, err
	}
	return back + "#c" + res.Msg.GetComment().GetId(), back, nil
}

// mrClose is POST /<repo>/merge-requests/<n>/close.
func (s *Server) mrClose(ctx context.Context, r *http.Request) (string, string, error) {
	repo := r.PathValue("repo")
	n, err := number(r)
	if err != nil {
		return "", "", connect.NewError(connect.CodeNotFound, err)
	}
	back := mrHref(repo, n)
	if _, err := s.cfg.MergeRequests.CloseMergeRequest(ctx, connect.NewRequest(&pb.CloseMergeRequestRequest{Repo: repo, Number: n})); err != nil {
		return "", back, err
	}
	return back, back, nil
}

// mrMerge is POST /<repo>/merge-requests/<n>/merge. The form carries the mode,
// the head the owner was shown (head_id): the server merges only if the
// branch still points to it, and delete_branch when the owner asked to delete
// the source branch after the merge.
func (s *Server) mrMerge(ctx context.Context, r *http.Request) (string, string, error) {
	repo := r.PathValue("repo")
	n, err := number(r)
	if err != nil {
		return "", "", connect.NewError(connect.CodeNotFound, err)
	}
	back := mrHref(repo, n)
	var mode pb.MergeMode
	switch r.PostFormValue("mode") {
	case "fast-forward":
		mode = pb.MergeMode_MERGE_MODE_FAST_FORWARD
	case "squash":
		mode = pb.MergeMode_MERGE_MODE_SQUASH
	default:
		return "", back, connect.NewError(connect.CodeInvalidArgument, errors.New("mode must be fast-forward or squash"))
	}
	if _, err := s.cfg.MergeRequests.MergeMergeRequest(ctx, connect.NewRequest(&pb.MergeMergeRequestRequest{
		Repo: repo, Number: n, Mode: mode, HeadId: r.PostFormValue("head_id"),
		DeleteSourceBranch: r.PostFormValue("delete_branch") == "1",
	})); err != nil {
		return "", back, err
	}
	return back, back, nil
}

// mrRead is POST /<repo>/merge-requests/<n>/read: the owner marks the head
// they were shown as read. Reading a page never does it (see the top of mr.go).
func (s *Server) mrRead(ctx context.Context, r *http.Request) (string, string, error) {
	repo := r.PathValue("repo")
	n, err := number(r)
	if err != nil {
		return "", "", connect.NewError(connect.CodeNotFound, err)
	}
	back := mrHref(repo, n)
	if _, err := s.cfg.MergeRequests.MarkRead(ctx, connect.NewRequest(&pb.MarkReadRequest{
		Repo: repo, Number: n, HeadId: r.PostFormValue("head_id"),
	})); err != nil {
		return "", back, err
	}
	return back, back, nil
}

// formBool reads the "true" or "false" a form field carries.
func formBool(s string) (bool, error) {
	switch s {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, errors.New("must be true or false")
}

// normalizeNewlines turns the CRLF of a browser textarea into LF, the line
// break the API stores.
func normalizeNewlines(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}
