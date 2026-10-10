// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import "bnema.dev/ermite/internal/domain"

// canRead is the access rule for reading a repository, the single place that
// decides it: root reads everything, anonymous callers read public
// repositories only. Callers answer domain.ErrNotFound when it is false, so
// a private repository never shows itself.
func canRead(p domain.Principal, repo domain.Repo) bool {
	if p.IsRoot() {
		return true
	}
	return repo.Visibility == domain.VisibilityPublic
}

// canCreate is the access rule for creating a repository: root only.
func canCreate(p domain.Principal) bool { return p.IsRoot() }
