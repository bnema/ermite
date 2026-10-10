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

// PushEvent is published after a push has been accepted.
type PushEvent struct {
	Repo    RepoName
	Updates []RefUpdate
	At      time.Time
}

// EventKind implements Event.
func (PushEvent) EventKind() string { return "repo.pushed" }

// EventGap tells a subscriber that it missed events because it was too slow;
// it must refresh its whole view.
type EventGap struct{}

// EventKind implements Event.
func (EventGap) EventKind() string { return "gap" }
