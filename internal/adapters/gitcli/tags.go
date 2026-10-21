// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli

import (
	"context"
	"errors"
	"fmt"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"github.com/bnema/zerowrap"
)

var _ ports.ReleaseTagWriter = (*Git)(nil)

// CreateTag creates a lightweight tag without ever moving an existing ref.
func (g *Git) CreateTag(ctx context.Context, repo domain.RepoName, tag string, target domain.ObjectID) error {
	if _, err := domain.ParseReleaseTag(tag); err != nil {
		return err
	}
	dir, err := g.repoDir(repo)
	if err != nil {
		return err
	}
	ref := "refs/tags/" + tag
	_, err = g.run(ctx, "--git-dir="+dir, "update-ref", ref, target.String(), domain.ZeroOID)
	if err != nil {
		// Distinguish a concurrent creator from an unrelated Git failure. ResolveRevision
		// peels tags, so use show-ref to also recognize tags pointing at non-commits.
		if _, existsErr := g.run(ctx, "--git-dir="+dir, "show-ref", "--verify", "--", ref); existsErr == nil {
			return fmt.Errorf("tag %q in %q: %w", tag, repo, domain.ErrAlreadyExists)
		}
		if errors.Is(err, context.Canceled) {
			return err
		}
		return fmt.Errorf("create tag %q in %q: %w", tag, repo, err)
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("repo", repo.String()).Str("tag", tag).Msg("release tag created")
	return nil
}
