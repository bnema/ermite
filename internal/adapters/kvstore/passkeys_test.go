// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func newPasskey(id string, at time.Time) domain.Passkey {
	return domain.Passkey{
		ID:              []byte(id),
		Name:            "key " + id,
		PublicKey:       []byte("cose-" + id),
		AttestationType: "none",
		Transports:      []string{"internal", "hybrid"},
		SignCount:       7,
		BackupEligible:  true,
		BackupState:     true,
		CreatedAt:       at,
		LastUsedAt:      at.Add(time.Hour),
	}
}

func TestPasskeysRoundTrip(t *testing.T) {
	ctx := context.Background()
	ps := openStore(t, t.TempDir()).Passkeys()

	p := newPasskey("cred-1", created)
	require.NoError(t, ps.Create(ctx, p))
	got, err := ps.Get(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, p, got)

	// Minimal passkey: no transports, never used. A binary id works too.
	min := domain.Passkey{ID: []byte{0, 1, 0xff, '/'}, Name: "min", PublicKey: []byte("k"), CreatedAt: created}
	require.NoError(t, ps.Create(ctx, min))
	got, err = ps.Get(ctx, min.ID)
	require.NoError(t, err)
	assert.Empty(t, got.Transports)
	assert.True(t, got.LastUsedAt.IsZero())
	assert.Equal(t, min.ID, got.ID)

	long := domain.Passkey{ID: bytes.Repeat([]byte{7}, domain.MaxPasskeyIDLen), Name: "long", PublicKey: []byte("k"), CreatedAt: created}
	require.NoError(t, ps.Create(ctx, long))
	tooLong := domain.Passkey{ID: bytes.Repeat([]byte{7}, domain.MaxPasskeyIDLen+1), Name: "x", PublicKey: []byte("k")}
	assert.ErrorIs(t, ps.Create(ctx, tooLong), domain.ErrInvalid)
	assert.ErrorIs(t, ps.Create(ctx, domain.Passkey{Name: "no id"}), domain.ErrInvalid)
}

func TestPasskeysDuplicateAndUnknown(t *testing.T) {
	ctx := context.Background()
	ps := openStore(t, t.TempDir()).Passkeys()
	p := newPasskey("cred-1", created)
	require.NoError(t, ps.Create(ctx, p))

	assert.ErrorIs(t, ps.Create(ctx, p), domain.ErrAlreadyExists)

	unknown := newPasskey("nope", created)
	_, err := ps.Get(ctx, unknown.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	assert.ErrorIs(t, ps.Update(ctx, unknown), domain.ErrNotFound)
	assert.ErrorIs(t, ps.Delete(ctx, unknown.ID), domain.ErrNotFound)
	_, err = ps.Get(ctx, nil)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	assert.ErrorIs(t, ps.Delete(ctx, nil), domain.ErrNotFound)
}

func TestPasskeysUpdateDelete(t *testing.T) {
	ctx := context.Background()
	ps := openStore(t, t.TempDir()).Passkeys()
	p := newPasskey("cred-1", created)
	require.NoError(t, ps.Create(ctx, p))

	p.SignCount = 42
	p.BackupState = false
	p.LastUsedAt = created.Add(48 * time.Hour)
	require.NoError(t, ps.Update(ctx, p))
	got, err := ps.Get(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, p, got)

	require.NoError(t, ps.Delete(ctx, p.ID))
	_, err = ps.Get(ctx, p.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	assert.ErrorIs(t, ps.Delete(ctx, p.ID), domain.ErrNotFound)
	list, err := ps.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)
}

// List orders by creation time, then by id.
func TestPasskeysListOrder(t *testing.T) {
	ctx := context.Background()
	ps := openStore(t, t.TempDir()).Passkeys()
	for _, p := range []domain.Passkey{
		newPasskey("c", created.Add(2*time.Hour)),
		newPasskey("b", created),
		newPasskey("a", created),
		newPasskey("d", created.Add(time.Hour)),
	} {
		require.NoError(t, ps.Create(ctx, p))
	}
	list, err := ps.List(ctx)
	require.NoError(t, err)
	var ids []string
	for _, p := range list {
		ids = append(ids, string(p.ID))
	}
	assert.Equal(t, []string{"a", "b", "d", "c"}, ids)
}

// The handle is generated once, stays stable across a restart, and concurrent
// first calls agree on it.
func TestPasskeysUserHandle(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openStore(t, dir)

	var wg sync.WaitGroup
	handles := make([][]byte, 8)
	for i := range handles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := s.Passkeys().UserHandle(ctx)
			assert.NoError(t, err)
			handles[i] = h
		}()
	}
	wg.Wait()
	handle := handles[0]
	assert.Len(t, handle, 32)
	for _, h := range handles[1:] {
		assert.Equal(t, handle, h)
	}
	require.NoError(t, s.Close(ctx))

	s = openStore(t, dir)
	again, err := s.Passkeys().UserHandle(ctx)
	require.NoError(t, err)
	assert.Equal(t, handle, again, "a restart keeps the handle")
}
