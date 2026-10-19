// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The issue pages (ADR 0031). They follow the merge request pages: they read
// through IssueService as the visitor of the request, anonymous sees the
// issues of public repositories, and only the signed-in owner gets the forms
// (issues_post.go handles them). A repository whose issues mode is off, a
// missing one and a private one for an anonymous visitor are the same 404.
//
// Routes:
//
//	GET  /<repo>/issues             the list, open or closed (?state=), paged (?page=)
//	GET  /<repo>/issues/new         the form that opens one (owner)
//	GET  /<repo>/issues/<n>         the issue with its comments
//
// Issues and merge requests share their numbers, so "#n" may name either: with
// an IssueNumberResolver the page of the wrong kind redirects to the right one.

// issuesBase is the URL of the issue pages of a repository.
func issuesBase(repo string) string { return repoHref(repo, "issues") }

func issueHref(repo string, n int64, sub ...string) string {
	return repoHref(repo, append([]string{"issues", strconv.FormatInt(n, 10)}, sub...)...)
}

// IssueRow is one issue in the list.
type IssueRow struct {
	Number string
	Href   string
	Title  string
	Author string
	State  string
	When   string
	WhenAt string
	Cursor bool
}

// IssueListPage is the list of the issues of a repository.
type IssueListPage struct {
	Rows []IssueRow
	// Closed is true on the tab of the closed issues.
	Closed               bool
	OpenHref, ClosedHref string
	Older, Newest        string
	NewHref              string // the form that opens an issue; "" unless the owner
	Live                 bool
}

// IssueActions are the buttons of the owner on an issue.
type IssueActions struct {
	Action string // the URL that closes or reopens
	Label  string
}

// IssuePage is an issue with its comments.
type IssuePage struct {
	Number    string
	Title     string
	State     string
	Open      bool
	Opened    Person
	Updated   string
	UpdatedAt string
	// Desc is the body rendered as Markdown, "" when it is empty.
	Desc       template.HTML
	Comments   []CommentView
	Form       *CommentForm
	Transition *IssueActions
	Live       bool
}

// IssueNewPage is the form that opens an issue.
type IssueNewPage struct {
	Action, Cancel string
}

// issueRepo reads the repository of an issue page: one with issues off is
// as missing as one the caller cannot see.
func (s *Server) issueRepo(ctx context.Context, rawName string) (*repoData, error) {
	d, err := s.loadRepo(ctx, rawName)
	if err != nil {
		return nil, err
	}
	if d.repo.Settings.Issues.OrDefault() == domain.IssuesOff {
		return nil, errNotFound
	}
	return d, nil
}

// externalAuthor is the label a visitor gave, as plain text, or "anonymous".
func (s *Server) externalAuthor(label string) string {
	if a := strings.TrimSpace(visibleText(label)); a != "" {
		return a
	}
	return "anonymous"
}

func (s *Server) issueRow(repo string, i domain.Issue) IssueRow {
	row := IssueRow{
		Number: "#" + strconv.FormatInt(i.Number, 10), Href: issueHref(repo, i.Number),
		Title: visibleText(i.Title.String()), State: string(i.State), Author: s.who(i.Author),
	}
	if i.External {
		row.Author = s.externalAuthor(i.Author)
	}
	ts := i.UpdatedAt
	if ts.IsZero() {
		ts = i.CreatedAt
	}
	row.When, row.WhenAt = whenOf(s.cfg.Now(), ts)
	return row
}

// issueList lists the issues of one state, a page at a time.
//
//hexcheck:ignore suspicious-business-logic-in-adapter builds the list page from the use case results; whether the New issue button shows only reads the mode
func (s *Server) issueList(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	d, err := s.issueRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	state := domain.IssueStateOpen
	switch r.URL.Query().Get("state") {
	case "", "open":
	case "closed":
		state = domain.IssueStateClosed
	default:
		return nil, 0, errNotFound
	}
	token := r.URL.Query().Get("page")
	base := issuesBase(d.name())
	closed := state == domain.IssueStateClosed
	link := func(page string) string {
		q := url.Values{}
		if closed {
			q.Set("state", "closed")
		}
		if page != "" {
			q.Set("page", page)
		}
		if len(q) == 0 {
			return base
		}
		return base + "?" + q.Encode()
	}

	res, err := s.cfg.Issues.List(ctx, principal(ctx), d.repo.Name, ports.ListIssuesParams{
		State: state, PageSize: pageSize, PageToken: token,
	})
	if err != nil {
		return nil, 0, err
	}
	p := IssueListPage{Closed: closed, OpenHref: base, ClosedHref: base + "?state=closed"}
	for _, i := range res.Issues {
		p.Rows = append(p.Rows, s.issueRow(d.name(), i))
	}
	if len(p.Rows) > 0 {
		p.Rows[0].Cursor = true
	}
	if res.NextPageToken != "" {
		p.Older = link(res.NextPageToken)
	}
	if token != "" {
		p.Newest = link("")
	}
	if SignedIn(ctx) || (s.cfg.IssueForm != nil && d.repo.Settings.Issues.OrDefault() == domain.IssuesPublic) {
		p.NewHref = issuesBase(d.name()) + "/new"
	}

	l := s.repoLayout(ctx, d, "issues")
	l.Title = "issues · " + d.name() + " · " + s.instance
	l.Crumbs[0].Current = false
	l.Crumbs = append(l.Crumbs, Crumb{Label: "issues", Current: true})
	if token == "" {
		p.Live = true
		l.Live, l.LiveURL = true, liveURL("repo:"+d.name(), "view=issues")
	}
	l.HasRail = true
	return IssueList(l, p), http.StatusOK, nil
}

// issueNew is the form that opens an issue; it is the owner's.
func (s *Server) issueNew(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	d, err := s.issueRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	if !SignedIn(ctx) {
		return nil, 0, errNotFound
	}
	l := s.repoLayout(ctx, d, "issues")
	l.Title = "new issue · " + d.name() + " · " + s.instance
	l.Crumbs[0].Current = false
	l.Crumbs = append(l.Crumbs,
		Crumb{Label: "issues", Href: issuesBase(d.name())},
		Crumb{Label: "new", Current: true},
	)
	l.HasRail = true
	return IssueNew(l, IssueNewPage{Action: issuesBase(d.name()), Cancel: issuesBase(d.name())}), http.StatusOK, nil
}

// issueShow shows an issue with its comments.
//
//hexcheck:ignore suspicious-business-logic-in-adapter builds the issue page from the use case results; no rule is decided here
func (s *Server) issueShow(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	n, err := mrNumber(r)
	if err != nil {
		return nil, 0, err
	}
	d, err := s.issueRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	res, err := s.cfg.Issues.Get(ctx, principal(ctx), d.repo.Name, n)
	if err != nil {
		return nil, 0, err
	}
	i := res.Issue
	now := s.cfg.Now()
	signed := SignedIn(ctx)
	self := issueHref(d.name(), n)

	p := IssuePage{
		Number: "#" + strconv.FormatInt(n, 10), Title: visibleText(i.Title.String()),
		State: string(i.State), Open: i.IsOpen(),
	}
	if body := i.Body.String(); strings.TrimSpace(body) != "" {
		var h template.HTML
		var err error
		if i.External {
			// Written by an anonymous visitor: the untrusted policy.
			h, err = s.md.renderUntrusted([]byte(body), "issue-desc-")
		} else {
			h, _, err = s.md.renderWithIDs([]byte(body), d.name(), d.repo.DefaultBranch, "", "issue-desc-")
		}
		if err != nil {
			return nil, 0, err
		}
		p.Desc = h
	}
	p.Opened.Name = s.who(i.Author)
	if i.External {
		// The label is the visitor's, not the owner's: shown as plain text.
		p.Opened.Name = s.externalAuthor(i.Author)
	}
	p.Opened.When, p.Opened.WhenAt = whenOf(now, i.CreatedAt)
	if !i.CreatedAt.IsZero() {
		p.Opened.Abs = absolute(i.CreatedAt)
	}
	p.Updated, p.UpdatedAt = whenOf(now, i.UpdatedAt)
	for _, c := range res.Comments {
		v := CommentView{
			ID: strconv.FormatInt(c.ID, 10), Author: s.who(c.Author), Parts: plainParts(c.Text.String()),
		}
		v.When, v.WhenAt = whenOf(now, c.CreatedAt)
		if !c.CreatedAt.IsZero() {
			v.Abs = absolute(c.CreatedAt)
		}
		p.Comments = append(p.Comments, v)
	}
	if signed {
		p.Form = &CommentForm{ID: "cf-general", Action: issueHref(d.name(), n, "comments"), Title: "comment on the issue", Plain: true}
		switch i.State {
		case domain.IssueStateOpen:
			p.Transition = &IssueActions{Action: issueHref(d.name(), n, "close"), Label: "Close issue"}
		case domain.IssueStateClosed:
			p.Transition = &IssueActions{Action: issueHref(d.name(), n, "reopen"), Label: "Reopen issue"}
		}
	}

	l := s.repoLayout(ctx, d, "issues")
	l.Title = p.Number + " " + p.Title + " · " + d.name() + " · " + s.instance
	l.Crumbs[0].Current = false
	l.Crumbs = append(l.Crumbs,
		Crumb{Label: "issues", Href: issuesBase(d.name())},
		Crumb{Label: p.Number, Href: self, Current: true},
	)
	p.Live = true
	l.Live, l.LiveURL = true, liveURL("repo:"+d.name(), "view=issue&n="+strconv.FormatInt(n, 10))
	l.HasRail = true
	return IssueShow(l, p), http.StatusOK, nil
}

// pageOrRedirect is page(), except that a page that is not found may be the
// page of the other kind of the number: target then names where to send the
// visitor ("" when there is none).
func (s *Server) pageOrRedirect(h handler, target func(ctx context.Context, r *http.Request) (string, error)) http.HandlerFunc {
	plain := s.page(h)
	if s.cfg.IssueNumbers == nil {
		return plain
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		c, status, err := h(ctx, r)
		if errors.Is(err, domain.ErrNotFound) {
			to, terr := target(ctx, r)
			switch {
			case terr != nil && !errors.Is(terr, domain.ErrNotFound):
				s.fail(ctx, w, r, terr)
				return
			case terr == nil && to != "":
				seeOther(w, r, to)
				return
			}
		}
		if err != nil {
			s.fail(ctx, w, r, err)
			return
		}
		s.write(ctx, w, r, status, c)
	}
}

// numberKind asks what the number of the path names.
func (s *Server) numberKind(ctx context.Context, r *http.Request) (domain.NumberKind, int64, error) {
	n, err := mrNumber(r)
	if err != nil {
		return domain.NumberNone, 0, err
	}
	name, err := domain.ParseRepoName(r.PathValue("repo"))
	if err != nil {
		return domain.NumberNone, 0, err
	}
	kind, err := s.cfg.IssueNumbers.Kind(ctx, principal(ctx), name, n)
	return kind, n, err
}

// issueToMR is where the number of an issue URL goes when it names a merge
// request.
func (s *Server) issueToMR(ctx context.Context, r *http.Request) (string, error) {
	kind, n, err := s.numberKind(ctx, r)
	if err != nil || kind != domain.NumberMergeRequest {
		return "", err
	}
	return mrHref(r.PathValue("repo"), n), nil
}

// mrToIssue is where the number of a merge request URL goes when it names an
// issue.
func (s *Server) mrToIssue(ctx context.Context, r *http.Request) (string, error) {
	kind, n, err := s.numberKind(ctx, r)
	if err != nil || kind != domain.NumberIssue {
		return "", err
	}
	return issueHref(r.PathValue("repo"), n), nil
}
