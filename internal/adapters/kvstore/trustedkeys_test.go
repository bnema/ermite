// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func newTrustedKey(id string, at time.Time) domain.TrustedKey {
	return domain.TrustedKey{
		ID:        id,
		Format:    domain.FormatSSH,
		Title:     "key " + id,
		PublicKey: "ssh-ed25519 AAAA" + id,
		Emails:    []string{"ada@example.org", "ada@work.example"},
		CreatedAt: at,
	}
}

func TestTrustedKeysRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openStore(t, dir)
	ks := s.TrustedKeys()

	k := newTrustedKey("SHA256:abc/def+ghi", created)
	require.NoError(t, ks.CreateTrustedKey(ctx, k))
	gpg := domain.TrustedKey{
		ID: strings.Repeat("AB", 20), Format: domain.FormatGPG, Title: "gpg é",
		PublicKey: "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nxyz\n-----END PGP PUBLIC KEY BLOCK-----\n",
		Emails:    []string{"grace@example.org"}, CreatedAt: created.Add(time.Hour),
	}
	require.NoError(t, ks.CreateTrustedKey(ctx, gpg))

	list, err := ks.ListTrustedKeys(ctx)
	require.NoError(t, err)
	assert.Equal(t, []domain.TrustedKey{k, gpg}, list)
	assert.False(t, list[0].Builtin)

	// The keys survive a restart.
	require.NoError(t, s.Close(ctx))
	list, err = openStore(t, dir).TrustedKeys().ListTrustedKeys(ctx)
	require.NoError(t, err)
	assert.Equal(t, []domain.TrustedKey{k, gpg}, list)
}

func TestTrustedKeysEmptyAndInvalid(t *testing.T) {
	ctx := context.Background()
	ks := openStore(t, t.TempDir()).TrustedKeys()

	list, err := ks.ListTrustedKeys(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)

	assert.ErrorIs(t, ks.CreateTrustedKey(ctx, domain.TrustedKey{Title: "no id"}), domain.ErrInvalid)
	assert.ErrorIs(t, ks.CreateTrustedKey(ctx, newTrustedKey(strings.Repeat("x", 129), created)), domain.ErrInvalid)
	bad := newTrustedKey("k", created)
	bad.Title = "\xff"
	assert.ErrorIs(t, ks.CreateTrustedKey(ctx, bad), domain.ErrInvalid)
	list, err = ks.ListTrustedKeys(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestTrustedKeysDuplicateAndUnknown(t *testing.T) {
	ctx := context.Background()
	ks := openStore(t, t.TempDir()).TrustedKeys()
	k := newTrustedKey("SHA256:one", created)
	require.NoError(t, ks.CreateTrustedKey(ctx, k))

	dup := newTrustedKey("SHA256:one", created.Add(time.Hour))
	dup.Title = "other"
	assert.ErrorIs(t, ks.CreateTrustedKey(ctx, dup), domain.ErrAlreadyExists)
	list, err := ks.ListTrustedKeys(ctx)
	require.NoError(t, err)
	assert.Equal(t, []domain.TrustedKey{k}, list, "the first key is kept")

	assert.ErrorIs(t, ks.DeleteTrustedKey(ctx, "SHA256:nope"), domain.ErrNotFound)
	assert.ErrorIs(t, ks.DeleteTrustedKey(ctx, ""), domain.ErrNotFound)
}

func TestTrustedKeysDelete(t *testing.T) {
	ctx := context.Background()
	ks := openStore(t, t.TempDir()).TrustedKeys()
	a, b := newTrustedKey("a", created), newTrustedKey("b", created.Add(time.Hour))
	require.NoError(t, ks.CreateTrustedKey(ctx, a))
	require.NoError(t, ks.CreateTrustedKey(ctx, b))

	require.NoError(t, ks.DeleteTrustedKey(ctx, "a"))
	assert.ErrorIs(t, ks.DeleteTrustedKey(ctx, "a"), domain.ErrNotFound)
	list, err := ks.ListTrustedKeys(ctx)
	require.NoError(t, err)
	assert.Equal(t, []domain.TrustedKey{b}, list)

	// A deleted id can be trusted again.
	require.NoError(t, ks.CreateTrustedKey(ctx, a))
}

// List orders by creation time, then by id.
func TestTrustedKeysListOrder(t *testing.T) {
	ctx := context.Background()
	ks := openStore(t, t.TempDir()).TrustedKeys()
	for _, k := range []domain.TrustedKey{
		newTrustedKey("c", created.Add(2*time.Hour)),
		newTrustedKey("b", created),
		newTrustedKey("a", created),
		newTrustedKey("d", created.Add(time.Hour)),
	} {
		require.NoError(t, ks.CreateTrustedKey(ctx, k))
	}
	list, err := ks.ListTrustedKeys(ctx)
	require.NoError(t, err)
	var ids []string
	for _, k := range list {
		ids = append(ids, k.ID)
	}
	assert.Equal(t, []string{"a", "b", "d", "c"}, ids)
}
