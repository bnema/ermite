// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"slices"
	"time"
)

// principalKind tells who a caller is.
type principalKind uint8

// Principal kinds. The zero value is anonymous, the least privileged.
const (
	kindAnonymous principalKind = iota
	kindRoot
	kindToken
)

// Principal is the authenticated identity of a caller: nobody, the root
// token, or a stored token with its scopes and repositories.
type Principal struct {
	kind      principalKind
	tokenID   string
	scopes    []Scope
	repos     []RepoName
	expiresAt time.Time
}

// Anonymous returns the principal of a caller without credentials.
func Anonymous() Principal { return Principal{kind: kindAnonymous} }

// Root returns the principal of the admin token holder.
func Root() Principal { return Principal{kind: kindRoot} }

// TokenPrincipal returns the principal of the holder of a stored token.
func TokenPrincipal(t Token) Principal {
	return Principal{
		kind:      kindToken,
		tokenID:   t.ID,
		scopes:    slices.Clone(t.Scopes),
		repos:     slices.Clone(t.Repos),
		expiresAt: t.ExpiresAt,
	}
}

// Label names the kind of principal for logs and messages: "anonymous",
// "root" or "token".
func (p Principal) Label() string {
	switch p.kind {
	case kindRoot:
		return "root"
	case kindToken:
		return "token"
	}
	return "anonymous"
}

// IsToken reports whether p holds a stored token.
func (p Principal) IsToken() bool { return p.kind == kindToken }

// IsRoot reports whether p may do everything.
func (p Principal) IsRoot() bool { return p.kind == kindRoot }

// IsAnonymous reports whether p has no credentials.
func (p Principal) IsAnonymous() bool { return p.kind == kindAnonymous }

// TokenID returns the ID of the stored token, or "" for other principals.
func (p Principal) TokenID() string { return p.tokenID }

// Scopes returns the scopes of a stored token; nil for other principals.
func (p Principal) Scopes() []Scope { return slices.Clone(p.scopes) }

// Repos returns the repository list of a stored token; empty means every
// repository.
func (p Principal) Repos() []RepoName { return slices.Clone(p.repos) }

// ExpiresAt returns the expiry of a stored token; zero means none.
func (p Principal) ExpiresAt() time.Time { return p.expiresAt }

// HasScope reports whether p holds the scope, for some repository. Root holds
// every scope; anonymous none.
func (p Principal) HasScope(s Scope) bool {
	switch p.kind {
	case kindRoot:
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
