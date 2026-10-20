// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

func TestAutoMergePendingIndex(t *testing.T) {
	ctx := context.Background()
	repo, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	store := mocks.NewMergeRequestStore(t)
	meta := mocks.NewRepoMetaStore(t)
	a := NewAutoMerge(ctx, AutoMergeConfig{Store: store, Meta: meta})
	defer a.Close(ctx)
	// No expectations: an unindexed kick must not read either store.
	a.OnRepoChanged(ctx, repo)
	_, found := a.next()
	require.False(t, found)
	a.OnAutoMergeEnabled(ctx, repo)
	a.OnRepoChanged(ctx, repo)
	got, found := a.next()
	require.True(t, found)
	require.Equal(t, repo, got)
	meta.EXPECT().Get(mock.Anything, repo).Return(domain.Repo{Name: repo}, nil).Once()
	store.EXPECT().ListOpen(mock.Anything, repo).Return(nil, nil).Once()
	require.True(t, a.evaluate(repo))
	require.NotContains(t, a.pending, repo)
	a.mu.Lock()
	delete(a.running, repo)
	a.mu.Unlock()
	a.OnRepoChanged(ctx, repo)
	_, found = a.next()
	require.False(t, found)
}

func TestAutoMergeEnableDuringEmptyScanKeepsPending(t *testing.T) {
	ctx := context.Background()
	repo, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	store := mocks.NewMergeRequestStore(t)
	meta := mocks.NewRepoMetaStore(t)
	a := NewAutoMerge(ctx, AutoMergeConfig{Store: store, Meta: meta})
	defer a.Close(ctx)
	a.OnAutoMergeEnabled(ctx, repo)
	_, found := a.next()
	require.True(t, found)
	meta.EXPECT().Get(mock.Anything, repo).Return(domain.Repo{Name: repo}, nil).Once()
	store.EXPECT().ListOpen(mock.Anything, repo).Run(func(context.Context, domain.RepoName) {
		// The returned empty snapshot predates this newly enabled intent.
		a.OnAutoMergeEnabled(ctx, repo)
	}).Return(nil, nil).Once()
	require.True(t, a.evaluate(repo))
	require.True(t, a.pending[repo])
	require.True(t, a.again[repo])
}

func TestAutoMergeStartupFillsPendingIndex(t *testing.T) {
	ctx := context.Background()
	repo, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	store := mocks.NewMergeRequestStore(t)
	meta := mocks.NewRepoMetaStore(t)
	store.EXPECT().ListOpenAll(mock.Anything).Return([]domain.MergeRequest{{Repo: repo, AutoMerge: &domain.AutoMerge{}}}, nil).Once()
	scanned := make(chan struct{})
	meta.EXPECT().Get(mock.Anything, repo).Run(func(context.Context, domain.RepoName) { close(scanned) }).Return(domain.Repo{}, domain.ErrNotFound).Once()
	a := NewAutoMerge(ctx, AutoMergeConfig{Store: store, Meta: meta})
	a.Start()
	defer a.Close(ctx)
	select {
	case <-scanned:
	case <-time.After(5 * time.Second):
		t.Fatal("startup did not scan the pending repository")
	}
	a.mu.Lock()
	pending := a.pending[repo]
	a.mu.Unlock()
	require.True(t, pending)
}

func TestManualPendingChecksMessage(t *testing.T) {
	err := checksPolicy([]domain.CICheck{{Name: "build", Status: domain.CIQueued}})
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	require.EqualError(t, err, "failed precondition: checks have not finished: build")
	require.NotContains(t, err.Error(), "auto-merge")
	require.NotErrorIs(t, err, errAutoMergeWait)
	require.NoError(t, checksPolicy(nil))
}
