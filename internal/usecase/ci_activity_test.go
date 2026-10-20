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
	trigger.EXPECT().OnRepoChanged(mock.Anything, repo).Return().Twice()
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
