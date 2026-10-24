// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"cmp"
	"fmt"
	"time"
)

// IssuesMode says who may write issues in a repository.
type IssuesMode string

// Issues modes.
const (
	// IssuesOff turns issues off: the default.
	IssuesOff IssuesMode = "off"
	// IssuesOwner lets the owner and tokens with ScopeIssueWrite write issues.
	IssuesOwner IssuesMode = "owner"
	// IssuesPublic also opens the repository to submissions from anyone.
	IssuesPublic IssuesMode = "public"
)

// IssuesModes returns the three modes in their canonical order.
func IssuesModes() []IssuesMode { return []IssuesMode{IssuesOff, IssuesOwner, IssuesPublic} }

// ParseIssuesMode parses "off", "owner" or "public".
func ParseIssuesMode(s string) (IssuesMode, error) {
	for _, m := range IssuesModes() {
		if string(m) == s {
			return m, nil
		}
	}
	return "", fmt.Errorf("%w: issues mode %q (want off, owner or public)", ErrInvalid, s)
}

// String returns the mode as text.
func (m IssuesMode) String() string { return string(m) }

// OrDefault returns IssuesOff for the empty mode, else m.
func (m IssuesMode) OrDefault() IssuesMode {
	if m == "" {
		return IssuesOff
	}
	return m
}

// IssueState is where an issue is in its life.
type IssueState string

// Issue states.
const (
	// IssueStateOpen is being worked on.
	IssueStateOpen IssueState = "open"
	// IssueStateClosed is closed without being resolved; it can be reopened.
	IssueStateClosed IssueState = "closed"
	// IssueStateResolved is resolved; it can be reopened.
	IssueStateResolved IssueState = "resolved"
)

// Issue is a problem or a request about a repository. It shares its number
// sequence with the merge requests of the repository.
type Issue struct {
	Repo   RepoName
	Number int64
	Title  IssueTitle
	// Body may be empty.
	Body  IssueBody
	State IssueState
	// Author is the label of the token that opened it, "root" or "owner".
	Author    string
	CreatedAt time.Time
	UpdatedAt time.Time
	// CommentSeq is the id of the last comment of the issue.
	CommentSeq int64
	// External is true for an issue approved from the public form: its text
	// comes from an anonymous visitor and is rendered as untrusted.
	External bool
	// ClosedByMergeRequest is set only while resolved by a merge.
	ClosedByMergeRequest int64
	// Version is set by the store on read: Update fails with ErrConflict when
	// the stored issue changed since. Opaque to everyone else.
	Version uint64
}

// IssueLink is a merge request naming an issue in the same repository.
type IssueLink struct {
	Issue, MergeRequest int64
	Closes              bool
}

// IsOpen reports whether the issue is open.
func (i Issue) IsOpen() bool { return i.State == IssueStateOpen }

// IssueComment is a comment on an issue.
type IssueComment struct {
	ID        int64
	Author    string
	Text      IssueBody
	CreatedAt time.Time
	// Version is set by the store on read, like Issue's.
	Version uint64
}

// NumberKind says what a number of a repository names. Issues and merge
// requests share one sequence, so a number names at most one of them.
type NumberKind int

// Number kinds.
const (
	// NumberNone: no issue or merge request has this number.
	NumberNone NumberKind = iota
	// NumberIssue: the number names an issue.
	NumberIssue
	// NumberMergeRequest: the number names a merge request.
	NumberMergeRequest
)

// IsDone reports whether the issue is closed or resolved.
func (i Issue) IsDone() bool { return i.State == IssueStateClosed || i.State == IssueStateResolved }

// IssueEventKind describes an entry in an issue's append-only history.
type IssueEventKind string

// Issue event kinds.
const (
	IssueEventReferenced IssueEventKind = "referenced"
	IssueEventClosed     IssueEventKind = "closed"
	IssueEventReopened   IssueEventKind = "reopened"
	IssueEventResolved   IssueEventKind = "resolved"
	IssueEventEdited     IssueEventKind = "edited"
)

// IssueEvent is a timestamped history entry with a per-issue sequence ID.
type IssueEvent struct {
	ID            int64
	ChangedFields []string
	Kind          IssueEventKind
	Issue         int64
	MergeRequest  int64
	Closes        bool
	Actor         string
	CreatedAt     time.Time
}

// CompareIssueEvents orders events by time, then issue number, then id. It
// suits slices.SortFunc.
func CompareIssueEvents(a, b IssueEvent) int {
	if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Issue, b.Issue); c != 0 {
		return c
	}
	return cmp.Compare(a.ID, b.ID)
}
