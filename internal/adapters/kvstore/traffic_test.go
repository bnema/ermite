// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"bnema.dev/ermite/internal/domain"
	"github.com/stretchr/testify/require"
)

// trafficEpoch is a repository creation time before every hit of the tests
// that record past traffic, so the creation-time fence keeps those hits.
var trafficEpoch = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

func TestTrafficMerge(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	tr := s.Traffic()
	repo := name(t, "proj")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, CreatedAt: trafficEpoch, Visibility: domain.VisibilityPrivate}))
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
	a, b := name(t, "a"), name(t, "a.b")
	c := name(t, "a-b")
	for _, repo := range []domain.RepoName{a, b, c} {
		require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, CreatedAt: trafficEpoch, Visibility: domain.VisibilityPrivate}))
		var hits []domain.TrafficHit
		for _, age := range []time.Duration{91 * 24 * time.Hour, 90 * 24 * time.Hour, 49 * time.Hour, 48 * time.Hour, 0} {
			hits = append(hits, domain.TrafficHit{Repo: repo, Kind: domain.TrafficClone, At: now.Add(-age)})
		}
		require.NoError(t, tr.Record(ctx, hits))
	}
	require.Equal(t, 30, s.CountKeysForTest(ctx, "traffic/u/"))
	rows, err := tr.Summaries(ctx, time.Time{}, now.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 3)
	for _, row := range rows {
		require.Equal(t, uint64(5), row.Clones)
	}
	n, err := tr.Prune(ctx, now)
	require.NoError(t, err)
	require.Equal(t, 12, n)
	// Only the buckets starting at now are not past their unique grace: one
	// daily and one hourly marker per repository.
	require.Equal(t, 6, s.CountKeysForTest(ctx, "traffic/u/"))
	require.Equal(t, 18, s.CountKeysForTest(ctx, "traffic/a/"))
	days, err := tr.Daily(ctx, a, now.Add(-100*24*time.Hour), now.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, days, 4)
	require.Equal(t, now.Add(-90*24*time.Hour), days[0].Start)
	hours, err := tr.Hourly(ctx, a, now.Add(-100*24*time.Hour), now.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, hours, 2)
	require.Equal(t, now.Add(-48*time.Hour), hours[0].Start)
	require.NoError(t, s.Delete(ctx, a))
	require.Zero(t, s.CountKeysForTest(ctx, "traffic/u/a/"))
	require.Zero(t, s.CountKeysForTest(ctx, "traffic/a/a/"))
	for _, repo := range []domain.RepoName{b, c} {
		require.Equal(t, 2, s.CountKeysForTest(ctx, "traffic/u/"+repo.String()+"/"))
	}
	require.NoError(t, tr.Record(ctx, []domain.TrafficHit{{Repo: a, Kind: domain.TrafficView, At: now}}))
	require.NoError(t, s.Create(ctx, domain.Repo{Name: a, CreatedAt: now, Visibility: domain.VisibilityPrivate}))
	days, err = tr.Daily(ctx, a, time.Time{}, now.Add(time.Hour))
	require.NoError(t, err)
	require.Empty(t, days)
	hours, err = tr.Hourly(ctx, a, time.Time{}, now.Add(time.Hour))
	require.NoError(t, err)
	require.Empty(t, hours)
	rows, err = tr.Summaries(ctx, time.Time{}, now.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, c, rows[0].Repo)
	require.Equal(t, b, rows[1].Repo)
	n, err = tr.Prune(ctx, now)
	require.NoError(t, err)
	require.Zero(t, n)
	require.NoError(t, tr.Record(ctx, []domain.TrafficHit{{Repo: a, Kind: domain.TrafficClone, At: now}}))
	days, err = tr.Daily(ctx, a, now, now.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, days, 1)
	require.Equal(t, uint64(1), days[0].CloneUniques)
}

func TestTrafficPruneDropsClosedUniques(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	tr := s.Traffic()
	repo := name(t, "proj")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, CreatedAt: trafficEpoch, Visibility: domain.VisibilityPrivate}))
	// now is 1h after the end of the day of "old" and of hour 10, and 30 min
	// after the end of hour 11 (inside its grace); hour 12 is current.
	now := time.Date(2026, 6, 2, 12, 30, 0, 0, time.UTC)
	day := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	boundary := domain.TrafficHit{Repo: repo, Kind: domain.TrafficView, At: day.Add(-time.Hour), VisitorHash: visitor(1)}
	var hits []domain.TrafficHit
	for i, at := range []time.Time{day.Add(10 * time.Hour), day.Add(11 * time.Hour), day.Add(12 * time.Hour)} {
		hits = append(hits,
			domain.TrafficHit{Repo: repo, Kind: domain.TrafficView, At: at, VisitorHash: visitor(uint64(10 + i))},
			domain.TrafficHit{Repo: repo, Kind: domain.TrafficClone, At: at, VisitorHash: visitor(uint64(10 + i))})
	}
	require.NoError(t, tr.Record(ctx, append(hits, boundary)))
	daysBefore, err := tr.Daily(ctx, repo, day.Add(-24*time.Hour), day.Add(24*time.Hour))
	require.NoError(t, err)
	hoursBefore, err := tr.Hourly(ctx, repo, day.Add(-24*time.Hour), day.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, daysBefore, 2)
	require.Len(t, hoursBefore, 4)

	exact := day.Add(10*time.Hour + 2*time.Hour) // hour 10 ends at 11:00, + 1h grace
	n, err := tr.Prune(ctx, exact)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Zero(t, s.CountKeysForTest(ctx, "traffic/u/proj/h/20260602T100000Z/"))
	require.Equal(t, 2, s.CountKeysForTest(ctx, "traffic/u/proj/h/20260602T110000Z/"))
	n, err = tr.Prune(ctx, now)
	require.NoError(t, err)
	require.Zero(t, n)

	require.Zero(t, s.CountKeysForTest(ctx, "traffic/u/proj/d/20260601T000000Z/"))
	require.Zero(t, s.CountKeysForTest(ctx, "traffic/u/proj/h/20260601T230000Z/"))
	require.Zero(t, s.CountKeysForTest(ctx, "traffic/u/proj/h/20260602T100000Z/"))
	require.Equal(t, 2, s.CountKeysForTest(ctx, "traffic/u/proj/h/20260602T110000Z/"))
	require.Equal(t, 2, s.CountKeysForTest(ctx, "traffic/u/proj/h/20260602T120000Z/"))
	require.Equal(t, 6, s.CountKeysForTest(ctx, "traffic/u/proj/d/20260602T000000Z/"))
	require.Equal(t, 10, s.CountKeysForTest(ctx, "traffic/u/"))

	days, err := tr.Daily(ctx, repo, day.Add(-24*time.Hour), day.Add(24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, daysBefore, days)
	hours, err := tr.Hourly(ctx, repo, day.Add(-24*time.Hour), day.Add(24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, hoursBefore, hours)
	require.Equal(t, uint64(1), hours[1].ViewUniques)
	require.Equal(t, uint64(1), hours[1].CloneUniques)
}

func TestTrafficPruneUniquesBoundsAndBatches(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	tr := s.Traffic()
	repo := name(t, "proj")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, CreatedAt: trafficEpoch, Visibility: domain.VisibilityPrivate}))
	day := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	hour := day.Add(5 * time.Hour)
	// 10,002 markers in one hourly bucket force a flush inside the delete loop.
	var hits []domain.TrafficHit
	for i := range 5001 {
		for _, kind := range []domain.TrafficKind{domain.TrafficView, domain.TrafficClone} {
			hits = append(hits, domain.TrafficHit{Repo: repo, Kind: kind, At: hour, VisitorHash: visitor(uint64(i))})
		}
	}
	require.NoError(t, tr.Record(ctx, hits))
	hourPrefix := "traffic/u/proj/h/20260601T050000Z/"
	dayPrefix := "traffic/u/proj/d/20260601T000000Z/"
	require.Equal(t, 10002, s.CountKeysForTest(ctx, hourPrefix))
	before, err := tr.Hourly(ctx, repo, day, day.Add(24*time.Hour))
	require.NoError(t, err)

	n, err := tr.Prune(ctx, hour.Add(2*time.Hour))
	require.NoError(t, err)
	require.Zero(t, n)
	require.Zero(t, s.CountKeysForTest(ctx, hourPrefix))
	after, err := tr.Hourly(ctx, repo, day, day.Add(24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, before, after)

	// The day bucket ends at June 2 00:00: kept at 00:59, dropped at 01:00.
	_, err = tr.Prune(ctx, day.Add(25*time.Hour-time.Minute))
	require.NoError(t, err)
	require.Equal(t, 10002, s.CountKeysForTest(ctx, dayPrefix))
	_, err = tr.Prune(ctx, day.Add(25*time.Hour))
	require.NoError(t, err)
	require.Zero(t, s.CountKeysForTest(ctx, dayPrefix))
	days, err := tr.Daily(ctx, repo, day, day.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, days, 1)
	require.Equal(t, uint64(5001), days[0].ViewUniques)
	require.Equal(t, uint64(5001), days[0].CloneUniques)
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

func TestTrafficRejectsInvalidReferrers(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	repo := name(t, "proj")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, CreatedAt: created, Visibility: domain.VisibilityPrivate}))
	hosts := []string{"example.com", "127.0.0.1", "[::1]", "bad_label.test", "bad..test", strings.Repeat("a", 254) + ".test", "example.com/path"}
	for _, host := range hosts {
		require.NoError(t, s.Traffic().Record(ctx, []domain.TrafficHit{{Repo: repo, Kind: domain.TrafficView, At: created, ReferrerDomain: host}}))
	}
	days, err := s.Traffic().Daily(ctx, repo, created.Add(-24*time.Hour), created.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, days, 1)
	require.Equal(t, map[string]uint64{"example.com": 1}, days[0].Referrers)
	require.Equal(t, uint64(len(hosts)), days[0].Views)
}

// visitor returns a distinct 16-byte visitor hash for n.
func visitor(n uint64) [16]byte {
	var h [16]byte
	binary.BigEndian.PutUint64(h[:8], n)
	binary.BigEndian.PutUint64(h[8:], ^n)
	return h
}

func TestTrafficSaturatedReposInOneRecord(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	tr := s.Traffic()
	var repos []domain.RepoName
	var hits []domain.TrafficHit
	for r := range 12 {
		repo := name(t, fmt.Sprintf("repo%02d", r))
		repos = append(repos, repo)
		require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, CreatedAt: created, Visibility: domain.VisibilityPrivate}))
		for i := range 10001 {
			h := domain.TrafficHit{Repo: repo, Kind: domain.TrafficView, At: created, VisitorHash: visitor(uint64(i))}
			hits = append(hits, h)
		}
	}
	require.NoError(t, tr.Record(ctx, hits))
	for _, repo := range repos {
		days, err := tr.Daily(ctx, repo, created.Add(-24*time.Hour), created.Add(24*time.Hour))
		require.NoError(t, err)
		require.Len(t, days, 1)
		require.Equal(t, uint64(10001), days[0].Views)
		require.Equal(t, uint64(10000), days[0].ViewUniques)
		require.True(t, days[0].Partial)
	}
	// Each saturated bucket keeps exactly the cap of unique keys.
	require.Equal(t, 12*2*10000, s.CountKeysForTest(ctx, "traffic/u/"))
	require.Equal(t, 12*2, s.CountKeysForTest(ctx, "traffic/a/"))
}

func TestTrafficDedupesAcrossCalls(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	tr := s.Traffic()
	repo := name(t, "proj")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, CreatedAt: created, Visibility: domain.VisibilityPrivate}))
	view := domain.TrafficHit{Repo: repo, Kind: domain.TrafficView, At: created, VisitorHash: visitor(1)}
	clone := view
	clone.Kind = domain.TrafficClone
	other := view
	other.VisitorHash = visitor(2)
	require.NoError(t, tr.Record(ctx, []domain.TrafficHit{view}))
	require.NoError(t, tr.Record(ctx, []domain.TrafficHit{view, clone}))
	require.NoError(t, tr.Record(ctx, []domain.TrafficHit{clone, other}))
	days, err := tr.Daily(ctx, repo, created.Add(-24*time.Hour), created.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, days, 1)
	require.Equal(t, domain.TrafficCounts{Views: 3, ViewUniques: 2, Clones: 2, CloneUniques: 1}, days[0].TrafficCounts)
}

func TestTrafficRangeReadsHonorBounds(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	tr := s.Traffic()
	repo, longer := name(t, "proj"), name(t, "proj-two")
	day := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	for _, r := range []domain.RepoName{repo, longer} {
		require.NoError(t, s.Create(ctx, domain.Repo{Name: r, CreatedAt: trafficEpoch, Visibility: domain.VisibilityPrivate}))
		var hits []domain.TrafficHit
		for d := range 5 {
			hits = append(hits, domain.TrafficHit{Repo: r, Kind: domain.TrafficView, At: day.Add(time.Duration(d)*24*time.Hour + 90*time.Minute)})
		}
		require.NoError(t, tr.Record(ctx, hits))
	}
	// A malformed record outside every requested range must never be read.
	require.NoError(t, s.PutRawForTest(ctx, "traffic/a/proj/d/20260301T000000Z", []byte("not json")))
	require.NoError(t, s.PutRawForTest(ctx, "traffic/a/proj/d/20260401T000000Z", []byte("not json")))
	days, err := tr.Daily(ctx, repo, day.Add(24*time.Hour), day.Add(3*24*time.Hour))
	require.NoError(t, err)
	require.Len(t, days, 2)
	require.Equal(t, day.Add(24*time.Hour), days[0].Start)
	require.Equal(t, day.Add(2*24*time.Hour), days[1].Start)
	// A sub-second bound still selects starts in [from, to).
	days, err = tr.Daily(ctx, repo, day.Add(time.Nanosecond), day.Add(24*time.Hour+time.Nanosecond))
	require.NoError(t, err)
	require.Len(t, days, 1)
	require.Equal(t, day.Add(24*time.Hour), days[0].Start)
	hours, err := tr.Hourly(ctx, repo, day.Add(time.Hour), day.Add(2*time.Hour))
	require.NoError(t, err)
	require.Len(t, hours, 1)
	require.Equal(t, day.Add(time.Hour), hours[0].Start)
	empty, err := tr.Daily(ctx, repo, day.Add(24*time.Hour), day.Add(24*time.Hour))
	require.NoError(t, err)
	require.Empty(t, empty)
	rows, err := tr.Summaries(ctx, day, day.Add(5*24*time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, uint64(5), rows[0].Views)
	require.Equal(t, uint64(5), rows[1].Views)
	_, err = tr.Daily(ctx, repo, day.Add(-30*24*time.Hour), day)
	require.Error(t, err)
}

func TestTrafficFencesRecreatedRepository(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	tr := s.Traffic()
	repo := name(t, "proj")
	t0 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t1, t2 := t0.Add(time.Minute), t0.Add(2*time.Minute)
	require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, CreatedAt: t0, Visibility: domain.VisibilityPrivate}))
	// A hit admitted for the first repository, still queued when it is deleted.
	old := domain.TrafficHit{Repo: repo, Kind: domain.TrafficView, At: t1, VisitorHash: visitor(1), ReferrerDomain: "old.test"}
	require.NoError(t, s.Delete(ctx, repo))
	require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, CreatedAt: t2, Visibility: domain.VisibilityPublic}))
	require.NoError(t, tr.Record(ctx, []domain.TrafficHit{old}))
	days, err := tr.Daily(ctx, repo, t0.Add(-24*time.Hour), t0.Add(24*time.Hour))
	require.NoError(t, err)
	require.Empty(t, days)
	require.Zero(t, s.CountKeysForTest(ctx, "traffic/"))
	// Hits admitted at or after the new creation time are kept.
	current := domain.TrafficHit{Repo: repo, Kind: domain.TrafficView, At: t2, VisitorHash: visitor(2), ReferrerDomain: "new.test"}
	require.NoError(t, tr.Record(ctx, []domain.TrafficHit{old, current}))
	days, err = tr.Daily(ctx, repo, t0.Add(-24*time.Hour), t0.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, days, 1)
	require.Equal(t, uint64(1), days[0].Views)
	require.Equal(t, map[string]uint64{"new.test": 1}, days[0].Referrers)
}

func TestTrafficImportHistoryKeepsPreCreationHits(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	repo := name(t, "proj")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, CreatedAt: created, Visibility: domain.VisibilityPrivate}))
	past := domain.TrafficHit{Repo: repo, Kind: domain.TrafficClone, At: created.Add(-30 * 24 * time.Hour)}
	require.NoError(t, s.Traffic().Record(ctx, []domain.TrafficHit{past}))
	require.Zero(t, s.CountKeysForTest(ctx, "traffic/a/"))
	require.NoError(t, s.Traffic().ImportHistory(ctx, []domain.TrafficHit{past}))
	require.Equal(t, 2, s.CountKeysForTest(ctx, "traffic/a/"))
}
