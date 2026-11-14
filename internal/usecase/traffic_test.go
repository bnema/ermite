// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/usecase"
	"github.com/stretchr/testify/require"
)

type trafficCapture struct {
	ports.TrafficStore
	mu      sync.Mutex
	batches [][]domain.TrafficHit
	pruned  int
}

func (s *trafficCapture) Record(ctx context.Context, h []domain.TrafficHit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, append([]domain.TrafficHit(nil), h...))
	return nil
}
func (s *trafficCapture) Prune(context.Context, time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruned++
	return 0, nil
}
func (s *trafficCapture) snapshot() ([][]domain.TrafficHit, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]domain.TrafficHit(nil), s.batches...), s.pruned
}

func TestTrafficRecorderPrivacyAndDrops(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 1, 2, 23, 59, 0, 0, time.UTC)}
	store := &trafficCapture{}
	tr := usecase.NewTraffic(usecase.TrafficConfig{Store: store, Clock: clock, SelfHost: "forge.test", QueueSize: 5})
	repo, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	ip := netip.MustParseAddr("192.0.2.1")
	tr.Hit(repo, domain.TrafficView, ip, "Mozilla", "https://WWW.Example.COM/private")
	tr.Hit(repo, domain.TrafficView, ip, "GPTBot", "https://forge.test/private")
	tr.Hit(repo, domain.TrafficClone, ip, "bot", "")
	clock.t = clock.t.Add(2 * time.Minute)
	tr.Hit(repo, domain.TrafficView, ip, "", "")
	tr.Hit(repo, domain.TrafficView, ip, "curl/8", "")
	tr.Hit(repo, domain.TrafficView, ip, "Mozilla", "")
	require.Equal(t, uint64(1), tr.Dropped())
	tr.Close(t.Context())
	var hits []domain.TrafficHit
	for _, batch := range store.batches {
		hits = append(hits, batch...)
	}
	require.Len(t, hits, 5)
	require.Equal(t, hits[0].VisitorHash, hits[1].VisitorHash)
	require.Equal(t, hits[0].VisitorHash, hits[2].VisitorHash)
	require.NotEqual(t, hits[0].VisitorHash, hits[3].VisitorHash)
	require.Equal(t, hits[3].VisitorHash, hits[4].VisitorHash)
	require.Equal(t, "example.com", hits[0].ReferrerDomain)
	require.Empty(t, hits[1].ReferrerDomain)
	require.False(t, hits[0].Bot)
	require.True(t, hits[1].Bot)
	require.False(t, hits[2].Bot)
	require.True(t, hits[3].Bot)
	require.False(t, hits[4].Bot)
	tr.Hit(repo, domain.TrafficView, ip, "Mozilla", "")
	require.Equal(t, uint64(2), tr.Dropped())
	tr.Close(t.Context())
}

func TestTrafficBatchTimerPruneAndCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &trafficCapture{}
		clock := &fakeClock{t: time.Now()}
		tr := usecase.NewTraffic(usecase.TrafficConfig{Store: store, Clock: clock, BatchSize: 2, FlushEvery: 5 * time.Second})
		ctx, cancel := context.WithCancel(t.Context())
		go tr.Run(ctx)
		repo, err := domain.ParseRepoName("proj")
		require.NoError(t, err)
		hit := func() { tr.Hit(repo, domain.TrafficClone, netip.MustParseAddr("192.0.2.1"), "git", "") }
		hit()
		hit()
		synctest.Wait()
		batches, _ := store.snapshot()
		require.Len(t, batches, 1)
		require.Len(t, batches[0], 2)
		hit()
		synctest.Wait()
		batches, _ = store.snapshot()
		require.Len(t, batches, 1)
		time.Sleep(5 * time.Second)
		synctest.Wait()
		batches, _ = store.snapshot()
		require.Len(t, batches, 2)
		time.Sleep(time.Hour)
		synctest.Wait()
		_, pruned := store.snapshot()
		require.Equal(t, 1, pruned)
		hit()
		synctest.Wait()
		cancel()
		synctest.Wait()
		tr.Close(t.Context())
		require.Len(t, store.batches, 3)
		require.Len(t, store.batches[2], 1)
	})
}

func TestTrafficReads(t *testing.T) {
	ctx := t.Context()
	db, err := kvstore.Open(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close(ctx)) })
	clock := &fakeClock{t: time.Date(2026, 6, 1, 12, 30, 0, 0, time.UTC)}
	tr := usecase.NewTraffic(usecase.TrafficConfig{Store: db.Traffic(), Clock: clock})
	a, err := domain.ParseRepoName("a")
	require.NoError(t, err)
	b, err := domain.ParseRepoName("b")
	require.NoError(t, err)
	for _, repo := range []domain.RepoName{a, b} {
		require.NoError(t, db.Create(ctx, domain.Repo{Name: repo, CreatedAt: clock.t, Visibility: domain.VisibilityPrivate}))
	}
	hits := []domain.TrafficHit{
		{Repo: a, Kind: domain.TrafficView, At: clock.t, ReferrerDomain: "example.test"},
		{Repo: a, Kind: domain.TrafficClone, At: clock.t},
		{Repo: a, Kind: domain.TrafficView, At: clock.t.Add(-48 * time.Hour)},
		{Repo: a, Kind: domain.TrafficView, At: clock.t, Bot: true},
		{Repo: b, Kind: domain.TrafficClone, At: clock.t.Add(-23 * time.Hour)},
		{Repo: b, Kind: domain.TrafficClone, At: clock.t.Add(-25 * time.Hour)},
	}
	require.NoError(t, db.Traffic().Record(ctx, hits))
	for _, window := range []time.Duration{24 * time.Hour, 30 * 24 * time.Hour} {
		report, err := tr.RepoTraffic(ctx, a, window)
		require.NoError(t, err)
		size, views := 30, uint64(2)
		if window == 24*time.Hour {
			size, views = 24, 1
		}
		require.Len(t, report.Points, size)
		require.Equal(t, views, report.Totals.Views)
		require.Equal(t, uint64(1), report.Totals.Clones)
		require.Equal(t, []ports.TrafficReferrer{{Domain: "example.test", Count: 1}}, report.Referrers)
		require.Zero(t, report.Points[0].Views)
		for i := 1; i < len(report.Points); i++ {
			require.True(t, report.Points[i-1].Start.Before(report.Points[i].Start))
		}
	}
	_, err = tr.RepoTraffic(ctx, a, time.Hour)
	require.ErrorIs(t, err, domain.ErrInvalid)
	daily, err := tr.RepoDaily(ctx, a, 4)
	require.NoError(t, err)
	require.Equal(t, []uint64{0, 1, 0, 2}, daily)
	counts, err := tr.RepoSummary(ctx, a)
	require.NoError(t, err)
	require.Equal(t, uint64(2), counts.Views)
	require.Equal(t, uint64(1), counts.Clones)
	require.Equal(t, uint64(1), counts.Bots)
	hourly, err := tr.RepoHourly(ctx, a)
	require.NoError(t, err)
	require.Len(t, hourly, 24)
	require.Equal(t, clock.t.Truncate(time.Hour).Add(-23*time.Hour), hourly[0].Start)
	require.Zero(t, hourly[0].Views)
	require.Equal(t, uint64(1), hourly[23].Views)
	forge, err := tr.Forge(ctx, 24*time.Hour)
	require.NoError(t, err)
	require.Len(t, forge.Repos, 2)
	require.Equal(t, a, forge.Repos[0].Repo)
	require.Equal(t, uint64(1), forge.Totals.Views)
	require.Equal(t, uint64(2), forge.Totals.Clones)
	require.Equal(t, uint64(1), forge.Totals.Bots)
	require.Equal(t, []ports.TrafficReferrer{{Domain: "example.test", Count: 1}}, forge.Referrers)
	forge, err = tr.Forge(ctx, 30*24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, uint64(2), forge.Totals.Views)
	require.Equal(t, uint64(3), forge.Totals.Clones)
	_, err = tr.RepoDaily(ctx, a, 91)
	require.ErrorIs(t, err, domain.ErrInvalid)
	_, err = tr.Forge(ctx, 0)
	require.ErrorIs(t, err, domain.ErrInvalid)
}

func TestTrafficForgeReferrerRanking(t *testing.T) {
	ctx := t.Context()
	db, err := kvstore.Open(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close(ctx)) })
	clock := &fakeClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	tr := usecase.NewTraffic(usecase.TrafficConfig{Store: db.Traffic(), Clock: clock})
	repo, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	require.NoError(t, db.Create(ctx, domain.Repo{Name: repo, CreatedAt: clock.t, Visibility: domain.VisibilityPrivate}))
	var hits []domain.TrafficHit
	for i := range 25 {
		hits = append(hits, domain.TrafficHit{Repo: repo, Kind: domain.TrafficView, At: clock.t, ReferrerDomain: fmt.Sprintf("site%02d.test", i)})
	}
	hits = append(hits, hits[24])
	require.NoError(t, db.Traffic().Record(ctx, hits))
	result, err := tr.Forge(ctx, 7*24*time.Hour)
	require.NoError(t, err)
	require.Len(t, result.Referrers, 20)
	require.Equal(t, ports.TrafficReferrer{Domain: "site24.test", Count: 2}, result.Referrers[0])
	require.Equal(t, "site00.test", result.Referrers[1].Domain)
	report, err := tr.RepoTraffic(ctx, repo, 24*time.Hour)
	require.NoError(t, err)
	require.Len(t, report.Referrers, 10)
	require.Equal(t, result.Referrers[:10], report.Referrers)
}
