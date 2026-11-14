// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"testing"
	"time"

	"bnema.dev/ermite/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestTrafficMerge(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	tr := s.Traffic()
	repo := name(t, "proj")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, CreatedAt: created, Visibility: domain.VisibilityPrivate}))
	at := time.Date(2026, 1, 2, 12, 30, 0, 0, time.FixedZone("offset", 3600))
	day := at.UTC().Truncate(24 * time.Hour)
	hit := domain.TrafficHit{Repo: repo, Kind: domain.TrafficView, At: at, ReferrerDomain: "example.com"}
	require.NoError(t, tr.Record(ctx, []domain.TrafficHit{hit, hit}))
	clone := hit
	clone.Kind = domain.TrafficClone
	clone.Bot = true
	bot := hit
	bot.Bot = true
	later := hit
	later.At = at.Add(time.Hour)
	next := hit
	next.At = at.Add(24 * time.Hour)
	require.NoError(t, tr.Record(ctx, []domain.TrafficHit{clone, bot, later, next}))
	days, err := tr.Daily(ctx, repo, day, day.Add(48*time.Hour))
	require.NoError(t, err)
	require.Len(t, days, 2)
	require.Equal(t, domain.TrafficCounts{Views: 3, ViewUniques: 1, Clones: 1, CloneUniques: 1, Bots: 1}, days[0].TrafficCounts)
	require.Equal(t, uint64(4), days[0].Referrers["example.com"])
	require.Equal(t, day, days[0].Start)
	hours, err := tr.Hourly(ctx, repo, day, day.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, hours, 2)
	require.Equal(t, uint64(1), hours[0].ViewUniques)
	require.Equal(t, uint64(1), hours[1].ViewUniques)
	require.Empty(t, hours[0].Referrers)
	rows, err := tr.Summaries(ctx, day, day.Add(48*time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, uint64(4), rows[0].Views)
	require.Equal(t, uint64(2), rows[0].ViewUniques)
	require.Equal(t, uint64(1), rows[0].Bots)
	require.Equal(t, uint64(5), rows[0].Referrers["example.com"])
	days, err = tr.Daily(ctx, repo, day, day.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, days, 1)
}

func TestTrafficCaps(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	tr := s.Traffic()
	repo := name(t, "proj")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, CreatedAt: created, Visibility: domain.VisibilityPrivate}))
	hits := make([]domain.TrafficHit, 10000)
	for i := range hits {
		hits[i] = domain.TrafficHit{Repo: repo, Kind: domain.TrafficView, At: created, ReferrerDomain: fmt.Sprintf("site%d.test", i%51)}
		binary.BigEndian.PutUint64(hits[i].VisitorHash[:], uint64(i))
	}
	require.NoError(t, tr.Record(ctx, hits))
	days, err := tr.Daily(ctx, repo, created.Add(-24*time.Hour), created.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, days, 1)
	require.False(t, days[0].Partial)
	require.Len(t, days[0].Referrers, 50)
	extra := hits[0]
	binary.BigEndian.PutUint64(extra.VisitorHash[:], 10000)
	require.NoError(t, tr.Record(ctx, []domain.TrafficHit{extra, hits[0]}))
	days, err = tr.Daily(ctx, repo, created.Add(-24*time.Hour), created.Add(24*time.Hour))
	require.NoError(t, err)
	require.True(t, days[0].Partial)
	require.Equal(t, uint64(10000), days[0].ViewUniques)
	require.Equal(t, uint64(10002), days[0].Views)
	require.NotContains(t, days[0].Referrers, "site50.test")
	hours, err := tr.Hourly(ctx, repo, created.Add(-24*time.Hour), created.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, hours, 1)
	require.True(t, hours[0].Partial)
	clone := extra
	clone.Kind = domain.TrafficClone
	require.NoError(t, tr.Record(ctx, []domain.TrafficHit{clone}))
	days, err = tr.Daily(ctx, repo, created.Add(-24*time.Hour), created.Add(24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, uint64(1), days[0].CloneUniques)
}

func TestTrafficPruneAndDelete(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	tr := s.Traffic()
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	a, b := name(t, "a"), name(t, "b")
	for _, repo := range []domain.RepoName{a, b} {
		require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, CreatedAt: created, Visibility: domain.VisibilityPrivate}))
		var hits []domain.TrafficHit
		for _, age := range []time.Duration{91 * 24 * time.Hour, 90 * 24 * time.Hour, 49 * time.Hour, 48 * time.Hour, 0} {
			hits = append(hits, domain.TrafficHit{Repo: repo, Kind: domain.TrafficClone, At: now.Add(-age)})
		}
		require.NoError(t, tr.Record(ctx, hits))
	}
	n, err := tr.Prune(ctx, now)
	require.NoError(t, err)
	require.Equal(t, 8, n)
	days, err := tr.Daily(ctx, a, now.Add(-100*24*time.Hour), now.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, days, 4)
	require.Equal(t, now.Add(-90*24*time.Hour), days[0].Start)
	hours, err := tr.Hourly(ctx, a, now.Add(-100*24*time.Hour), now.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, hours, 2)
	require.Equal(t, now.Add(-48*time.Hour), hours[0].Start)
	require.NoError(t, s.Delete(ctx, a))
	require.NoError(t, tr.Record(ctx, []domain.TrafficHit{{Repo: a, Kind: domain.TrafficView, At: now}}))
	require.NoError(t, s.Create(ctx, domain.Repo{Name: a, CreatedAt: now, Visibility: domain.VisibilityPrivate}))
	days, err = tr.Daily(ctx, a, time.Time{}, now.Add(time.Hour))
	require.NoError(t, err)
	require.Empty(t, days)
	hours, err = tr.Hourly(ctx, a, time.Time{}, now.Add(time.Hour))
	require.NoError(t, err)
	require.Empty(t, hours)
	rows, err := tr.Summaries(ctx, time.Time{}, now.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, b, rows[0].Repo)
	n, err = tr.Prune(ctx, now)
	require.NoError(t, err)
	require.Zero(t, n)
}

func TestTrafficConcurrentAndCanceled(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	repo := name(t, "proj")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, CreatedAt: created, Visibility: domain.VisibilityPrivate}))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			errs <- s.Traffic().Record(ctx, []domain.TrafficHit{{Repo: repo, Kind: domain.TrafficView, At: created}})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	days, err := s.Traffic().Daily(ctx, repo, time.Time{}, created.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, days, 1)
	require.Equal(t, uint64(8), days[0].Views)
	require.Equal(t, uint64(1), days[0].ViewUniques)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, s.Traffic().Record(canceled, nil), context.Canceled)
	_, err = s.Traffic().Daily(canceled, repo, time.Time{}, created)
	require.ErrorIs(t, err, context.Canceled)
	_, err = s.Traffic().Prune(canceled, created)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, s.Traffic().Record(ctx, []domain.TrafficHit{{Repo: repo, Kind: "bad", At: created}}), domain.ErrInvalid)
}

func TestTrafficPersistsUniqueSets(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := openStore(t, dir)
	repo := name(t, "proj")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, CreatedAt: created, Visibility: domain.VisibilityPrivate}))
	hit := domain.TrafficHit{Repo: repo, Kind: domain.TrafficClone, At: created}
	require.NoError(t, s.Traffic().Record(ctx, []domain.TrafficHit{hit}))
	require.NoError(t, s.Close(ctx))
	s = openStore(t, dir)
	require.NoError(t, s.Traffic().Record(ctx, []domain.TrafficHit{hit}))
	days, err := s.Traffic().Daily(ctx, repo, time.Time{}, created.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, days, 1)
	require.Equal(t, uint64(2), days[0].Clones)
	require.Equal(t, uint64(1), days[0].CloneUniques)
}
