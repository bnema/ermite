// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
)

// MaxDescriptionLen is the longest repository description, in bytes.
const MaxDescriptionLen = 500

// validateDescription accepts a one-line description of valid UTF-8.
func validateDescription(s string) error {
	if len(s) > MaxDescriptionLen {
		return fmt.Errorf("%w: description is longer than %d bytes", domain.ErrInvalid, MaxDescriptionLen)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("%w: description is not valid UTF-8", domain.ErrInvalid)
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return fmt.Errorf("%w: description must be one line without control characters", domain.ErrInvalid)
		}
	}
	return nil
}

// RepoView is a repository with the Git state that belongs in its summary.
type RepoView struct {
	domain.Repo
	// DefaultBranch is the branch HEAD points to, even when it does not exist
	// yet; empty when HEAD is detached or the directory is missing.
	DefaultBranch string
}

// View adds the default branch to a repository the caller has already been
// allowed to see. A repository without a directory has no default branch.
func (r *Repos) View(ctx context.Context, repo domain.Repo) (RepoView, error) {
	branch, err := r.git.HeadBranch(ctx, repo.Name)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return RepoView{}, fmt.Errorf("read default branch of %q: %w", repo.Name, err)
	}
	return RepoView{Repo: repo, DefaultBranch: branch}, nil
}

// UpdateParams are the changes of Update; a nil field is left alone.
type UpdateParams struct {
	Description   *string
	Visibility    *domain.Visibility
	DefaultBranch *string
}

// Update changes the description, the visibility and the default branch of a
// repository, and publishes RepoUpdated. The principal needs repo:admin on it
// (see adminAccess for what the others learn). At least one field must be set,
// else domain.ErrInvalid. A default branch that does not exist gives
// domain.ErrPreconditionFailed; nothing is written before every check passed.
// The metadata is written first, then HEAD, and RepoUpdated is published after
// both (see the comment on the writes for the partial state).
func (r *Repos) Update(ctx context.Context, p domain.Principal, name domain.RepoName, params UpdateParams) (domain.Repo, error) {
	repo, err := lookupRepo(ctx, r.meta, name)
	if err != nil {
		return domain.Repo{}, err
	}
	if err := adminAccess(p, repo); err != nil {
		return domain.Repo{}, err
	}
	before := repo
	if params.Description == nil && params.Visibility == nil && params.DefaultBranch == nil {
		return domain.Repo{}, fmt.Errorf("%w: nothing to update", domain.ErrInvalid)
	}
	if params.Description != nil {
		if err := validateDescription(*params.Description); err != nil {
			return domain.Repo{}, err
		}
	}
	if params.Visibility != nil {
		if _, err := domain.ParseVisibility(params.Visibility.String()); err != nil {
			return domain.Repo{}, err
		}
	}
	if params.DefaultBranch != nil {
		if *params.DefaultBranch == "" {
			return domain.Repo{}, fmt.Errorf("%w: default branch is empty", domain.ErrInvalid)
		}
		ok, err := r.git.HasBranch(ctx, name, *params.DefaultBranch)
		if err != nil {
			return domain.Repo{}, fmt.Errorf("check branch %q of %q: %w", *params.DefaultBranch, name, err)
		}
		if !ok {
			return domain.Repo{}, fmt.Errorf("%w: branch %q does not exist", domain.ErrPreconditionFailed, *params.DefaultBranch)
		}
	}

	// Metadata first, then HEAD. The two writes are not atomic: when moving
	// HEAD fails after the metadata was stored, the call fails with the
	// description and visibility already changed, and no RepoUpdated is
	// published for that partial state. The branch was checked above, so the
	// remaining causes are a vanished repository or an I/O failure.
	if params.Description != nil || params.Visibility != nil {
		if repo, err = r.updateMeta(ctx, name, params); err != nil {
			return domain.Repo{}, err
		}
	}
	if params.DefaultBranch != nil {
		if err := r.git.SetDefaultBranch(ctx, name, *params.DefaultBranch); err != nil {
			return domain.Repo{}, fmt.Errorf("set default branch of %q: %w", name, err)
		}
	}

	log := zerowrap.FromCtx(ctx)
	log.Info().
		Str("repo", name.String()).
		Bool("description", params.Description != nil).
		Bool("visibility", params.Visibility != nil).
		Bool("default_branch", params.DefaultBranch != nil).
		Str("principal", p.Label()).Str("token_id", p.TokenID()).
		Msg("repository updated")
	r.bus.Publish(ctx, domain.RepoUpdated{
		Repo: name, At: r.clock.Now().UTC(),
		Visibility: repo.Visibility, Previous: before.Visibility, CreatedAt: repo.CreatedAt,
	})
	return repo, nil
}

// updateMeta applies the metadata fields, retrying when a write (a push
// recording pushed_at) gets in between.
func (r *Repos) updateMeta(ctx context.Context, name domain.RepoName, params UpdateParams) (domain.Repo, error) {
	for range pushRetries {
		repo, err := r.meta.Get(ctx, name)
		if err != nil {
			return domain.Repo{}, err
		}
		if params.Description != nil {
			repo.Description = *params.Description
		}
		if params.Visibility != nil {
			repo.Visibility = *params.Visibility
		}
		err = r.meta.Update(ctx, repo)
		if errors.Is(err, domain.ErrConflict) {
			continue
		}
		if err != nil {
			return domain.Repo{}, fmt.Errorf("update repository %q: %w", name, err)
		}
		return repo, nil
	}
	return domain.Repo{}, fmt.Errorf("update repository %q: %w", name, domain.ErrConflict)
}

// Delete moves the repository directory to the trash, removes its metadata
// and publishes RepoDeleted. The principal needs repo:admin on it (see
// adminAccess). A repository whose directory is already gone, left by a
// delete that failed half way, loses its metadata.
func (r *Repos) Delete(ctx context.Context, p domain.Principal, name domain.RepoName) error {
	repo, err := lookupRepo(ctx, r.meta, name)
	if err != nil {
		return err
	}
	if err := adminAccess(p, repo); err != nil {
		return err
	}
	log := zerowrap.FromCtx(ctx)
	switch err := r.git.Trash(ctx, name); {
	case errors.Is(err, domain.ErrNotFound):
		log.Warn().Str("repo", name.String()).Msg("repository directory already gone, removing its metadata")
	case err != nil:
		return fmt.Errorf("trash repository %q: %w", name, err)
	}
	if err := r.meta.Delete(ctx, name); err != nil {
		return fmt.Errorf("delete metadata of %q: %w", name, err)
	}
	log.Info().Str("repo", name.String()).Str("principal", p.Label()).Str("token_id", p.TokenID()).Msg("repository deleted")
	r.bus.Publish(ctx, domain.RepoDeleted{Repo: name, Visibility: repo.Visibility, At: r.clock.Now().UTC(), CreatedAt: repo.CreatedAt})
	return nil
}
