// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"fmt"
	"regexp"
	"time"
)

// ZeroOID is the object ID that stands for "no object" in a ref update.
const ZeroOID = "0000000000000000000000000000000000000000"

// RefUpdate is one ref change of a push, as Git reports it to the hooks.
type RefUpdate struct {
	// Old is the previous object ID, ZeroOID when the ref is created.
	Old string
	// New is the new object ID, ZeroOID when the ref is deleted.
	New string
	// Ref is the full ref name, such as refs/heads/main.
	Ref string
}

var oidRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// maxRefLen bounds a ref name.
const maxRefLen = 4096

// NewRefUpdate validates the parts of a ref update as Git reports them: two
// 40-digit lowercase hexadecimal object IDs (sha1) and a non-empty ref name.
func NewRefUpdate(oldOID, newOID, ref string) (RefUpdate, error) {
	if !oidRE.MatchString(oldOID) || !oidRE.MatchString(newOID) {
		return RefUpdate{}, fmt.Errorf("invalid object id in the update of %q", ref)
	}
	if ref == "" || len(ref) > maxRefLen {
		return RefUpdate{}, fmt.Errorf("invalid ref name %q", ref)
	}
	return RefUpdate{Old: oldOID, New: newOID, Ref: ref}, nil
}

// IsCreate reports whether the update creates the ref.
func (u RefUpdate) IsCreate() bool { return u.Old == ZeroOID }

// IsDelete reports whether the update deletes the ref.
func (u RefUpdate) IsDelete() bool { return u.New == ZeroOID }

// Event is something that happened in the forge, published on the event bus.
type Event interface {
	// EventKind names the kind of event.
	EventKind() string
}

// RepoCreated is published when a repository comes into being.
type RepoCreated struct {
	Repo       RepoName
	Visibility Visibility
	At         time.Time
}

// EventKind implements Event.
func (RepoCreated) EventKind() string { return "repo.created" }

// RepoUpdated is published when the metadata of a repository, or its default
// branch, changed. The new state is read from the repository.
//
// The visibility fields tell who could read the repository before and after
// the change, and CreatedAt tells which repository of that name it was (a
// name can be deleted and created again): the event stream decides who may be
// told from them.
type RepoUpdated struct {
	Repo RepoName
	At   time.Time
	// Visibility is the visibility after the change, Previous the one before.
	Visibility, Previous Visibility
	// CreatedAt is the creation time of the repository.
	CreatedAt time.Time
}

// EventKind implements Event.
func (RepoUpdated) EventKind() string { return "repo.updated" }

// RepoDeleted is published after a repository was deleted. It records the
// visibility the repository had, which decides who may be told.
type RepoDeleted struct {
	Repo       RepoName
	Visibility Visibility
	At         time.Time
	// CreatedAt is the creation time of the repository that was deleted.
	CreatedAt time.Time
}

// EventKind implements Event.
func (RepoDeleted) EventKind() string { return "repo.deleted" }

// PushEvent is published after a push has been accepted.
//
// It carries the visibility and the creation time of the repository as read
// when the push was recorded, so the event stream does not read them later,
// when the repository may have changed or been recreated. A zero CreatedAt
// means the repository was unknown: the event is not delivered.
type PushEvent struct {
	Repo    RepoName
	Updates []RefUpdate
	At      time.Time
	// Visibility is the visibility of the repository at publish time.
	Visibility Visibility
	// CreatedAt is the creation time of the repository that was pushed to.
	CreatedAt time.Time
}

// EventKind implements Event.
func (PushEvent) EventKind() string { return "repo.pushed" }

// MergeRequestOpened is published when a merge request is opened.
type MergeRequestOpened struct {
	MergeRequest MergeRequest
	At           time.Time
	// Visibility and CreatedAt are those of the repository when the event
	// was published, like PushEvent's.
	Visibility Visibility
	CreatedAt  time.Time
}

// EventKind implements Event.
func (MergeRequestOpened) EventKind() string { return "mr.opened" }

// MergeRequestMerged is published when a merge request was merged, before the
// PushEvent of the target branch.
type MergeRequestMerged struct {
	MergeRequest MergeRequest
	At           time.Time
	Visibility   Visibility
	CreatedAt    time.Time
}

// EventKind implements Event.
func (MergeRequestMerged) EventKind() string { return "mr.merged" }

// MergeRequestClosed is published when a merge request was closed without
// merging.
type MergeRequestClosed struct {
	MergeRequest MergeRequest
	At           time.Time
	Visibility   Visibility
	CreatedAt    time.Time
}

// EventKind implements Event.
func (MergeRequestClosed) EventKind() string { return "mr.closed" }

// MergeRequestReady is published when a draft merge request was marked ready
// for review: its title lost the draft prefix.
type MergeRequestReady struct {
	MergeRequest MergeRequest
	At           time.Time
	Visibility   Visibility
	CreatedAt    time.Time
}

// EventKind implements Event.
func (MergeRequestReady) EventKind() string { return "mr.ready" }

// CommentAdded is published when a comment was added to a merge request.
type CommentAdded struct {
	Repo       RepoName
	Number     int64
	Comment    Comment
	At         time.Time
	Visibility Visibility
	CreatedAt  time.Time
}

// EventKind implements Event.
func (CommentAdded) EventKind() string { return "mr.comment_added" }

// IssueOpened is published when an issue is opened.
type IssueOpened struct {
	Issue Issue
	At    time.Time
	// Visibility and CreatedAt are those of the repository when the event
	// was published, like PushEvent's.
	Visibility Visibility
	CreatedAt  time.Time
}

// EventKind implements Event.
func (IssueOpened) EventKind() string { return "issue.opened" }

// IssueClosed is published when an issue was closed.
type IssueClosed struct {
	Issue      Issue
	At         time.Time
	Visibility Visibility
	CreatedAt  time.Time
}

// EventKind implements Event.
func (IssueClosed) EventKind() string { return "issue.closed" }

// IssueReopened is published when a closed issue was reopened.
type IssueReopened struct {
	Issue      Issue
	At         time.Time
	Visibility Visibility
	CreatedAt  time.Time
}

// EventKind implements Event.
func (IssueReopened) EventKind() string { return "issue.reopened" }

// IssueCommented is published when a comment was added to an issue.
type IssueCommented struct {
	Repo       RepoName
	Number     int64
	Comment    IssueComment
	At         time.Time
	Visibility Visibility
	CreatedAt  time.Time
}

// EventKind implements Event.
func (IssueCommented) EventKind() string { return "issue.commented" }

// IssueSubmitted is published when an anonymous submission awaits moderation.
// It carries no content, and is delivered to the owner topic only: never to
// home or to a repository topic.
type IssueSubmitted struct {
	Repo RepoName
	ID   PendingIssueID
	At   time.Time
}

// EventKind implements Event.
func (IssueSubmitted) EventKind() string { return "issue.submitted" }

// IssuePendingRemoved is published when a pending submission is rejected, so
// the other open inboxes drop it. Like IssueSubmitted it carries no content
// and is delivered to the owner topic only.
type IssuePendingRemoved struct {
	Repo RepoName
	ID   PendingIssueID
	At   time.Time
}

// EventKind implements Event.
func (IssuePendingRemoved) EventKind() string { return "issue.pending_removed" }

// EventGap tells a subscriber that it missed events because it was too slow;
// it must refresh its whole view.
type EventGap struct{}

// EventKind implements Event.
func (EventGap) EventKind() string { return "gap" }
