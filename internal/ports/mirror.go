// SPDX-License-Identifier: AGPL-3.0-or-later

package ports

import (
	"bnema.dev/ermite/internal/domain"
	"context"
	"crypto/ed25519"
	"time"
)

// MirrorStore keeps the mirror configuration, state and snapshots (kvstore implements it).
type MirrorStore interface {
	GetGlobal(ctx context.Context) (domain.MirrorGlobalConfig, error) // zero value, nil error when unset
	PutGlobal(ctx context.Context, g domain.MirrorGlobalConfig) error
	GetRepoConfig(ctx context.Context, repo domain.RepoName) (domain.MirrorRepoConfig, error) // domain.ErrNotFound when none
	PutRepoConfig(ctx context.Context, c domain.MirrorRepoConfig) error                       // ErrNotFound when the repository is gone (guardRepoKey)
	DeleteRepoConfig(ctx context.Context, repo domain.RepoName) error                         // missing is not an error
	ListRepoConfigs(ctx context.Context) ([]domain.MirrorRepoConfig, error)                   // by repo name
	GetStatus(ctx context.Context, repo domain.RepoName) (domain.MirrorStatus, error)         // ErrNotFound when none
	PutStatus(ctx context.Context, s domain.MirrorStatus) error
	ListStatus(ctx context.Context) ([]domain.MirrorStatus, error)
	GetSnapshot(ctx context.Context, repo domain.RepoName) (domain.MirrorSnapshot, error) // ErrNotFound when none
	PutSnapshot(ctx context.Context, repo domain.RepoName, s domain.MirrorSnapshot) error
}

// MirrorKeyStore keeps the sealed seed of the mirror key. The use case seals/opens it with
// SecretSealer (AAD domain.MirrorKeyPath), as OIDC does.
type MirrorKeyStore interface {
	// GetMirrorKey returns the sealed seed and its creation time, or ErrNotFound.
	GetMirrorKey(ctx context.Context) (sealed []byte, createdAt time.Time, err error)
	// PutMirrorKey replaces the key; ifAbsent makes it fail with ErrAlreadyExists when one exists.
	PutMirrorKey(ctx context.Context, sealed []byte, createdAt time.Time, ifAbsent bool) error
}

// MirrorPush is one push request to the transport.
type MirrorPush struct {
	Repo       domain.RepoName
	URL        string
	Refspecs   []string           // "<ref>:<ref>", no "+", from domain.MirrorRefspecs
	PrivateKey ed25519.PrivateKey // written to a 0600 file for the duration of the push
}
type MirrorRefStatus string

const (
	MirrorRefOK       MirrorRefStatus = "ok"         // porcelain ' ' or '*'
	MirrorRefUpToDate MirrorRefStatus = "up_to_date" // '='
	MirrorRefRejected MirrorRefStatus = "rejected"   // '!'
)

type MirrorRefOutcome struct {
	Ref    string
	Status MirrorRefStatus
	Reason string
}
type MirrorPushResult struct {
	Refs     []MirrorRefOutcome
	Duration time.Duration
}

// MirrorPusher runs `git push --porcelain` (gitpush implements it). A failed push returns a
// *domain.MirrorError (classified); ctx cancellation is returned as is. A result with a rejected
// ref and err == nil never happens: a rejection is a MirrorError of class diverged.
type MirrorPusher interface {
	Push(ctx context.Context, req MirrorPush) (MirrorPushResult, error)
}

// MirrorTrigger is what Pushes.Record tells the mirrors. *usecase.Mirrors implements it.
type MirrorTrigger interface {
	OnPush(ctx context.Context, repo domain.RepoName, updates []domain.RefUpdate) // never blocks
}

// MirrorWarner is what the post-receive hook and the web ask. *usecase.Mirrors implements it.
type MirrorWarner interface {
	// Warning is non-empty when the mirror of repo is failing or paused ("mirror to github.com:x/y is paused: <reason>").
	Warning(ctx context.Context, repo domain.RepoName) string
	// Badge is the state and reason for a viewer: reason is "" unless p may administer the repository.
	Badge(ctx context.Context, p domain.Principal, repo domain.RepoName) (domain.MirrorState, string)
}
type MirrorSyncResult struct {
	Status domain.MirrorStatus
	Queued bool
	NoOp   bool
}

// MirrorService is the inbound port of the API. Authorization: key show needs a non-anonymous
// principal; key rotate, global config and ListStatus need HasOwnerRights(); per-repo config and
// Sync need Allows(ScopeRepoAdmin, repo); GetStatus needs read access to the repository.
type MirrorService interface {
	ShowKey(ctx context.Context, p domain.Principal) (domain.MirrorPublicKey, error) // generates lazily
	RotateKey(ctx context.Context, p domain.Principal) (domain.MirrorPublicKey, error)
	GetGlobalConfig(ctx context.Context, p domain.Principal) (domain.MirrorGlobalConfig, error)
	SetGlobalConfig(ctx context.Context, p domain.Principal, patch MirrorGlobalPatch) (domain.MirrorGlobalConfig, error)
	SetRepoMirror(ctx context.Context, p domain.Principal, c domain.MirrorRepoConfig) (domain.MirrorTarget, error)
	RemoveRepoMirror(ctx context.Context, p domain.Principal, repo domain.RepoName) error
	SetRepoExcluded(ctx context.Context, p domain.Principal, repo domain.RepoName, excluded bool) (domain.MirrorTarget, error)
	GetStatus(ctx context.Context, p domain.Principal, repo domain.RepoName) (MirrorStatusView, error)
	ListStatus(ctx context.Context, p domain.Principal) ([]MirrorStatusView, error)
	Sync(ctx context.Context, p domain.Principal, repo domain.RepoName, force bool) (MirrorSyncResult, error)
	SyncAll(ctx context.Context, p domain.Principal, force bool) ([]MirrorSyncResult, error)
}
type MirrorGlobalPatch struct {
	Enabled     *bool
	URLTemplate *string
	Cron        *string
	AllBranches *bool
}
type MirrorStatusView struct {
	Target    domain.MirrorTarget
	Status    domain.MirrorStatus
	NextRunAt time.Time
}
