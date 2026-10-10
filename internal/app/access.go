// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"fmt"

	"bnema.dev/ermite/internal/domain"
)

// canRead is the access rule for reading a repository, the single place that
// decides it: root reads everything, anyone reads a public repository, and a
// stored token reads a private one only with repo:read on it (the scope, and
// the repository within its list). Callers answer domain.ErrNotFound when it
// is false, so a private repository never shows itself.
func canRead(p domain.Principal, repo domain.Repo) bool {
	return canReadAs(p, repo.Name, repo.Visibility)
}

// canReadAs is canRead for a repository known by its name and visibility only,
// such as one that no longer exists.
func canReadAs(p domain.Principal, name domain.RepoName, vis domain.Visibility) bool {
	if vis == domain.VisibilityPublic {
		return true
	}
	return p.Allows(domain.ScopeRepoRead, name)
}

// adminAccess is the access rule for changing or deleting a repository. A
// caller that may not administer it learns nothing it could not already read:
// not found when it cannot see the repository, unauthenticated when it is
// anonymous, forbidden otherwise.
func adminAccess(p domain.Principal, repo domain.Repo) error {
	switch {
	case canAdmin(p, repo.Name):
		return nil
	case !canRead(p, repo):
		return domain.ErrNotFound
	case p.IsAnonymous():
		return domain.ErrUnauthenticated
	}
	return domain.ErrForbidden
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

// canAdmin is the access rule for deleting a repository and changing its
// visibility, description or default branch: repo:admin on it.
func canAdmin(p domain.Principal, name domain.RepoName) bool {
	return p.Allows(domain.ScopeRepoAdmin, name)
}

// scopeAccess is the access rule for an action that needs a scope on a
// repository the caller can already read, such as mr:write or mr:merge:
// unauthenticated for an anonymous caller, forbidden for one without the scope
// on this repository.
func scopeAccess(p domain.Principal, repo domain.Repo, scope domain.Scope) error {
	switch {
	case p.Allows(scope, repo.Name):
		return nil
	case p.IsAnonymous():
		return domain.ErrUnauthenticated
	}
	return fmt.Errorf("%w: this needs the %s scope on %s", domain.ErrForbidden, scope, repo.Name)
}

// authorOf names who acts, for the author of a merge request or a comment: the
// label of the token, "root" for the root token, "owner" for any other caller
// (a web session).
func authorOf(p domain.Principal) string {
	switch {
	case p.IsRoot():
		return "root"
	case p.TokenLabel() != "":
		return p.TokenLabel()
	}
	return "owner"
}
