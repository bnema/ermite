// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

type mergeTestClock struct{}

func (mergeTestClock) Now() time.Time { return time.Time{} }

func TestMergeabilityBackoffIsBounded(t *testing.T) {
	assert.Equal(t, time.Second, mergeBackoff(1))
	assert.Equal(t, 2*time.Second, mergeBackoff(2))
	assert.Equal(t, 4*time.Second, mergeBackoff(3))
	assert.Equal(t, time.Minute, mergeBackoff(20))
}

func TestMergeabilityReadMarksTheRepoDirty(t *testing.T) {
	repo, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	oid, err := domain.ParseObjectID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, err)
	store := mocks.NewMergeRequestStore(t)
	store.EXPECT().GetMergeability(mock.Anything, repo, int64(7)).Return(domain.Mergeability{}, domain.ErrNotFound)
	svc := NewMergeability(context.Background(), MergeabilityConfig{Store: store})
	defer svc.Close(context.Background())
	mr := domain.MergeRequest{Repo: repo, Number: 7, HeadID: oid}
	assert.Equal(t, domain.MergeabilityChecking, svc.Read(context.Background(), mr, oid).State)
	// Another MR in the same repository is the same unit of work.
	mr.Number = 8
	mr.HeadID = domain.ObjectID{}
	svc.Read(context.Background(), mr, oid)
	got, found := svc.next()
	require.True(t, found)
	assert.Equal(t, repo, got)
	_, found = svc.next()
	assert.False(t, found)
}

func TestMergeabilityOnePassPerRepoAtATimeAndAgain(t *testing.T) {
	repo, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	other, err := domain.ParseRepoName("other")
	require.NoError(t, err)
	meta, store := mocks.NewRepoMetaStore(t), mocks.NewMergeRequestStore(t)
	meta.EXPECT().List(mock.Anything).Return(nil, nil)
	meta.EXPECT().Get(mock.Anything, repo).Return(domain.Repo{Name: repo}, nil)
	meta.EXPECT().Get(mock.Anything, other).Return(domain.Repo{Name: other}, nil)
	entered, release, otherPassed := make(chan struct{}, 4), make(chan struct{}), make(chan struct{})
	var passes, inflight, peak atomic.Int32
	store.EXPECT().ListOpen(mock.Anything, repo).RunAndReturn(func(context.Context, domain.RepoName) ([]domain.MergeRequest, error) {
		n := inflight.Add(1)
		if n > peak.Load() {
			peak.Store(n)
		}
		pass := passes.Add(1)
		entered <- struct{}{}
		if pass == 1 {
			<-release
		}
		inflight.Add(-1)
		return nil, nil
	})
	store.EXPECT().ListOpen(mock.Anything, other).RunAndReturn(func(context.Context, domain.RepoName) ([]domain.MergeRequest, error) {
		close(otherPassed)
		return nil, nil
	})
	svc := NewMergeability(context.Background(), MergeabilityConfig{Store: store, Meta: meta, Workers: 2})
	svc.Start()
	svc.OnOpened(context.Background(), domain.MergeRequest{Repo: repo})
	<-entered
	for range 50 {
		svc.OnOpened(context.Background(), domain.MergeRequest{Repo: repo})
	}
	svc.OnOpened(context.Background(), domain.MergeRequest{Repo: other})
	<-otherPassed // the other worker has serviced all work eligible meanwhile
	assert.EqualValues(t, 1, passes.Load())
	close(release)
	<-entered
	svc.Close(context.Background())
	assert.EqualValues(t, 2, passes.Load(), "all triggers during a pass coalesce into exactly one more pass")
	assert.EqualValues(t, 1, peak.Load())
}
