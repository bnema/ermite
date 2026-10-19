// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// recordActivity is best-effort: a metadata failure never fails the action.
func recordActivity(ctx context.Context, meta ports.RepoMetaStore, name domain.RepoName, at time.Time) {
	if _, err := touchActiveAt(ctx, meta, name, at); err != nil {
		log := zerowrap.FromCtx(ctx)
		log.Error().Err(err).Str("repo", name.String()).Msg("record repository activity")
	}
}

// touchActiveAt preserves later activity and retries concurrent metadata writes.
// A repository deleted meanwhile is not an error.
func touchActiveAt(ctx context.Context, meta ports.RepoMetaStore, name domain.RepoName, now time.Time) (domain.Repo, error) {
	repo, err := meta.Mutate(ctx, name, func(repo *domain.Repo) (bool, error) {
		changed := false
		if now.After(repo.ActiveAt) {
			repo.ActiveAt = now
			changed = true
		}
		return changed, nil
	})
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Repo{}, nil
	}
	if err != nil {
		return repo, fmt.Errorf("update repository %q: %w", name, err)
	}
	return repo, nil
}
