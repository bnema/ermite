// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

type zeroClock struct{}

func (zeroClock) Now() time.Time { return time.Time{} }

func TestMergeabilityBackoffIsBounded(t *testing.T) {
	assert.Equal(t, time.Second, mergeBackoff(1))
	assert.Equal(t, 2*time.Second, mergeBackoff(2))
	assert.Equal(t, 4*time.Second, mergeBackoff(3))
	assert.Equal(t, time.Minute, mergeBackoff(20))
}

func TestMergeabilityClosingForgetsTheSeenKey(t *testing.T) {
	repo, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	store := mocks.NewMergeRequestStore(t)
	store.EXPECT().GetMergeability(mock.Anything, repo, int64(7)).Return(domain.Mergeability{}, domain.ErrNotFound)
	store.EXPECT().DeleteMergeability(mock.Anything, repo, int64(7)).Return(nil)
	svc := NewMergeability(context.Background(), MergeabilityConfig{Store: store, Clock: zeroClock{}})
	t.Cleanup(func() { svc.Close(context.Background()) })

	mr := domain.MergeRequest{Repo: repo, Number: 7, State: domain.MRStateOpen}
	oid, err := domain.ParseObjectID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, err)
	mr.HeadID = oid
	svc.Read(context.Background(), mr, oid)
	require.Len(t, svc.q.seen, 1)

	svc.OnClosed(context.Background(), mr)
	assert.Empty(t, svc.q.seen)
}
