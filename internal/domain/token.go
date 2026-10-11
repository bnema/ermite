// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"time"
)

// TokenSecretPrefix starts every token secret.
const TokenSecretPrefix = "ermite_"

// Scope is one permission of a stored token.
type Scope string

// The scopes of ADR 0006. The last three only matter after v1 and are defined
// now so that the token format never changes.
const (
	// ScopeRepoRead reads private repositories.
	ScopeRepoRead Scope = "repo:read"
	// ScopeRepoWrite pushes and creates a repository on first push.
	ScopeRepoWrite Scope = "repo:write"
	// ScopeRepoAdmin deletes a repository and changes its visibility or description.
	ScopeRepoAdmin Scope = "repo:admin"
	// ScopeMRWrite creates, comments on and closes merge requests.
	ScopeMRWrite Scope = "mr:write"
	// ScopeMRMerge merges, and deletes the merged source branch on request.
	ScopeMRMerge Scope = "mr:merge"
	// ScopeCIWrite re-runs or cancels CI runs.
	ScopeCIWrite Scope = "ci:write"
	// ScopeSecretsWrite sets and deletes secrets and variables.
	ScopeSecretsWrite Scope = "secrets:write"
	// ScopeRunnerRegister registers a CI runner.
	ScopeRunnerRegister Scope = "runner:register"
	// ScopeTokenAdmin creates, lists and revokes stored tokens.
	ScopeTokenAdmin Scope = "token:admin"
)

// AllScopes returns the nine scopes in a stable order.
func AllScopes() []Scope {
	return []Scope{
		ScopeRepoRead, ScopeRepoWrite, ScopeRepoAdmin,
		ScopeMRWrite, ScopeMRMerge,
		ScopeCIWrite, ScopeSecretsWrite, ScopeRunnerRegister,
		ScopeTokenAdmin,
	}
}

// ParseScope parses the text form of a scope, such as "repo:read".
func ParseScope(s string) (Scope, error) {
	sc := Scope(s)
	if slices.Contains(AllScopes(), sc) {
		return sc, nil
	}
	return "", fmt.Errorf("%w: %q", ErrInvalidScope, s)
}

// String returns the scope as text.
func (s Scope) String() string { return string(s) }

// TokenHash is the SHA-256 of a token secret. Secrets are never stored.
type TokenHash [sha256.Size]byte

// HashTokenSecret returns the hash of a token secret.
func HashTokenSecret(secret string) TokenHash { return sha256.Sum256([]byte(secret)) }

// String returns the hash as lowercase hex.
func (h TokenHash) String() string { return hex.EncodeToString(h[:]) }

// Token is a stored credential: what it may do, where, and until when.
type Token struct {
	// ID identifies the token; it is not secret.
	ID    string
	Label string
	Hash  TokenHash
	// Scopes is never empty.
	Scopes []Scope
	// Repos limits the token to these repositories; empty means every one.
	Repos []RepoName
	// ExpiresAt is the zero time for a token that never expires.
	ExpiresAt  time.Time
	CreatedAt  time.Time
	LastUsedAt time.Time // zero until first used
}

// Expired reports whether the token is no longer valid at now.
func (t Token) Expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt)
}
