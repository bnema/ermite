package web

import (
	"bytes"
	"context"
	"net/url"

	"github.com/a-h/templ"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// ownerEvent is the name of the live events of the owner's pages.
const ownerEvent = "owner"

// ownerStreamOK tells whether the request may follow the owner topic: the
// signed-in owner, with one of the owner views the server has, and a valid
// range for the views that have one.
func (s *Server) ownerStreamOK(ctx context.Context, q url.Values) bool {
	if s.cfg.Inbox == nil || !SignedIn(ctx) {
		return false
	}
	switch q.Get("view") {
	case "inbox", "work":
		return true
	case "activity":
		_, err := parseRange(journalRanges, q.Get("range"))
		return err == nil
	case "deps":
		return s.cfg.Deps != nil
	case "insights":
		if _, ok := s.cfg.Traffic.(TrafficForgeReader); !ok {
			return false
		}
		_, err := parseRange(trafficRanges, q.Get("range"))
		return err == nil
	}
	return false
}

// ownerViewEvent reports whether the event changes the live sections of view.
func ownerViewEvent(view string, ev domain.Event) bool {
	switch ev.(type) {
	case domain.RepoUpdated, domain.RepoDeleted:
		return view != "insights"
	}
	switch view {
	case "inbox":
		switch ev.(type) {
		case domain.IssueSubmitted, domain.IssuePendingRemoved,
			domain.MergeRequestOpened, domain.MergeRequestEdited, domain.MergeRequestMerged, domain.MergeRequestClosed, domain.MergeRequestReady,
			domain.JournalUpdated, domain.DependencyUpdatesFound, domain.DependencyUpdatesChanged, domain.ReleasePublished:
			return true
		case domain.RunUpdated:
			return navCountEvent(ev)
		}
	case "work":
		switch ev.(type) {
		case domain.MergeRequestOpened, domain.MergeRequestEdited, domain.MergeRequestMerged, domain.MergeRequestClosed, domain.MergeRequestReady,
			domain.IssueEdited, domain.IssueOpened, domain.IssueResolved, domain.IssueClosed, domain.IssueReopened, domain.RunUpdated:
			return true
		}
	case "activity":
		switch ev.(type) {
		case domain.RunUpdated, domain.JournalUpdated, domain.ReleasePublished,
			// The journal records these without a JournalUpdated of their own.
			domain.MergeRequestOpened, domain.MergeRequestMerged, domain.MergeRequestClosed,
			domain.IssueOpened, domain.IssueClosed, domain.IssueResolved:
			return true
		}
	case "deps":
		switch ev.(type) {
		case domain.DependencyUpdatesFound, domain.DependencyUpdatesChanged, domain.RunUpdated:
			return true
		}
	}
	return false
}

// navCountEvent reports whether the event can change the counts of the owner
// navigation. A run counts only when it is queued or completes.
func navCountEvent(ev domain.Event) bool {
	switch e := ev.(type) {
	case domain.IssueSubmitted, domain.IssuePendingRemoved, domain.IssueOpened, domain.IssueClosed, domain.IssueReopened, domain.IssueResolved,
		domain.MergeRequestOpened, domain.MergeRequestEdited, domain.MergeRequestMerged, domain.MergeRequestClosed, domain.MergeRequestReady,
		domain.JournalUpdated, domain.DependencyUpdatesFound, domain.DependencyUpdatesChanged, domain.ReleasePublished,
		domain.RepoUpdated, domain.RepoDeleted:
		return true
	case domain.RunUpdated:
		return e.Run.Status == domain.CIQueued || e.Run.Status == domain.CICompleted
	}
	return false
}

// owner renders the live sections of the owner view the stream follows when
// the event changes them, and the owner navigation when the event can change
// its counts, in one fragment; nothing when neither applies.
func (l *liveStream) owner(ctx context.Context, we ports.WatchEvent) ([]fragment, error) {
	if l.s.cfg.Inbox == nil || !SignedIn(ctx) {
		return nil, nil
	}
	view, nav := ownerViewEvent(l.view, we.Event), navCountEvent(we.Event)
	var parts []templ.Component
	if l.view == "inbox" && (view || nav) {
		// One Attention read gives the rows and the navigation counts.
		rows, counts, err := l.s.attention(ctx)
		if err != nil {
			return nil, err
		}
		if view {
			parts = append(parts, InboxAttention(rows))
		}
		if n := l.s.ownerNavWith(ctx, l.view, counts); nav && n != nil {
			parts = append(parts, OwnerNavBar(*n))
		}
		return renderOwner(ctx, parts)
	}
	if view {
		c, err := l.ownerSections(ctx)
		if err != nil {
			return nil, err
		}
		parts = append(parts, c...)
	}
	if nav {
		if n := l.s.ownerNav(ctx, l.view); n != nil {
			parts = append(parts, OwnerNavBar(*n))
		}
	}
	return renderOwner(ctx, parts)
}

// ownerNavFragment is the owner navigation, with current shown, when the
// event can change its counts; nothing otherwise or for anyone but the owner.
func (l *liveStream) ownerNavFragment(ctx context.Context, ev domain.Event, current string) ([]fragment, error) {
	if !navCountEvent(ev) {
		return nil, nil
	}
	n := l.s.ownerNav(ctx, current)
	if n == nil {
		return nil, nil
	}
	return renderOwner(ctx, []templ.Component{OwnerNavBar(*n)})
}

// renderOwner renders parts into one owner fragment; none without parts.
func renderOwner(ctx context.Context, parts []templ.Component) ([]fragment, error) {
	if len(parts) == 0 {
		return nil, nil
	}
	var b bytes.Buffer
	for _, c := range parts {
		if err := c.Render(ctx, &b); err != nil {
			return nil, err
		}
	}
	return []fragment{{event: ownerEvent, html: b.String()}}, nil
}

// ownerSections reads the view the stream follows, other than the inbox
// (read in owner with its counts), and returns its live sections.
func (l *liveStream) ownerSections(ctx context.Context) ([]templ.Component, error) {
	s := l.s
	switch l.view {
	case "work":
		page, err := s.workPage(ctx)
		if err != nil {
			return nil, err
		}
		return []templ.Component{WorkMRs(page), WorkIssues(page)}, nil
	case "activity":
		rng, err := parseRange(journalRanges, l.rng) // checked when the stream was opened
		if err != nil {
			return nil, err
		}
		page, err := s.activityPage(ctx, rng)
		if err != nil {
			return nil, err
		}
		return []templ.Component{ActivityRuns(page), ActivityJournal(page)}, nil
	case "deps":
		page, err := s.depsPage(ctx, depsQuery{sort: depsSortParam(l.sort), filter: depsFilterParam(l.filter)})
		if err != nil {
			return nil, err
		}
		return []templ.Component{DepsTable(page)}, nil
	}
	return nil, nil
}
