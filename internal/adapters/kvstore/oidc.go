// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"fmt"
	"slices"
	"strings"

	kv "bnema.dev/ermite-kv"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Key layout of the signing keys of the OIDC ID tokens (CI-4):
//
//	oidc/key/<kid>    OIDCKey (the private key sealed, see secretbox)
const oidcKeyPrefix = "oidc/key/"

var _ ports.OIDCKeyStore = (*Store)(nil)

// ListOIDCKeys returns every stored key, oldest first.
func (s *Store) ListOIDCKeys(ctx context.Context) ([]domain.OIDCKey, error) {
	var out []domain.OIDCKey
	for key, item := range s.db.Snapshot(ctx).Prefix(ctx, []byte(oidcKeyPrefix)) {
		var pb storagepb.OIDCKey
		if err := proto.Unmarshal(item.Value, &pb); err != nil {
			return nil, fmt.Errorf("decode oidc key %s: %w", key, err)
		}
		out = append(out, domain.OIDCKey{
			ID: strings.TrimPrefix(string(key), oidcKeyPrefix), Sealed: pb.GetSealed(),
			CreatedAt: decodeTime(pb.GetCreatedAt()), RetiredAt: decodeTime(pb.GetRetiredAt()),
		})
	}
	slices.SortStableFunc(out, func(a, b domain.OIDCKey) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

// SaveOIDCKeys writes the keys of put (created or replaced) and deletes those
// of drop, in one batch.
func (s *Store) SaveOIDCKeys(ctx context.Context, put []domain.OIDCKey, drop []string) error {
	var b kv.Batch
	for _, k := range put {
		if k.ID == "" || strings.Contains(k.ID, "/") {
			return fmt.Errorf("%w: oidc key id %q", domain.ErrInvalid, k.ID)
		}
		b.Put([]byte(domain.OIDCKeyPath(k.ID)), mustMarshal(&storagepb.OIDCKey{
			Sealed: k.Sealed, CreatedAt: encodeTime(k.CreatedAt), RetiredAt: encodeTime(k.RetiredAt),
		}))
	}
	for _, id := range drop {
		b.Delete([]byte(domain.OIDCKeyPath(id)))
	}
	if _, err := s.db.Apply(ctx, &b); err != nil {
		return fmt.Errorf("store the oidc keys: %w", err)
	}
	return nil
}

// RawOIDCKey returns the stored value of the key, as is. Tests use it to check
// that no private key is stored in the clear.
func (s *Store) RawOIDCKey(ctx context.Context, kid string) ([]byte, bool) {
	item, ok := s.db.Get(ctx, []byte(domain.OIDCKeyPath(kid)))
	if !ok {
		return nil, false
	}
	return item.Value, true
}
