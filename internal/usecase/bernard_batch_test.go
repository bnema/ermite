// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

func TestBernardFindOpenMRUsesBatchBranch(t *testing.T) {
	for _, key := range []domain.DepBatchKey{"", "github-actions/.github/workflows/ci.yml/actions/checkout@v5"} {
		t.Run(string(key), func(t *testing.T) {
			repo, err := domain.ParseRepoName("proj")
			require.NoError(t, err)
			store := mocks.NewMergeRequestStore(t)
			branch := key.Branch()
			store.EXPECT().OpenBySource(mock.Anything, repo, []string{branch}, 16).Return([]int64{3, 4}, false, nil)
			store.EXPECT().Get(mock.Anything, repo, int64(3)).Return(domain.MergeRequest{Number: 3, Source: "other", Target: "main", State: domain.MRStateOpen}, nil)
			store.EXPECT().Get(mock.Anything, repo, int64(4)).Return(domain.MergeRequest{Number: 4, Source: branch, Target: "main", State: domain.MRStateOpen}, nil)
			b := Bernard{cfg: BernardConfig{MRStore: store}}
			got, err := b.findOpenMR(t.Context(), repo, "main", branch)
			require.NoError(t, err)
			require.Equal(t, int64(4), got)
		})
	}
}

func TestBernardBatchFailureAndMRCloseAreIsolated(t *testing.T) {
	for _, operation := range []string{"failed run", "closed MR"} {
		t.Run(operation, func(t *testing.T) {
			repo, err := domain.ParseRepoName("proj")
			require.NoError(t, err)
			u := domain.DepUpdate{DepRef: domain.DepRef{Name: "pkg", Version: "v2"}}
			base := domain.DepBatch{Updates: []domain.DepUpdate{u}, MR: 2, State: domain.DepsApplying, RunID: 10}
			rec := domain.RepoDeps{Repo: repo, Batches: []domain.DepBatch{base, {Key: "major", Updates: []domain.DepUpdate{u}, MR: 3, State: domain.DepsApplying, RunID: 11}}}
			store := mocks.NewDepsStore(t)
			bus := mocks.NewEventBus(t)
			clock := mocks.NewClock(t)
			clock.EXPECT().Now().Return(time.Time{}).Maybe()
			bus.EXPECT().Publish(mock.Anything, mock.Anything).Return().Once()
			store.EXPECT().Mutate(mock.Anything, repo, mock.Anything).RunAndReturn(func(_ context.Context, _ domain.RepoName, fn func(*domain.RepoDeps) (bool, error)) (domain.RepoDeps, error) {
				changed, err := fn(&rec)
				require.True(t, changed)
				return rec, err
			}).Once()
			b := Bernard{ctx: t.Context(), cfg: BernardConfig{Store: store, Bus: bus, Clock: clock}}
			if operation == "failed run" {
				b.failRun(t.Context(), repo, 11, "failed major")
				require.Equal(t, domain.DepsFailed, rec.Batches[1].State)
				require.Equal(t, "failed major", rec.Batches[1].Error)
				require.Equal(t, int64(11), rec.Batches[1].RunID)
			} else {
				store.EXPECT().Get(mock.Anything, repo).Return(rec, nil).Once()
				b.onMRDone(domain.MergeRequest{Repo: repo, Number: 3}, false)
				require.Equal(t, int64(0), rec.Batches[1].MR)
				require.Equal(t, domain.DepsIdle, rec.Batches[1].State)
			}
			require.Equal(t, base, rec.Batches[0])
		})
	}
}

func TestBernardBatchSelectors(t *testing.T) {
	d := domain.RepoDeps{Batches: []domain.DepBatch{{MR: 1, RunID: 7}, {Key: "major", MR: 2, RunID: 8, State: domain.DepsApplying}}}
	require.Nil(t, applyingBatch(&d, 7), "historical runs are not active")
	require.Nil(t, applyingBatch(&d, 9))
	require.Equal(t, &d.Batches[1], applyingBatch(&d, 8))
	require.Nil(t, mrBatch(&d, 0))
	require.Nil(t, mrBatch(&d, 3))
	require.Equal(t, &d.Batches[1], mrBatch(&d, 2))
}

func TestBernardReconcilesEveryBatch(t *testing.T) {
	repo, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	rec := domain.RepoDeps{Repo: repo, Batches: []domain.DepBatch{{MR: 2, State: domain.DepsApplying, RunID: 10}, {Key: "major", MR: 3, State: domain.DepsApplying, RunID: 11}}}
	store := mocks.NewDepsStore(t)
	mrs := mocks.NewMergeRequestStore(t)
	runs := mocks.NewDepsRunner(t)
	store.EXPECT().List(mock.Anything).Return([]domain.RepoDeps{rec}, nil).Twice()
	for _, batch := range rec.Batches {
		mrs.EXPECT().Get(mock.Anything, repo, batch.MR).Return(domain.MergeRequest{Repo: repo, Number: batch.MR, State: domain.MRStateOpen}, nil).Once()
		runs.EXPECT().GetDepsRun(mock.Anything, repo, batch.RunID).Return(domain.CIRun{ID: batch.RunID, Status: domain.CIInProgress}, nil).Once()
	}
	b := Bernard{ctx: t.Context(), cfg: BernardConfig{Store: store, MRStore: mrs, Runs: runs}}
	b.reconcileMergeRequests()
	b.reconcileRuns(t.Context(), 0)
}

func TestBernardDropsEmptyMajorOnMRDone(t *testing.T) {
	for _, merged := range []bool{false, true} {
		t.Run(fmt.Sprint(merged), func(t *testing.T) {
			repo, err := domain.ParseRepoName("proj")
			require.NoError(t, err)
			base := domain.DepBatch{MR: 2}
			rec := domain.RepoDeps{Repo: repo, Batches: []domain.DepBatch{base, {Key: "major", MR: 3}}}
			store := mocks.NewDepsStore(t)
			bus := mocks.NewEventBus(t)
			clock := mocks.NewClock(t)
			clock.EXPECT().Now().Return(time.Time{}).Maybe()
			bus.EXPECT().Publish(mock.Anything, mock.Anything).Once()
			store.EXPECT().Get(mock.Anything, repo).Return(rec, nil).Once()
			store.EXPECT().Mutate(mock.Anything, repo, mock.Anything).RunAndReturn(func(_ context.Context, _ domain.RepoName, fn func(*domain.RepoDeps) (bool, error)) (domain.RepoDeps, error) {
				_, err := fn(&rec)
				return rec, err
			}).Once()
			// The merged path schedules a check; an archived repo makes it read-only.
			meta := mocks.NewRepoMetaStore(t)
			if merged {
				meta.EXPECT().Get(mock.Anything, repo).Return(domain.Repo{Name: repo, Archived: true}, nil).Once()
				store.EXPECT().Get(mock.Anything, repo).Return(domain.RepoDeps{Repo: repo}, nil).Once()
			}
			b := Bernard{ctx: t.Context(), cfg: BernardConfig{Store: store, Bus: bus, Clock: clock, Meta: meta, Registry: func() ports.DepsRegistry { return nil }}}
			b.onMRDone(domain.MergeRequest{Repo: repo, Number: 3}, merged)
			b.wg.Wait()
			require.Equal(t, []domain.DepBatch{base}, rec.Batches)
		})
	}
}
