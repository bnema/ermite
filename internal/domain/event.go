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

// EventGap tells a subscriber that it missed events because it was too slow;
// it must refresh its whole view.
type EventGap struct{}

// EventKind implements Event.
func (EventGap) EventKind() string { return "gap" }
