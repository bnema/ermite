// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"slices"
	"strings"
	"time"
)

// principalKind tells who a caller is.
type principalKind uint8

// Principal kinds. The zero value is anonymous, the least privileged.
const (
	kindAnonymous principalKind = iota
	kindRoot
	kindToken
	kindOwner
)

// Principal is the authenticated identity of a caller: nobody, the root
// token, the owner signed in on the web, or a stored token with its scopes
// and repositories.
type Principal struct {
	kind    principalKind
	tokenID string
	scopes  []Scope
	repos   []RepoName
	// branchPrefixes limits the branches a token may write; empty = all.
	branchPrefixes []string
	expiresAt      time.Time
	label          string
}

// Anonymous returns the principal of a caller without credentials.
func Anonymous() Principal { return Principal{kind: kindAnonymous} }

// Root returns the principal of the admin token holder.
func Root() Principal { return Principal{kind: kindRoot} }

// Owner returns the principal of the owner's web session (ADR 0018): the
// rights of root, labelled "owner" in logs and messages. Only the web
// middleware creates it, for a valid session cookie; no secret maps to it.
func Owner() Principal { return Principal{kind: kindOwner} }

// TokenPrincipal returns the principal of the holder of a stored token.
func TokenPrincipal(t Token) Principal {
	return Principal{
		kind:           kindToken,
		tokenID:        t.ID,
		scopes:         slices.Clone(t.Scopes),
		repos:          slices.Clone(t.Repos),
		branchPrefixes: slices.Clone(t.BranchPrefixes),
		expiresAt:      t.ExpiresAt,
		label:          t.Label,
	}
}

// Label names the kind of principal for logs and messages: "anonymous",
// "root", "owner" or "token".
func (p Principal) Label() string {
	switch p.kind {
	case kindRoot:
		return "root"
	case kindOwner:
		return "owner"
	case kindToken:
		return "token"
	}
	return "anonymous"
}

// IsRoot reports whether p may do everything: the root token, or the owner
// signed in on the web.
func (p Principal) IsRoot() bool { return p.kind == kindRoot || p.kind == kindOwner }

// HasOwnerRights reports whether p is the owner: root, the owner's web
// session, or a stored token that holds every scope and is limited neither to
// a list of repositories nor to branch prefixes. It decides who may manage browser sessions.
func (p Principal) HasOwnerRights() bool {
	if p.IsRoot() {
		return true
	}
	if p.kind != kindToken || len(p.repos) > 0 || len(p.branchPrefixes) > 0 {
		return false
	}
	for _, s := range AllScopes() {
		if !slices.Contains(p.scopes, s) {
			return false
		}
	}
	return true
}

// IsOwner reports whether p is the owner signed in on the web.
func (p Principal) IsOwner() bool { return p.kind == kindOwner }

// IsAnonymous reports whether p has no credentials.
func (p Principal) IsAnonymous() bool { return p.kind == kindAnonymous }

// TokenID returns the ID of the stored token, or "" for other principals.
func (p Principal) TokenID() string { return p.tokenID }

// Scopes returns the scopes of a stored token; nil for other principals.
func (p Principal) Scopes() []Scope { return slices.Clone(p.scopes) }

// Repos returns the repository list of a stored token; empty means every
// repository.
func (p Principal) Repos() []RepoName { return slices.Clone(p.repos) }

// BranchPrefixes returns the branch prefixes of a stored token; empty means
// every branch.
func (p Principal) BranchPrefixes() []string { return slices.Clone(p.branchPrefixes) }

// AllowsRef reports whether p may write the ref (create, update or delete
// it). Without branch prefixes every ref is allowed. With them, only a branch
// "refs/heads/<name>" under one of the prefixes is: tags, every other ref and
// a branch outside the prefixes are not.
func (p Principal) AllowsRef(ref string) bool {
	if len(p.branchPrefixes) == 0 {
		return true
	}
	name, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok {
		return false
	}
	if _, err := ParseBranchName(name); err != nil {
		return false
	}
	for _, prefix := range p.branchPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// ExpiresAt returns the expiry of a stored token; zero means none.
func (p Principal) ExpiresAt() time.Time { return p.expiresAt }

// HasScope reports whether p holds the scope, for some repository. Root and
// the owner hold every scope; anonymous none.
func (p Principal) HasScope(s Scope) bool {
	switch p.kind {
	case kindRoot, kindOwner:
		return true
	case kindToken:
		return slices.Contains(p.scopes, s)
	}
	return false
}

// Allows reports whether p holds the scope on the repository: the scope, and
// the repository within the token's list when it has one.
func (p Principal) Allows(s Scope, repo RepoName) bool {
	if !p.HasScope(s) {
		return false
	}
	if p.kind == kindToken && len(p.repos) > 0 {
		return slices.Contains(p.repos, repo)
	}
	return true
}

// TokenLabel returns the label of the stored token, or "" for other
// principals.
func (p Principal) TokenLabel() string { return p.label }
