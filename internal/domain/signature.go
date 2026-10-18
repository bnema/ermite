// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"bytes"
	"fmt"
	"net/mail"
	"slices"
	"strings"
	"time"
)

// SignatureFormat is the protocol of a commit signature (ADR 0030).
type SignatureFormat string

// Signature formats. FormatNone is an unsigned commit.
const (
	FormatNone SignatureFormat = ""
	FormatSSH  SignatureFormat = "ssh"
	FormatGPG  SignatureFormat = "gpg"
	// FormatX509 is a gpgsm (S/MIME) signature: recognized, never verified.
	FormatX509 SignatureFormat = "x509"
)

// ParseSignatureFormat parses the text form of a key format, "ssh" or "gpg".
// x509 keys cannot be trusted.
func ParseSignatureFormat(s string) (SignatureFormat, error) {
	switch f := SignatureFormat(strings.ToLower(strings.TrimSpace(s))); f {
	case FormatSSH, FormatGPG:
		return f, nil
	}
	return FormatNone, fmt.Errorf("%w: key format %q, want ssh or gpg", ErrInvalid, s)
}

// SignatureStatus is the outcome of checking a commit signature.
type SignatureStatus string

// Signature statuses. StatusUnchecked is the zero value: the commit was not
// checked (or could not be read); it is never cached.
const (
	StatusUnchecked SignatureStatus = ""
	// StatusUnsigned: the commit carries no signature.
	StatusUnsigned SignatureStatus = "unsigned"
	// StatusVerified: valid signature by a trusted key whose emails include
	// the committer email.
	StatusVerified SignatureStatus = "verified"
	// StatusUnknownKey: well-formed signature by a key that is not trusted.
	StatusUnknownKey SignatureStatus = "unknown_key"
	// StatusBadSignature: malformed, or does not match the commit.
	StatusBadSignature SignatureStatus = "bad_signature"
	// StatusEmailMismatch: valid signature by a trusted key that is not
	// trusted for the committer email.
	StatusEmailMismatch SignatureStatus = "email_mismatch"
	// StatusExpired: the GPG key or signature had expired.
	StatusExpired SignatureStatus = "expired"
	// StatusRevoked: the GPG key is revoked.
	StatusRevoked SignatureStatus = "revoked"
	// StatusUnsupported: a format ermite does not verify (x509).
	StatusUnsupported SignatureStatus = "unsupported"
)

// CommitVerification is what ermite knows about the signature of a commit.
type CommitVerification struct {
	Format SignatureFormat
	Status SignatureStatus
	// Fingerprint is the fingerprint of the signing key when it could be
	// read: "SHA256:<base64>" for SSH, 40 upper-case hex digits (or the
	// 16-digit issuer key id when the key is unknown) for GPG.
	Fingerprint string
	// KeyID is the ID of the matching trusted key; empty when none matched.
	KeyID string
	// KeyTitle is the title of the matching trusted key.
	KeyTitle string
}

// Signed reports whether the commit carries a signature.
func (v CommitVerification) Signed() bool { return v.Format != FormatNone }

// Limits of trusted keys.
const (
	MaxTrustedKeys        = 100
	MaxTrustedKeyEmails   = 10
	MaxTrustedKeyTitleLen = 100
	MaxTrustedKeyBytes    = 64 << 10
	MaxCommitObjectBytes  = 1 << 20
	// MaxCommitObjectsBytes bounds the total size of the commit objects one
	// ReadCommitObjects call returns.
	MaxCommitObjectsBytes  = 8 << 20
	maxTrustedKeyEmailSize = 254
)

// TrustedKey is a public key whose signatures ermite accepts for the listed
// committer emails (ADR 0030).
type TrustedKey struct {
	// ID is the key fingerprint: "SHA256:<base64>" for SSH, the primary key
	// fingerprint in upper-case hex for GPG.
	ID     string
	Format SignatureFormat
	Title  string
	// PublicKey is the key as given: an authorized_keys line for SSH, an
	// armored public key block for GPG.
	PublicKey string
	// Emails are the committer emails the key may sign for, lower-case.
	Emails    []string
	CreatedAt time.Time
	// Builtin marks the instance signing key (ADR 0022): listed, never stored
	// nor deletable.
	Builtin bool
}

// TrustsEmail reports whether the key may sign for the email (case-insensitive).
func (k TrustedKey) TrustsEmail(email string) bool {
	return slices.Contains(k.Emails, strings.ToLower(strings.TrimSpace(email)))
}

// ParseTrustedKeyTitle checks a key title: 1 to MaxTrustedKeyTitleLen bytes
// once trimmed, no control characters.
func ParseTrustedKeyTitle(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > MaxTrustedKeyTitleLen || strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", fmt.Errorf("%w: key title must be 1 to %d printable bytes", ErrInvalid, MaxTrustedKeyTitleLen)
	}
	return s, nil
}

// NormalizeKeyEmails lower-cases, trims, de-duplicates and checks emails:
// 1 to MaxTrustedKeyEmails plain addresses ("a@b", no display name).
func NormalizeKeyEmails(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		a, err := mail.ParseAddress(e)
		if err != nil || a.Address != e || len(e) > maxTrustedKeyEmailSize {
			return nil, fmt.Errorf("%w: email %q", ErrInvalid, e)
		}
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	if len(out) == 0 || len(out) > MaxTrustedKeyEmails {
		return nil, fmt.Errorf("%w: a key needs 1 to %d emails", ErrInvalid, MaxTrustedKeyEmails)
	}
	return out, nil
}

// SplitCommitSignature splits the raw content of a commit object (as stored,
// without the "commit <size>\0" prefix) into the payload that was signed and
// the signature, exactly as `git verify-commit` does for a SHA-1 repository
// (checked against git 2.56 with a fake gpg.program):
//
//   - in the header section only (up to the first empty line, so a message
//     cannot inject a header), every header whose name starts with "gpgsig"
//     (gpgsig, gpgsig-sha256, ...) is removed from the payload with its
//     continuation lines (lines that start with a space);
//   - the signature is the value of the "gpgsig " headers (name, one space,
//     value), with the leading space of each continuation line removed.
//     Several gpgsig headers are concatenated; their order and their line
//     endings are kept, and no line break is added;
//   - gpgsig-sha256 is the signature of SHA-256 repositories: it is never
//     the signature here, so a commit that only has it is unsigned (ok false).
//
// ok is false for an unsigned commit.
func SplitCommitSignature(raw []byte) (payload, signature []byte, ok bool) {
	const (
		prefix = "gpgsig"
		header = "gpgsig "
	)
	var pay, sig bytes.Buffer
	pay.Grow(len(raw))
	// inSig: the last header is a "gpgsig " one; skipping: the last header is
	// any gpgsig* one, so its continuation lines are removed from the payload.
	inSig, skipping, found := false, false, false
	rest := raw
	for len(rest) > 0 {
		i := bytes.IndexByte(rest, '\n')
		var line []byte
		if i < 0 {
			line, rest = rest, nil
		} else {
			line, rest = rest[:i+1], rest[i+1:]
		}
		if len(line) == 1 && line[0] == '\n' { // end of headers
			pay.Write(line)
			pay.Write(rest)
			break
		}
		switch {
		case skipping && line[0] == ' ':
			if inSig {
				sig.Write(line[1:])
			}
		case bytes.HasPrefix(line, []byte(prefix)):
			skipping = true
			inSig = bytes.HasPrefix(line, []byte(header))
			if inSig {
				found = true
				sig.Write(line[len(header):])
			}
		default:
			skipping, inSig = false, false
			pay.Write(line)
		}
	}
	if !found {
		return nil, nil, false
	}
	return pay.Bytes(), sig.Bytes(), true
}

// SignatureFormatOf tells the format of an armored signature from its first
// line; FormatNone when it is not recognized.
func SignatureFormatOf(signature []byte) SignatureFormat {
	switch {
	case bytes.HasPrefix(signature, []byte("-----BEGIN SSH SIGNATURE-----")):
		return FormatSSH
	case bytes.HasPrefix(signature, []byte("-----BEGIN PGP SIGNATURE-----")):
		return FormatGPG
	case bytes.HasPrefix(signature, []byte("-----BEGIN SIGNED MESSAGE-----")):
		return FormatX509
	}
	return FormatNone
}
