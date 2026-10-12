// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

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

// canCreateRepo is the access rule for creating a repository, by the API or on
// first push: repo:write on the name, and no branch prefixes. A token limited
// to some branches may push to a repository that exists; it never makes one.
func canCreateRepo(p domain.Principal, name domain.RepoName) bool {
	return canWrite(p, name) && len(p.BranchPrefixes()) == 0
}

// canDeleteRepo is the access rule for deleting a repository on top of
// adminAccess: no branch prefixes. Deleting a repository deletes every branch,
// those outside the prefixes of a limited token included.
func canDeleteRepo(p domain.Principal) bool {
	return len(p.BranchPrefixes()) == 0
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
// label of the token, "root" for the root token, "owner" for the owner signed
// in on the web and for any other caller without a label.
func authorOf(p domain.Principal) string {
	switch {
	case p.IsOwner():
		return "owner"
	case p.IsRoot():
		return "root"
	case p.TokenLabel() != "":
		return p.TokenLabel()
	}
	return "owner"
}
