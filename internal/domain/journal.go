// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import "time"

// JournalRetention is how long the activity journal keeps an entry.
const JournalRetention = 30 * 24 * time.Hour

// MaxJournalCommits bounds the commits a push entry counts.
const MaxJournalCommits = 100

// MaxTagDistance bounds the commits counted since the last tag.
const MaxTagDistance = 500

// JournalKind is what an entry of the activity journal records.
type JournalKind string

// The kinds of journal entries.
const (
	JournalMergeRequestOpened JournalKind = "mr_opened"
	JournalMergeRequestMerged JournalKind = "mr_merged"
	JournalMergeRequestClosed JournalKind = "mr_closed"
	JournalIssueOpened        JournalKind = "issue_opened"
	// JournalIssueClosed is an issue closed by hand or resolved by a merge.
	JournalIssueClosed       JournalKind = "issue_closed"
	JournalTagCreated        JournalKind = "tag_created"
	JournalTagDeleted        JournalKind = "tag_deleted"
	JournalReleasePublished  JournalKind = "release_published"
	JournalDefaultBranchPush JournalKind = "push"
)

// JournalEntry is one thing that happened in a repository, as the owner's
// activity journal keeps it.
type JournalEntry struct {
	// ID identifies a stored entry: 32 lowercase hexadecimal digits that sort
	// like the entries (time, then append order). The store sets it; it is
	// empty before the entry is stored.
	ID   string
	At   time.Time
	Repo RepoName
	Kind JournalKind
	// Actor is who did it ("owner", "root", a token label); empty when unknown.
	Actor string
	// Number is the merge request or the issue; zero for the other kinds.
	Number int64
	// Title is the title of the merge request, the issue or the release.
	Title string
	// Ref is the short name of the tag, or the branch a push moved.
	Ref string
	// Commits counts the commits a push brought, at most MaxJournalCommits;
	// More tells that there were more.
	Commits int
	More    bool
	// Commit is the new head of a push, the commit of a merge, or the object a
	// new tag points to. Zero when unknown.
	Commit ObjectID
	// Summary is the subject of the newest commit of a push.
	Summary string
}

// JournalIDLen is the length of a JournalEntry.ID.
const JournalIDLen = 32

// ValidJournalID reports whether s has the form of a JournalEntry.ID.
func ValidJournalID(s string) bool {
	if len(s) != JournalIDLen {
		return false
	}
	for _, c := range []byte(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// JournalKinds lists every kind of journal entry.
func JournalKinds() []JournalKind {
	return []JournalKind{
		JournalMergeRequestOpened, JournalMergeRequestMerged, JournalMergeRequestClosed,
		JournalIssueOpened, JournalIssueClosed,
		JournalTagCreated, JournalTagDeleted, JournalReleasePublished, JournalDefaultBranchPush,
	}
}

// TagDistance is how far the head of a repository is from its nearest tag:
// the commits met walking back from the head (newest first) before one that a
// tag points to.
type TagDistance struct {
	// Tag is the tag of that commit (the most recent one when several point to
	// it). Zero when no tagged commit was met within the limit.
	Tag TagInfo
	// Commits is the number of commits before it, at most the limit; More
	// tells that the limit was reached first.
	Commits int
	More    bool
}

// Ahead reports whether the head has commits that no tag covers.
func (d TagDistance) Ahead() bool { return d.Commits > 0 || d.More }
