// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/a-h/templ"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The owner's inbox (ADR 0031): GET /-/inbox lists the open merge requests and
// issues of every repository, newest activity first, with a filter by
// repository (?repo=<name>, a plain link, so it works without scripts). Only a
// signed-in owner sees it: for anyone else the path is as missing as any other.
// It follows the stream "owner" with view=inbox: each event of a merge request
// or an issue sends the counters, the two lists and the filter again.

// inboxPath is the page of the inbox.
const inboxPath = "/-/inbox"

// inboxEvent is the name of the live events of the inbox.
const inboxEvent = "inbox"

// InboxRow is one open item of the inbox.
type InboxRow struct {
	Repo     string
	RepoHref string
	Number   string
	Href     string
	Title    string
	When     string
	WhenAt   string
	Cursor   bool
}

// InboxRepoLink is an entry of the repository filter.
type InboxRepoLink struct {
	Label   string
	Href    string
	Current bool
}

// InboxPage is the inbox: the counters, the filter and the two lists.
type InboxPage struct {
	// MergeRequests and Issues count every open item, whatever the filter.
	MergeRequests, Issues int
	Filter                string // the repository filtered on, "" for all
	Repos                 []InboxRepoLink
	MRs                   []InboxRow
	IssueRows             []InboxRow
}

// inboxHref is the inbox page, filtered on repo when it is not "".
func inboxHref(repo string) string {
	if repo == "" {
		return inboxPath
	}
	return inboxPath + "?repo=" + url.QueryEscape(repo)
}

// inboxFilter reads ?repo=. A name that is not valid is a page that is not
// there.
func inboxFilter(r *http.Request) (domain.RepoName, error) {
	raw := r.URL.Query().Get("repo")
	if raw == "" {
		return domain.RepoName{}, nil
	}
	name, err := domain.ParseRepoName(raw)
	if err != nil {
		return domain.RepoName{}, errNotFound
	}
	return name, nil
}

// inboxPage builds the page from the overview.
func (s *Server) inboxPage(ov ports.InboxOverview, filter domain.RepoName) InboxPage {
	now := s.cfg.Now()
	p := InboxPage{MergeRequests: ov.Counts.MergeRequests, Issues: ov.Counts.Issues, Filter: filter.String()}
	row := func(repo domain.RepoName, n int64, href, title string, updated, created time.Time) InboxRow {
		ts := updated
		if ts.IsZero() {
			ts = created
		}
		r := InboxRow{
			Repo: repo.String(), RepoHref: repoHref(repo.String()),
			Number: "#" + strconv.FormatInt(n, 10), Href: href, Title: visibleText(title),
		}
		r.When, r.WhenAt = whenOf(now, ts)
		return r
	}
	for _, m := range ov.MergeRequests {
		p.MRs = append(p.MRs, row(m.Repo, m.Number, mrHref(m.Repo.String(), m.Number), m.Title.String(), m.UpdatedAt, m.CreatedAt))
	}
	for _, i := range ov.Issues {
		p.IssueRows = append(p.IssueRows, row(i.Repo, i.Number, issueHref(i.Repo.String(), i.Number), i.Title.String(), i.UpdatedAt, i.CreatedAt))
	}
	switch {
	case len(p.MRs) > 0:
		p.MRs[0].Cursor = true
	case len(p.IssueRows) > 0:
		p.IssueRows[0].Cursor = true
	}
	p.Repos = append(p.Repos, InboxRepoLink{Label: "all", Href: inboxHref(""), Current: filter.IsZero()})
	for _, r := range ov.Repos {
		p.Repos = append(p.Repos, InboxRepoLink{Label: r.String(), Href: inboxHref(r.String()), Current: r == filter})
	}
	return p
}

// inbox is GET /-/inbox.
func (s *Server) inbox(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	if !SignedIn(ctx) {
		return nil, 0, errNotFound
	}
	filter, err := inboxFilter(r)
	if err != nil {
		return nil, 0, err
	}
	ov, err := s.cfg.Inbox.Overview(ctx, principal(ctx), filter)
	if err != nil {
		return nil, 0, err
	}
	l := s.base()
	l.Title = "inbox · " + s.instance
	l.Crumbs = []Crumb{{Label: "inbox", Current: true}}
	l.Cmd = []CmdItem{{Kind: "go", Label: "home", Href: "/"}}
	if s.cfg.Events != nil {
		q := "view=inbox"
		if !filter.IsZero() {
			q += "&repo=" + url.QueryEscape(filter.String())
		}
		l.Live, l.LiveURL = true, liveURL("owner", q)
	}
	return Inbox(l, s.inboxPage(ov, filter)), http.StatusOK, nil
}

// inboxCount is the number of open items the home button shows, and whether
// there is a button: only for the signed-in owner, and only when the inbox is
// there. A failed read leaves the button without a number.
func (s *Server) inboxCount(ctx context.Context) (n int, show bool) {
	if s.cfg.Inbox == nil || !SignedIn(ctx) {
		return 0, false
	}
	ov, err := s.cfg.Inbox.Overview(ctx, principal(ctx), domain.RepoName{})
	if err != nil {
		return -1, true
	}
	return ov.Counts.MergeRequests + ov.Counts.Issues, true
}

// inbox renders the fragments an event of a merge request or an issue changes
// on the inbox page: the counters, the filter and the two lists.
func (l *liveStream) inbox(ctx context.Context, we ports.WatchEvent) ([]fragment, error) {
	switch we.Event.(type) {
	case domain.MergeRequestOpened, domain.MergeRequestMerged, domain.MergeRequestClosed, domain.MergeRequestReady, domain.CommentAdded,
		domain.IssueOpened, domain.IssueClosed, domain.IssueReopened, domain.IssueCommented:
	default:
		return nil, nil
	}
	if l.s.cfg.Inbox == nil {
		return nil, nil
	}
	var filter domain.RepoName
	if l.filter != "" {
		filter, _ = domain.ParseRepoName(l.filter)
	}
	ov, err := l.s.cfg.Inbox.Overview(ctx, principal(ctx), filter)
	if err != nil {
		return nil, err
	}
	p := l.s.inboxPage(ov, filter)
	var out []fragment
	for _, c := range []templ.Component{InboxCounters(p), InboxRepos(p), InboxMRs(p), InboxIssues(p)} {
		f, err := renderFragment(ctx, inboxEvent, c)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}
