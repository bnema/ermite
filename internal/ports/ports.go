// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ports declares every interface that crosses a layer, and the
// parameter and result types of the use case interfaces.
//
// Outbound ports (ports.go) are what the use cases need from the outside
// world: adapters implement them. Inbound ports (services.go) are the use
// cases as the adapters that serve requests see them: the types of package usecase
// implement them. Adapters and use cases depend on this package and on domain,
// never on each other; internal/app wires the implementations. All the mocks are generated in
// ports/mocks.
package ports

import (
	"context"
	"fmt"
	"io"
	"time"

	"bnema.dev/ermite/internal/domain"
)

//go:generate go tool mockery

// RepoMetaStore keeps repository metadata. It is the source of truth for
// everything ermite owns about a repository except its Git data.
type RepoMetaStore interface {
	// Create stores repo. It returns domain.ErrAlreadyExists if a repository
	// with that name exists, including when a concurrent Create wins.
	Create(ctx context.Context, repo domain.Repo) error
	// Get returns the repository, or domain.ErrNotFound.
	Get(ctx context.Context, name domain.RepoName) (domain.Repo, error)
	// List returns every repository ordered by name.
	List(ctx context.Context) ([]domain.Repo, error)
	// Update replaces the stored repository. It returns domain.ErrNotFound if
	// there is none and domain.ErrConflict if another write changed the
	// stored repository since it was read.
	Update(ctx context.Context, repo domain.Repo) error
	// Delete removes the repository and everything keyed to it: its merge
	// requests, their comments and read positions, and its number sequence.
	// A repository created again under the same name starts empty, with its
	// numbers starting at 1. Deleting a missing one is not an error.
	Delete(ctx context.Context, name domain.RepoName) error
}

// StagedID identifies a repository being created. It is opaque to callers.
type StagedID string

// RepoDirs is the result of listing the repository directories.
type RepoDirs struct {
	// Names are the directories that hold a valid repository name.
	Names []domain.RepoName
	// Invalid are the entries that cannot be a repository directory.
	Invalid []string
}

// GitRepos manages bare repository directories.
type GitRepos interface {
	// Stage creates an empty bare repository outside the public directory
	// and returns its identifier.
	Stage(ctx context.Context) (StagedID, error)
	// Publish moves a staged repository to its final place under name. It
	// returns domain.ErrAlreadyExists if a directory for name exists; the
	// staged repository is left in place for Discard.
	Publish(ctx context.Context, id StagedID, name domain.RepoName) error
	// Discard removes a staged repository. A missing one is not an error.
	Discard(ctx context.Context, id StagedID) error
	// PurgeStaging removes every staged repository left by a previous run.
	PurgeStaging(ctx context.Context) error
	// List returns the repository directories.
	List(ctx context.Context) (RepoDirs, error)
	// Trash moves the repository directory to the trash, where it can be
	// recovered by hand. It returns domain.ErrNotFound if there is none.
	Trash(ctx context.Context, name domain.RepoName) error
	// DefaultBranch returns the branch HEAD points to (without refs/heads/)
	// and whether that branch exists. It returns domain.ErrNotFound if there
	// is no such repository.
	DefaultBranch(ctx context.Context, name domain.RepoName) (branch string, exists bool, err error)
	// HasBranch reports whether refs/heads/<branch> exists. A name that
	// cannot be a branch does not exist. It returns domain.ErrNotFound if
	// there is no such repository.
	HasBranch(ctx context.Context, name domain.RepoName, branch string) (bool, error)
	// HeadBranch returns the short name of the branch HEAD points to, whether
	// or not that branch exists, and "" when HEAD is detached. It reads no
	// refs, so it is cheap enough for a listing. It returns
	// domain.ErrNotFound if there is no such repository.
	HeadBranch(ctx context.Context, name domain.RepoName) (string, error)
	// SetDefaultBranch points HEAD to the branch, which need not exist yet.
	SetDefaultBranch(ctx context.Context, name domain.RepoName, branch string) error
	// Maintain refreshes the commit-graph of the repository.
	Maintain(ctx context.Context, name domain.RepoName) error
}

// RepoFreshener is implemented by a RepoReader that keeps repositories open
// between reads. The use cases that write to a repository (a push, the
// maintenance that follows it) call Refresh so that the reader picks the change
// up at once instead of when its own timer says so. It is optional: reads
// stay correct without it.
type RepoFreshener interface {
	// Refresh tells the reader that the repository changed on disk. It does
	// not fail: a reader that cannot refresh finds out on its next read.
	Refresh(ctx context.Context, name domain.RepoName)
}

// CommitQuery selects a page of history.
type CommitQuery struct {
	// Revision is where the history starts; the zero value is HEAD.
	Revision domain.Revision
	// Path restricts the history to the commits that changed this file or
	// directory; the root means the whole history.
	Path domain.TreePath
	// Skip is the number of commits of the listing to leave out. A caller that
	// pages passes the commit it got in CommitPage.Head as Revision and the
	// number of commits already read, so that every page walks the same
	// history even when the revision moves.
	Skip int
	// Limit is the number of commits wanted, 1 to MaxCommitLimit.
	Limit int
}

// MaxCommitLimit is the most commits one CommitQuery may ask for.
const MaxCommitLimit = 1000

// Validate checks the page size and the offset of the query. It returns an
// error matching domain.ErrInvalidPageSize.
func (q CommitQuery) Validate() error {
	if q.Limit < 1 || q.Limit > MaxCommitLimit || q.Skip < 0 {
		return fmt.Errorf("list commits: %w: limit %d, skip %d", domain.ErrInvalidPageSize, q.Limit, q.Skip)
	}
	return nil
}

// CommitPage is a page of history, newest first.
type CommitPage struct {
	// Head is the commit the revision resolved to.
	Head domain.ObjectID
	// Commits has at most Limit commits.
	Commits []domain.Commit
	// HasMore tells that commits follow the last one.
	HasMore bool
}

// RepoReader reads the Git data of repositories: refs, trees, blobs, history
// and diffs. A repository that has no directory gives domain.ErrNotFound; so
// do an unknown revision, an empty repository (which has no commit) and a
// path that does not exist at the revision.
type RepoReader interface {
	// ListRefs returns HEAD, then the branches, then the tags, the last two
	// ordered by name. HEAD is always listed: when its branch does not exist
	// yet its ShortName and ids are empty.
	ListRefs(ctx context.Context, repo domain.RepoName) ([]domain.Ref, error)
	// ResolveRevision turns a revision into the commit it designates. A name
	// that is both a branch and a tag means the branch.
	ResolveRevision(ctx context.Context, repo domain.RepoName, rev domain.Revision) (domain.ResolvedRevision, error)
	// ResolveRefPath splits a path such as "feature/x/dir/file" into the
	// longest leading part that names a branch or a tag and the rest. When no
	// ref matches, the first segment may be a commit id (full, or abbreviated
	// to MinAbbrevLen or more). It returns domain.ErrNotFound if nothing
	// matches.
	ResolveRefPath(ctx context.Context, repo domain.RepoName, path string) (domain.RefPath, error)
	// GetTree lists the directory at path, at most domain.MaxTreeEntries
	// entries (Tree.Truncated tells). With includeLastCommit, the first
	// domain.MaxLastCommitEntries entries get their last commit. It returns
	// domain.ErrNotDirectory if path names something else.
	GetTree(ctx context.Context, repo domain.RepoName, rev domain.Revision, path domain.TreePath, includeLastCommit bool) (domain.Tree, error)
	// ListPaths lists the file and symlink paths of rev recursively in tree
	// order, at most maxPaths paths and maxBytes bytes of paths (PathList.Truncated
	// tells), skipping submodules and paths that are not addressable
	// (domain.IsAddressablePath).
	ListPaths(ctx context.Context, repo domain.RepoName, rev domain.Revision, maxPaths, maxBytes int) (domain.PathList, error)
	// OpenBlob returns the description of the file at path and a reader of its
	// content, which streams without holding the whole file in memory. The
	// caller must close the reader. It returns domain.ErrNotFile if path names
	// something else.
	OpenBlob(ctx context.Context, repo domain.RepoName, rev domain.Revision, path domain.TreePath) (domain.BlobInfo, io.ReadCloser, error)
	// ListCommits returns a page of history.
	ListCommits(ctx context.Context, repo domain.RepoName, q CommitQuery) (CommitPage, error)
	// GetCommit returns a commit with its statistics and its diff against the
	// first parent, bounded by limits.
	GetCommit(ctx context.Context, repo domain.RepoName, rev domain.Revision, limits domain.DiffLimits) (domain.CommitDetail, error)
}

// HTTPBackend tells how to run `git http-backend` for one request.
type HTTPBackend struct {
	// Path is the git-http-backend executable.
	Path string
	// Dir is the working directory of the process.
	Dir string
	// Env is the complete environment of the process: the isolated Git
	// environment, GIT_PROJECT_ROOT and the Git configuration of the request
	// (GIT_CONFIG_COUNT/KEY/VALUE). The caller may append to it.
	Env []string
}

// GitBackend provides the Git side of smart HTTP.
type GitBackend interface {
	// HTTPBackend returns how to run git http-backend. For a push it also
	// enables http.receivepack and sets REMOTE_USER; a read request cannot
	// reach receive-pack.
	HTTPBackend(ctx context.Context, push bool) (HTTPBackend, error)
}

// GitArchive writes the archive of a commit.
type GitArchive interface {
	// Archive writes the tree of the commit as a gzip-compressed tar to w, all
	// the paths below the directory prefix. It writes nothing and returns
	// domain.ErrNotFound when the repository or the commit does not exist.
	Archive(ctx context.Context, repo domain.RepoName, commit domain.ObjectID, prefix string, w io.Writer) error
}

// TokenStore keeps the stored tokens and the index from secret hash to token.
// The secret itself is never stored.
type TokenStore interface {
	// Create stores the token and its hash index in one write. It returns
	// domain.ErrAlreadyExists if the id or the hash is taken.
	Create(ctx context.Context, token domain.Token) error
	// Get returns the token with that id, or domain.ErrNotFound.
	Get(ctx context.Context, id string) (domain.Token, error)
	// FindByHash returns the token whose secret has that hash, or
	// domain.ErrNotFound.
	FindByHash(ctx context.Context, hash domain.TokenHash) (domain.Token, error)
	// List returns every token, oldest first.
	List(ctx context.Context) ([]domain.Token, error)
	// Revoke removes the token and its hash index in one write, or returns
	// domain.ErrNotFound.
	Revoke(ctx context.Context, id string) error
	// Touch records that the token was used at the given time. It returns
	// domain.ErrNotFound if the token is gone.
	Touch(ctx context.Context, id string, at time.Time) error
}

// MergeRequestStore keeps the merge requests of the repositories, their
// comments and the read position of the reviewer. A repository has no entry
// until its first merge request. Create, SaveComment and SetRead return
// domain.ErrNotFound when the repository does not exist, so that deleting a
// repository leaves nothing for one created again under its name.
type MergeRequestStore interface {
	// Create stores the merge request under the next number of its
	// repository, which it sets, and returns it. Numbers start at 1 and are
	// never reused, even by a concurrent Create.
	Create(ctx context.Context, mr domain.MergeRequest) (domain.MergeRequest, error)
	// Get returns the merge request, or domain.ErrNotFound. Its Version
	// tells Update which state it was read at.
	Get(ctx context.Context, repo domain.RepoName, number int64) (domain.MergeRequest, error)
	// List returns every merge request of the repository, highest number
	// first.
	List(ctx context.Context, repo domain.RepoName) ([]domain.MergeRequest, error)
	// ListOpen returns the open merge requests of the repository, highest
	// number first, reading only the open ones (through an index).
	ListOpen(ctx context.Context, repo domain.RepoName) ([]domain.MergeRequest, error)
	// OpenBySource returns the numbers of the open merge requests whose source
	// branch is one of branches (short names), from an index of keys: a push
	// looks up the branches it moved, whatever the number of merge requests the
	// repository ever had. No request is read. It stops after limit numbers (a
	// limit of 0 or less means none) and reports truncated when there are more,
	// without counting them; the context is checked between keys, and a
	// cancelled one ends the lookup with its error and what was found so far.
	OpenBySource(ctx context.Context, repo domain.RepoName, branches []string, limit int) (numbers []int64, truncated bool, err error)
	// OpenByTarget is OpenBySource for the open merge requests whose target
	// branch is one of branches.
	OpenByTarget(ctx context.Context, repo domain.RepoName, branches []string, limit int) (numbers []int64, truncated bool, err error)
	// Update replaces the stored merge request. It returns domain.ErrNotFound
	// if there is none and domain.ErrConflict if another write changed it
	// since mr was read.
	Update(ctx context.Context, mr domain.MergeRequest) error
	// ListComments returns the comments of the merge request, oldest first.
	ListComments(ctx context.Context, repo domain.RepoName, number int64) ([]domain.Comment, error)
	// GetComment returns one comment, or domain.ErrNotFound.
	GetComment(ctx context.Context, repo domain.RepoName, number int64, id int64) (domain.Comment, error)
	// SaveComment writes a comment and the merge request it belongs to in one
	// batch: a new comment (Version 0) must not exist yet, an existing one
	// must be unchanged since it was read, and so must mr. It returns
	// domain.ErrConflict otherwise. The caller sets mr.CommentSeq and the
	// comment's ID for a new comment, and mr.UpdatedAt.
	SaveComment(ctx context.Context, mr domain.MergeRequest, c domain.Comment) error
	// GetRead returns the head the reviewer last marked read, or the zero
	// ObjectID when there is none.
	GetRead(ctx context.Context, repo domain.RepoName, number int64) (domain.ObjectID, error)
	// SetRead records the head the reviewer has read. It returns
	// domain.ErrNotFound when the repository does not exist, and
	// domain.ErrConflict when concurrent writes to the repository keep it
	// from landing.
	SetRead(ctx context.Context, repo domain.RepoName, number int64, head domain.ObjectID) error
}

// MergeGit is what merge requests need from the Git repositories beyond
// RepoReader. Every method returns domain.ErrNotFound when the repository has
// no directory.
type MergeGit interface {
	// BranchHead returns the commit refs/heads/<branch> points to, or
	// domain.ErrNotFound when there is no such branch.
	BranchHead(ctx context.Context, repo domain.RepoName, branch string) (domain.ObjectID, error)
	// HasCommit reports whether the repository holds that commit.
	HasCommit(ctx context.Context, repo domain.RepoName, id domain.ObjectID) (bool, error)
	// MergeBase returns the best common ancestor of two commits, or
	// domain.ErrNotFound when they share no history.
	MergeBase(ctx context.Context, repo domain.RepoName, a, b domain.ObjectID) (domain.ObjectID, error)
	// IsAncestor reports whether ancestor is reachable from descendant, a
	// commit being its own ancestor.
	IsAncestor(ctx context.Context, repo domain.RepoName, ancestor, descendant domain.ObjectID) (bool, error)
	// ListRange returns the commits reachable from head and not from exclude
	// ("exclude..head"), newest first, at most limit; more is true when the
	// range holds more. A zero exclude means the whole history of head.
	ListRange(ctx context.Context, repo domain.RepoName, exclude, head domain.ObjectID, limit int) (commits []domain.Commit, more bool, err error)
	// DiffCommits returns the statistics and the diff from commit base to
	// commit head (renames detected), bounded by limits.
	DiffCommits(ctx context.Context, repo domain.RepoName, base, head domain.ObjectID, limits domain.DiffLimits) (domain.DiffStats, domain.Diff, error)
	// CommitTree returns the tree of a commit.
	CommitTree(ctx context.Context, repo domain.RepoName, id domain.ObjectID) (domain.ObjectID, error)
	// WriteCommit stores a commit object, given as the text of the object,
	// and returns its id.
	WriteCommit(ctx context.Context, repo domain.RepoName, object []byte) (domain.ObjectID, error)
	// UpdateBranch moves refs/heads/<branch> from old to new. It returns
	// domain.ErrConflict when the branch no longer points to old.
	UpdateBranch(ctx context.Context, repo domain.RepoName, branch string, old, new domain.ObjectID) error
	// DeleteBranch deletes refs/heads/<branch> if it still points to old. It
	// returns domain.ErrConflict when the branch moved or is gone.
	DeleteBranch(ctx context.Context, repo domain.RepoName, branch string, old domain.ObjectID) error
}

// CommitSigner signs the commits ermite creates (ADR 0022). The private key
// never leaves it.
type CommitSigner interface {
	// PublicKey returns the public key and its fingerprint.
	PublicKey() domain.SigningPublicKey
	// Sign signs the text of a commit object and returns the armored SSH
	// signature (namespace "git") to put in its gpgsig header.
	Sign(payload []byte) (string, error)
}

// SessionStore keeps what the browser sessions of the owner (ADR 0018) need
// to survive a restart: the key that signs them and the epoch that voids them.
type SessionStore interface {
	// Key returns the 32-byte secret that signs the session cookies. The first
	// call on an empty store generates and stores it; a restart keeps it.
	Key(ctx context.Context) ([]byte, error)
	// Epoch returns the current epoch, 0 until the first BumpEpoch. A session
	// is valid only while its epoch is the current one.
	Epoch(ctx context.Context) (uint64, error)
	// BumpEpoch increments the epoch, which ends every session, and returns
	// the new value.
	BumpEpoch(ctx context.Context) (uint64, error)
}

// PasskeyStore keeps the owner's passkeys (ADR 0026) and the WebAuthn user
// handle they belong to.
type PasskeyStore interface {
	// UserHandle returns the 32-byte WebAuthn user handle of the owner. The
	// first call on an empty store generates and stores it; it never changes.
	UserHandle(ctx context.Context) ([]byte, error)
	// List returns every passkey, oldest first.
	List(ctx context.Context) ([]domain.Passkey, error)
	// Get returns the passkey with that credential id, or domain.ErrNotFound.
	Get(ctx context.Context, id []byte) (domain.Passkey, error)
	// Create stores the passkey. It returns domain.ErrAlreadyExists if the
	// credential id is taken. It does not bound the number of passkeys: the
	// use case checks domain.MaxPasskeys.
	Create(ctx context.Context, p domain.Passkey) error
	// Update replaces the stored passkey (sign count, backup state, last
	// use), or returns domain.ErrNotFound.
	Update(ctx context.Context, p domain.Passkey) error
	// Delete removes the passkey, or returns domain.ErrNotFound.
	Delete(ctx context.Context, id []byte) error
}

// PasskeyVerifier runs the WebAuthn ceremonies (ADR 0026). It hides the
// WebAuthn library: the ceremony data it hands out is opaque to the caller,
// which keeps it between the begin and the finish of a ceremony and passes it
// back untouched. Every verification failure is domain.ErrUnauthenticated,
// wrapped with a reason meant for logs, not for callers.
type PasskeyVerifier interface {
	// BeginRegistration starts the registration of a passkey for the owner.
	// options is the JSON of the credential creation options for the browser
	// ({"publicKey": {...}}); existing passkeys are excluded.
	BeginRegistration(userHandle []byte, existing []domain.Passkey) (options []byte, ceremony []byte, err error)
	// FinishRegistration checks the browser's response (the JSON of
	// PublicKeyCredential.toJSON()) and returns the new passkey, without a name
	// or creation time.
	FinishRegistration(userHandle []byte, ceremony, response []byte) (domain.Passkey, error)
	// BeginLogin starts a discoverable login. options is the JSON of the
	// credential request options for the browser ({"publicKey": {...}}).
	BeginLogin() (options []byte, ceremony []byte, err error)
	// FinishLogin checks the browser's response and returns the stored passkey
	// updated with the sign count and backup state the response reports. lookup
	// returns domain.ErrNotFound for an unknown credential id. A counter that
	// does not advance is not an error here: the returned SignCount is then the
	// stored one, and the caller, which holds the lock that orders the logins,
	// refuses it.
	FinishLogin(userHandle []byte, ceremony, response []byte, lookup func(id []byte) (domain.Passkey, error)) (domain.Passkey, error)
}

// EventBus fans events out to in-process subscribers.
type EventBus interface {
	// Publish hands the event to every subscriber without blocking. A
	// subscriber that cannot keep up loses events and receives a
	// domain.EventGap.
	Publish(ctx context.Context, ev domain.Event)
	// Subscribe returns a channel of events published from now on. The
	// channel is closed when ctx ends.
	Subscribe(ctx context.Context) <-chan domain.Event
}

// Clock tells the time.
type Clock interface {
	Now() time.Time
}
