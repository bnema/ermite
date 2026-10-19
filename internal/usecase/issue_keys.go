// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// HKDF info strings of the subkeys of the issue form key. Each purpose has its
// own key, so that a value signed for one can never pass for another.
const (
	infoIPHash    = "ermite issue ip-hash"
	infoPoW       = "ermite issue pow"
	infoFormToken = "ermite issue form-token"
)

// IssueKeys holds the subkeys of the public issue form, derived with
// HKDF-SHA256 from the server key of the IssueKeyStore.
type IssueKeys struct {
	ipHash, pow, formToken []byte
}

// NewIssueKeys loads the server key (generated on the first call) and derives
// the subkeys.
func NewIssueKeys(ctx context.Context, store ports.IssueKeyStore) (*IssueKeys, error) {
	master, err := store.Key(ctx)
	if err != nil {
		return nil, fmt.Errorf("load issue key: %w", err)
	}
	k := &IssueKeys{}
	for _, d := range []struct {
		dst  *[]byte
		info string
	}{{&k.ipHash, infoIPHash}, {&k.pow, infoPoW}, {&k.formToken, infoFormToken}} {
		if *d.dst, err = hkdf.Key(sha256.New, master, nil, d.info, sha256.Size); err != nil {
			return nil, fmt.Errorf("derive %q: %w", d.info, err)
		}
	}
	return k, nil
}

// IPHash returns HMAC-SHA256 of the client key (clientlimit.Key of the
// address) under the ip-hash subkey: domain.IPHashLen bytes. It is stable
// across restarts and says nothing about the address without the key.
func (k *IssueKeys) IPHash(addrKey string) []byte {
	m := hmac.New(sha256.New, k.ipHash)
	m.Write([]byte(addrKey))
	return m.Sum(nil)[:domain.IPHashLen]
}

// PoWKey returns a copy of the subkey that signs the proof-of-work challenges.
func (k *IssueKeys) PoWKey() []byte { return append([]byte(nil), k.pow...) }

// FormTokenKey returns a copy of the subkey that signs the form tokens.
func (k *IssueKeys) FormTokenKey() []byte { return append([]byte(nil), k.formToken...) }
