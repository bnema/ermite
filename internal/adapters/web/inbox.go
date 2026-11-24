// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
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
	ID       string
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
	// Deps are the repository rows the filter shows; DepsBatches and
	// DepsUpdates count every batch and update, whatever the filter.
	Deps        []InboxDepsRepo
	DepsBatches int
	// DepsUpdates counts the updates of the rows of Deps; DepsChecked is the latest check
	// (RFC 3339 and short UTC date and time), empty when unknown.
	DepsUpdates                int
	DepsChecked, DepsCheckedAt string
	// DepsQueue is the progress of Bernard's queue, empty when idle.
	DepsQueue string
	// DepsSorts toggles the order of the rows: failures and most updates
	// first (the default) or by repository name.
	DepsSorts []InboxNavLink
	// DepsFilters are the chips All, Failures, Majors and MRs.
	DepsFilters []InboxDepsFilter
	Tab         string
	Tabs        []InboxNavLink
	MRs         []InboxRow
	IssueRows   []InboxRow
	// Journal is the activity section; zero without a journal.
	Journal JournalView
	Traffic *InboxTrafficView
}

// JournalView is the activity section of the inbox: the range toggle, the
// repositories ahead of their nearest tag, then the entries newest first.
type JournalView struct {
	Ranges     []InboxNavLink
	RangeLabel string // "24 hours" or "7 days", for the empty state
	Unreleased []UnreleasedGroup
	Rows       []JournalRow
	// Truncated tells that the window held more than the rows shown.
	Truncated bool
}

type UnreleasedGroup struct {
	Key, Label string
	Rows       []UnreleasedRow
	Open       bool
}

func (j JournalView) unreleasedCount() int {
	n := 0
	for _, g := range j.Unreleased {
		n += len(g.Rows)
	}
	return n
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

// inboxRange reads the tab-specific window: 24h/7d normally, 24h/30d for
// traffic. It returns the index in that tab's ranges; unknown ranges are missing.
func inboxRange(raw string, tab ...string) (int, error) {
	ranges := journalRanges
	if len(tab) > 0 && tab[0] == "traffic" {
		ranges = trafficRanges
	}
	if raw == "" {
		return 0, nil
	}
	for i, r := range ranges {
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
		ranges := journalRanges
		if tab == "traffic" {
			ranges = trafficRanges
		}
		q.Set("range", ranges[rng].param)
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
	groups := []UnreleasedGroup{
		{Key: "due", Label: "Release due", Open: true},
		{Key: "soon", Label: "Soon", Open: true},
		{Key: "later", Label: "Nothing urgent"},
	}
	unreleased := append([]ports.UnreleasedRepo(nil), j.Unreleased...)
	sort.Slice(unreleased, func(i, k int) bool {
		a, b := unreleased[i], unreleased[k]
		at, bt := a.Distance.Tag.At, b.Distance.Tag.At
		ak := a.Distance.Tag.Name != "" && !at.IsZero()
		bk := b.Distance.Tag.Name != "" && !bt.IsZero()
		if ak != bk {
			return ak
		}
		if ak && !at.Equal(bt) {
			return at.Before(bt)
		}
		if a.Distance.Commits != b.Distance.Commits {
			return a.Distance.Commits > b.Distance.Commits
		}
		return a.Repo.String() < b.Repo.String()
	})
	for _, u := range unreleased {
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
			_, row.WhenAt = whenOf(now, t.At)
			if !t.At.IsZero() {
				row.Title = absolute(t.At)
				row.When = itoa(max(0, int(now.Sub(t.At)/(24*time.Hour)))) + "d"
			}
		}
		if row.When == "" {
			row.When = "—"
		}
		for i := range groups {
			if groups[i].Key == string(domain.ReleaseUrgency(u.Distance, now)) {
				groups[i].Rows = append(groups[i].Rows, row)
				break
			}
		}
	}
	for _, g := range groups {
		if len(g.Rows) > 0 {
			v.Unreleased = append(v.Unreleased, g)
		}
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
	case "deps", "activity", "moderation", "traffic":
		return raw
	default:
		return "work"
	}
}

// depsSortName is ?sort= for the deps rows by repository name; the default
// puts the repositories with the most updates first.
const depsSortName = "name"

// depsSortParam reads ?sort=: "" (most updates first) or "name".
func depsSortParam(raw string) string {
	if raw == depsSortName {
		return raw
	}
	return ""
}

func inboxHref(tab string, rng int) string {
	if q := inboxQuery(tab, rng); q != "" {
		return inboxPath + "?" + q
	}
	return inboxPath
}

// inboxReads is what a tab read: the data of its own section, and the
// owner counts for the labels of the other tabs.
type inboxReads struct {
	work    ports.InboxWork
	runs    ports.InboxRuns
	deps    ports.InboxDepsView
	pending []domain.PendingIssue
	journal ports.InboxJournal
	counts  ports.OwnerCounts
}

// inboxPage builds the page from what the tab read.
func (s *Server) inboxPage(ctx context.Context, d inboxReads, tab string, rng int, dq depsQuery) InboxPage {
	// Links to other tabs reset the traffic-specific window.
	if tab == "traffic" {
		rng = 0
	}
	now := s.cfg.Now()
	p := InboxPage{MergeRequests: len(d.work.MergeRequests), Issues: len(d.work.Issues), PendingCount: len(d.pending), Tab: tab}
	p.Running, p.RunsEnabled = len(d.runs.Runs), d.runs.Enabled
	p.DepsCount, p.DepsEnabled = len(d.deps.Deps), s.cfg.Deps != nil
	var all []InboxDepsRow
	for _, d := range d.deps.Deps {
		row := s.depsRow(d, "")
		all = append(all, row)
		p.DepsUpdates += row.Count
	}
	p.DepsBatches = len(all)
	p.Deps = s.depsRepos(d.deps.Deps, dq)
	p.DepsFilters = depsFilters(all, dq, rng)
	p.DepsQueue = depsQueueText(d.deps.Queue)
	sort := dq.sort
	for _, o := range []struct{ param, label string }{{"", "most updates"}, {depsSortName, "name"}} {
		href := inboxHref("deps", rng)
		if o.param != "" {
			sep := "?"
			if strings.Contains(href, "?") {
				sep = "&"
			}
			href += sep + "sort=" + o.param
		}
		if dq.filter != "" {
			sep := "?"
			if strings.Contains(href, "?") {
				sep = "&"
			}
			href += sep + "filter=" + dq.filter
		}
		p.DepsSorts = append(p.DepsSorts, InboxNavLink{Label: o.label, Href: href + "#inbox-deps", Current: o.param == sort})
	}
	if at := d.deps.CheckedAt; !at.IsZero() {
		p.DepsChecked, p.DepsCheckedAt = at.UTC().Format("2006-01-02 15:04"), rfc3339(at)
	}
	for _, run := range d.runs.Runs {
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
	for _, pi := range d.pending {
		p.Pending = append(p.Pending, s.pendingRow(now, pi))
	}
	p.MRs, p.IssueRows = s.workRows(ctx, d.work)
	p.Journal = s.journalView(d.journal, rng)
	// Open work is counted on every tab; the other tabs are counted only on
	// themselves, where their data is read.
	work := d.counts.Work
	if tab == "work" {
		work = p.MergeRequests + p.Issues
	}
	for _, t := range []struct {
		key, label string
		count      int
	}{
		{"deps", "Deps Updates", p.DepsUpdates},
		{"work", "Open Issues & Merge Requests", work},
		{"activity", "Activity", len(p.Runs) + len(p.Journal.Rows) + p.Journal.unreleasedCount()},
		{"moderation", "Awaiting Moderation", p.PendingCount},
	} {
		label := t.label
		if t.key == tab || t.key == "work" {
			label += " (" + itoa(t.count) + ")"
		}
		p.Tabs = append(p.Tabs, InboxNavLink{Label: label, Href: inboxHref(t.key, rng), Current: t.key == tab})
	}

	if _, ok := s.cfg.Traffic.(TrafficForgeReader); ok {
		p.Tabs = append(p.Tabs, InboxNavLink{Label: "Traffic", Href: inboxHref("traffic", 0), Current: tab == "traffic"})
	}
	return p
}

// inboxData reads what the tab shows, and the owner counts for the work tab
// label when the tab is not open work itself.
func (s *Server) inboxData(ctx context.Context, tab string, rng int, sort depsQuery) (InboxPage, error) {
	if tab == "traffic" {
		if _, ok := s.cfg.Traffic.(TrafficForgeReader); !ok {
			return InboxPage{}, errNotFound
		}
	}
	p := principal(ctx)
	var d inboxReads
	var err error
	switch tab {
	case "work":
		d.work, err = s.cfg.Inbox.Work(ctx, p)
	case "activity":
		if d.runs, err = s.cfg.Inbox.Runs(ctx, p); err == nil {
			d.journal, err = s.cfg.Inbox.Journal(ctx, p, journalRanges[rng].window)
		}
	case "deps":
		d.deps, err = s.cfg.Inbox.Deps(ctx, p)
	case "moderation":
		d.pending, err = s.pendingIssues(ctx)
	}
	if err != nil {
		return InboxPage{}, err
	}
	if tab != "work" {
		if d.counts, err = s.cfg.Inbox.Counts(ctx, p); err != nil {
			return InboxPage{}, err
		}
	}
	page := s.inboxPage(ctx, d, tab, rng, sort)
	if tab == "traffic" {
		page.Traffic, err = s.inboxTraffic(ctx, rng)
		if err != nil {
			return InboxPage{}, err
		}
	}
	return page, nil
}

// pendingIssues is the moderation queue: the moderation items of Attention.
func (s *Server) pendingIssues(ctx context.Context) ([]domain.PendingIssue, error) {
	items, err := s.cfg.Inbox.Attention(ctx, principal(ctx))
	if err != nil {
		return nil, err
	}
	var out []domain.PendingIssue
	for _, it := range items {
		if it.Kind == ports.AttentionModeration && it.Pending != nil {
			out = append(out, *it.Pending)
		}
	}
	return out, nil
}

// pendingRow is the row of a pending issue: the texts as plain strings, the
// invisible characters marked, the body cut.
func (s *Server) pendingRow(now time.Time, pi domain.PendingIssue) PendingRow {
	repo := pi.Repo.String()
	row := PendingRow{
		ID:   pendingRowID(repo, pi.ID.String()),
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
func (s *Server) pendingApprove(ctx context.Context, r *http.Request) (ActionResult, error) {
	name, id, err := pendingID(r)
	if err != nil {
		return ActionResult{}, err
	}
	if _, err := s.cfg.Moderation.Approve(ctx, principal(ctx), name, id); err != nil {
		return ActionResult{}, err
	}
	return s.pendingResult(ctx, r)
}

// pendingReject is POST /-/inbox/pending/<repo>/<id>/reject, or .../ban, which
// also bans the submitter.
func (s *Server) pendingReject(ban bool) func(ctx context.Context, r *http.Request) (ActionResult, error) {
	return func(ctx context.Context, r *http.Request) (ActionResult, error) {
		name, id, err := pendingID(r)
		if err != nil {
			return ActionResult{}, err
		}
		if err := s.cfg.Moderation.Reject(ctx, principal(ctx), name, id, ban); err != nil {
			return ActionResult{}, err
		}
		return s.pendingResult(ctx, r)
	}
}

// inbox is GET /-/inbox.
func (s *Server) inbox(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	if !SignedIn(ctx) {
		return nil, 0, errNotFound
	}
	tab := inboxTab(r.URL.Query().Get("tab"))
	rng, err := inboxRange(r.URL.Query().Get("range"), tab)
	if err != nil {
		return nil, 0, err
	}
	dq := depsQueryOf(r)
	page, err := s.inboxData(ctx, tab, rng, dq)
	if err != nil {
		return nil, 0, err
	}
	l := s.base()
	l.Title = "inbox · " + s.instance
	l.OwnerNav = s.ownerNav(ctx, "inbox")
	l.Crumbs = []Crumb{{Label: "inbox", Current: true}}
	l.Cmd = []CmdItem{{Kind: "go", Label: "home", Href: "/"}}
	if s.cfg.Events != nil {
		q := "view=inbox"
		if rest := inboxQuery(tab, rng); rest != "" {
			q += "&" + rest
		}
		if e := dq.encode(); e != "" {
			q += "&" + e
		}
		l.Live, l.LiveURL = true, liveURL("owner", q)
	}
	return Inbox(l, page), http.StatusOK, nil
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
	rng, _ := inboxRange(l.rng, tab) // checked when the stream was opened
	p, err := l.s.inboxData(ctx, tab, rng, depsQuery{sort: depsSortParam(l.sort), filter: depsFilterParam(l.filter)})
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

func pendingRowID(repo, id string) string { return "pending-" + inboxIDPart(repo) + "-" + id }

func (s *Server) pendingResult(ctx context.Context, r *http.Request) (ActionResult, error) {
	result := ActionResult{Redirect: pendingBack(r)}
	if !isHX(r) {
		return result, nil
	}
	pending, err := s.pendingIssues(ctx)
	if err != nil {
		return s.actionRefreshFailed(ctx, r, result, result.Redirect, err)
	}
	result.Fragment = inboxModerated(pendingRowID(r.PathValue("repo"), r.PathValue("id")), len(pending))
	return result, nil
}

// inboxIDPart is an injective encoding for repository names used in #selectors.
// Keep ordinary names readable; encode punctuation (including the escape marker).
func inboxIDPart(name string) string {
	var b strings.Builder
	for _, c := range []byte(name) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
			b.WriteString(hex.EncodeToString([]byte{c}))
		}
	}
	return b.String()
}
