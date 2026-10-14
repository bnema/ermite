// SPDX-License-Identifier: AGPL-3.0-or-later

// Package secretbox seals the values of CI secrets with AES-256-GCM under the
// server's master key (ERMITE_SECRETS_KEY, ADR 0019). Each value gets its own
// random nonce, and the additional data given by the caller (the scope and the
// name of the secret) is authenticated with it, so a sealed value moved to
// another secret does not open.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"

	"bnema.dev/ermite/internal/ports"
)

// KeyLen is the length of the master key.
const KeyLen = 32

// version is the first byte of a sealed value, so that the format can change.
const version byte = 1

// Box seals and opens secret values. It is safe for concurrent use.
type Box struct {
	aead  cipher.AEAD
	check []byte
}

var _ ports.SecretSealer = (*Box)(nil)

// New builds a Box from a key of KeyLen bytes. The error never holds the key.
func New(key []byte) (*Box, error) {
	if len(key) != KeyLen {
		return nil, fmt.Errorf("the secrets key must be %d bytes, got %d", KeyLen, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("the secrets key is not usable")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("the secrets key is not usable")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("ermite secrets key check v1"))
	return &Box{aead: aead, check: mac.Sum(nil)}, nil
}

// Seal returns version, nonce and ciphertext (with its tag) of plaintext,
// bound to aad.
func (b *Box) Seal(plaintext, aad []byte) []byte {
	out := make([]byte, 1, 1+b.aead.NonceSize()+len(plaintext)+b.aead.Overhead())
	out[0] = version
	nonce := make([]byte, b.aead.NonceSize())
	_, _ = rand.Read(nonce) // crypto/rand does not fail: it aborts the program
	out = append(out, nonce...)
	return b.aead.Seal(out, nonce, plaintext, aad)
}

// Open returns the plaintext of a value sealed with the same key and aad. Any
// other key, aad or a changed byte is ports.ErrSecretNotOpened.
func (b *Box) Open(sealed, aad []byte) ([]byte, error) {
	ns := b.aead.NonceSize()
	if len(sealed) < 1+ns+b.aead.Overhead() || sealed[0] != version {
		return nil, ports.ErrSecretNotOpened
	}
	plain, err := b.aead.Open(nil, sealed[1:1+ns], sealed[1+ns:], aad)
	if err != nil {
		return nil, ports.ErrSecretNotOpened
	}
	return plain, nil
}

// KeyCheck is a value derived from the key that does not reveal it (an HMAC of
// a constant), stored beside the secrets so that the server can tell, at start,
// that it runs with another key than the one they were sealed with.
func (b *Box) KeyCheck() []byte { return append([]byte(nil), b.check...) }
