// SPDX-License-Identifier: AGPL-3.0-or-later

// Package sigverify checks commit signatures against the trusted keys
// (ADR 0030). It is pure Go: no gpg or ssh-keygen binary is run. SSH
// signatures (SSHSIG, gpg.format=ssh) and OpenPGP detached signatures are
// verified; X.509 signatures are recognized and reported as unsupported.
//
// Every verifier is a stateless value, safe for concurrent use, and never
// panics on hostile input: a malformed signature is a bad_signature status.
package sigverify

import (
	"bytes"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// maxSignatureBytes bounds the signature a verifier parses; a larger one is
// reported as bad_signature without being read.
const maxSignatureBytes = 64 << 10

// Verifier dispatches a signature to the verifier of its format.
type Verifier struct{}

var _ ports.SignatureVerifier = Verifier{}

// Verify implements ports.SignatureVerifier. An x509 signature is
// unsupported; an unrecognized armor is a bad signature without format.
func (Verifier) Verify(payload, signature []byte, committerEmail string, signedAt int64, keys []domain.TrustedKey) domain.CommitVerification {
	switch domain.SignatureFormatOf(bytes.TrimLeft(signature, " \t\r\n")) {
	case domain.FormatSSH:
		return SSH{}.Verify(payload, signature, committerEmail, signedAt, keys)
	case domain.FormatGPG:
		return GPG{}.Verify(payload, signature, committerEmail, signedAt, keys)
	case domain.FormatX509:
		return domain.CommitVerification{Format: domain.FormatX509, Status: domain.StatusUnsupported}
	}
	return domain.CommitVerification{Format: domain.FormatNone, Status: domain.StatusBadSignature}
}

// trusted completes the verification of a signature whose cryptography is
// valid and whose key is trusted: verified when the key may sign for the
// committer email, email_mismatch otherwise.
func trusted(v domain.CommitVerification, key domain.TrustedKey, committerEmail string) domain.CommitVerification {
	v.KeyID, v.KeyTitle = key.ID, key.Title
	if key.TrustsEmail(committerEmail) {
		v.Status = domain.StatusVerified
	} else {
		v.Status = domain.StatusEmailMismatch
	}
	return v
}
