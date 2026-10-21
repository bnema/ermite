// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"testing"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestReleaseTagDeletionHook(t *testing.T) {
	ctx := context.Background()
	n := name(t, "tools")
	f := newPushFixture(t)
	trigger := mocks.NewReleaseTagTrigger(t)
	f.pushes.WithReleases(trigger)
	updates := []domain.RefUpdate{{Old: oidA, New: domain.ZeroOID, Ref: "refs/tags/v1"}}
	expectPushMutation(t, f.meta, domain.Repo{Name: n}, nil)
	f.git.EXPECT().DefaultBranch(ctx, n).Return("main", true, nil)
	trigger.EXPECT().OnRefsMoved(ctx, n, updates).Return().Once()
	f.bus.EXPECT().Publish(ctx, mock.Anything).Return().Once()
	f.sched.EXPECT().Schedule(ctx, n).Return().Once()
	require.NoError(t, f.pushes.Record(ctx, domain.Root(), n, updates))
}

func TestReleaseRepositoryDeletionHook(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "deleted", true: "metadata failure"}[failed], func(t *testing.T) {
			ctx := context.Background()
			n := name(t, "tools")
			f := newFixture(t)
			cleaner := mocks.NewReleaseRepoCleaner(t)
			f.repos.WithReleases(cleaner)
			assets := []domain.ReleaseAsset{{ID: 42, Repo: n, ReleaseID: 1}}
			f.meta.EXPECT().Get(ctx, n).Return(domain.Repo{Name: n}, nil)
			f.git.EXPECT().Trash(ctx, n).Return(nil)
			cleaner.EXPECT().AssetsOf(ctx, n).Return(assets, nil).Once()
			if failed {
				f.meta.EXPECT().Delete(ctx, n).Return(domain.ErrUnavailable).Once()
				require.ErrorIs(t, f.repos.Delete(ctx, domain.Root(), n), domain.ErrUnavailable)
			} else {
				deleted := false
				f.meta.EXPECT().Delete(ctx, n).Run(func(context.Context, domain.RepoName) { deleted = true }).Return(nil).Once()
				cleaner.EXPECT().OnRepoDeleted(mock.Anything, n, assets).Run(func(ctx context.Context, _ domain.RepoName, _ []domain.ReleaseAsset) {
					require.True(t, deleted)
					require.Nil(t, ctx.Done())
				}).Return().Once()
				f.bus.EXPECT().Publish(ctx, mock.Anything).Return().Once()
				require.NoError(t, f.repos.Delete(ctx, domain.Root(), n))
			}
		})
	}
}
