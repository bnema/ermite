// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"sort"
	"time"

	"bnema.dev/ermite/internal/domain"
)

// TrafficReport contains window totals and points, and top referrers over 30 days.
type TrafficReport struct {
	Totals    domain.TrafficCounts
	Points    []domain.TrafficBucket
	Referrers []TrafficReferrer
}

// RepoTraffic reads 24 hourly or 30 daily UTC buckets, including the current bucket.
// Referrers always cover the current UTC day and preceding 29 days.
func (t *Traffic) RepoTraffic(ctx context.Context, repo domain.RepoName, window time.Duration) (TrafficReport, error) {
	if window != 24*time.Hour && window != 30*24*time.Hour {
		return TrafficReport{}, domain.ErrInvalid
	}
	from, to := t.dayWindow(30)
	daily, err := t.cfg.Store.Daily(ctx, repo, from, to)
	if err != nil {
		return TrafficReport{}, err
	}
	result := TrafficReport{Points: make([]domain.TrafficBucket, 30), Referrers: []TrafficReferrer{}}
	for i := range result.Points {
		result.Points[i].Start = from.Add(time.Duration(i) * 24 * time.Hour)
	}
	refs := map[string]uint64{}
	for _, b := range daily {
		i := int(b.Start.Sub(from) / (24 * time.Hour))
		if i >= 0 && i < len(result.Points) {
			result.Points[i] = b
		}
		for host, n := range b.Referrers {
			refs[host] += n
		}
	}
	if window == 24*time.Hour {
		result.Points, err = t.RepoHourly(ctx, repo)
		if err != nil {
			return TrafficReport{}, err
		}
	}
	for _, b := range result.Points {
		addTraffic(&result.Totals, b.TrafficCounts)
	}
	for host, n := range refs {
		result.Referrers = append(result.Referrers, TrafficReferrer{Domain: host, Count: n})
	}
	sort.Slice(result.Referrers, func(i, j int) bool {
		a, b := result.Referrers[i], result.Referrers[j]
		if a.Count == b.Count {
			return a.Domain < b.Domain
		}
		return a.Count > b.Count
	})
	if len(result.Referrers) > 10 {
		result.Referrers = result.Referrers[:10]
	}
	return result, nil
}
