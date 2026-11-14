// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	kv "bnema.dev/ermite-kv"
	"bnema.dev/ermite/internal/domain"
	"github.com/stretchr/testify/require"
)

// TestTrafficAggregateBound checks that the largest possible aggregate put
// fits trafficAggregateMaxBytes, which the sub-batch budget relies on.
func TestTrafficAggregateBound(t *testing.T) {
	repo, err := domain.ParseRepoName(strings.Repeat("r", domain.MaxRepoNameLen))
	require.NoError(t, err)
	agg := trafficAggregate{
		TrafficCounts: domain.TrafficCounts{Views: math.MaxUint64, ViewUniques: math.MaxUint64, Clones: math.MaxUint64, CloneUniques: math.MaxUint64, Bots: math.MaxUint64, Partial: true},
		Referrers:     map[string]uint64{},
	}
	for i := range trafficReferrerCap {
		host := fmt.Sprintf("%02d.", i) + strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 63)
		host = host[:trafficReferrerMaxLen]
		agg.Referrers[host] = math.MaxUint64
	}
	value, err := json.Marshal(agg)
	require.NoError(t, err)
	key := trafficAggKey(repo, trafficDaily, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	require.LessOrEqual(t, putSize(len(key), len(value)), trafficAggregateMaxBytes)
}

// TestTrafficRecordDedupesAcrossSubBatches verifies the snapshot of a later
// sub-batch sees a visitor marker written by an earlier one in the same call.
func TestTrafficRecordDedupesAcrossSubBatches(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close(ctx)) })
	repo, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, CreatedAt: at, Visibility: domain.VisibilityPrivate}))
	hit := domain.TrafficHit{Repo: repo, Kind: domain.TrafficView, At: at}
	hits := []domain.TrafficHit{hit}
	// Distinct daily and hourly buckets consume the real sub-batch budget.
	for i := 1; i <= trafficBatchBudget/trafficAggregateMaxBytes; i++ {
		other := hit
		other.At = at.Add(time.Duration(i) * 24 * time.Hour)
		hits = append(hits, other)
	}
	hits = append(hits, hit)
	w := newTrafficWrite(s.db.Snapshot(ctx), true)
	n, err := w.addHits(ctx, hits)
	require.NoError(t, err)
	require.Positive(t, n)
	require.Less(t, n, len(hits)-1, "duplicate must fall in a later sub-batch")
	require.NoError(t, s.Traffic().Record(ctx, hits))
	for _, width := range []string{trafficDaily, trafficHourly} {
		buckets, err := s.Traffic().buckets(ctx, repo, width, at, at.Add(time.Hour))
		require.NoError(t, err)
		require.Len(t, buckets, 1)
		require.Equal(t, uint64(2), buckets[0].Views)
		require.Equal(t, uint64(1), buckets[0].ViewUniques)
	}
}

func TestTrafficDeletionKeysKeepAggregatesUntilUniquesDeleted(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close(ctx)) })
	repo, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, CreatedAt: at, Visibility: domain.VisibilityPrivate}))
	var hits []domain.TrafficHit
	for i := range 600 {
		h := domain.TrafficHit{Repo: repo, Kind: domain.TrafficView, At: at}
		h.VisitorHash[0], h.VisitorHash[1] = byte(i), byte(i>>8)
		hits = append(hits, h)
	}
	require.NoError(t, s.Traffic().Record(ctx, hits))
	keys := trafficKeys(ctx, s.db.Snapshot(ctx), repo)
	require.Len(t, keys, 1202)
	for i, key := range keys {
		prefix := trafficUniqueRepoPrefix(repo)
		if i >= 1200 {
			prefix = trafficAggRepoPrefix(repo)
		}
		require.True(t, strings.HasPrefix(string(key), prefix))
	}
	// Simulate interruption after the first repository-delete chunk.
	var batch kv.Batch
	for _, key := range keys[:1000] {
		batch.Delete(key)
	}
	_, err = s.db.Apply(ctx, &batch)
	require.NoError(t, err)
	snap := s.db.Snapshot(ctx)
	for _, width := range []string{trafficDaily, trafficHourly} {
		_, ok := snap.Get(ctx, []byte(trafficAggKey(repo, width, at)))
		require.True(t, ok)
	}
}
