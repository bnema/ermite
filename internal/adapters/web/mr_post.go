// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The writes of the merge request pages. Each is a POST that passed the gate
// (an Origin of this site and the owner's session), calls the use case as the
// owner, and answers 303 to the page that shows the result, with
// "Cache-Control: no-store". A request the use case refuses is answered with a
// state page that says why, in the words of the use case for the rules the
// owner can fix (a text rule, a moved branch, a full merge request), and
// without detail for anything else.

// maxFormBytes bounds the body of a form: a description of 8,000 characters is
// at most 32,000 bytes.
const maxFormBytes = 128 << 10

// form reads the form of a POST, bounded.
func (s *Server) form(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		back, what := mrBase(r.PathValue("repo")), "a merge request"
		if strings.HasPrefix(r.URL.Path, inboxPath+"/") {
			back, what = inboxPath, "a moderation action"
		}
		if strings.Contains(r.URL.Path, "/issues") {
			back, what = issuesBase(r.PathValue("repo")), "an issue"
		}
		s.reject(r.Context(), w, r, http.StatusRequestEntityTooLarge, "form too large", "the form is larger than "+what+" can be.", back)
		return false
	}
	return true
}

// seeOther redirects to a page, never cached.
func seeOther(w http.ResponseWriter, r *http.Request, to string) {
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// mrAction wraps a POST handler: it bounds the use case calls, reads the form
// and maps the error of the use case to a page.
func (s *Server) mrAction(h func(ctx context.Context, r *http.Request) (to string, back string, err error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		if !s.form(w, r) {
			return
		}
		to, back, err := h(ctx, r)
		var re *rerender
		if errors.As(err, &re) && s.rerenderMR(ctx, w, r, re) {
			return
		}
		if err != nil {
			s.actionFailed(ctx, w, r, err, back)
			return
		}
		seeOther(w, r, to)
	}
}

// rerender is the error of a merge the use case refused for a reason the
// owner reads on the page of the request: the page is shown again, with the
// status, and the reason in its merge box. Any other error keeps actionFailed.
type rerender struct {
	status int
	msg    string
	err    error
}

func (e *rerender) Error() string { return e.err.Error() }
func (e *rerender) Unwrap() error { return e.err }

type mergeErrorKey struct{}

// withMergeError carries the reason of a refused merge to the page.
func withMergeError(ctx context.Context, msg string) context.Context {
	return context.WithValue(ctx, mergeErrorKey{}, msg)
}

func mergeErrorOf(ctx context.Context) string {
	msg, _ := ctx.Value(mergeErrorKey{}).(string)
	return msg
}

// rerenderMR writes the page of the request with the reason of the refusal.
// It reports false when the page cannot be built; the caller then answers as
// for any refused action.
func (s *Server) rerenderMR(ctx context.Context, w http.ResponseWriter, r *http.Request, re *rerender) bool {
	c, _, err := s.mrPage(withMergeError(ctx, re.msg), r, false)
	if err != nil {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	s.write(ctx, w, r, re.status, c)
	return true
}

// actionFailed answers a refused action.
func (s *Server) actionFailed(ctx context.Context, w http.ResponseWriter, r *http.Request, err error, back string) {
	// The words of the use case, for the rules the owner can fix.
	msg := err.Error()
	// The order is that of the API's mapping of the domain errors to codes
	// (rpc.codeOf), so that a web action answers what the API would.
	switch {
	case errors.Is(err, context.Canceled):
		// The browser is gone.
	case errors.Is(err, domain.ErrNotFound):
		w.Header().Set("Cache-Control", "no-store")
		s.write(ctx, w, r, http.StatusNotFound, s.notFoundPage())
	case errors.Is(err, domain.ErrAlreadyExists):
		s.reject(ctx, w, r, http.StatusConflict, "already there", rule(msg, "this already exists."), back)
	case errors.Is(err, domain.ErrUnauthenticated), errors.Is(err, domain.ErrForbidden), errors.Is(err, domain.ErrRejected):
		s.reject(ctx, w, r, http.StatusForbidden, "forbidden", "the session is not allowed to do this.", back)
	case errors.Is(err, domain.ErrPreconditionFailed), errors.Is(err, domain.ErrConflict):
		s.reject(ctx, w, r, http.StatusConflict, "not possible now", rule(msg, "the merge request is not in a state that allows this."), back)
	case errors.Is(err, domain.ErrResourceExhausted):
		s.reject(ctx, w, r, http.StatusConflict, "limit reached", rule(msg, "a limit of the merge request is reached."), back)
	case domain.IsInvalid(err):
		s.reject(ctx, w, r, http.StatusBadRequest, "not accepted", rule(msg, "the server refused what was sent."), back)
	default:
		s.internalFailure(ctx, w, r, err)
	}
}

// internalFailure answers a failure of an action that is not the owner's to
// fix: the 500 page with an id, and the cause in the log.
func (s *Server) internalFailure(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) {
	id := requestID()
	log := zerowrap.FromCtx(ctx)
	log.Error().Err(err).Str("request_id", id).Str("path", logPath(r.URL.Path)).Msg("web action failed")
	w.Header().Set("Cache-Control", "no-store")
	s.write(ctx, w, r, http.StatusInternalServerError, s.errorPage(id))
}

// rule is a message of the use case fit to show: the invisible characters marked,
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
	l := s.stateLayout(title, []Crumb{{Label: title, Current: true}})
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
	name, err := domain.ParseRepoName(repo)
	if err != nil {
		return "", back, err
	}
	mr, err := s.cfg.MergeRequests.Create(ctx, principal(ctx), name, ports.CreateMergeRequestParams{
		Title: r.PostFormValue("title"), Description: normalizeNewlines(r.PostFormValue("description")),
		Source: source, Target: r.PostFormValue("target"),
	})
	if err != nil {
		return "", back, err
	}
	return mrHref(repo, mr.Number), back, nil
}

// formError is a form the server refuses before asking a use case: it matches
// domain.ErrInvalid and says just what is wrong.
type formError string

func (e formError) Error() string { return string(e) }

func (formError) Unwrap() error { return domain.ErrInvalid }

// mrComment is POST /<repo>/merge-requests/<n>/comments: a general comment, or
// one on a line (path, line and side).
func (s *Server) mrComment(ctx context.Context, r *http.Request) (string, string, error) {
	repo := r.PathValue("repo")
	n, err := number(r)
	if err != nil {
		return "", "", err
	}
	back := mrHref(repo, n)
	params := ports.AddCommentParams{Text: normalizeNewlines(r.PostFormValue("text")), Path: r.PostFormValue("path")}
	if params.Path != "" || r.PostFormValue("line") != "" || r.PostFormValue("side") != "" {
		line, err := strconv.ParseInt(r.PostFormValue("line"), 10, 32)
		if err != nil {
			return "", back, formError("line is not a number")
		}
		params.Line = int(line)
		switch r.PostFormValue("side") {
		case "new":
			params.Side = domain.SideNew
		case "old":
			params.Side = domain.SideOld
		default:
			return "", back, formError("side must be new or old")
		}
	}
	if raw := r.PostFormValue("reply_to"); raw != "" {
		params.ReplyTo = commentID(raw)
		if params.ReplyTo == 0 {
			return "", back, formError("reply_to is not a note id")
		}
	}
	if raw := r.PostFormValue("start_line"); raw != "" {
		v, e := strconv.ParseInt(raw, 10, 32)
		if e != nil {
			return "", back, formError("start_line is not a number")
		}
		params.StartLine = int(v)
	}
	params.Verdict, err = domain.ParseVerdict(r.PostFormValue("verdict"))
	if err != nil {
		return "", back, err
	}
	name, err := domain.ParseRepoName(repo)
	if err != nil {
		return "", back, err
	}
	back = withFrom(back, formFrom(r))
	c, err := s.cfg.MergeRequests.AddComment(ctx, principal(ctx), name, n, params)
	if err != nil {
		return "", back, err
	}
	return back + "#c" + strconv.FormatInt(c.ID, 10), back, nil
}

// mrResolve is POST /<repo>/merge-requests/<n>/comments/<id>/resolve.
//
//hexcheck:ignore suspicious-business-logic-in-adapter reads two form fields into a use case call; the state of the comment is decided by the use case
func (s *Server) mrResolve(ctx context.Context, r *http.Request) (string, string, error) {
	repo := r.PathValue("repo")
	n, err := number(r)
	if err != nil {
		return "", "", err
	}
	back := mrHref(repo, n)
	resolved, err := formBool(r.PostFormValue("resolved"))
	if err != nil {
		return "", back, formError("resolved: " + err.Error())
	}
	name, err := domain.ParseRepoName(repo)
	if err != nil {
		return "", back, err
	}
	// An id this server never issued is 0: the use case answers not found,
	// after the repository and the scope were checked, as for any unknown
	// comment.
	back = withFrom(back, formFrom(r))
	c, err := s.cfg.MergeRequests.ResolveComment(ctx, principal(ctx), name, n, commentID(r.PathValue("id")), resolved)
	if err != nil {
		return "", back, err
	}
	return back + "#c" + strconv.FormatInt(c.ID, 10), back, nil
}

// commentID parses the id of a comment in a path: a positive decimal number,
// written without sign or leading zeros. Anything else is 0, which names no
// comment.
func commentID(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 || strconv.FormatInt(n, 10) != s {
		return 0
	}
	return n
}

// headID parses a commit id of a form; the empty string is the zero id.
func headID(s string) (domain.ObjectID, error) {
	if s == "" {
		return domain.ObjectID{}, nil
	}
	var v domain.Violations
	id, err := domain.ParseObjectID(s)
	if err != nil {
		v.Add("head_id", "is not a full commit id")
	}
	return id, v.Err()
}

// mrClose is POST /<repo>/merge-requests/<n>/close.
func (s *Server) mrClose(ctx context.Context, r *http.Request) (string, string, error) {
	repo := r.PathValue("repo")
	n, err := number(r)
	if err != nil {
		return "", "", err
	}
	back := mrHref(repo, n)
	name, err := domain.ParseRepoName(repo)
	if err != nil {
		return "", back, err
	}
	if _, err := s.cfg.MergeRequests.Close(ctx, principal(ctx), name, n); err != nil {
		return "", back, err
	}
	return back, back, nil
}

// mrReady is POST /<repo>/merge-requests/<n>/ready: the draft prefix leaves
// the title.
func (s *Server) mrReady(ctx context.Context, r *http.Request) (string, string, error) {
	repo := r.PathValue("repo")
	n, err := number(r)
	if err != nil {
		return "", "", err
	}
	back := mrHref(repo, n)
	name, err := domain.ParseRepoName(repo)
	if err != nil {
		return "", back, err
	}
	if _, err := s.cfg.MergeRequests.MarkReady(ctx, principal(ctx), name, n); err != nil {
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
		return "", "", err
	}
	back := mrHref(repo, n)
	var mode domain.MergeMode
	switch r.PostFormValue("mode") {
	case "fast-forward":
		mode = domain.MergeModeFastForward
	case "squash":
		mode = domain.MergeModeSquash
	case "merge":
		mode = domain.MergeModeMerge
	default:
		err := formError("mode must be fast-forward, squash or merge")
		return "", back, &rerender{status: http.StatusBadRequest, msg: err.Error(), err: err}
	}
	name, err := domain.ParseRepoName(repo)
	if err != nil {
		return "", back, err
	}
	head, err := headID(r.PostFormValue("head_id"))
	if err != nil {
		return "", back, err
	}
	res, err := s.cfg.MergeRequests.Merge(ctx, principal(ctx), name, n, ports.MergeParams{
		Mode: mode, HeadID: head, DeleteSource: r.PostFormValue("delete_branch") == "1",
	})
	switch {
	case errors.Is(err, domain.ErrPreconditionFailed), errors.Is(err, domain.ErrConflict):
		return "", back, &rerender{status: http.StatusConflict, msg: rule(err.Error(), "the merge request is not in a state that allows this."), err: err}
	case err != nil:
		return "", back, err
	}
	if res.SourceDeleted {
		// The deletion follows the answer: the page says it is being done.
		return back + "?deleting=1", back, nil
	}
	return back, back, nil
}

// mrAutoMerge is POST /<repo>/merge-requests/<n>/auto-merge: the form of the
// merge (mode, head_id) asks to merge once the checks pass. The server records
// the head as it is when it enables (the params carry no head); head_id is only
// validated.
func (s *Server) mrAutoMerge(ctx context.Context, r *http.Request) (string, string, error) {
	repo := r.PathValue("repo")
	n, err := number(r)
	if err != nil {
		return "", "", err
	}
	back := mrHref(repo, n)
	var mode domain.MergeMode
	switch r.PostFormValue("mode") {
	case "fast-forward":
		mode = domain.MergeModeFastForward
	case "squash":
		mode = domain.MergeModeSquash
	case "merge":
		mode = domain.MergeModeMerge
	default:
		err := formError("mode must be fast-forward, squash or merge")
		return "", back, &rerender{status: http.StatusBadRequest, msg: err.Error(), err: err}
	}
	name, err := domain.ParseRepoName(repo)
	if err != nil {
		return "", back, err
	}
	if _, err := headID(r.PostFormValue("head_id")); err != nil {
		return "", back, err
	}
	_, err = s.cfg.MergeRequests.EnableAutoMerge(ctx, principal(ctx), name, n, ports.EnableAutoMergeParams{Mode: mode})
	switch {
	case errors.Is(err, domain.ErrPreconditionFailed), errors.Is(err, domain.ErrConflict):
		return "", back, &rerender{status: http.StatusConflict, msg: rule(err.Error(), "the merge request is not in a state that allows this."), err: err}
	case err != nil:
		return "", back, err
	}
	return back, back, nil
}

// mrCancelAutoMerge is POST /<repo>/merge-requests/<n>/auto-merge/cancel.
func (s *Server) mrCancelAutoMerge(ctx context.Context, r *http.Request) (string, string, error) {
	repo := r.PathValue("repo")
	n, err := number(r)
	if err != nil {
		return "", "", err
	}
	back := mrHref(repo, n)
	name, err := domain.ParseRepoName(repo)
	if err != nil {
		return "", back, err
	}
	_, err = s.cfg.MergeRequests.CancelAutoMerge(ctx, principal(ctx), name, n)
	switch {
	case errors.Is(err, domain.ErrPreconditionFailed), errors.Is(err, domain.ErrConflict):
		return "", back, &rerender{status: http.StatusConflict, msg: rule(err.Error(), "the merge request is not in a state that allows this."), err: err}
	case err != nil:
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
		return "", "", err
	}
	back := mrHref(repo, n)
	name, err := domain.ParseRepoName(repo)
	if err != nil {
		return "", back, err
	}
	head, err := headID(r.PostFormValue("head_id"))
	if err != nil {
		return "", back, err
	}
	if _, err := s.cfg.MergeRequests.MarkRead(ctx, principal(ctx), name, n, head); err != nil {
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
// break the use case stores.
func normalizeNewlines(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

// mrEditComment passes source text to the author-checked use case.
func (s *Server) mrEditComment(ctx context.Context, r *http.Request) (string, string, error) {
	n, err := number(r)
	if err != nil {
		return "", "", err
	}
	back := withFrom(mrHref(r.PathValue("repo"), n), formFrom(r))
	name, err := domain.ParseRepoName(r.PathValue("repo"))
	if err != nil {
		return "", back, err
	}
	c, err := s.cfg.MergeRequests.EditComment(ctx, principal(ctx), name, n, commentID(r.PathValue("id")), normalizeNewlines(r.PostFormValue("text")))
	return back + "#c" + strconv.FormatInt(c.ID, 10), back, err
}

// mrDeleteComment passes the note id to the author-checked use case.
func (s *Server) mrDeleteComment(ctx context.Context, r *http.Request) (string, string, error) {
	n, err := number(r)
	if err != nil {
		return "", "", err
	}
	back := withFrom(mrHref(r.PathValue("repo"), n), formFrom(r))
	name, err := domain.ParseRepoName(r.PathValue("repo"))
	if err != nil {
		return "", back, err
	}
	c, err := s.cfg.MergeRequests.DeleteComment(ctx, principal(ctx), name, n, commentID(r.PathValue("id")))
	return back + "#c" + strconv.FormatInt(c.ID, 10), back, err
}
