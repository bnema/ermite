// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	kv "bnema.dev/ermite-kv"
	"github.com/bnema/zerowrap"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

const (
	passkeyUserKey    = "passkey/user"
	passkeyCredPrefix = "passkey/cred/"

	// passkeyHandleLen is the size of the WebAuthn user handle, in bytes.
	passkeyHandleLen = 32
)

// Passkeys is the ermite-kv backed ports.PasskeyStore. It shares the database
// of the Store it comes from.
type Passkeys struct {
	db *kv.DB
}

var _ ports.PasskeyStore = (*Passkeys)(nil)

// Passkeys returns the passkey store over the same database. It is valid until
// the Store is closed.
func (s *Store) Passkeys() *Passkeys { return &Passkeys{db: s.db} }

func passkeyKey(id []byte) []byte {
	return []byte(passkeyCredPrefix + base64.RawURLEncoding.EncodeToString(id))
}

// validPasskeyID reports whether id can be a credential id.
func validPasskeyID(id []byte) bool { return len(id) > 0 && len(id) <= domain.MaxPasskeyIDLen }

// UserHandle reads passkey/user and generates it when absent. The handle is
// stable for the life of the store.
func (s *Passkeys) UserHandle(ctx context.Context) ([]byte, error) {
	for range updateAttempts {
		if item, ok := s.db.Get(ctx, []byte(passkeyUserKey)); ok {
			var m storagepb.PasskeyUser
			if err := proto.Unmarshal(item.Value, &m); err != nil {
				return nil, fmt.Errorf("decode passkey user: %w", err)
			}
			if len(m.GetHandle()) != passkeyHandleLen {
				return nil, fmt.Errorf("decode passkey user: %d bytes, want %d", len(m.GetHandle()), passkeyHandleLen)
			}
			return bytes.Clone(m.GetHandle()), nil
		}
		handle := make([]byte, passkeyHandleLen)
		if _, err := rand.Read(handle); err != nil {
			return nil, fmt.Errorf("generate passkey user handle: %w", err)
		}
		var b kv.Batch
		b.RequireAbsent([]byte(passkeyUserKey))
		b.Put([]byte(passkeyUserKey), mustMarshal(&storagepb.PasskeyUser{Handle: handle}))
		_, err := s.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue // written meanwhile: read it
		}
		if err != nil {
			return nil, fmt.Errorf("store passkey user handle: %w", err)
		}
		log := zerowrap.FromCtx(ctx)
		log.Info().Msg("passkey user handle generated")
		return handle, nil
	}
	return nil, fmt.Errorf("store passkey user handle: %w", domain.ErrConflict)
}

// List returns the passkeys ordered by creation time, then id.
func (s *Passkeys) List(ctx context.Context) ([]domain.Passkey, error) {
	var out []domain.Passkey
	for key, item := range s.db.Snapshot(ctx).Prefix(ctx, []byte(passkeyCredPrefix)) {
		p, err := decodePasskey(item.Value)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		out = append(out, p)
	}
	slices.SortStableFunc(out, func(a, b domain.Passkey) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return bytes.Compare(a.ID, b.ID)
	})
	return out, nil
}

// Get reads passkey/cred/<id>.
func (s *Passkeys) Get(ctx context.Context, id []byte) (domain.Passkey, error) {
	if !validPasskeyID(id) {
		return domain.Passkey{}, domain.ErrNotFound
	}
	item, ok := s.db.Get(ctx, passkeyKey(id))
	if !ok {
		return domain.Passkey{}, domain.ErrNotFound
	}
	return decodePasskey(item.Value)
}

// Create stores passkey/cred/<id> with an "absent" precondition: a taken id
// yields domain.ErrAlreadyExists. The caller bounds the number of passkeys.
func (s *Passkeys) Create(ctx context.Context, p domain.Passkey) error {
	val, err := encodePasskey(p)
	if err != nil {
		return err
	}
	var b kv.Batch
	b.RequireAbsent(passkeyKey(p.ID))
	b.Put(passkeyKey(p.ID), val)
	if _, err := s.db.Apply(ctx, &b); err != nil {
		if errors.Is(err, kv.ErrConflict) {
			return domain.ErrAlreadyExists
		}
		return fmt.Errorf("create passkey: %w", err)
	}
	return nil
}

// Update replaces a stored passkey, or returns domain.ErrNotFound.
//
//hexcheck:ignore suspicious-business-logic-in-adapter version-checked replace, not a policy
func (s *Passkeys) Update(ctx context.Context, p domain.Passkey) error {
	val, err := encodePasskey(p)
	if err != nil {
		return err
	}
	for range updateAttempts {
		item, ok := s.db.Get(ctx, passkeyKey(p.ID))
		if !ok {
			return domain.ErrNotFound
		}
		var b kv.Batch
		b.RequireVersion(passkeyKey(p.ID), item.Version)
		b.Put(passkeyKey(p.ID), val)
		_, err := s.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue // changed or removed meanwhile: read again
		}
		if err != nil {
			return fmt.Errorf("update passkey: %w", err)
		}
		return nil
	}
	return fmt.Errorf("update passkey: %w", domain.ErrConflict)
}

// Delete removes passkey/cred/<id>, or returns domain.ErrNotFound.
func (s *Passkeys) Delete(ctx context.Context, id []byte) error {
	if !validPasskeyID(id) {
		return domain.ErrNotFound
	}
	for range updateAttempts {
		item, ok := s.db.Get(ctx, passkeyKey(id))
		if !ok {
			return domain.ErrNotFound
		}
		var b kv.Batch
		b.RequireVersion(passkeyKey(id), item.Version)
		b.Delete(passkeyKey(id))
		_, err := s.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue
		}
		if err != nil {
			return fmt.Errorf("delete passkey: %w", err)
		}
		return nil
	}
	return fmt.Errorf("delete passkey: %w", domain.ErrConflict)
}

func encodePasskey(p domain.Passkey) ([]byte, error) {
	if !validPasskeyID(p.ID) {
		return nil, fmt.Errorf("%w: passkey id has %d bytes", domain.ErrInvalid, len(p.ID))
	}
	if !utf8.ValidString(p.Name) || !utf8.ValidString(p.AttestationType) {
		return nil, fmt.Errorf("%w: passkey text is not valid UTF-8", domain.ErrInvalid)
	}
	for _, t := range p.Transports {
		if !utf8.ValidString(t) {
			return nil, fmt.Errorf("%w: passkey transport is not valid UTF-8", domain.ErrInvalid)
		}
	}
	return mustMarshal(&storagepb.Passkey{
		Id:              p.ID,
		Name:            p.Name,
		PublicKey:       p.PublicKey,
		AttestationType: p.AttestationType,
		Transports:      p.Transports,
		SignCount:       p.SignCount,
		BackupEligible:  p.BackupEligible,
		BackupState:     p.BackupState,
		CreatedAt:       encodeTime(p.CreatedAt),
		LastUsedAt:      encodeTime(p.LastUsedAt),
	}), nil
}

func decodePasskey(val []byte) (domain.Passkey, error) {
	var pb storagepb.Passkey
	if err := proto.Unmarshal(val, &pb); err != nil {
		return domain.Passkey{}, fmt.Errorf("decode passkey: %w", err)
	}
	if !validPasskeyID(pb.GetId()) {
		return domain.Passkey{}, fmt.Errorf("decode passkey: id has %d bytes", len(pb.GetId()))
	}
	return domain.Passkey{
		ID:              bytes.Clone(pb.GetId()),
		Name:            pb.GetName(),
		PublicKey:       bytes.Clone(pb.GetPublicKey()),
		AttestationType: pb.GetAttestationType(),
		Transports:      slices.Clone(pb.GetTransports()),
		SignCount:       pb.GetSignCount(),
		BackupEligible:  pb.GetBackupEligible(),
		BackupState:     pb.GetBackupState(),
		CreatedAt:       decodeTime(pb.GetCreatedAt()),
		LastUsedAt:      decodeTime(pb.GetLastUsedAt()),
	}, nil
}
