// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"bnema.dev/ermite-api/gen/ermite"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const jsonIssueEventHelp = `
JSON fields of a history event: id, kind (referenced, closed, reopened, resolved, edited), issue,
merge_request (0 for none), merge_request_title, closes, actor, created_at (RFC 3339 UTC), changed_fields (edited fields, when present).`

type jsonIssueEvent struct {
	ID                string   `json:"id"`
	Kind              string   `json:"kind"`
	Issue             int64    `json:"issue"`
	MergeRequest      int64    `json:"merge_request"`
	MergeRequestTitle string   `json:"merge_request_title"`
	Closes            bool     `json:"closes"`
	Actor             string   `json:"actor"`
	CreatedAt         *string  `json:"created_at"`
	ChangedFields     []string `json:"changed_fields,omitempty"`
}

func toJSONIssueEvents(events []*ermite.IssueEvent) []jsonIssueEvent {
	out := make([]jsonIssueEvent, 0, len(events))
	for _, e := range events {
		out = append(out, jsonIssueEvent{ID: e.GetId(), ChangedFields: e.GetChangedFields(), Kind: strings.ToLower(strings.TrimPrefix(e.GetKind().String(), "ISSUE_EVENT_KIND_")), Issue: e.GetIssue(), MergeRequest: e.GetMergeRequest(), MergeRequestTitle: e.GetMergeRequestTitle(), Closes: e.GetCloses(), Actor: e.GetActor(), CreatedAt: jsonTime(e.GetCreatedAt())})
	}
	return out
}

func issueHistoryText(e *ermite.IssueEvent, mrSide bool) string {
	subject := "this"
	if mrSide {
		subject = fmt.Sprintf("#%d", e.GetIssue())
	}
	var text string
	switch e.GetKind() {
	case ermite.IssueEventKind_ISSUE_EVENT_KIND_REFERENCED:
		text = fmt.Sprintf("referenced %s in !%d", subject, e.GetMergeRequest())
		if e.GetCloses() {
			text += " (will resolve)"
		}
	case ermite.IssueEventKind_ISSUE_EVENT_KIND_RESOLVED:
		text = "resolved " + subject
		if e.GetMergeRequest() > 0 {
			text += fmt.Sprintf(" by merging !%d", e.GetMergeRequest())
		}
	case ermite.IssueEventKind_ISSUE_EVENT_KIND_EDITED:
		text = "edited " + strings.Join(e.GetChangedFields(), " and ")
	case ermite.IssueEventKind_ISSUE_EVENT_KIND_CLOSED:
		text = "closed " + subject
	case ermite.IssueEventKind_ISSUE_EVENT_KIND_REOPENED:
		text = "reopened " + subject
	default:
		text = "updated " + subject
	}
	return clean(e.GetActor()) + " " + text
}

type issueTimelineRow struct {
	at    *timestamppb.Timestamp
	print func(io.Writer) error
}

func newIssueTimeline(events []*ermite.IssueEvent, mrSide bool) []issueTimelineRow {
	rows := make([]issueTimelineRow, 0, len(events))
	for _, e := range events {
		rows = append(rows, issueTimelineRow{at: e.GetCreatedAt(), print: func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "  → %s %s\n", shortTime(e.GetCreatedAt(), "-"), issueHistoryText(e, mrSide))
			return err
		}})
	}
	return rows
}

func printIssueTimeline(w io.Writer, rows []issueTimelineRow) error {
	// Stable sorting puts comments before events at the same instant.
	slices.SortStableFunc(rows, func(a, b issueTimelineRow) int {
		if n := a.at.AsTime().Compare(b.at.AsTime()); n != 0 {
			return n
		}
		return 0
	})
	for _, row := range rows {
		if err := row.print(w); err != nil {
			return err
		}
	}
	return nil
}

func mrIssueStateName(s ermite.MergeRequestIssueState) string {
	switch s {
	case ermite.MergeRequestIssueState_MERGE_REQUEST_ISSUE_STATE_OPEN:
		return stateOpen
	case ermite.MergeRequestIssueState_MERGE_REQUEST_ISSUE_STATE_CLOSED:
		return stateClosed
	case ermite.MergeRequestIssueState_MERGE_REQUEST_ISSUE_STATE_RESOLVED:
		return stateResolved
	default:
		return "unknown"
	}
}
