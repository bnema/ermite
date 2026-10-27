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

// The owner’s inbox shows open work, dependency updates, activity and moderation.
// Only a signed-in owner may read it. Tabs use plain links and live fragments.

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
	// Checks is the CI state of the head of a merge request; zero otherwise.
	Checks ChecksSummary
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

// InboxNavLink is a plain navigation link.
type InboxNavLink struct {
	Label   string
	Href    string
	Current bool
}

// InboxRunRow adds repository and start time to the Actions run display.
type InboxRunRow struct {
	RunRow
	Repo, RepoHref, When, WhenAt string
}

// InboxPage is the inbox: tab counts and item lists.
type InboxPage struct {
	Running     int
	RunsEnabled bool
	Runs        []InboxRunRow
	// MergeRequests and Issues count every open item, across repositories.
	MergeRequests, Issues int
	// PendingCount counts every issue awaiting moderation.
	PendingCount int
	Pending      []PendingRow
	// DepsCount counts repositories with pending dependency updates;
	// DepsEnabled tells that the buttons are wired.
	DepsCount   int
	DepsEnabled bool
	Deps        []InboxDepsRow
	// DepsUpdates counts the updates of the rows of Deps; DepsChecked is the latest check
	// (RFC 3339 and short date), empty when unknown.
	DepsUpdates                int
	DepsChecked, DepsCheckedAt string
	Tab                        string
	Tabs                       []InboxNavLink
	MRs                        []InboxRow
	IssueRows                  []InboxRow
	// Journal is the activity section; zero without a journal.
	Journal JournalView
}

// JournalView is the activity section of the inbox: the range toggle, the
// repositories ahead of their nearest tag, then the entries newest first.
type JournalView struct {
	Ranges     []InboxNavLink
	RangeLabel string // "24 hours" or "7 days", for the empty state
	Unreleased []UnreleasedRow
	Rows       []JournalRow
	// Truncated tells that the window held more than the rows shown.
	Truncated bool
}

// UnreleasedRow is a repository whose default branch is ahead of its nearest
// tag.
type UnreleasedRow struct {
	Repo, RepoHref string
	// Commits is "4 commits" or "500+ commits", CommitsHref the history of
	// the default branch.
	Commits, CommitsHref string
	Tag, TagHref         string
	When, WhenAt, Title  string
}

// JournalRow is one entry of the activity journal.
type JournalRow struct {
	When, WhenAt, Title string
	Repo, RepoHref      string
	// Kind is the data-kind of the row, Label the word of the Type column.
	Kind, Label string
	// Ref is "!4", "#7", a tag or a branch; Text the title or the commit
	// subject. Href links the row when there is a page for it.
	Ref, Text, Href string
	// Note follows the text: "3 commits" for a push.
	Note  string
	Actor string
}

// journalRanges are the windows of the activity section, by ?range=.
var journalRanges = []struct {
	param, label, long string
	window             time.Duration
}{
	{"24h", "24 h", "24 hours", 24 * time.Hour},
	{"7d", "7 days", "7 days", 7 * 24 * time.Hour},
}

// inboxRange reads ?range=: "24h" (the default) or "7d"; anything else is a
// page that is not there. It returns the index in journalRanges.
func inboxRange(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	for i, r := range journalRanges {
		if r.param == raw {
			return i, nil
		}
	}
	return 0, errNotFound
}

// inboxQuery is the query of the inbox for a tab and a range; the default
// range is left out.
func inboxQuery(tab string, rng int) string {
	q := url.Values{}
	if tab != "work" {
		q.Set("tab", tab)
	}
	if rng != 0 {
		q.Set("range", journalRanges[rng].param)
	}
	return q.Encode()
}

// journalView builds the activity section.
func (s *Server) journalView(j ports.InboxJournal, rng int) JournalView {
	now := s.cfg.Now()
	v := JournalView{RangeLabel: journalRanges[rng].long, Truncated: j.Truncated}
	for i, r := range journalRanges {
		href := inboxPath
		if q := inboxQuery("activity", i); q != "" {
			href += "?" + q
		}
		v.Ranges = append(v.Ranges, InboxNavLink{Label: r.label, Href: href + "#inbox-journal", Current: i == rng})
	}
	for _, u := range j.Unreleased {
		repo := u.Repo.String()
		n := itoa(u.Distance.Commits)
		if u.Distance.More {
			n += "+"
		}
		word := " commits"
		if u.Distance.Commits == 1 && !u.Distance.More {
			word = " commit"
		}
		row := UnreleasedRow{
			Repo: repo, RepoHref: repoHref(repo),
			Commits: n + word, CommitsHref: repoHref(repo, "commits", u.Branch),
		}
		if t := u.Distance.Tag; t.Name != "" {
			row.Tag, row.TagHref = visibleText(t.Name), repoHref(repo, "tree", t.Name)
			row.When, row.WhenAt = whenOf(now, t.At)
			if !t.At.IsZero() {
				row.Title = absolute(t.At)
			}
		}
		v.Unreleased = append(v.Unreleased, row)
	}
	for _, e := range j.Entries {
		v.Rows = append(v.Rows, journalRow(now, e))
	}
	return v
}

// journalRow is the row of an entry: the texts fit to display, a link to the
// merge request, the issue, the release, the tag or the commit.
func journalRow(now time.Time, e domain.JournalEntry) JournalRow {
	repo := e.Repo.String()
	row := JournalRow{
		Repo: repo, RepoHref: repoHref(repo), Kind: string(e.Kind),
		Text: visibleText(e.Title), Actor: visibleText(e.Actor), Title: absolute(e.At),
	}
	row.When, row.WhenAt = whenOf(now, e.At)
	number := strconv.FormatInt(e.Number, 10)
	switch e.Kind {
	case domain.JournalMergeRequestOpened, domain.JournalMergeRequestMerged, domain.JournalMergeRequestClosed:
		row.Ref, row.Href = "!"+number, mrHref(repo, e.Number)
		row.Label = map[domain.JournalKind]string{
			domain.JournalMergeRequestOpened: "opened", domain.JournalMergeRequestMerged: "merged", domain.JournalMergeRequestClosed: "closed",
		}[e.Kind]
	case domain.JournalIssueOpened, domain.JournalIssueClosed:
		row.Ref, row.Href, row.Label = "#"+number, issueHref(repo, e.Number), "issue opened"
		if e.Kind == domain.JournalIssueClosed {
			row.Label = "issue closed"
		}
	case domain.JournalReleasePublished:
		row.Label, row.Ref, row.Href = "release", visibleText(e.Ref), releaseHref(repo, e.Ref)
	case domain.JournalTagCreated:
		row.Label, row.Ref, row.Href = "tag", visibleText(e.Ref), repoHref(repo, "tree", e.Ref)
	case domain.JournalTagDeleted:
		row.Label, row.Ref = "tag deleted", visibleText(e.Ref)
	case domain.JournalDefaultBranchPush:
		row.Label, row.Ref, row.Text = "push", visibleText(e.Ref), visibleText(e.Summary)
		if !e.Commit.IsZero() {
			row.Href = repoHref(repo, "commit", e.Commit.String())
		}
		n := itoa(e.Commits)
		if e.More {
			n += "+"
		}
		switch {
		case e.Commits == 0:
		case e.Commits == 1 && !e.More:
			row.Note = "1 commit"
		default:
			row.Note = n + " commits"
		}
	default:
		row.Label = string(e.Kind)
	}
	return row
}

// inboxTab normalizes unknown or absent tabs to open work.
func inboxTab(raw string) string {
	switch raw {
	case "deps", "activity", "moderation":
		return raw
	default:
		return "work"
	}
}

func inboxHref(tab string, rng int) string {
	if q := inboxQuery(tab, rng); q != "" {
		return inboxPath + "?" + q
	}
	return inboxPath
}

// inboxPage builds the page from the overview and the journal.
func (s *Server) inboxPage(ctx context.Context, ov ports.InboxOverview, j ports.InboxJournal, tab string, rng int) InboxPage {
	now := s.cfg.Now()
	p := InboxPage{MergeRequests: ov.Counts.MergeRequests, Issues: ov.Counts.Issues, PendingCount: ov.Counts.Pending, Tab: tab}
	p.Running, p.RunsEnabled = ov.Counts.Running, ov.RunsEnabled
	p.DepsCount, p.DepsEnabled = ov.Counts.Deps, s.cfg.Deps != nil
	for _, d := range ov.Deps {
		row := s.depsRow(d)
		p.Deps = append(p.Deps, row)
		p.DepsUpdates += row.Count
	}
	if !ov.DepsCheckedAt.IsZero() {
		p.DepsChecked, p.DepsCheckedAt = ov.DepsCheckedAt.UTC().Format("2006-01-02"), rfc3339(ov.DepsCheckedAt)
	}
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
		p.Pending = append(p.Pending, s.pendingRow(now, pi))
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
	sums := make(map[string]ChecksSummary) // one read per (repo, head)
	for _, m := range ov.MergeRequests {
		r := row(m.Repo, m.Number, mrHref(m.Repo.String(), m.Number), m.Title.String(), m.UpdatedAt, m.CreatedAt)
		if !m.HeadID.IsZero() {
			head := m.HeadID.String()
			key := m.Repo.String() + "@" + head
			sum, ok := sums[key]
			if !ok {
				sum = checksSummary(m.Repo, head, s.checksFor(ctx, m.Repo, head))
				sums[key] = sum
			}
			r.Checks = sum
		}
		p.MRs = append(p.MRs, r)
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
	p.Journal = s.journalView(j, rng)
	for _, t := range []struct {
		key, label string
		count      int
	}{
		{"deps", "Deps Updates", p.DepsUpdates},
		{"work", "Open Issues & Merge Requests", p.MergeRequests + p.Issues},
		{"activity", "Activity", len(p.Runs) + len(p.Journal.Rows) + len(p.Journal.Unreleased)},
		{"moderation", "Awaiting Moderation", p.PendingCount},
	} {
		p.Tabs = append(p.Tabs, InboxNavLink{Label: t.label + " (" + itoa(t.count) + ")", Href: inboxHref(t.key, rng), Current: t.key == tab})
	}

	return p
}

// inboxData reads what the page shows: the overview and the journal of the
// range.
func (s *Server) inboxData(ctx context.Context, tab string, rng int) (InboxPage, error) {
	ov, err := s.cfg.Inbox.Overview(ctx, principal(ctx))
	if err != nil {
		return InboxPage{}, err
	}
	j, err := s.cfg.Inbox.Journal(ctx, principal(ctx), journalRanges[rng].window)
	if err != nil {
		return InboxPage{}, err
	}
	return s.inboxPage(ctx, ov, j, tab, rng), nil
}

// pendingRow is the row of a pending issue: the texts as plain strings, the
// invisible characters marked, the body cut.
func (s *Server) pendingRow(now time.Time, pi domain.PendingIssue) PendingRow {
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
		row.Approve, row.Reject, row.Ban = base+"approve", base+"reject", base+"ban"
	}
	return row
}

// pendingBack returns moderation actions to their tab.
func pendingBack(r *http.Request) string { return inboxHref("moderation", 0) }

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
	tab := inboxTab(r.URL.Query().Get("tab"))
	rng, err := inboxRange(r.URL.Query().Get("range"))
	if err != nil {
		return nil, 0, err
	}
	page, err := s.inboxData(ctx, tab, rng)
	if err != nil {
		return nil, 0, err
	}
	l := s.base()
	l.Title = "inbox · " + s.instance
	l.Crumbs = []Crumb{{Label: "inbox", Current: true}}
	l.Cmd = []CmdItem{{Kind: "go", Label: "home", Href: "/"}}
	if s.cfg.Events != nil {
		q := "view=inbox"
		if rest := inboxQuery(tab, rng); rest != "" {
			q += "&" + rest
		}
		l.Live, l.LiveURL = true, liveURL("owner", q)
	}
	return Inbox(l, page), http.StatusOK, nil
}

// inboxCount is the number of open items the home button shows, and whether
// there is a button: only for the signed-in owner, and only when the inbox is
// there. A failed read leaves the button without a number.
func (s *Server) inboxCount(ctx context.Context) (n int, show bool) {
	if s.cfg.Inbox == nil || !SignedIn(ctx) {
		return 0, false
	}
	ov, err := s.cfg.Inbox.Overview(ctx, principal(ctx))
	if err != nil {
		return -1, true
	}
	return ov.Counts.MergeRequests + ov.Counts.Issues, true
}

// inbox renders the fragments an event of a merge request, an issue, a
// release, a push or a repository changes on the inbox page: the counters,
// the tabs and their selected content.
func (l *liveStream) inbox(ctx context.Context, we ports.WatchEvent) ([]fragment, error) {
	switch we.Event.(type) {
	case domain.MergeRequestEdited, domain.MergeRequestOpened, domain.MergeRequestMerged, domain.MergeRequestClosed, domain.MergeRequestReady, domain.CommentAdded, domain.CommentEdited, domain.CommentDeleted, domain.CommentResolved,
		domain.IssueEdited, domain.IssueOpened, domain.IssueResolved, domain.IssueClosed, domain.IssueReopened, domain.IssueCommented, domain.IssueSubmitted, domain.IssuePendingRemoved,
		domain.RepoUpdated, domain.RepoDeleted, domain.RunUpdated, domain.DependencyUpdatesFound, domain.DependencyUpdatesChanged,
		domain.JournalUpdated, domain.ReleasePublished:
	default:
		return nil, nil
	}
	if l.s.cfg.Inbox == nil {
		return nil, nil
	}
	tab := inboxTab(l.tab)
	rng, _ := inboxRange(l.rng) // checked when the stream was opened
	p, err := l.s.inboxData(ctx, tab, rng)
	if err != nil {
		return nil, err
	}
	var out []fragment
	for _, c := range []templ.Component{InboxContent(p)} {
		f, err := renderFragment(ctx, inboxEvent, c)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}
