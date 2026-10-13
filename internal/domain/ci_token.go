// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"strings"
	"time"
)

// RepoTokenPrefix starts the secret of a job repository token: the
// "github.token" of a job, or the token of one action archive. Unlike the
// stored tokens (TokenSecretPrefix) it is never created, listed or revoked by
// a person: the CI engine issues it with a job and revokes it with the job.
const RepoTokenPrefix = "ermitegh_"

// IsRepoToken reports whether s has the shape of a job repository token.
func IsRepoToken(s string) bool { return hasTokenShape(s, RepoTokenPrefix) }

// repoTokenIDPrefix starts the TokenID of the principal of a job repository
// token, so that it never collides with the ID of a stored token.
const repoTokenIDPrefix = "job:"

// IsRepoTokenID reports whether id is the TokenID of the principal of a job
// repository token (see RepoTokenPrincipal).
func IsRepoTokenID(id string) bool { return strings.HasPrefix(id, repoTokenIDPrefix) }

// CIRepoToken is a job repository token: it reads one repository, until
// ExpiresAt or the end of its job. The secret is never stored, only its hash.
type CIRepoToken struct {
	// ID identifies the token; it is not secret.
	ID   string
	Hash TokenHash
	// Repo is the one repository the token reads.
	Repo RepoName
	// RunID, RunAttempt and JobID name the job it belongs to.
	RunID      int64
	RunAttempt int32
	JobID      string
	// Archive is set for the token of an action archive, unset for the job's
	// own token ("github.token").
	Archive   bool
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Expired reports whether the token is no longer valid at now.
func (t CIRepoToken) Expired(now time.Time) bool { return !now.Before(t.ExpiresAt) }

// RepoTokenGrace is how long a job repository token outlives the deadline of
// its job at the most (a safety net: it is revoked when the job ends).
const RepoTokenGrace = 5 * time.Minute

// RepoTokenPrincipal returns the principal of the holder of a job repository
// token. It is shaped like the principal of a stored token (the scope
// repo:read on one repository, an expiry), so that every authorization rule
// of the forge applies to it unchanged: it reads that repository, even when
// private, and writes nothing. Its TokenID starts with "job:" and matches no
// stored token.
func RepoTokenPrincipal(t CIRepoToken) Principal {
	return TokenPrincipal(Token{
		ID: repoTokenIDPrefix + t.ID, Label: "ci job", Scopes: []Scope{ScopeRepoRead},
		Repos: []RepoName{t.Repo}, ExpiresAt: t.ExpiresAt, CreatedAt: t.CreatedAt,
	})
}

// CIActionSource says where an action was found.
type CIActionSource int

// Action sources, with the numbers of ActionSource in the protocol.
const (
	CIActionGitHub CIActionSource = 1
	CIActionErmite CIActionSource = 2
)

// CIActionResolution is the resolution of an action reference for one run
// attempt: every job of the attempt gets the same commit.
type CIActionResolution struct {
	RunID      int64
	RunAttempt int32
	// Key is "owner/repo@ref", lower-cased.
	Key           string
	NameWithOwner string
	ResolvedSHA   string
	Source        CIActionSource
}
