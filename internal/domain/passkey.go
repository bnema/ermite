// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"fmt"
	"time"
)

const (
	// MaxPasskeys is the most passkeys the owner may register.
	MaxPasskeys = 20
	// MaxPasskeyIDLen is the longest credential id, in bytes (the WebAuthn
	// limit).
	MaxPasskeyIDLen = 1023
	// MaxPasskeyNameLen is the longest passkey name, in bytes.
	MaxPasskeyNameLen = 64
	// DefaultPasskeyName names a passkey registered without a name.
	DefaultPasskeyName = "passkey"
	// PasskeyCeremonyTTL is how long a registration or a sign-in may take, from
	// its begin to its finish. The WebAuthn adapter enforces it on the
	// challenge and the use case on the ceremony it keeps.
	PasskeyCeremonyTTL = 2 * time.Minute
)

// Passkey is a WebAuthn credential that can sign the owner in. It holds the
// public key only.
type Passkey struct {
	// ID is the credential id: at most MaxPasskeyIDLen bytes, never empty.
	ID   []byte
	Name string
	// PublicKey is the COSE public key.
	PublicKey       []byte
	AttestationType string
	Transports      []string
	// SignCount is the last counter the authenticator reported; 0 for
	// authenticators that have none (synced passkeys).
	SignCount uint32
	// BackupEligible and BackupState are the WebAuthn flags: the credential
	// can be backed up, and currently is (a synced passkey).
	BackupEligible bool
	BackupState    bool
	CreatedAt      time.Time
	LastUsedAt     time.Time // zero until first used
}

// PasskeyName is a validated passkey name: one line of plain text, 1 to
// MaxPasskeyNameLen bytes, not blank. Only ParsePasskeyName builds a non-zero
// value.
type PasskeyName struct{ s string }

// ParsePasskeyName validates s with the rules of the other one-line texts: valid
// UTF-8, no control or bidirectional override character, and not blank.
func ParsePasskeyName(s string) (PasskeyName, error) {
	if len(s) > MaxPasskeyNameLen {
		return PasskeyName{}, &TextError{Reason: fmt.Sprintf("is longer than %d bytes (%d)", MaxPasskeyNameLen, len(s))}
	}
	if reason := textProblem(s, false, MaxPasskeyNameLen); reason != "" {
		return PasskeyName{}, &TextError{Reason: reason}
	}
	if isBlank(s) {
		return PasskeyName{}, &TextError{Reason: "is empty"}
	}
	return PasskeyName{s: s}, nil
}

// String returns the name.
func (n PasskeyName) String() string { return n.s }
