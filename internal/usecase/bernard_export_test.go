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

// SetRevokeJob replaces the revoke hook, before the tests start Bernard.
func (b *Bernard) SetRevokeJob(fn func(context.Context, string)) { b.cfg.RevokeJob = fn }
