// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"strconv"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The writes of the issue pages follow those of the merge request pages
// (mr_post.go): each is a POST that passed the gate (an Origin of this site
// and the owner's session), calls the use case as the owner and answers 303
// to the page that shows the result, "Cache-Control: no-store".

// issueAction wraps a POST handler like mrAction does.
func (s *Server) issueAction(h func(ctx context.Context, r *http.Request) (to string, back string, err error)) http.HandlerFunc {
	return s.mrAction(h)
}

// issueCreate is POST /<repo>/issues: it opens an issue.
func (s *Server) issueCreate(ctx context.Context, r *http.Request) (string, string, error) {
	repo := r.PathValue("repo")
	back := issuesBase(repo) + "/new"
	name, err := domain.ParseRepoName(repo)
	if err != nil {
		return "", back, err
	}
	i, err := s.cfg.Issues.Create(ctx, principal(ctx), name, ports.CreateIssueParams{
		Title: r.PostFormValue("title"), Body: normalizeNewlines(r.PostFormValue("body")),
	})
	if err != nil {
		return "", back, err
	}
	return issueHref(repo, i.Number), back, nil
}

// issueComment is POST /<repo>/issues/<n>/comments.
func (s *Server) issueComment(ctx context.Context, r *http.Request) (string, string, error) {
	repo := r.PathValue("repo")
	n, err := number(r)
	if err != nil {
		return "", "", err
	}
	back := issueHref(repo, n)
	name, err := domain.ParseRepoName(repo)
	if err != nil {
		return "", back, err
	}
	c, err := s.cfg.Issues.AddComment(ctx, principal(ctx), name, n, normalizeNewlines(r.PostFormValue("text")))
	if err != nil {
		return "", back, err
	}
	return back + "#c" + strconv.FormatInt(c.ID, 10), back, nil
}

// issueClose is POST /<repo>/issues/<n>/close.
func (s *Server) issueClose(ctx context.Context, r *http.Request) (string, string, error) {
	return s.issueTransition(ctx, r, s.cfg.Issues.Close)
}

// issueReopen is POST /<repo>/issues/<n>/reopen.
func (s *Server) issueReopen(ctx context.Context, r *http.Request) (string, string, error) {
	return s.issueTransition(ctx, r, s.cfg.Issues.Reopen)
}

//hexcheck:ignore suspicious-business-logic-in-adapter closes or reopens through the use case that owns the transition; the form only picks which
func (s *Server) issueTransition(ctx context.Context, r *http.Request, do func(context.Context, domain.Principal, domain.RepoName, int64) (domain.Issue, error)) (string, string, error) {
	repo := r.PathValue("repo")
	n, err := number(r)
	if err != nil {
		return "", "", err
	}
	back := issueHref(repo, n)
	name, err := domain.ParseRepoName(repo)
	if err != nil {
		return "", back, err
	}
	if _, err := do(ctx, principal(ctx), name, n); err != nil {
		return "", back, err
	}
	return back, back, nil
}
