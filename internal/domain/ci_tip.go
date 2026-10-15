// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

// CIRefTip records who moves a branch or a tag to an object (CI-3, D1): the
// actor and pusher of the push or merge that sets Ref to Sha, and whether that
// principal was trusted (TrustedActor).
//
// It is what a scheduled run takes its actor and its trust from (GitHub runs a
// schedule as the author of the last commit of the default branch), and what a
// dispatched run's trust is capped by: the default branch is not necessarily
// the owner's work (a token limited to some branches can push the first branch
// of an empty repository, which becomes the default branch), and an agent's
// branch is not the owner's work either.
//
// The record is written BEFORE the ref moves (in the pre-receive decision of a
// push, and inside the merge before it updates the target), so that no moment
// exists when the tip is the new commit and its record the previous pusher's;
// writing it after would let a slow post-receive of an agent's push leave the
// owner's record on a tip the agent has since moved back to the same commit
// (owner A, agent B, agent A). It names the object it was written for, and is
// valid only while the ref still points to it. A push that is refused after the
// record was written leaves a record that matches no tip: not trusted (fails
// closed). A tip with no record (a push from before records existed, a record
// that could not be written) is not trusted either.
//
// Two concurrent pushes of the same object by the owner and by an agent can
// leave the owner's record: the object is the same and the owner pushed it, so
// the content is endorsed by the owner. That case is accepted.
type CIRefTip struct {
	Repo RepoName
	// Ref is the full name: "refs/heads/main" or "refs/tags/v1".
	Ref string
	// Sha is the object the ref is set to (the tag object for an annotated tag).
	Sha string
	// Actor is who the run shows, as for a push (authorOf).
	Actor string
	// Pusher is the principal whose rights the actions of other repositories are
	// resolved with.
	Pusher  CIPusher
	Trusted bool
}
