// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"maps"
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
	// own token ("github.token"). An archive token downloads the commit Sha of
	// Repo and nothing else.
	Archive bool
	// Sha is the commit an archive token is bound to; empty for the job's own.
	Sha string
	// OIDC is set for the ID token request token of the job
	// (OIDCRequestTokenPrefix): it only asks the token endpoint for the job's
	// ID tokens, and authenticates nothing else (RepoTokenPrincipal).
	OIDC bool
	// Permissions are the permissions of the job's own token (key to level, see
	// TokenPermissions). Empty: the default, which reads the repository.
	Permissions map[string]string
	ExpiresAt   time.Time
	CreatedAt   time.Time
}

// Expired reports whether the token is no longer valid at now.
func (t CIRepoToken) Expired(now time.Time) bool { return !now.Before(t.ExpiresAt) }

// RepoTokenGrace is how long a job repository token outlives the deadline of
// its job at the most (a safety net: it is revoked when the job ends).
const RepoTokenGrace = 5 * time.Minute

// RepoTokenPrincipal returns the principal of the holder of a job repository
// token.
//
// The job's own token is shaped like a stored token (scopes on one repository,
// an expiry), so that every authorization rule of the forge applies to it
// unchanged. Its scopes follow its permissions (RepoTokenScopes): by default it
// reads that repository, even when private, and writes nothing; with
// contents: write it also pushes to it. It has no branch prefixes: a run whose
// actor is limited to branches is not trusted and holds no write permission.
//
// An archive token holds no scope at all: it is the grant to download one
// commit of one repository as an archive, which only the archive endpoint
// reads (Principal.ArchiveGrant). Git over HTTP, the repository endpoint and
// the rest of the API see a caller with no rights, so a private repository
// does not show itself to it.
//
// Both have a TokenID that starts with "job:" and matches no stored token.
//
// An ID token request token is nobody: it is anonymous everywhere but at the
// token endpoint, which looks it up itself.
func RepoTokenPrincipal(t CIRepoToken) Principal {
	if t.OIDC {
		return Anonymous()
	}
	if t.Archive {
		return Principal{
			kind: kindArchive, tokenID: repoTokenIDPrefix + t.ID, label: "ci archive", expiresAt: t.ExpiresAt,
			archiveRepo: t.Repo, archiveSha: t.Sha,
		}
	}
	p := TokenPrincipal(Token{
		ID: repoTokenIDPrefix + t.ID, Label: "ci job", Scopes: RepoTokenScopes(t.Permissions),
		Repos: []RepoName{t.Repo}, ExpiresAt: t.ExpiresAt, CreatedAt: t.CreatedAt,
	})
	p.jobPerms = maps.Clone(t.Permissions)
	if len(p.jobPerms) == 0 {
		p.jobPerms = DefaultTokenPermissions()
	}
	return p
}

// CIPusherKind tells who pushed the commit of a run.
type CIPusherKind string

// Pusher kinds. The zero value is a run stored before the pusher was recorded.
const (
	CIPusherNone  CIPusherKind = ""
	CIPusherRoot  CIPusherKind = "root"
	CIPusherOwner CIPusherKind = "owner"
	CIPusherToken CIPusherKind = "token"
	// CIPusherBot is Bernard; TokenID holds the repository it is bound to.
	CIPusherBot CIPusherKind = "bot"
)

// CIPusher is who pushed the commit of a run, as recorded with the run. A
// token is recorded by its ID only: what it may read is looked up when it is
// needed, so a token revoked since no longer reads anything on behalf of the
// run.
type CIPusher struct {
	Kind    CIPusherKind
	TokenID string
}

// PusherOf records the pusher p. Anonymous callers, which cannot push, and the
// holders of job tokens, which cannot either, are recorded as nobody.
func PusherOf(p Principal) CIPusher {
	switch {
	case p.kind == kindBot:
		return CIPusher{Kind: CIPusherBot, TokenID: p.botRepo().String()}
	case p.IsOwner():
		return CIPusher{Kind: CIPusherOwner}
	case p.IsRoot():
		return CIPusher{Kind: CIPusherRoot}
	case p.TokenID() != "" && !IsRepoTokenID(p.TokenID()):
		return CIPusher{Kind: CIPusherToken, TokenID: p.TokenID()}
	}
	return CIPusher{}
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
