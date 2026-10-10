// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import "bnema.dev/ermite/internal/domain"

// canRead is the access rule for reading a repository, the single place that
// decides it: root reads everything, anyone reads a public repository, and a
// stored token reads a private one only with repo:read on it (the scope, and
// the repository within its list). Callers answer domain.ErrNotFound when it
// is false, so a private repository never shows itself.
func canRead(p domain.Principal, repo domain.Repo) bool {
	if repo.Visibility == domain.VisibilityPublic {
		return true
	}
	return p.Allows(domain.ScopeRepoRead, repo.Name)
}

// canWrite is the access rule for pushing to a repository, or creating it on
// first push: repo:write on it.
func canWrite(p domain.Principal, name domain.RepoName) bool {
	return p.Allows(domain.ScopeRepoWrite, name)
}

// canManageTokens is the access rule for creating, listing and revoking
// stored tokens: token:admin, which root holds.
func canManageTokens(p domain.Principal) bool {
	return p.HasScope(domain.ScopeTokenAdmin)
}
