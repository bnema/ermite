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
	// Update replaces the stored repository, or returns domain.ErrNotFound.
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
}

// Clock tells the time.
type Clock interface {
	Now() time.Time
}
