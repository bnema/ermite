// SPDX-License-Identifier: AGPL-3.0-or-later
package usecase

import (
	"testing"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"github.com/stretchr/testify/require"
)

// No ListReleases expectation: asset-only changes must not scan releases.
func TestAssetOnlyWriteDoesNotListReleases(t *testing.T) {
	ctx := t.Context()
	repo, err := domain.ParseRepoName("assets")
	require.NoError(t, err)
	store := mocks.NewReleaseStore(t)
	asset := domain.ReleaseAsset{ID: 1, Repo: repo}
	store.EXPECT().ReleaseUsage(ctx, repo).Return(int64(0), uint64(2), nil).Once()
	store.EXPECT().ApplyReleases(ctx, ports.ReleaseWrite{Repo: repo, UsageVersion: 2, PutAssets: []domain.ReleaseAsset{asset}}).Return(nil).Once()
	svc := &Releases{store: store}
	require.NoError(t, svc.updateReleases(ctx, repo, func(w *ports.ReleaseWrite) error {
		w.PutAssets = append(w.PutAssets, asset)
		return nil
	}))
}
