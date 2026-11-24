// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/a-h/templ"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The owner’s inbox lists what needs a decision: for now, the issues awaiting
// moderation. Only a signed-in owner may read it.

// inboxPath is the page of the inbox.
const inboxPath = "/-/inbox"

// moderationPath is the prefix of the moderation actions.
const moderationPath = "/-/moderation"

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

// InboxPage is what the owner pages render: each fills its own sections.
type InboxPage struct {
	RunsEnabled bool
	Runs        []InboxRunRow
	Pending     []PendingRow
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

// inbox is GET /-/inbox: the issues awaiting moderation.
func (s *Server) inbox(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	if !SignedIn(ctx) {
		return nil, 0, errNotFound
	}
	p, err := s.inboxModeration(ctx)
	if err != nil {
		return nil, 0, err
	}
	l := s.base()
	l.Title = "Inbox · " + s.instance
	l.OwnerNav = s.ownerNav(ctx, "inbox")
	l.Cmd = []CmdItem{{Kind: "go", Label: "home", Href: "/"}}
	if s.cfg.Events != nil {
		l.Live, l.LiveURL = true, liveURL("owner", "view=inbox")
	}
	return Inbox(l, p), http.StatusOK, nil
}

// inboxModeration reads the issues awaiting moderation into their rows.
func (s *Server) inboxModeration(ctx context.Context) (InboxPage, error) {
	pending, err := s.pendingIssues(ctx)
	if err != nil {
		return InboxPage{}, err
	}
	now := s.cfg.Now()
	var p InboxPage
	for _, pi := range pending {
		p.Pending = append(p.Pending, s.pendingRow(now, pi))
	}
	return p, nil
}

// inbox renders the moderation list again when an issue is submitted or
// leaves the queue, or a repository changes.
func (l *liveStream) inbox(ctx context.Context, we ports.WatchEvent) ([]fragment, error) {
	switch we.Event.(type) {
	case domain.IssueSubmitted, domain.IssuePendingRemoved, domain.RepoUpdated, domain.RepoDeleted:
	default:
		return nil, nil
	}
	if l.s.cfg.Inbox == nil {
		return nil, nil
	}
	p, err := l.s.inboxModeration(ctx)
	if err != nil {
		return nil, err
	}
	f, err := renderFragment(ctx, inboxEvent, InboxPending(p))
	if err != nil {
		return nil, err
	}
	return []fragment{f}, nil
}

func pendingRowID(repo, id string) string { return "pending-" + inboxIDPart(repo) + "-" + id }

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
