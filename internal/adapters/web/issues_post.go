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

// issueCreate is POST /<repo>/issues: it opens an issue.
func (s *Server) issueCreate(ctx context.Context, r *http.Request) (ActionResult, error) {
	repo := r.PathValue("repo")
	name, err := domain.ParseRepoName(repo)
	if err != nil {
		return ActionResult{}, err
	}
	i, err := s.cfg.Issues.Create(ctx, principal(ctx), name, ports.CreateIssueParams{
		Title: r.PostFormValue("title"), Body: normalizeNewlines(r.PostFormValue("body")),
	})
	if err != nil {
		return ActionResult{}, err
	}
	return ActionResult{Redirect: issueHref(repo, i.Number)}, nil
}

// issueComment is POST /<repo>/issues/<n>/comments.
func (s *Server) issueComment(ctx context.Context, r *http.Request) (ActionResult, error) {
	repo := r.PathValue("repo")
	n, err := number(r)
	if err != nil {
		return ActionResult{}, err
	}
	back := issueHref(repo, n)
	name, err := domain.ParseRepoName(repo)
	if err != nil {
		return ActionResult{}, err
	}
	c, err := s.cfg.Issues.AddComment(ctx, principal(ctx), name, n, normalizeNewlines(r.PostFormValue("text")))
	if err != nil {
		return ActionResult{}, err
	}
	result := ActionResult{Redirect: back + "#c" + strconv.FormatInt(c.ID, 10)}
	if !isHX(r) {
		return result, nil
	}
	d, err := s.issueRepo(ctx, repo)
	if err != nil {
		return s.actionRefreshFailed(ctx, r, result, back, err)
	}
	detail, err := s.cfg.Issues.Get(ctx, principal(ctx), name, n)
	if err != nil {
		return s.actionRefreshFailed(ctx, r, result, back, err)
	}
	views := s.issueCommentViews(ctx, d, []domain.IssueComment{c})
	result.Fragment = issueCommentCreated(views[0], len(detail.Comments), CommentForm{ID: "cf-general", Action: issueHref(repo, n, "comments"), Title: "comment on the issue", Issue: true})
	return result, nil
}

// issueClose is POST /<repo>/issues/<n>/close.
func (s *Server) issueClose(ctx context.Context, r *http.Request) (ActionResult, error) {
	return s.issueTransition(ctx, r, s.cfg.Issues.Close)
}

// issueReopen is POST /<repo>/issues/<n>/reopen.
func (s *Server) issueReopen(ctx context.Context, r *http.Request) (ActionResult, error) {
	return s.issueTransition(ctx, r, s.cfg.Issues.Reopen)
}

func (s *Server) issueTransition(ctx context.Context, r *http.Request, do func(context.Context, domain.Principal, domain.RepoName, int64) (domain.Issue, error)) (ActionResult, error) {
	repo := r.PathValue("repo")
	n, err := number(r)
	if err != nil {
		return ActionResult{}, err
	}
	back := issueHref(repo, n)
	name, err := domain.ParseRepoName(repo)
	if err != nil {
		return ActionResult{}, err
	}
	i, err := do(ctx, principal(ctx), name, n)
	if err != nil {
		return ActionResult{}, err
	}
	result := ActionResult{Redirect: back}
	if !isHX(r) {
		return result, nil
	}
	d, err := s.issueRepo(ctx, repo)
	if err != nil {
		return s.actionRefreshFailed(ctx, r, result, back, err)
	}
	result.Fragment = issueStateFragment(IssuePage{State: string(i.State), Transition: issueTransitionAction(repo, i, d.repo.Archived)})
	return result, nil
}

func issueHrefFromPath(r *http.Request) string {
	n, err := number(r)
	if err != nil {
		return ""
	}
	return issueHref(r.PathValue("repo"), n)
}
func issueCreateBack(r *http.Request) string { return issuesBase(r.PathValue("repo")) + "/new" }
