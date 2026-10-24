// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// TimelineItem holds either a comment or a compact system event.
type TimelineItem struct {
	Comment *CommentView
	Event   *IssueEventView
	at      time.Time
}

type IssueEventView struct {
	ID, Kind, Actor, Text, When, WhenAt, Abs string
	MR                                       *Link
	Closes                                   bool
	Icon                                     string
}

type IssueLinkView struct {
	Number, Title, State, Href string
	Closes                     bool
}

// issueTimeline keeps comments ahead of events with the same timestamp.
func (s *Server) issueTimeline(repo string, comments []CommentView, events []ports.IssueEventDetail, mr bool) []TimelineItem {
	items := make([]TimelineItem, 0, len(comments)+len(events))
	for _, c := range comments {
		at := c.createdAt
		items = append(items, TimelineItem{Comment: &c, at: at})
	}
	for _, detail := range events {
		e := detail.Event
		v := IssueEventView{ID: strconv.FormatInt(e.ID, 10), Kind: string(e.Kind), Actor: s.who(e.Actor), Closes: e.Closes}
		v.When, v.WhenAt = whenOf(s.cfg.Now(), e.CreatedAt)
		if !e.CreatedAt.IsZero() {
			v.Abs = absolute(e.CreatedAt)
		}
		switch e.Kind {
		case domain.IssueEventReferenced:
			v.Text, v.Icon = "referenced this in", "↗"
			if e.Closes {
				v.Text = "will resolve this in"
			}
		case domain.IssueEventEdited:
			v.Text, v.Icon = "edited "+strings.Join(e.ChangedFields, " and "), "✎"
		case domain.IssueEventClosed:
			v.Text, v.Icon = "closed this", "✗"
		case domain.IssueEventReopened:
			v.Text, v.Icon = "reopened this", "↻"
		case domain.IssueEventResolved:
			v.Text, v.Icon = "resolved this", "✓"
			if e.MergeRequest != 0 {
				v.Text += " with"
			}
		default:
			continue
		}
		if mr {
			// IDs are per issue, so MR event anchors also include the issue number.
			v.ID = strconv.FormatInt(e.Issue, 10) + "-" + v.ID
			v.MR = &Link{Label: "#" + strconv.FormatInt(e.Issue, 10), Href: issueHref(repo, e.Issue)}
			switch e.Kind {
			case domain.IssueEventReferenced:
				v.Text = "referenced"
				if e.Closes {
					v.Text = "will resolve"
				}
			case domain.IssueEventResolved:
				v.Text = "resolved"
			case domain.IssueEventClosed:
				v.Text = "closed"
			case domain.IssueEventReopened:
				v.Text = "reopened"
			}
		} else if e.MergeRequest != 0 {
			v.MR = numberLink(repo, e.MergeRequest)
			if title := visibleText(detail.MergeRequestTitle.String()); title != "" {
				v.MR.Label += " " + title
			}
		}
		items = append(items, TimelineItem{Event: &v, at: e.CreatedAt})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].at.Before(items[j].at) })
	return items
}
