// SPDX-License-Identifier: AGPL-3.0-or-later

// Package sshsig signs the commits ermite creates with the instance key
// (ADR 0022): an ed25519 key, in the SSH signature format Git reads with
// gpg.format=ssh. The private key lives in memory only.
package sshsig

import (
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

const (
	// Namespace is the SSHSIG namespace of Git signatures.
	Namespace = "git"
	// HashAlgorithm is the hash of the message in the signed data.
	HashAlgorithm = "sha512"

	magic   = "SSHSIG"
	version = 1

	armorBegin = "-----BEGIN SSH SIGNATURE-----"
	armorEnd   = "-----END SSH SIGNATURE-----"
	armorWidth = 70
)

// Signer signs with one ed25519 key. It is safe for concurrent use.
type Signer struct {
	signer ssh.Signer
	pub    domain.SigningPublicKey
}

var _ ports.CommitSigner = (*Signer)(nil)

// New builds a signer from the 32-byte seed of an ed25519 key. The error does
// not contain the seed.
func New(seed []byte) (*Signer, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("signing key seed has %d bytes, want %d", len(seed), ed25519.SeedSize)
	}
	signer, err := ssh.NewSignerFromSigner(ed25519.NewKeyFromSeed(seed))
	if err != nil {
		return nil, fmt.Errorf("build the signing key: %w", err)
	}
	pk := signer.PublicKey()
	return &Signer{
		signer: signer,
		pub: domain.SigningPublicKey{
			AuthorizedKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk))),
			Fingerprint:   ssh.FingerprintSHA256(pk),
		},
	}, nil
}

// PublicKey returns the public key as an authorized_keys line, and its
// SHA-256 fingerprint.
func (s *Signer) PublicKey() domain.SigningPublicKey { return s.pub }

// signedData is what the signature covers after the magic (PROTOCOL.sshsig).
type signedData struct {
	Namespace     string
	Reserved      string
	HashAlgorithm string
	Hash          string
}

// sigBlob is the signature file (PROTOCOL.sshsig), after the magic.
type sigBlob struct {
	Version       uint32
	PublicKey     string
	Namespace     string
	Reserved      string
	HashAlgorithm string
	Signature     string
}

// signatureField is the "signature" string of an SSHSIG: an SSH signature.
type signatureField struct {
	Format string
	Blob   string
}

// Sign signs payload, the text of a commit object, and returns the armored
// signature for its gpgsig header.
func (s *Signer) Sign(payload []byte) (string, error) {
	sum := sha512.Sum512(payload)
	// The magic is a fixed 6-byte preamble, not a length-prefixed string.
	msg := append([]byte(magic), ssh.Marshal(signedData{
		Namespace: Namespace, HashAlgorithm: HashAlgorithm, Hash: string(sum[:]),
	})...)

	sig, err := s.signer.Sign(nil, msg)
	if err != nil {
		return "", errors.New("sign the commit")
	}
	blob := append([]byte(magic), ssh.Marshal(sigBlob{
		Version:       version,
		PublicKey:     string(s.signer.PublicKey().Marshal()),
		Namespace:     Namespace,
		HashAlgorithm: HashAlgorithm,
		Signature:     string(ssh.Marshal(signatureField{Format: sig.Format, Blob: string(sig.Blob)})),
	})...)
	return armor(blob), nil
}

// armor wraps the signature file in its PEM-like armor, 70 columns wide.
func armor(blob []byte) string {
	enc := base64.StdEncoding.EncodeToString(blob)
	var b strings.Builder
	b.WriteString(armorBegin + "\n")
	for len(enc) > armorWidth {
		b.WriteString(enc[:armorWidth] + "\n")
		enc = enc[armorWidth:]
	}
	b.WriteString(enc + "\n" + armorEnd + "\n")
	return b.String()
}
