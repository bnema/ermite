// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"

	"bnema.dev/ermite/internal/domain"
)

// UnderRepoLock holds Bernard's per-repository lock while fn runs.
func (b *Bernard) UnderRepoLock(repo domain.RepoName, fn func()) {
	defer b.repoLock(repo)()
	fn()
}

// SetHoldForTest sets the hold release of b.
func (b *Bernard) SetHoldForTest(h interface {
	ReleaseHold(ctx context.Context, repo domain.RepoName, branch string, sha domain.ObjectID) error
}) {
	b.cfg.Hold = h
}
