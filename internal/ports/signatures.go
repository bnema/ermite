// SPDX-License-Identifier: AGPL-3.0-or-later

package ports

import (
	"context"

	"bnema.dev/ermite/internal/domain"
)

// CommitObjectReader reads raw commit objects (ADR 0030).
type CommitObjectReader interface {
	// ReadCommitObjects returns the raw content of each commit (without the
	// "commit <size>\0" prefix), keyed by id. Ids that are missing, are not
	// commits, or exceed domain.MaxCommitObjectBytes are left out of the map.
	// The total size of the returned objects stays within
	// domain.MaxCommitObjectsBytes: ids are read in order, and the first one
	// that would exceed the budget ends the read, so it and the ids after it
	// are left out (the caller leaves those commits unchecked). A cancelled
	// ctx is returned as its error.
	// domain.ErrNotFound when the repository has no directory.
	ReadCommitObjects(ctx context.Context, repo domain.RepoName, ids []domain.ObjectID) (map[domain.ObjectID][]byte, error)
}

// KeyInspector parses public keys for the trusted key list.
type KeyInspector interface {
	// Inspect parses a public key of the given format and returns its ID
	// (fingerprint) and its canonical text: the "<type> <base64>" part of an
	// authorized_keys line for SSH (comment dropped), the armored public key
	// for GPG (exactly one entity, private material refused). Any parse
	// failure wraps domain.ErrInvalid.
	Inspect(format domain.SignatureFormat, publicKey string) (id, canonical string, err error)
}

// SignatureVerifier checks one signature against a set of trusted keys. It
// never errors: every failure is a status.
type SignatureVerifier interface {
	// Verify checks signature over payload, signed by committerEmail.
	// signedAt is the committer date: the committer chooses it, and it is
	// authenticated as part of the payload but is not a trusted timestamp.
	// SSH ignores it, and so does GPG, which
	// judges key and signature expiry at the signature creation time and
	// revocation at the current time. keys are the trusted keys of the
	// verifier's format. It returns Format, Status, Fingerprint, and
	// KeyID/KeyTitle of the key that verified (even on email_mismatch).
	Verify(payload, signature []byte, committerEmail string, signedAt int64, keys []domain.TrustedKey) domain.CommitVerification
}

// TrustedKeyStore keeps the trusted keys (ADR 0030). It does not bound their
// number: the use case checks domain.MaxTrustedKeys.
type TrustedKeyStore interface {
	// ListTrustedKeys returns every stored key, oldest first.
	ListTrustedKeys(ctx context.Context) ([]domain.TrustedKey, error)
	// CreateTrustedKey stores a key; domain.ErrAlreadyExists if the ID is taken.
	CreateTrustedKey(ctx context.Context, k domain.TrustedKey) error
	// DeleteTrustedKey removes a key or returns domain.ErrNotFound.
	DeleteTrustedKey(ctx context.Context, id string) error
}

// TrustedKeyService manages the trusted keys. *usecase.Signatures implements it.
type TrustedKeyService interface {
	// ListTrustedKeys lists the keys, the builtin instance key first. Any
	// authenticated caller may.
	ListTrustedKeys(ctx context.Context, p domain.Principal) ([]domain.TrustedKey, error)
	// AddTrustedKey adds a key. Owner only.
	AddTrustedKey(ctx context.Context, p domain.Principal, format domain.SignatureFormat, title, publicKey string, emails []string) (domain.TrustedKey, error)
	// RemoveTrustedKey removes a key by ID. Owner only; the builtin key is
	// refused with domain.ErrPreconditionFailed.
	RemoveTrustedKey(ctx context.Context, p domain.Principal, id string) error
}

// CommitVerifier tells the signature state of commits the caller already read.
// *usecase.Signatures implements it.
type CommitVerifier interface {
	// VerifyCommits returns one verification per commit, keyed by id. A
	// commit that could not be read is left out (unchecked).
	VerifyCommits(ctx context.Context, repo domain.RepoName, commits []domain.Commit) map[domain.ObjectID]domain.CommitVerification
}
