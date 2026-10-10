// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

// PrincipalKind tells who a caller is.
type PrincipalKind uint8

// Principal kinds. The zero value is anonymous, the least privileged.
const (
	// PrincipalAnonymous is a caller without credentials.
	PrincipalAnonymous PrincipalKind = iota
	// PrincipalRoot is the holder of the admin token.
	PrincipalRoot
)

// Principal is the authenticated identity of a caller. Stored tokens add
// scoped principals later.
type Principal struct{ kind PrincipalKind }

// Anonymous returns the principal of a caller without credentials.
func Anonymous() Principal { return Principal{kind: PrincipalAnonymous} }

// Root returns the principal of the admin token holder.
func Root() Principal { return Principal{kind: PrincipalRoot} }

// Kind returns the kind of principal.
func (p Principal) Kind() PrincipalKind { return p.kind }

// IsRoot reports whether p may do everything.
func (p Principal) IsRoot() bool { return p.kind == PrincipalRoot }

// IsAnonymous reports whether p has no credentials.
func (p Principal) IsAnonymous() bool { return p.kind == PrincipalAnonymous }
