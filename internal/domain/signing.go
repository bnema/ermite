// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

// SigningPublicKey is the public half of the key with which ermite signs the
// commits it creates (ADR 0022).
type SigningPublicKey struct {
	// AuthorizedKey is the key as one OpenSSH authorized_keys line, for
	// example "ssh-ed25519 AAAA...".
	AuthorizedKey string
	// Fingerprint is the SHA-256 fingerprint in the form ssh-keygen prints,
	// "SHA256:<base64>".
	Fingerprint string
}

// SigningKeyInfo is what a verifier needs to check the commits of ermite.
type SigningKeyInfo struct {
	Key       SigningPublicKey
	Committer Identity
}

// AllowedSigners returns the line of an allowed signers file for the key:
// "<committer email> <authorized key>".
func (i SigningKeyInfo) AllowedSigners() string {
	return i.Committer.Email + " " + i.Key.AuthorizedKey
}
