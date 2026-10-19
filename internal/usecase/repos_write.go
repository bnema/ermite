// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
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

// View adds the default branch to a repository the caller has already been
// allowed to see. A repository without a directory has no default branch.
func (r *Repos) View(ctx context.Context, repo domain.Repo) (ports.RepoView, error) {
	branch, err := r.git.HeadBranch(ctx, repo.Name)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return ports.RepoView{}, fmt.Errorf("read default branch of %q: %w", repo.Name, err)
	}
	return ports.RepoView{Repo: repo, DefaultBranch: branch}, nil
}

// Update changes the description, the topics, the visibility, the default branch and the
// merge settings of a repository, and publishes RepoUpdated. The principal needs repo:admin on it
// (see adminAccess for what the others learn) and no branch prefixes. At least one field must be set,
// else domain.ErrInvalid. A default branch that does not exist gives
// domain.ErrPreconditionFailed; nothing is written before every check passed.
// The metadata is written first, then HEAD, and RepoUpdated is published after
// both. When only the HEAD move fails, RepoUpdated is still published for the
// stored metadata and the HEAD error is returned.
func (r *Repos) Update(ctx context.Context, p domain.Principal, name domain.RepoName, params ports.UpdateRepoParams) (domain.Repo, error) {
	repo, err := lookupRepo(ctx, r.meta, name)
	if err != nil {
		return domain.Repo{}, err
	}
	if err := adminAccess(p, repo); err != nil {
		return domain.Repo{}, err
	}
	before := repo
	if params.Description == nil && params.Topics == nil && params.Visibility == nil && params.DefaultBranch == nil &&
		params.AllowedMergeModes == nil && params.FailingChecksBlock == nil && params.Issues == nil {
		return domain.Repo{}, fmt.Errorf("%w: nothing to update", domain.ErrInvalid)
	}
	if params.AllowedMergeModes != nil {
		modes, err := domain.NormalizeMergeModes(*params.AllowedMergeModes)
		if err != nil {
			return domain.Repo{}, err
		}
		params.AllowedMergeModes = &modes
	}
	if params.Issues != nil {
		mode, err := domain.ParseIssuesMode(params.Issues.String())
		if err != nil {
			return domain.Repo{}, err
		}
		if mode == domain.IssuesPublic {
			return domain.Repo{}, fmt.Errorf("%w: public issues are not available yet", domain.ErrInvalid)
		}
		params.Issues = &mode
	}
	if params.Description != nil {
		if err := validateDescription(*params.Description); err != nil {
			return domain.Repo{}, err
		}
	}
	if params.Topics != nil {
		topics, err := domain.NormalizeTopics(*params.Topics)
		if err != nil {
			return domain.Repo{}, err
		}
		params.Topics = &topics
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
	// HEAD fails after the metadata was stored, the stored change is still
	// published as RepoUpdated (a repository made private must be withdrawn
	// from the watchers) and the call then fails with the HEAD error. The
	// branch was checked above, so the remaining causes are a vanished
	// repository or an I/O failure.
	metaDone := false
	if params.Description != nil || params.Topics != nil || params.Visibility != nil || params.AllowedMergeModes != nil || params.FailingChecksBlock != nil || params.Issues != nil {
		if repo, err = r.updateMeta(ctx, name, params); err != nil {
			return domain.Repo{}, err
		}
		metaDone = true
	}
	var headErr error
	if params.DefaultBranch != nil {
		if err := r.git.SetDefaultBranch(ctx, name, *params.DefaultBranch); err != nil {
			headErr = fmt.Errorf("set default branch of %q: %w", name, err)
			if !metaDone {
				return domain.Repo{}, headErr
			}
		}
	}

	log := zerowrap.FromCtx(ctx)
	log.Info().
		Str("repo", name.String()).
		Bool("description", params.Description != nil).
		Bool("topics", params.Topics != nil).
		Bool("visibility", params.Visibility != nil).
		Bool("default_branch", params.DefaultBranch != nil).
		Bool("settings", params.AllowedMergeModes != nil || params.FailingChecksBlock != nil || params.Issues != nil).
		Str("principal", p.Label()).Str("token_id", p.TokenID()).
		Msg("repository updated")
	r.bus.Publish(ctx, domain.RepoUpdated{
		Repo: name, At: r.clock.Now().UTC(),
		Visibility: repo.Visibility, Previous: before.Visibility, CreatedAt: repo.CreatedAt,
	})
	if headErr != nil {
		return domain.Repo{}, headErr
	}
	return repo, nil
}

// updateMeta applies the metadata fields, retrying when a write (a push
// recording pushed_at) gets in between.
func (r *Repos) updateMeta(ctx context.Context, name domain.RepoName, params ports.UpdateRepoParams) (domain.Repo, error) {
	for range pushRetries {
		repo, err := r.meta.Get(ctx, name)
		if err != nil {
			return domain.Repo{}, err
		}
		if params.Description != nil {
			repo.Description = *params.Description
		}
		if params.Topics != nil {
			repo.Topics = *params.Topics
		}
		if params.Visibility != nil {
			repo.Visibility = *params.Visibility
		}
		if repo.Settings.IsZero() {
			repo.Settings = domain.DefaultRepoSettings()
		}
		if params.AllowedMergeModes != nil {
			repo.Settings.AllowedMergeModes = *params.AllowedMergeModes
		}
		if params.FailingChecksBlock != nil {
			repo.Settings.FailingChecksBlock = *params.FailingChecksBlock
		}
		if params.Issues != nil {
			repo.Settings.Issues = *params.Issues
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
// adminAccess) and no branch prefixes. A repository whose directory is already gone, left by a
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
	var runIDs []int64
	if r.ci != nil {
		// Listed before the records go: the logs are found by run id.
		if ids, err := r.ci.RunIDs(ctx, name); err == nil {
			runIDs = ids
		} else {
			log.Warn().Err(err).Str("repo", name.String()).Msg("list the CI runs of the repository; their logs are left to the sweep")
		}
	}
	if err := r.meta.Delete(ctx, name); err != nil {
		return fmt.Errorf("delete metadata of %q: %w", name, err)
	}
	if r.ci != nil {
		r.ci.ForgetRepo(name)
	}
	if len(runIDs) > 0 {
		if err := r.ci.RemoveLogs(context.WithoutCancel(ctx), runIDs); err != nil {
			log.Warn().Err(err).Str("repo", name.String()).Msg("remove the CI logs of the repository; the sweep will")
		}
	}
	log.Info().Str("repo", name.String()).Str("principal", p.Label()).Str("token_id", p.TokenID()).Msg("repository deleted")
	r.bus.Publish(ctx, domain.RepoDeleted{Repo: name, Visibility: repo.Visibility, At: r.clock.Now().UTC(), CreatedAt: repo.CreatedAt})
	return nil
}
