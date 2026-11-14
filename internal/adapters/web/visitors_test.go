// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

// countingTraffic counts the visitor reads.
type countingTraffic struct {
	homeTraffic
	calls int
}

func (f *countingTraffic) RepoDaily(ctx context.Context, repo domain.RepoName, days int) ([]uint64, error) {
	f.calls++
	return f.homeTraffic.RepoDaily(ctx, repo, days)
}

func TestVisitorsCache(t *testing.T) {
	t.Parallel()
	repo, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	daily := make([]uint64, ports.ActivityDays)
	daily[89] = 12

	t.Run("fresh for 30 s", func(t *testing.T) {
		t.Parallel()
		var c visitorsCache
		f := &countingTraffic{homeTraffic: homeTraffic{daily: daily}}
		got, err := c.get(t.Context(), f, repo, fixedNow)
		require.NoError(t, err)
		assert.Equal(t, daily, got)
		got[89] = 99 // the caller's copy: the cache keeps its own
		got, err = c.get(t.Context(), f, repo, fixedNow.Add(29*time.Second))
		require.NoError(t, err)
		assert.Equal(t, uint64(12), got[89])
		assert.Equal(t, 1, f.calls)
		_, err = c.get(t.Context(), f, repo, fixedNow.Add(30*time.Second))
		require.NoError(t, err)
		assert.Equal(t, 2, f.calls)
	})

	t.Run("new day", func(t *testing.T) {
		t.Parallel()
		var c visitorsCache
		f := &countingTraffic{homeTraffic: homeTraffic{daily: daily}}
		late := time.Date(2026, 10, 12, 23, 59, 50, 0, time.UTC)
		_, err := c.get(t.Context(), f, repo, late)
		require.NoError(t, err)
		_, err = c.get(t.Context(), f, repo, late.Add(15*time.Second))
		require.NoError(t, err)
		assert.Equal(t, 2, f.calls)
	})

	t.Run("errors are not kept", func(t *testing.T) {
		t.Parallel()
		var c visitorsCache
		f := &countingTraffic{homeTraffic: homeTraffic{err: errors.New("traffic unavailable")}}
		_, err := c.get(t.Context(), f, repo, fixedNow)
		require.Error(t, err)
		f.err, f.daily = nil, daily
		got, err := c.get(t.Context(), f, repo, fixedNow.Add(time.Second))
		require.NoError(t, err)
		assert.Equal(t, daily, got)
		assert.Equal(t, 2, f.calls)
	})
}

// The home row reads the visitors through the cache, on the server clock.
func TestHomeVisitorsCached(t *testing.T) {
	t.Parallel()
	browse := mocks.NewBrowseService(t)
	browse.EXPECT().GetTree(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, false).Return(domain.Tree{}, nil).Maybe()
	browse.EXPECT().ListCommits(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.HistoryPage{Commits: homeCommits(t, 1)}, nil)
	browse.EXPECT().RepoStats(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.RepoStats{Activity: make([]int, ports.ActivityDays)}, nil)
	now := fixedNow
	f := &countingTraffic{homeTraffic: homeTraffic{daily: make([]uint64, ports.ActivityDays)}}
	srv := newTestServer(t, Config{Repos: reposServing(t, pushedRepo(t, "proj")), Browse: browse, Traffic: f, Now: func() time.Time { return now }})
	for range 3 {
		renderRow(t, srv.homeRowOf(t.Context(), pushedRepo(t, "proj"), fixedNow, false))
	}
	assert.Equal(t, 1, f.calls)
	now = now.Add(time.Minute)
	renderRow(t, srv.homeRowOf(t.Context(), pushedRepo(t, "proj"), fixedNow, false))
	assert.Equal(t, 2, f.calls)
}
