// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

func TestTouchActiveAt(t *testing.T) {
	name, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	now := time.Now().UTC()
	t.Run("preserves later activity", func(t *testing.T) {
		meta := mocks.NewRepoMetaStore(t)
		later := domain.Repo{Name: name, ActiveAt: now.Add(time.Hour)}
		meta.EXPECT().Mutate(mock.Anything, name, mock.Anything).RunAndReturn(func(_ context.Context, _ domain.RepoName, fn func(*domain.Repo) (bool, error)) (domain.Repo, error) {
			repo := later
			changed, err := fn(&repo)
			require.NoError(t, err)
			require.False(t, changed)
			return repo, nil
		})
		got, err := touchActiveAt(t.Context(), meta, name, now)
		require.NoError(t, err)
		require.Equal(t, later, got)
	})
	t.Run("deleted repository", func(t *testing.T) {
		meta := mocks.NewRepoMetaStore(t)
		meta.EXPECT().Mutate(mock.Anything, name, mock.Anything).Return(domain.Repo{}, domain.ErrNotFound)
		_, err := touchActiveAt(t.Context(), meta, name, now)
		require.NoError(t, err)
	})
	t.Run("best effort failure", func(t *testing.T) {
		meta := mocks.NewRepoMetaStore(t)
		meta.EXPECT().Mutate(mock.Anything, name, mock.Anything).Return(domain.Repo{}, errors.New("offline"))
		require.NotPanics(t, func() { recordActivity(t.Context(), meta, name, now) })
	})
}
