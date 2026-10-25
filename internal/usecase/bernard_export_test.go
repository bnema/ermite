// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"bnema.dev/ermite/internal/domain"
)

// UnderRepoLock holds Bernard's per-repository lock while fn runs.
func (b *Bernard) UnderRepoLock(repo domain.RepoName, fn func()) {
	defer b.repoLock(repo)()
	fn()
}
