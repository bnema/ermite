// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	kv "bnema.dev/ermite-kv"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
)

const signingKeyKey = "signing/key"

// SigningSeedLen is the length of the stored seed of the signing key.
const SigningSeedLen = 32

// SigningSeed returns the 32-byte seed of the commit signing key (ADR 0022).
// When none is stored, it calls generate, stores what it returns and returns
// that; of two concurrent first calls one wins and both return its seed. The
// seed is the private key: neither it nor an error carrying it is logged.
func (s *Store) SigningSeed(ctx context.Context, generate func() ([]byte, error), now time.Time) ([]byte, error) {
	key := []byte(signingKeyKey)
	for range 2 {
		if item, ok := s.db.Get(ctx, key); ok {
			return decodeSigningSeed(item.Value)
		}
		seed, err := generate()
		if err != nil {
			return nil, fmt.Errorf("generate the signing key: %w", err)
		}
		if len(seed) != SigningSeedLen {
			return nil, fmt.Errorf("generate the signing key: seed has %d bytes, want %d", len(seed), SigningSeedLen)
		}
		var b kv.Batch
		b.RequireAbsent(key)
		b.Put(key, mustMarshal(&storagepb.SigningKey{Seed: seed, CreatedAt: encodeTime(now)}))
		_, err = s.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue // written meanwhile: read it
		}
		if err != nil {
			return nil, fmt.Errorf("store the signing key: %w", err)
		}
		return seed, nil
	}
	return nil, errors.New("store the signing key: concurrent initialization did not settle")
}

func decodeSigningSeed(val []byte) ([]byte, error) {
	var pb storagepb.SigningKey
	if err := proto.Unmarshal(val, &pb); err != nil {
		return nil, fmt.Errorf("decode the signing key: %w", err)
	}
	if len(pb.GetSeed()) != SigningSeedLen {
		return nil, fmt.Errorf("decode the signing key: seed has %d bytes, want %d", len(pb.GetSeed()), SigningSeedLen)
	}
	return pb.GetSeed(), nil
}
