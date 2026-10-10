// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ports declares the interfaces the use cases need from the outside
// world. Adapters implement them; cmd wires them.
package ports

import (
	"context"
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
	// SetDefaultBranch points HEAD to the branch, which need not exist yet.
	SetDefaultBranch(ctx context.Context, name domain.RepoName, branch string) error
	// Maintain refreshes the commit-graph of the repository.
	Maintain(ctx context.Context, name domain.RepoName) error
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
