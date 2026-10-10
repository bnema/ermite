// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"slices"
	"time"
)

// PrincipalKind tells who a caller is.
type PrincipalKind uint8

// Principal kinds. The zero value is anonymous, the least privileged.
const (
	// PrincipalAnonymous is a caller without credentials.
	PrincipalAnonymous PrincipalKind = iota
	// PrincipalRoot is the holder of the admin token.
	PrincipalRoot
	// PrincipalToken is the holder of a stored token.
	PrincipalToken
)

// String returns "anonymous", "root" or "token".
func (k PrincipalKind) String() string {
	switch k {
	case PrincipalRoot:
		return "root"
	case PrincipalToken:
		return "token"
	}
	return "anonymous"
}

// Principal is the authenticated identity of a caller: nobody, the root
// token, or a stored token with its scopes and repositories.
type Principal struct {
	kind      PrincipalKind
	tokenID   string
	scopes    []Scope
	repos     []RepoName
	expiresAt time.Time
}

// Anonymous returns the principal of a caller without credentials.
func Anonymous() Principal { return Principal{kind: PrincipalAnonymous} }

// Root returns the principal of the admin token holder.
func Root() Principal { return Principal{kind: PrincipalRoot} }

// TokenPrincipal returns the principal of the holder of a stored token.
func TokenPrincipal(t Token) Principal {
	return Principal{
		kind:      PrincipalToken,
		tokenID:   t.ID,
		scopes:    slices.Clone(t.Scopes),
		repos:     slices.Clone(t.Repos),
		expiresAt: t.ExpiresAt,
	}
}

// Kind returns the kind of principal.
func (p Principal) Kind() PrincipalKind { return p.kind }

// IsRoot reports whether p may do everything.
func (p Principal) IsRoot() bool { return p.kind == PrincipalRoot }

// IsAnonymous reports whether p has no credentials.
func (p Principal) IsAnonymous() bool { return p.kind == PrincipalAnonymous }

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
	case PrincipalRoot:
		return true
	case PrincipalToken:
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
	if p.kind == PrincipalToken && len(p.repos) > 0 {
		return slices.Contains(p.repos, repo)
	}
	return true
}
