// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
)

func newToken(t *testing.T, id, secret string) domain.Token {
	t.Helper()
	return domain.Token{
		ID:        id,
		Label:     "label " + id,
		Hash:      domain.HashTokenSecret(secret),
		Scopes:    []domain.Scope{domain.ScopeRepoRead, domain.ScopeRepoWrite},
		Repos:     []domain.RepoName{name(t, "a"), name(t, "b")},
		ExpiresAt: created.Add(24 * time.Hour),
		CreatedAt: created,
	}
}

func TestTokensRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	ts := s.Tokens()

	tok := newToken(t, "t1", "secret-1")
	require.NoError(t, ts.Create(ctx, tok))

	got, err := ts.FindByHash(ctx, tok.Hash)
	require.NoError(t, err)
	assert.Equal(t, tok, got)

	_, err = ts.FindByHash(ctx, domain.HashTokenSecret("other"))
	assert.ErrorIs(t, err, domain.ErrNotFound)

	// Minimal token: no repos, no expiry.
	min := domain.Token{ID: "t2", Hash: domain.HashTokenSecret("secret-2"), Scopes: []domain.Scope{domain.ScopeRepoRead}, CreatedAt: created.Add(time.Minute)}
	require.NoError(t, ts.Create(ctx, min))
	got, err = ts.FindByHash(ctx, min.Hash)
	require.NoError(t, err)
	assert.Empty(t, got.Repos)
	assert.True(t, got.ExpiresAt.IsZero())
	assert.True(t, got.LastUsedAt.IsZero())

	list, err := ts.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "t1", list[0].ID)
	assert.Equal(t, "t2", list[1].ID)
}

func TestTokensCreateRejects(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	ts := s.Tokens()
	require.NoError(t, ts.Create(ctx, newToken(t, "t1", "s1")))

	assert.ErrorIs(t, ts.Create(ctx, newToken(t, "t1", "other")), domain.ErrAlreadyExists, "same id")
	assert.ErrorIs(t, ts.Create(ctx, newToken(t, "t9", "s1")), domain.ErrAlreadyExists, "same hash")

	// A failed create leaves no half-written index.
	_, err := ts.FindByHash(ctx, domain.HashTokenSecret("other"))
	assert.ErrorIs(t, err, domain.ErrNotFound)
	list, err := ts.List(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	bad := newToken(t, "t3", "s3")
	bad.Scopes = nil
	assert.ErrorIs(t, ts.Create(ctx, bad), domain.ErrInvalidToken)
	bad = newToken(t, "t3", "s3")
	bad.Scopes = []domain.Scope{"nope"}
	assert.ErrorIs(t, ts.Create(ctx, bad), domain.ErrInvalidScope)
	bad = newToken(t, "a/b", "s3")
	assert.ErrorIs(t, ts.Create(ctx, bad), domain.ErrInvalidToken)
	bad = newToken(t, "", "s3")
	assert.ErrorIs(t, ts.Create(ctx, bad), domain.ErrInvalidToken)
}

func TestTokensRevokeKeepsIndexConsistent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openStore(t, dir)
	ts := s.Tokens()
	a, b := newToken(t, "a", "sa"), newToken(t, "b", "sb")
	require.NoError(t, ts.Create(ctx, a))
	require.NoError(t, ts.Create(ctx, b))

	require.NoError(t, ts.Revoke(ctx, "a"))
	_, err := ts.FindByHash(ctx, a.Hash)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	got, err := ts.FindByHash(ctx, b.Hash)
	require.NoError(t, err)
	assert.Equal(t, "b", got.ID)
	assert.ErrorIs(t, ts.Revoke(ctx, "a"), domain.ErrNotFound)
	assert.ErrorIs(t, ts.Revoke(ctx, "../x"), domain.ErrNotFound)

	// The hash of a revoked token can be used again, and the index survives a restart.
	require.NoError(t, ts.Create(ctx, newToken(t, "c", "sa")))
	require.NoError(t, s.Close(ctx))
	s2 := openStore(t, dir)
	got, err = s2.Tokens().FindByHash(ctx, a.Hash)
	require.NoError(t, err)
	assert.Equal(t, "c", got.ID)
	list, err := s2.Tokens().List(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 2)
}

func TestTokensGet(t *testing.T) {
	ctx := context.Background()
	ts := openStore(t, t.TempDir()).Tokens()
	tok := newToken(t, "t1", "s1")
	require.NoError(t, ts.Create(ctx, tok))

	got, err := ts.Get(ctx, "t1")
	require.NoError(t, err)
	assert.Equal(t, tok, got)

	for _, id := range []string{"gone", "", "../t1"} {
		_, err = ts.Get(ctx, id)
		assert.ErrorIs(t, err, domain.ErrNotFound, id)
	}
	require.NoError(t, ts.Revoke(ctx, "t1"))
	_, err = ts.Get(ctx, "t1")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestTokensTouch(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	ts := s.Tokens()
	tok := newToken(t, "t1", "s1")
	require.NoError(t, ts.Create(ctx, tok))

	at := created.Add(time.Hour)
	require.NoError(t, ts.Touch(ctx, "t1", at))
	got, err := ts.FindByHash(ctx, tok.Hash)
	require.NoError(t, err)
	assert.True(t, at.Equal(got.LastUsedAt))

	// An older time does not move it back.
	require.NoError(t, ts.Touch(ctx, "t1", created))
	got, err = ts.FindByHash(ctx, tok.Hash)
	require.NoError(t, err)
	assert.True(t, at.Equal(got.LastUsedAt))

	assert.ErrorIs(t, ts.Touch(ctx, "gone", at), domain.ErrNotFound)

	// Everything else is untouched.
	got.LastUsedAt = time.Time{}
	assert.Equal(t, tok, got)
}

func TestTokensConcurrentTouchAndRevoke(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	ts := s.Tokens()
	tok := newToken(t, "t1", "s1")
	require.NoError(t, ts.Create(ctx, tok))

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = ts.Touch(ctx, "t1", created.Add(time.Duration(i+1)*time.Second))
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); _ = ts.Revoke(ctx, "t1") }()
	wg.Wait()

	// Whatever the interleaving, token and index are both there or both gone.
	_, errTok := ts.FindByHash(ctx, tok.Hash)
	list, err := ts.List(ctx)
	require.NoError(t, err)
	if errTok != nil {
		assert.ErrorIs(t, errTok, domain.ErrNotFound, fmt.Sprint(errTok))
		assert.Empty(t, list)
	} else {
		assert.Len(t, list, 1)
	}
}

func TestTokensBranchPrefixes(t *testing.T) {
	ctx := context.Background()

	t.Run("round trip, order kept", func(t *testing.T) {
		s := openStore(t, t.TempDir())
		tok := newToken(t, "t1", "s1")
		tok.BranchPrefixes = []string{"b/", "agent/claude/"}
		require.NoError(t, s.Tokens().Create(ctx, tok))
		got, err := s.Tokens().Get(ctx, "t1")
		require.NoError(t, err)
		assert.Equal(t, tok, got)
		byHash, err := s.Tokens().FindByHash(ctx, tok.Hash)
		require.NoError(t, err)
		assert.Equal(t, []string{"b/", "agent/claude/"}, byHash.BranchPrefixes)
	})

	t.Run("a token stored before the field existed decodes with no prefixes", func(t *testing.T) {
		dir := t.TempDir()
		old := newToken(t, "old", "s-old")
		putRaw(t, dir, "token/old", mustProto(t, &storagepb.Token{
			Id: "old", Label: "old", Hash: old.Hash[:], Scopes: []string{"repo:write"},
		}))
		s := openStore(t, dir)
		got, err := s.Tokens().Get(ctx, "old")
		require.NoError(t, err)
		assert.Empty(t, got.BranchPrefixes)
		assert.Equal(t, []domain.Scope{domain.ScopeRepoWrite}, got.Scopes)
	})

	t.Run("an invalid prefix is not stored", func(t *testing.T) {
		s := openStore(t, t.TempDir())
		for _, bad := range []string{"", "agent", "/", "a b/"} {
			tok := newToken(t, "t1", "s1")
			tok.BranchPrefixes = []string{bad}
			assert.ErrorIs(t, s.Tokens().Create(ctx, tok), domain.ErrInvalidToken, "%q", bad)
		}
		_, err := s.Tokens().Get(ctx, "t1")
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("a stored prefix survives touch and stays limiting", func(t *testing.T) {
		s := openStore(t, t.TempDir())
		tok := newToken(t, "t1", "s1")
		tok.BranchPrefixes = []string{"agent/"}
		require.NoError(t, s.Tokens().Create(ctx, tok))
		require.NoError(t, s.Tokens().Touch(ctx, "t1", created.Add(time.Hour)))
		got, err := s.Tokens().Get(ctx, "t1")
		require.NoError(t, err)
		assert.Equal(t, []string{"agent/"}, got.BranchPrefixes)
	})
}
