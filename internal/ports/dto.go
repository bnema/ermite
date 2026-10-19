// SPDX-License-Identifier: AGPL-3.0-or-later

package ports

import (
	"time"

	"bnema.dev/ermite/internal/domain"
)

// The parameters and results of the use case interfaces (services.go). They
// live here so that the adapters that call the use cases and the use cases
// themselves share them without importing each other.

// RepoPage is a page of repositories ordered by name.
type RepoPage struct {
	Repos []domain.Repo
	// NextPageToken is empty on the last page.
	NextPageToken string
}

// RepoView is a repository with the Git state that belongs in its summary.
type RepoView struct {
	domain.Repo
	// DefaultBranch is the branch HEAD points to, even when it does not exist
	// yet; empty when HEAD is detached or the directory is missing.
	DefaultBranch string
}

// CreateRepoParams are the settings of a new repository.
type CreateRepoParams struct {
	Name        domain.RepoName
	Description string
	// Topics are normalized by Create (see domain.NormalizeTopics).
	Topics []string
	// Visibility defaults to private when empty.
	Visibility domain.Visibility
}

// UpdateRepoParams are the changes of Update; a nil field is left alone.
type UpdateRepoParams struct {
	Description *string
	// Topics replaces the topics; an empty list clears them.
	Topics        *[]string
	Visibility    *domain.Visibility
	DefaultBranch *string
	// AllowedMergeModes replaces the allowed merge modes; it must hold at
	// least one known mode.
	AllowedMergeModes *[]domain.MergeMode
	// FailingChecksBlock replaces the setting of that name.
	FailingChecksBlock *bool
}

// TokenPage is a page of stored tokens, newest first.
type TokenPage struct {
	Tokens []domain.Token
	// NextPageToken is empty on the last page.
	NextPageToken string
}

// CreateTokenParams are the settings of a new token.
type CreateTokenParams struct {
	Label string
	// Scopes must not be empty.
	Scopes []domain.Scope
	// Repos limits the token to these repositories; empty means every one.
	Repos []domain.RepoName
	// BranchPrefixes limits the token to the branches under these prefixes
	// (see domain.ParseBranchPrefix); empty means every branch.
	BranchPrefixes []string
	// ExpiresAt is the zero time for a token that never expires.
	ExpiresAt time.Time
}

// CreatedToken is a new token with its secret, which is shown once and never
// stored.
type CreatedToken struct {
	Token  domain.Token
	Secret string
}

// ListRefsParams selects the refs to list.
type ListRefsParams struct {
	// Kinds restricts the result; empty means every kind.
	Kinds []domain.RefKind
	// PageSize is 0 for the default, and lowered to MaxRefPageSize.
	PageSize int
	// PageToken is the NextPageToken of the previous page, or empty.
	PageToken string
}

// RefPage is a page of refs.
type RefPage struct {
	Refs []domain.Ref
	// NextPageToken is empty on the last page.
	NextPageToken string
}

// ListCommitsParams selects a page of history.
type ListCommitsParams struct {
	// Revision is where the history starts; the zero value is HEAD. Ignored
	// when PageToken is set: the token pins the commit the first page started
	// from, so every page walks the same history even if a branch moves.
	Revision domain.Revision
	// Path limits the history to the commits that changed it. It must be the
	// same on every page of a listing.
	Path domain.TreePath
	// PageSize is 0 for the default, and lowered to MaxPageSize.
	PageSize int
	// PageToken is the NextPageToken of the previous page, or empty.
	PageToken string
}

// HistoryPage is a page of history, newest first.
type HistoryPage struct {
	Commits []domain.Commit
	// NextPageToken is empty on the last page.
	NextPageToken string
}

// RepoStats are the figures of a repository the home page shows.
type RepoStats struct {
	// Commits is the number of commits of the default branch; Recent of them
	// were committed in the last RecentDays days.
	Commits, Recent int
	// LatestTag is the most recently created tag; nil without tags.
	LatestTag *domain.TagInfo
	// Activity is the number of commits of each of the last ActivityDays
	// days, oldest first; the last day ends now. A day is 24 hours.
	Activity []int
	// Partial tells that the recent commits were more than MaxCommitTimes:
	// Recent and Activity count only some of them.
	Partial bool
}

// RecentDays is the window of RepoStats.Recent.
const RecentDays = 30

// ActivityDays is the span of RepoStats.Activity.
const ActivityDays = 90

// WatchParams select the events of a watch.
type WatchParams struct {
	// Topics are TopicHome or "repo:<name>"; empty means TopicHome.
	Topics []string
	// ResumeAfter is the id of the last event the caller saw; empty to
	// receive only new events.
	ResumeAfter string
	// Client identifies where the watch comes from, for the limit per client
	// (MaxWatchersPerClient): an opaque key such as a normalized address; the
	// web passes its visitor's. Empty only for callers with no client, which
	// count against MaxWatchers alone.
	Client string
	// Ready, when not nil, is called once the topics are valid and the watch
	// is registered: events published from then on are not missed. Nothing is
	// sent before it returns.
	Ready func()
}

// WatchEvent is one item of a watch.
type WatchEvent struct {
	// ID identifies the event, for resuming. Empty on a gap.
	ID string
	// Gap tells that events were lost: the caller must reload what it shows.
	// Event is nil then.
	Gap bool
	// Event is a domain.RepoCreated, RepoUpdated, RepoDeleted, PushEvent,
	// MergeRequestOpened, MergeRequestMerged, MergeRequestClosed,
	// MergeRequestReady, CommentAdded, RunUpdated or JobUpdated.
	Event domain.Event
	// Repo is the state of the repository at delivery, for RepoCreated and
	// RepoUpdated.
	Repo *RepoView
}

// CreateMergeRequestParams describes the merge request to open.
type CreateMergeRequestParams struct {
	Title       string
	Description string
	// Source is required; Target defaults to the default branch.
	Source, Target string
}

// ListMergeRequestsParams selects a page of merge requests.
type ListMergeRequestsParams struct {
	// State restricts the result; empty means every state.
	State domain.MRState
	// PageSize is 0 for the default, and lowered to MaxPageSize.
	PageSize int
	// PageToken is the NextPageToken of the previous page, or empty.
	PageToken string
}

// MergeRequestPage is a page of merge requests, highest number first.
type MergeRequestPage struct {
	MergeRequests []domain.MergeRequest
	// NextPageToken is empty on the last page.
	NextPageToken string
}

// ListIssuesParams selects a page of issues from an IssueStore.
type ListIssuesParams struct {
	// State restricts the result; empty means every state.
	State domain.IssueState
	// PageSize is the most issues returned; 0 or less means no limit. The
	// use case chooses it.
	PageSize int
	// PageToken is the NextPageToken of the previous page, or empty.
	PageToken string
}

// IssuePage is a page of issues, highest number first.
type IssuePage struct {
	Issues []domain.Issue
	// NextPageToken is empty on the last page.
	NextPageToken string
}

// MergeRequestDetail is a merge request with its review data.
type MergeRequestDetail struct {
	MergeRequest domain.MergeRequest
	// Commits are the commits of the head that the target lacks, newest
	// first, at most MaxMergeRequestCommits.
	Commits          []domain.Commit
	CommitsTruncated bool
	// DiffBaseID is the commit the diff compares from; zero when the source
	// branch is gone, then the diff is empty.
	DiffBaseID domain.ObjectID
	Stats      domain.DiffStats
	Diff       domain.Diff
	// Comments are the comments, oldest first.
	Comments []domain.Comment
	// LastReadID is the head recorded by MarkRead; zero when none.
	LastReadID domain.ObjectID
	// LastReadLost tells that since-last-read was asked for, a head was
	// recorded, but that commit is gone: the diff is against the target.
	LastReadLost bool
}

// GetMergeRequestOptions say which diff MergeRequestService.Get reads.
type GetMergeRequestOptions struct {
	// SinceLastRead compares the head recorded by MarkRead with the head (see Get).
	SinceLastRead bool
	// Files, when set, lists every changed file, without the file-count
	// limit, and reads the hunks of the files in the window only, each within
	// the per-file and the total limits of domain.DefaultDiffLimits. Unset,
	// the diff is bounded by domain.DefaultDiffLimits (the API contract).
	Files *FileWindow
	// Base and Head, when not zero, are the diff the caller shows: Get
	// returns domain.ErrConflict when the request's diff base or head differ
	// (the branch moved, a read was marked).
	Base, Head domain.ObjectID
	// DiffOnly skips the commits of the request (Commits stays empty).
	DiffOnly bool
}

// FileWindow is the range [From, To) of files whose hunks are read.
type FileWindow struct{ From, To int }

// AddCommentParams describes the comment to add.
type AddCommentParams struct {
	// Path, Line and Side designate a line of the diff; all empty for a
	// general comment.
	Path string
	Line int
	Side domain.DiffSide
	Text string
}

// MergeParams describes the merge to perform.
type MergeParams struct {
	// Mode defaults to fast-forward.
	Mode domain.MergeMode
	// HeadID is the commit the caller reviewed; the source branch must still
	// point to it.
	HeadID domain.ObjectID
	// DeleteSource deletes the source branch after the merge.
	DeleteSource bool
}

// MergeResult is a merged request and what happened to its source branch.
type MergeResult struct {
	domain.MergeRequest
	// SourceDeleted is true when DeleteSource was asked and the branch is
	// gone.
	SourceDeleted bool
}

// LoginLink is a login link as the owner receives it.
type LoginLink struct {
	// URL holds the secret: show it once, never log it.
	URL       string
	ExpiresAt time.Time
}
