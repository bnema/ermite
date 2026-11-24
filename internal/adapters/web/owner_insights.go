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
	Referrers []ports.TrafficReferrer
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
		href := insightsPath
		if i != 0 {
			href += "?range=" + r.param
		}
		v.Ranges = append(v.Ranges, InboxNavLink{Label: r.label, Href: href, Current: i == rng})
	}
	return v, nil
}

// insightsRange reads ?range= of the insights: its index in trafficRanges;
// an unknown range is missing.
func insightsRange(raw string) (int, error) {
	for i, r := range trafficRanges {
		if r.param == raw || (raw == "" && i == 0) {
			return i, nil
		}
	}
	return 0, errNotFound
}

// insights is GET /-/insights?range=: the traffic of every repository.
func (s *Server) insights(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	if _, ok := s.cfg.Traffic.(TrafficForgeReader); !ok || !SignedIn(ctx) {
		return nil, 0, errNotFound
	}
	rng, err := insightsRange(r.URL.Query().Get("range"))
	if err != nil {
		return nil, 0, err
	}
	v, err := s.inboxTraffic(ctx, rng)
	if err != nil {
		return nil, 0, err
	}
	l := s.base()
	l.Title = "Insights · " + s.instance
	l.OwnerNav = s.ownerNav(ctx, "insights")
	l.Cmd = []CmdItem{{Kind: "go", Label: "home", Href: "/"}}
	return Insights(l, *v), http.StatusOK, nil
}
