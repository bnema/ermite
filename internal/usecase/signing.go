// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// SigningKey is the public side of the key that signs the commits ermite
// creates (ADR 0022).
type SigningKey struct {
	signer    ports.CommitSigner
	committer domain.Identity
}

var _ ports.SigningKeyService = (*SigningKey)(nil)

// NewSigningKey builds the use case over the signer and the identity recorded
// as the committer of the signed commits.
func NewSigningKey(signer ports.CommitSigner, committer domain.Identity) *SigningKey {
	return &SigningKey{signer: signer, committer: committer}
}

// Info returns what a verifier needs. It is public: no access rule applies,
// and the private key never leaves the signer.
func (s *SigningKey) Info() domain.SigningKeyInfo {
	return domain.SigningKeyInfo{Key: s.signer.PublicKey(), Committer: s.committer}
}
