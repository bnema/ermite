// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ports declares the interfaces the use cases need from the outside
// world. Adapters implement them; cmd wires them.
package ports

import (
	"context"
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
	// Delete removes the repository. Deleting a missing one is not an error.
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
	// GetTree lists the directory at path. With includeLastCommit, the first
	// domain.MaxLastCommitEntries entries get their last commit. It returns
	// domain.ErrNotDirectory if path names something else.
	GetTree(ctx context.Context, repo domain.RepoName, rev domain.Revision, path domain.TreePath, includeLastCommit bool) (domain.Tree, error)
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
