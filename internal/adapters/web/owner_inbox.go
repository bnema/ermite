// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The owner’s inbox lists what needs a decision: the Attention items, one row
// each. Only a signed-in owner may read it.

// inboxPath is the page of the inbox.
const inboxPath = "/-/inbox"

// moderationPath is the prefix of the moderation actions.
const moderationPath = "/-/moderation"

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

// InboxPage is what the owner pages render: each fills its own sections.
type InboxPage struct {
	RunsEnabled bool
	Runs        []InboxRunRow
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
	MRs         []InboxRow
	IssueRows   []InboxRow
	// Journal is the activity section; zero without a journal.
	Journal JournalView
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
		base := moderationPath + "/" + url.PathEscape(repo) + "/" + url.PathEscape(pi.ID.String()) + "/"
		row.Approve, row.Reject, row.Ban = base+"approve", base+"reject", base+"ban"
	}
	return row
}

// pendingBack returns moderation actions to the inbox.
func pendingBack(r *http.Request) string { return inboxPath }

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

// pendingApprove is POST /-/moderation/<repo>/<id>/approve.
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

// pendingReject is POST /-/moderation/<repo>/<id>/reject, or .../ban, which
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

// inbox is GET /-/inbox: what awaits the owner.
func (s *Server) inbox(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	if !SignedIn(ctx) {
		return nil, 0, errNotFound
	}
	rows, counts, err := s.attention(ctx)
	if err != nil {
		return nil, 0, err
	}
	l := s.base()
	l.Title = "Inbox · " + s.instance
	l.OwnerNav = s.ownerNavWith(ctx, "inbox", counts)
	l.Cmd = []CmdItem{{Kind: "go", Label: "home", Href: "/"}}
	if s.cfg.Events != nil {
		l.Live, l.LiveURL = true, liveURL("owner", "view=inbox")
	}
	return Inbox(l, rows), http.StatusOK, nil
}

// AttentionRow is one item of the inbox. Its texts are plain strings; a
// pending issue comes from an anonymous visitor and is never Markdown.
type AttentionRow struct {
	ID, Kind, Chip string
	Title, Href    string
	Repo, RepoHref string
	Note, NoteTip  string
	When, WhenAt   string
	Cursor         bool
	// Checks is the CI state of a ready merge request.
	Checks ChecksSummary
	// Pending is the issue awaiting moderation: its preview and actions.
	Pending *PendingRow
	// Action is the link of the row: View log, Open or Commits; empty for
	// none.
	ActionLabel, ActionHref string
}

// attentionChips are the labels of the kinds.
var attentionChips = map[ports.AttentionKind]string{
	ports.AttentionModeration: "Moderation",
	ports.AttentionDepsFailed: "Deps failed",
	ports.AttentionReady:      "Ready",
	ports.AttentionRelease:    "Release due",
}

// attentionID is the id of the row of an item; ref tells items of one kind
// and repository apart.
func attentionID(kind ports.AttentionKind, repo, ref string) string {
	return "att-" + string(kind) + "-" + inboxIDPart(repo) + "-" + inboxIDPart(ref)
}

// attention reads the Attention items into their rows, in their order, with
// the owner counts of the same read.
func (s *Server) attention(ctx context.Context) ([]AttentionRow, ports.OwnerCounts, error) {
	a, err := s.cfg.Inbox.Attention(ctx, principal(ctx))
	if err != nil {
		return nil, ports.OwnerCounts{}, err
	}
	now := s.cfg.Now()
	var rows []AttentionRow
	for _, it := range a.Items {
		if row, ok := s.attentionRow(now, it); ok {
			rows = append(rows, row)
		}
	}
	if len(rows) > 0 {
		rows[0].Cursor = true
	}
	return rows, a.Counts, nil
}

// attentionRow is the row of an item; false for an item without its data.
func (s *Server) attentionRow(now time.Time, it ports.AttentionItem) (AttentionRow, bool) {
	repo := it.Repo.String()
	row := AttentionRow{Kind: string(it.Kind), Chip: attentionChips[it.Kind], Repo: repo, RepoHref: repoHref(repo)}
	row.When, row.WhenAt = whenOf(now, it.Since)
	switch it.Kind {
	case ports.AttentionModeration:
		if it.Pending == nil {
			return row, false
		}
		p := s.pendingRow(now, *it.Pending)
		row.ID, row.Title, row.Note, row.Pending = p.ID, p.Title, p.Author, &p
	case ports.AttentionDepsFailed:
		if it.Deps == nil {
			return row, false
		}
		d := s.depsRow(*it.Deps, "")
		ref := d.Batch
		if ref == "" {
			ref = "base"
		}
		row.ID = attentionID(it.Kind, repo, ref)
		row.Title, row.Href = d.Title, depsPath+"#"+d.ID
		row.Note, _, _ = strings.Cut(strings.TrimSpace(visibleText(it.Deps.Batch.Error)), "\n")
		if it.Deps.Run != nil {
			row.ActionLabel, row.ActionHref = "View log", s.runRow(repo, *it.Deps.Run, now).Href
		}
	case ports.AttentionReady:
		m := it.MergeRequest
		if m == nil {
			return row, false
		}
		row.ID = attentionID(it.Kind, repo, strconv.FormatInt(m.Number, 10))
		row.Title, row.Href = visibleText(m.Title.String()), mrHref(repo, m.Number)
		views := make([]ports.CheckView, 0, len(it.Checks))
		for _, c := range it.Checks {
			views = append(views, ports.CheckView{CICheck: c})
		}
		row.Checks = checksSummary(it.Repo, m.HeadID.String(), views)
		row.ActionLabel, row.ActionHref = "Open", row.Href
	case ports.AttentionRelease:
		u := it.Unreleased
		if u == nil {
			return row, false
		}
		row.ID = attentionID(it.Kind, repo, "release")
		n := itoa(u.Distance.Commits)
		if u.Distance.More {
			n += "+"
		}
		word := " commits"
		if u.Distance.Commits == 1 && !u.Distance.More {
			word = " commit"
		}
		since := " since the last tag"
		if t := u.Distance.Tag; t.Name != "" {
			since = " since " + visibleText(t.Name)
			if !t.At.IsZero() {
				row.Note, row.NoteTip = tagAge(now, t.At), absolute(t.At)
			}
		}
		row.Title, row.Href = n+word+since, repoHref(repo, "commits", u.Branch)
		row.ActionLabel, row.ActionHref = "Commits", row.Href
	default:
		return row, false
	}
	return row, true
}

// pendingRowID is the id of the row of a pending issue in the inbox.
func pendingRowID(repo, id string) string { return attentionID(ports.AttentionModeration, repo, id) }

func (s *Server) pendingResult(ctx context.Context, r *http.Request) (ActionResult, error) {
	result := ActionResult{Redirect: pendingBack(r)}
	if !isHX(r) {
		return result, nil
	}
	result.Fragment = inboxModerated(pendingRowID(r.PathValue("repo"), r.PathValue("id")), s.ownerNav(ctx, "inbox"))
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

// tagAge is the age of a tag in days: "tagged today" under a day.
func tagAge(now, at time.Time) string {
	days := int(now.Sub(at) / (24 * time.Hour))
	if days <= 0 {
		return "tagged today"
	}
	return "tagged " + itoa(days) + "d ago"
}
