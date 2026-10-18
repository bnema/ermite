// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	kv "bnema.dev/ermite-kv"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

const (
	trustedKeyPrefix = "signing/trusted/"

	// maxTrustedKeyIDLen bounds a key id: a fingerprint is 50 bytes at most.
	maxTrustedKeyIDLen = 128
)

// TrustedKeys is the ermite-kv backed ports.TrustedKeyStore. It shares the
// database of the Store it comes from.
type TrustedKeys struct {
	db *kv.DB
}

var _ ports.TrustedKeyStore = (*TrustedKeys)(nil)

// TrustedKeys returns the trusted key store over the same database. It is
// valid until the Store is closed.
func (s *Store) TrustedKeys() *TrustedKeys { return &TrustedKeys{db: s.db} }

func trustedKeyKey(id string) []byte { return []byte(trustedKeyPrefix + id) }

// ListTrustedKeys returns the stored keys, oldest first, then by id.
func (s *TrustedKeys) ListTrustedKeys(ctx context.Context) ([]domain.TrustedKey, error) {
	var out []domain.TrustedKey
	for key, item := range s.db.Snapshot(ctx).Prefix(ctx, []byte(trustedKeyPrefix)) {
		k, err := decodeTrustedKey(item.Value)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		out = append(out, k)
	}
	slices.SortStableFunc(out, func(a, b domain.TrustedKey) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

// CreateTrustedKey stores signing/trusted/<id> with an "absent" precondition:
// a taken id yields domain.ErrAlreadyExists. The caller bounds the number of
// keys.
func (s *TrustedKeys) CreateTrustedKey(ctx context.Context, k domain.TrustedKey) error {
	val, err := encodeTrustedKey(k)
	if err != nil {
		return err
	}
	var b kv.Batch
	b.RequireAbsent(trustedKeyKey(k.ID))
	b.Put(trustedKeyKey(k.ID), val)
	if _, err := s.db.Apply(ctx, &b); err != nil {
		if errors.Is(err, kv.ErrConflict) {
			return domain.ErrAlreadyExists
		}
		return fmt.Errorf("create trusted key: %w", err)
	}
	return nil
}

// DeleteTrustedKey removes signing/trusted/<id>, or returns domain.ErrNotFound.
func (s *TrustedKeys) DeleteTrustedKey(ctx context.Context, id string) error {
	if !validTrustedKeyID(id) {
		return domain.ErrNotFound
	}
	for range updateAttempts {
		item, ok := s.db.Get(ctx, trustedKeyKey(id))
		if !ok {
			return domain.ErrNotFound
		}
		var b kv.Batch
		b.RequireVersion(trustedKeyKey(id), item.Version)
		b.Delete(trustedKeyKey(id))
		_, err := s.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue // changed or removed meanwhile: read again
		}
		if err != nil {
			return fmt.Errorf("delete trusted key: %w", err)
		}
		return nil
	}
	return fmt.Errorf("delete trusted key: %w", domain.ErrConflict)
}

// validTrustedKeyID reports whether id can be a key id: a fingerprint, so
// non-empty, bounded, valid UTF-8 text.
func validTrustedKeyID(id string) bool {
	return id != "" && len(id) <= maxTrustedKeyIDLen && utf8.ValidString(id)
}

func encodeTrustedKey(k domain.TrustedKey) ([]byte, error) {
	if !validTrustedKeyID(k.ID) {
		return nil, fmt.Errorf("%w: trusted key id has %d bytes", domain.ErrInvalid, len(k.ID))
	}
	texts := append([]string{string(k.Format), k.Title, k.PublicKey}, k.Emails...)
	for _, s := range texts {
		if !utf8.ValidString(s) {
			return nil, fmt.Errorf("%w: trusted key text is not valid UTF-8", domain.ErrInvalid)
		}
	}
	return mustMarshal(&storagepb.TrustedKey{
		Id:        k.ID,
		Format:    string(k.Format),
		Title:     k.Title,
		PublicKey: k.PublicKey,
		Emails:    k.Emails,
		CreatedAt: encodeTime(k.CreatedAt),
	}), nil
}

func decodeTrustedKey(val []byte) (domain.TrustedKey, error) {
	var pb storagepb.TrustedKey
	if err := proto.Unmarshal(val, &pb); err != nil {
		return domain.TrustedKey{}, fmt.Errorf("decode trusted key: %w", err)
	}
	if !validTrustedKeyID(pb.GetId()) {
		return domain.TrustedKey{}, fmt.Errorf("decode trusted key: id has %d bytes", len(pb.GetId()))
	}
	return domain.TrustedKey{
		ID:        pb.GetId(),
		Format:    domain.SignatureFormat(pb.GetFormat()),
		Title:     pb.GetTitle(),
		PublicKey: pb.GetPublicKey(),
		Emails:    slices.Clone(pb.GetEmails()),
		CreatedAt: decodeTime(pb.GetCreatedAt()),
	}, nil
}
