// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/a-h/templ"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// activityPath is the owner's running runs, unreleased repositories and
// journal.
const activityPath = "/-/activity"

// ActivityPage is the running runs, the unreleased repositories and the
// journal of the range.
type ActivityPage struct {
	RunsEnabled bool
	Runs        []RunRowView
	// Journal is the activity section; zero without a journal.
	Journal JournalView
}

// NavLink is a plain navigation link.
type NavLink struct {
	Label   string
	Href    string
	Current bool
}

// RunRowView adds repository and start time to the Actions run display.
type RunRowView struct {
	RunRow
	Repo, RepoHref, When, WhenAt string
}

// activity is GET /-/activity?range=.
func (s *Server) activity(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	if !SignedIn(ctx) {
		return nil, 0, errNotFound
	}
	rng, err := parseRange(journalRanges, r.URL.Query().Get("range"))
	if err != nil {
		return nil, 0, err
	}
	p, err := s.activityPage(ctx, rng)
	if err != nil {
		return nil, 0, err
	}
	return Activity(s.ownerLayout(ctx, "activity", "Activity", "range="+journalRanges[rng].param), p), http.StatusOK, nil
}

// activityPage reads the runs and the journal of the range rng.
func (s *Server) activityPage(ctx context.Context, rng int) (ActivityPage, error) {
	p := principal(ctx)
	runs, err := s.cfg.Inbox.Runs(ctx, p)
	if err != nil {
		return ActivityPage{}, err
	}
	j, err := s.cfg.Inbox.Journal(ctx, p, journalRanges[rng].window)
	if err != nil {
		return ActivityPage{}, err
	}
	return ActivityPage{RunsEnabled: runs.Enabled, Runs: s.runRows(runs.Runs, s.cfg.Now()), Journal: s.journalView(j, rng)}, nil
}

// runRows are the rows of the running runs, with their repository and start.
func (s *Server) runRows(runs []domain.CIRun, now time.Time) []RunRowView {
	var rows []RunRowView
	for _, run := range runs {
		r := RunRowView{RunRow: s.runRow(run.Repo.String(), run, now), Repo: run.Repo.String(), RepoHref: repoHref(run.Repo.String())}
		if run.Status == domain.CIInProgress {
			r.State = "in progress"
		}
		ts := run.StartedAt
		if ts.IsZero() {
			ts = run.CreatedAt
		}
		r.When, r.WhenAt = whenOf(now, ts)
		rows = append(rows, r)
	}
	return rows
}

// JournalView is the journal of the Activity page: the range toggle, the
// repositories ahead of their nearest tag, then the entries newest first.
type JournalView struct {
	Ranges     []NavLink
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
var journalRanges = []pageRange{
	{"24h", "24 h", "24 hours", 24 * time.Hour},
	{"7d", "7 days", "7 days", 7 * 24 * time.Hour},
}

// journalView builds the activity section.
func (s *Server) journalView(j ports.InboxJournal, rng int) JournalView {
	now := s.cfg.Now()
	v := JournalView{RangeLabel: journalRanges[rng].long, Truncated: j.Truncated}
	v.Ranges = rangeLinks(activityPath, journalRanges, rng)
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
