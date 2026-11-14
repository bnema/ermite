// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"sort"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/usecase"
)

var trafficRanges = []struct {
	param, label, long string
	window             time.Duration
}{
	{"24h", "24 h", "24 hours", 24 * time.Hour},
	{"30d", "30 days", "30 days", 30 * 24 * time.Hour},
}

// InboxTrafficView is the owner's traffic overview for the selected window.
type InboxTrafficView struct {
	Ranges    []InboxNavLink
	Totals    domain.TrafficCounts
	Repos     []domain.TrafficSummary
	Referrers []usecase.TrafficReferrer
}

func (s *Server) inboxTraffic(ctx context.Context, rng int) (*InboxTrafficView, error) {
	reader, ok := s.cfg.Traffic.(TrafficForgeReader)
	if !ok {
		return nil, errNotFound
	}
	data, err := reader.Forge(ctx, trafficRanges[rng].window)
	if err != nil {
		return nil, err
	}
	v := &InboxTrafficView{Totals: data.Totals, Repos: append([]domain.TrafficSummary(nil), data.Repos...), Referrers: data.Referrers}
	// Own the ordering without mutating the reader's result.
	sort.SliceStable(v.Repos, func(i, j int) bool {
		a, b := v.Repos[i], v.Repos[j]
		return a.Views+a.Clones > b.Views+b.Clones
	})
	for i, r := range trafficRanges {
		v.Ranges = append(v.Ranges, InboxNavLink{Label: r.label, Href: inboxHref("traffic", i) + "#inbox-traffic", Current: i == rng})
	}
	return v, nil
}
