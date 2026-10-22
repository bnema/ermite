// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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

// PendingRow is one issue awaiting moderation. Everything in it is plain text:
// it comes from an anonymous visitor and is never rendered as Markdown.
type PendingRow struct {
	Repo     string
	RepoHref string
	Author   string
	Title    string
	// Preview is the start of the body, at most pendingPreviewRunes characters.
	Preview string
	When    string
	WhenAt  string
	// Approve, Reject and Ban are the URLs the buttons post to; empty without
	// the moderation use case.
	Approve, Reject, Ban string
}

// pendingPreviewRunes bounds the preview of a pending body.
const pendingPreviewRunes = 300

// InboxRepoLink is an entry of the repository filter.
type InboxRepoLink struct {
	Label   string
	Href    string
	Current bool
}

// InboxRunRow adds repository and start time to the Actions run display.
type InboxRunRow struct {
	RunRow
	Repo, RepoHref, When, WhenAt string
}

// InboxPage is the inbox: counters, repository filter and item lists.
type InboxPage struct {
	Running     int
	RunsEnabled bool
	Runs        []InboxRunRow
	// MergeRequests and Issues count every open item, whatever the filter.
	MergeRequests, Issues int
	// PendingCount counts every issue awaiting moderation.
	PendingCount int
	Pending      []PendingRow
	Filter       string // the repository filtered on, "" for all
	Repos        []InboxRepoLink
	MRs          []InboxRow
	IssueRows    []InboxRow
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
	p := InboxPage{MergeRequests: ov.Counts.MergeRequests, Issues: ov.Counts.Issues, PendingCount: ov.Counts.Pending, Filter: filter.String()}
	p.Running, p.RunsEnabled = ov.Counts.Running, ov.RunsEnabled
	for _, run := range ov.Runs {
		r := InboxRunRow{RunRow: s.runRow(run.Repo.String(), run, now), Repo: run.Repo.String(), RepoHref: repoHref(run.Repo.String())}
		if run.Status == domain.CIInProgress {
			r.State = "in progress"
		}
		ts := run.StartedAt
		if ts.IsZero() {
			ts = run.CreatedAt
		}
		r.When, r.WhenAt = whenOf(now, ts)
		p.Runs = append(p.Runs, r)
	}
	for _, pi := range ov.Pending {
		p.Pending = append(p.Pending, s.pendingRow(now, pi, filter))
	}
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

// pendingRow is the row of a pending issue: the texts as plain strings, the
// invisible characters marked, the body cut.
func (s *Server) pendingRow(now time.Time, pi domain.PendingIssue, filter domain.RepoName) PendingRow {
	repo := pi.Repo.String()
	row := PendingRow{
		Repo: repo, RepoHref: repoHref(repo), Author: "anonymous",
		Title: visibleText(pi.Title.String()),
	}
	if a := strings.TrimSpace(visibleText(pi.Author)); a != "" {
		row.Author = a
	}
	body := strings.TrimSpace(visibleText(pi.Body.String()))
	row.Preview = cutRunes(body, pendingPreviewRunes)
	if len(row.Preview) < len(body) {
		row.Preview += "…"
	}
	row.When, row.WhenAt = whenOf(now, pi.CreatedAt)
	if s.cfg.Moderation != nil {
		base := inboxPath + "/pending/" + url.PathEscape(repo) + "/" + url.PathEscape(pi.ID.String()) + "/"
		var q string
		if !filter.IsZero() {
			q = "?repo=" + url.QueryEscape(filter.String())
		}
		row.Approve, row.Reject, row.Ban = base+"approve"+q, base+"reject"+q, base+"ban"+q
	}
	return row
}

// pendingBack is where a moderation action returns: the inbox, on the same
// repository filter.
func pendingBack(r *http.Request) string {
	if f, err := inboxFilter(r); err == nil {
		return inboxHref(f.String())
	}
	return inboxPath
}

// pendingID reads the repository and the id of the path.
func pendingID(r *http.Request) (domain.RepoName, domain.PendingIssueID, error) {
	name, err := domain.ParseRepoName(r.PathValue("repo"))
	if err != nil {
		return domain.RepoName{}, "", err
	}
	id, err := domain.ParsePendingIssueID(r.PathValue("id"))
	if err != nil {
		return domain.RepoName{}, "", domain.ErrNotFound
	}
	return name, id, nil
}

// pendingApprove is POST /-/inbox/pending/<repo>/<id>/approve.
func (s *Server) pendingApprove(ctx context.Context, r *http.Request) (string, string, error) {
	back := pendingBack(r)
	name, id, err := pendingID(r)
	if err != nil {
		return "", back, err
	}
	if _, err := s.cfg.Moderation.Approve(ctx, principal(ctx), name, id); err != nil {
		return "", back, err
	}
	return back, back, nil
}

// pendingReject is POST /-/inbox/pending/<repo>/<id>/reject, or .../ban, which
// also bans the submitter.
func (s *Server) pendingReject(ban bool) func(ctx context.Context, r *http.Request) (string, string, error) {
	return func(ctx context.Context, r *http.Request) (string, string, error) {
		back := pendingBack(r)
		name, id, err := pendingID(r)
		if err != nil {
			return "", back, err
		}
		if err := s.cfg.Moderation.Reject(ctx, principal(ctx), name, id, ban); err != nil {
			return "", back, err
		}
		return back, back, nil
	}
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

// inbox renders the fragments an event of a merge request, an issue or a repository changes
// on the inbox page: the counters, the filter and the two lists.
func (l *liveStream) inbox(ctx context.Context, we ports.WatchEvent) ([]fragment, error) {
	switch we.Event.(type) {
	case domain.MergeRequestOpened, domain.MergeRequestMerged, domain.MergeRequestClosed, domain.MergeRequestReady, domain.CommentAdded, domain.CommentEdited, domain.CommentDeleted, domain.CommentResolved,
		domain.IssueOpened, domain.IssueClosed, domain.IssueReopened, domain.IssueCommented, domain.IssueSubmitted, domain.IssuePendingRemoved,
		domain.RepoUpdated, domain.RepoDeleted, domain.RunUpdated:
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
	for _, c := range []templ.Component{InboxCounters(p), InboxRepos(p), InboxPending(p), InboxRuns(p), InboxMRs(p), InboxIssues(p)} {
		f, err := renderFragment(ctx, inboxEvent, c)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}
