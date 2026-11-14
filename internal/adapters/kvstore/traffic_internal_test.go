// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

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

// TestTrafficBatchBudget checks that the budget leaves room under the
// ermite-kv batch limit for the one hit that may start a sub-batch.
func TestTrafficBatchBudget(t *testing.T) {
	const kvLimit = 16 << 20
	repo, err := domain.ParseRepoName(strings.Repeat("r", domain.MaxRepoNameLen))
	require.NoError(t, err)
	hit := domain.TrafficHit{Repo: repo, Kind: domain.TrafficView, At: time.Now()}
	uniqueKey := trafficUniqueKey(hit, trafficDaily, trafficStart(hit.At, trafficDaily))
	worstHit := 2*trafficAggregateMaxBytes + 2*putSize(len(uniqueKey), 0) + putSize(len(repoKey(repo)), 1<<20)
	require.Less(t, trafficBatchBudget+worstHit, kvLimit/2)
}
