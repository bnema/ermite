// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/a-h/templ"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// insightsPath is the owner's traffic across the forge.
const insightsPath = "/-/insights"

// trafficRanges are the windows of the insights, by ?range=.
var trafficRanges = []pageRange{
	{"24h", "24 h", "24 hours", 24 * time.Hour},
	{"30d", "30 days", "30 days", 30 * 24 * time.Hour},
}

// InsightsView is the owner's traffic overview for the selected window.
type InsightsView struct {
	Ranges    []NavLink
	Totals    domain.TrafficCounts
	Repos     []domain.TrafficSummary
	Referrers []ports.TrafficReferrer
}

func (s *Server) insightsView(ctx context.Context, rng int) (*InsightsView, error) {
	reader, ok := s.cfg.Traffic.(TrafficForgeReader)
	if !ok {
		return nil, errNotFound
	}
	data, err := reader.Forge(ctx, trafficRanges[rng].window)
	if err != nil {
		return nil, err
	}
	v := &InsightsView{Totals: data.Totals, Repos: append([]domain.TrafficSummary(nil), data.Repos...), Referrers: data.Referrers}
	// Own the ordering without mutating the reader's result.
	sort.SliceStable(v.Repos, func(i, j int) bool {
		a, b := v.Repos[i], v.Repos[j]
		return a.Views+a.Clones > b.Views+b.Clones
	})
	v.Ranges = rangeLinks(insightsPath, trafficRanges, rng)
	return v, nil
}

// insights is GET /-/insights?range=: the traffic of every repository.
func (s *Server) insights(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	if _, ok := s.cfg.Traffic.(TrafficForgeReader); !ok || !SignedIn(ctx) {
		return nil, 0, errNotFound
	}
	rng, err := parseRange(trafficRanges, r.URL.Query().Get("range"))
	if err != nil {
		return nil, 0, err
	}
	v, err := s.insightsView(ctx, rng)
	if err != nil {
		return nil, 0, err
	}
	return Insights(s.ownerLayout(ctx, "insights", "Insights", "range="+trafficRanges[rng].param), *v), http.StatusOK, nil
}
