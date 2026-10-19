// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/usecase"
)

func TestActivityTimestampsOverStore(t *testing.T) {
	ctx := t.Context()
	s, err := kvstore.Open(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close(ctx)) })
	n, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	require.NoError(t, s.Create(ctx, domain.Repo{Name: n, Visibility: domain.VisibilityPrivate, CreatedAt: at}))
	got, err := usecase.TouchPushedAt(ctx, s, n, at.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, got.PushedAt, got.ActiveAt)
	_, err = usecase.TouchActiveAt(ctx, s, n, at.Add(3*time.Hour))
	require.NoError(t, err)
	_, err = usecase.TouchPushedAt(ctx, s, n, at.Add(2*time.Hour))
	require.NoError(t, err)
	_, err = usecase.TouchPushedAt(ctx, s, n, at)
	require.NoError(t, err)
	_, err = usecase.TouchActiveAt(ctx, s, n, at)
	require.NoError(t, err)
	got, err = s.Get(ctx, n)
	require.NoError(t, err)
	require.Equal(t, at.Add(2*time.Hour), got.PushedAt)
	require.Equal(t, at.Add(3*time.Hour), got.ActiveAt)
	require.Equal(t, domain.VisibilityPrivate, got.Visibility)
	require.NoError(t, s.Delete(ctx, n))
	_, err = usecase.TouchPushedAt(ctx, s, n, at)
	require.NoError(t, err)
	_, err = usecase.TouchActiveAt(ctx, s, n, at)
	require.NoError(t, err)
}
