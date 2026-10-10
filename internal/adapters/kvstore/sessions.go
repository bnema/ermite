// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	kv "bnema.dev/ermite-kv"
	"github.com/bnema/zerowrap"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

const (
	sessionKeyKey   = "session/key"
	sessionEpochKey = "session/epoch"

	// sessionKeyLen is the size of the HS256 secret, in bytes.
	sessionKeyLen = 32
)

// Sessions is the ermite-kv backed ports.SessionStore. It shares the database
// of the Store it comes from.
type Sessions struct {
	db *kv.DB
}

var _ ports.SessionStore = (*Sessions)(nil)

// Sessions returns the session store over the same database. It is valid
// until the Store is closed.
func (s *Store) Sessions() *Sessions { return &Sessions{db: s.db} }

// Key reads session/key and generates it when absent. The key is never
// logged.
func (s *Sessions) Key(ctx context.Context) ([]byte, error) {
	for range updateAttempts {
		if item, ok := s.db.Get(ctx, []byte(sessionKeyKey)); ok {
			var m storagepb.SessionKey
			if err := proto.Unmarshal(item.Value, &m); err != nil {
				return nil, fmt.Errorf("decode session key: %w", err)
			}
			if len(m.GetKey()) != sessionKeyLen {
				return nil, fmt.Errorf("decode session key: %d bytes, want %d", len(m.GetKey()), sessionKeyLen)
			}
			return append([]byte(nil), m.GetKey()...), nil
		}
		key := make([]byte, sessionKeyLen)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate session key: %w", err)
		}
		var b kv.Batch
		b.RequireAbsent([]byte(sessionKeyKey))
		b.Put([]byte(sessionKeyKey), mustMarshal(&storagepb.SessionKey{Key: key}))
		_, err := s.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue // written meanwhile: read it
		}
		if err != nil {
			return nil, fmt.Errorf("store session key: %w", err)
		}
		log := zerowrap.FromCtx(ctx)
		log.Info().Msg("session key generated")
		return key, nil
	}
	return nil, fmt.Errorf("store session key: %w", domain.ErrConflict)
}

// Epoch reads session/epoch; a missing key is epoch 0.
func (s *Sessions) Epoch(ctx context.Context) (uint64, error) {
	e, _, err := s.readEpoch(ctx)
	return e, err
}

func (s *Sessions) readEpoch(ctx context.Context) (epoch, version uint64, err error) {
	item, ok := s.db.Get(ctx, []byte(sessionEpochKey))
	if !ok {
		return 0, 0, nil
	}
	var m storagepb.SessionEpoch
	if err := proto.Unmarshal(item.Value, &m); err != nil {
		return 0, 0, fmt.Errorf("decode session epoch: %w", err)
	}
	return m.GetEpoch(), item.Version, nil
}

// BumpEpoch increments session/epoch with a version check, so that two
// concurrent bumps give two different values.
func (s *Sessions) BumpEpoch(ctx context.Context) (uint64, error) {
	for range updateAttempts {
		cur, version, err := s.readEpoch(ctx)
		if err != nil {
			return 0, err
		}
		var b kv.Batch
		if version == 0 {
			b.RequireAbsent([]byte(sessionEpochKey))
		} else {
			b.RequireVersion([]byte(sessionEpochKey), version)
		}
		b.Put([]byte(sessionEpochKey), mustMarshal(&storagepb.SessionEpoch{Epoch: cur + 1}))
		_, err = s.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("bump session epoch: %w", err)
		}
		return cur + 1, nil
	}
	return 0, fmt.Errorf("bump session epoch: %w", domain.ErrConflict)
}
