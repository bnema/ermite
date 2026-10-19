// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	kv "bnema.dev/ermite-kv"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

const (
	tokenPrefix     = "token/"
	tokenHashPrefix = "tokenhash/"

	// updateAttempts bounds the retries of a read-modify-write that loses a
	// race with another write.
	updateAttempts = 5
)

// Tokens is the ermite-kv backed ports.TokenStore. It shares the database of
// the Store it comes from.
type Tokens struct {
	db *kv.DB
}

var _ ports.TokenStore = (*Tokens)(nil)

// Tokens returns the token store over the same database. It is valid until
// the Store is closed.
func (s *Store) Tokens() *Tokens { return &Tokens{db: s.db} }

func tokenKey(id string) []byte              { return []byte(tokenPrefix + id) }
func tokenHashKey(h domain.TokenHash) []byte { return []byte(tokenHashPrefix + h.String()) }

// validTokenID reports whether id is usable inside a key.
func validTokenID(id string) bool {
	return id != "" && len(id) <= 64 && !strings.ContainsAny(id, "/\x00") && utf8.ValidString(id)
}

// Create stores token/<id> and tokenhash/<hex> in one batch, each with an
// "absent" precondition: a taken id or hash yields domain.ErrAlreadyExists.
func (s *Tokens) Create(ctx context.Context, t domain.Token) error {
	val, err := encodeToken(t)
	if err != nil {
		return err
	}
	var b kv.Batch
	b.RequireAbsent(tokenKey(t.ID))
	b.RequireAbsent(tokenHashKey(t.Hash))
	b.Put(tokenKey(t.ID), val)
	b.Put(tokenHashKey(t.Hash), []byte(t.ID))
	if _, err := s.db.Apply(ctx, &b); err != nil {
		if errors.Is(err, kv.ErrConflict) {
			return domain.ErrAlreadyExists
		}
		return fmt.Errorf("create token %q: %w", t.ID, err)
	}
	return nil
}

// Get reads token/<id>.
func (s *Tokens) Get(ctx context.Context, id string) (domain.Token, error) {
	if !validTokenID(id) {
		return domain.Token{}, domain.ErrNotFound
	}
	item, ok := s.db.Get(ctx, tokenKey(id))
	if !ok {
		return domain.Token{}, domain.ErrNotFound
	}
	return decodeToken(item.Value)
}

// FindByHash resolves the hash index, then reads the token.
func (s *Tokens) FindByHash(ctx context.Context, hash domain.TokenHash) (domain.Token, error) {
	snap := s.db.Snapshot(ctx) // one snapshot: the index and the token agree
	idx, ok := snap.Get(ctx, tokenHashKey(hash))
	if !ok {
		return domain.Token{}, domain.ErrNotFound
	}
	item, ok := snap.Get(ctx, tokenKey(string(idx.Value)))
	if !ok {
		return domain.Token{}, domain.ErrNotFound
	}
	t, err := decodeToken(item.Value)
	if err != nil {
		return domain.Token{}, err
	}
	if t.Hash != hash {
		return domain.Token{}, fmt.Errorf("token %q: hash index does not match the stored hash", t.ID)
	}
	return t, nil
}

// List returns every token ordered by creation time, then id.
func (s *Tokens) List(ctx context.Context) ([]domain.Token, error) {
	var out []domain.Token
	for key, item := range s.db.Snapshot(ctx).Prefix(ctx, []byte(tokenPrefix)) {
		t, err := decodeToken(item.Value)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		out = append(out, t)
	}
	slices.SortStableFunc(out, func(a, b domain.Token) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

// Revoke deletes token/<id> and its hash index in one batch.
func (s *Tokens) Revoke(ctx context.Context, id string) error {
	if !validTokenID(id) {
		return domain.ErrNotFound
	}
	for range updateAttempts {
		item, ok := s.db.Get(ctx, tokenKey(id))
		if !ok {
			return domain.ErrNotFound
		}
		t, err := decodeToken(item.Value)
		if err != nil {
			return err
		}
		var b kv.Batch
		b.RequireVersion(tokenKey(id), item.Version)
		b.Delete(tokenKey(id))
		b.Delete(tokenHashKey(t.Hash))
		_, err = s.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue // touched or revoked meanwhile: read again
		}
		if err != nil {
			return fmt.Errorf("revoke token %q: %w", id, err)
		}
		return nil
	}
	return fmt.Errorf("revoke token %q: %w", id, domain.ErrConflict)
}

// Touch sets last_used_at when the given time is later than the stored one,
// so that racing writers never move it back.
func (s *Tokens) Touch(ctx context.Context, id string, at time.Time) error {
	if !validTokenID(id) {
		return domain.ErrNotFound
	}
	for range updateAttempts {
		item, ok := s.db.Get(ctx, tokenKey(id))
		if !ok {
			return domain.ErrNotFound
		}
		t, err := decodeToken(item.Value)
		if err != nil {
			return err
		}
		if !at.After(t.LastUsedAt) {
			return nil
		}
		t.LastUsedAt = at
		val, err := encodeToken(t)
		if err != nil {
			return err
		}
		var b kv.Batch
		b.RequireVersion(tokenKey(id), item.Version)
		b.Put(tokenKey(id), val)
		_, err = s.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue
		}
		if err != nil {
			return fmt.Errorf("touch token %q: %w", id, err)
		}
		return nil
	}
	return fmt.Errorf("touch token %q: %w", id, domain.ErrConflict)
}

// encodeToken refuses what the use case already refused (scopes, branch
// prefixes): a malformed prefix must never reach the store, where it would
// limit a token wrongly or not at all.
func encodeToken(t domain.Token) ([]byte, error) {
	if !validTokenID(t.ID) {
		return nil, fmt.Errorf("%w: invalid id %q", domain.ErrInvalidToken, t.ID)
	}
	if !utf8.ValidString(t.Label) {
		return nil, fmt.Errorf("%w: label is not valid UTF-8", domain.ErrInvalidToken)
	}
	if len(t.Scopes) == 0 {
		return nil, fmt.Errorf("%w: no scopes", domain.ErrInvalidToken)
	}
	pb := &storagepb.Token{
		Id:         t.ID,
		Label:      t.Label,
		Hash:       t.Hash[:],
		ExpiresAt:  encodeTime(t.ExpiresAt),
		CreatedAt:  encodeTime(t.CreatedAt),
		LastUsedAt: encodeTime(t.LastUsedAt),
	}
	for _, sc := range t.Scopes {
		if _, err := domain.ParseScope(sc.String()); err != nil {
			return nil, err
		}
		pb.Scopes = append(pb.Scopes, sc.String())
	}
	for _, r := range t.Repos {
		if r.IsZero() {
			return nil, fmt.Errorf("%w: empty repository name", domain.ErrInvalidName)
		}
		pb.Repos = append(pb.Repos, r.String())
	}
	for _, bp := range t.BranchPrefixes {
		if _, err := domain.ParseBranchPrefix(bp); err != nil {
			return nil, err
		}
		pb.BranchPrefixes = append(pb.BranchPrefixes, bp)
	}
	return mustMarshal(pb), nil
}

// decodeToken drops scopes it does not know (written by a newer ermite): a
// token never gains rights by being read by an older build.
func decodeToken(val []byte) (domain.Token, error) {
	var pb storagepb.Token
	if err := proto.Unmarshal(val, &pb); err != nil {
		return domain.Token{}, fmt.Errorf("decode token: %w", err)
	}
	t := domain.Token{
		ID:         pb.GetId(),
		Label:      pb.GetLabel(),
		ExpiresAt:  decodeTime(pb.GetExpiresAt()),
		CreatedAt:  decodeTime(pb.GetCreatedAt()),
		LastUsedAt: decodeTime(pb.GetLastUsedAt()),
	}
	if len(pb.GetHash()) != len(t.Hash) {
		return domain.Token{}, fmt.Errorf("decode token %q: hash has %d bytes", t.ID, len(pb.GetHash()))
	}
	copy(t.Hash[:], pb.GetHash())
	for _, s := range pb.GetScopes() {
		if sc, err := domain.ParseScope(s); err == nil {
			t.Scopes = append(t.Scopes, sc)
		}
	}
	for _, r := range pb.GetRepos() {
		name, err := domain.ParseRepoName(r)
		if err != nil {
			return domain.Token{}, fmt.Errorf("decode token %q: %w", t.ID, err)
		}
		t.Repos = append(t.Repos, name)
	}
	// Kept as stored: a token with prefixes stays limited, whatever they say.
	t.BranchPrefixes = slices.Clone(pb.GetBranchPrefixes())
	return t, nil
}
