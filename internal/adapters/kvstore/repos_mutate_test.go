// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestRepoMutateInterleavedWrites(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	n := name(t, "proj")
	original := domain.Repo{Name: n, Visibility: domain.VisibilityPublic, CreatedAt: created}
	require.NoError(t, s.Create(ctx, original))
	later := created.Add(2 * time.Hour)
	calls := 0
	got, err := s.Mutate(ctx, n, func(r *domain.Repo) (bool, error) {
		calls++
		// Test-only interleaving: inject a committed write after the outer read.
		if calls == 1 {
			_, err := s.Mutate(ctx, n, func(current *domain.Repo) (bool, error) {
				current.Visibility = domain.VisibilityPrivate
				current.ActiveAt = later
				current.PushedAt = later
				return true, nil
			})
			require.NoError(t, err)
		}
		r.Description = "updated"
		at := created.Add(time.Hour)
		if at.After(r.ActiveAt) {
			r.ActiveAt = at
		}
		if at.After(r.PushedAt) {
			r.PushedAt = at
		}
		return true, nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Equal(t, domain.VisibilityPrivate, got.Visibility)
	require.Equal(t, "updated", got.Description)
	require.Equal(t, later, got.ActiveAt)
	require.Equal(t, later, got.PushedAt)
	stored, err := s.Get(ctx, n)
	require.NoError(t, err)
	require.Equal(t, got, stored)
}

func TestRepoMutateContracts(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	n := name(t, "proj")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: n, Visibility: domain.VisibilityPublic, CreatedAt: created}))
	original, err := s.Get(ctx, n)
	require.NoError(t, err)
	boom := errors.New("callback failed")
	_, err = s.Mutate(ctx, n, func(r *domain.Repo) (bool, error) { r.Description = "discard"; return true, boom })
	require.ErrorIs(t, err, boom)
	got, err := s.Mutate(ctx, n, func(r *domain.Repo) (bool, error) { r.Description = "discard"; return false, nil })
	require.NoError(t, err)
	require.Equal(t, original, got)
	for _, changed := range []bool{false, true} {
		_, err = s.Mutate(ctx, n, func(r *domain.Repo) (bool, error) { r.Name = name(t, "other"); return changed, nil })
		require.ErrorIs(t, err, domain.ErrInvalidName)
	}
	got, err = s.Get(ctx, n)
	require.NoError(t, err)
	require.Equal(t, original, got)
	calls := 0
	_, err = s.Mutate(ctx, n, func(r *domain.Repo) (bool, error) {
		calls++
		_, err := s.Mutate(ctx, n, func(current *domain.Repo) (bool, error) { current.Description = "concurrent"; return true, nil })
		require.NoError(t, err)
		return true, nil
	})
	require.ErrorIs(t, err, domain.ErrConflict)
	require.Equal(t, 8, calls)
	_, err = s.Mutate(ctx, n, func(r *domain.Repo) (bool, error) { require.NoError(t, s.Delete(ctx, n)); return true, nil })
	require.ErrorIs(t, err, domain.ErrNotFound)
}
