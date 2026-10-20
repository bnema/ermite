// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

func TestSettledCountsQueuedPushesAndPullEvents(t *testing.T) {
	trigger := mocks.NewAutoMergeTrigger(t)
	u := newCIUnit(t, func(cfg *usecase.CIConfig) { cfg.TriggerQueue = 1; cfg.AutoMerge = trigger })
	repo := name(t, "proj")
	trigger.EXPECT().OnRepoChanged(mock.Anything, repo).Return().Times(3)
	u.store.EXPECT().ListRunners(mock.Anything).Return(nil, nil).Once()
	ctx := context.Background()
	settled := func(n int64) bool { return u.ci.Settled(ctx, repo, n, "feat", domain.ObjectID{}) }
	assert.True(t, settled(1))
	u.ci.QueuePush(ctx, repo)
	u.ci.QueuePush(ctx, repo) // dropped: queue capacity is one
	assert.False(t, settled(1))
	assert.False(t, settled(2)) // pushes conservatively cover the whole repo
	u.ci.QueuePull(repo, 1)
	u.ci.QueuePull(repo, 1) // dropped: no counter leak
	u.ci.PlanQueuedPush()
	assert.False(t, settled(1))
	assert.True(t, settled(2))
	u.ci.FinishQueuedPull()
	assert.True(t, settled(1))
}

func TestSettledWaitsForPushDiscoveryAndReleasesOnError(t *testing.T) {
	mrs := mocks.NewMergeRequestStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	u := newCIUnit(t, func(cfg *usecase.CIConfig) {
		cfg.MRs = mrs
		cfg.Pull = mocks.NewPullGit(t)
		cfg.PullStage = func(string) { close(entered); <-release }
	})
	repo := name(t, "proj")
	mrs.EXPECT().OpenByTarget(mock.Anything, repo, mock.Anything, mock.Anything).Return(nil, false, nil)
	mrs.EXPECT().OpenBySource(mock.Anything, repo, mock.Anything, mock.Anything).Return(nil, false, domain.ErrUnavailable)
	u.store.EXPECT().ListRunners(mock.Anything).Return(nil, nil).Once()
	done := make(chan struct{})
	go func() {
		u.ci.OnPush(context.Background(), repo, []domain.RefUpdate{{Ref: "refs/heads/feat", New: "1111111111111111111111111111111111111111"}}, domain.Root())
		close(done)
	}()
	<-entered
	u.ci.PlanQueuedPush()
	assert.False(t, u.ci.Settled(context.Background(), repo, 1, "feat", domain.ObjectID{}))
	close(release)
	<-done
	assert.True(t, u.ci.Settled(context.Background(), repo, 1, "feat", domain.ObjectID{}))
}

func TestSettledReleasesQueuedPushOnShutdown(t *testing.T) {
	u := newCIUnit(t, nil)
	repo := name(t, "proj")
	u.ci.QueuePush(context.Background(), repo)
	u.ci.Close(context.Background())
	assert.True(t, u.ci.Settled(context.Background(), repo, 1, "feat", domain.ObjectID{}))
	u.ci.QueuePush(context.Background(), repo)
	assert.True(t, u.ci.Settled(context.Background(), repo, 1, "feat", domain.ObjectID{}))
}
