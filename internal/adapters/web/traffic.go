// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"net/netip"

	"bnema.dev/ermite/internal/clientlimit"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// TrafficRecorder collects repository visits without blocking.
type TrafficRecorder = ports.TrafficRecorder

// TrafficDailyReader supplies daily unique visitors.
type TrafficDailyReader = ports.TrafficDailyReader

// TrafficSummaryReader supplies the repository's last 30 days of traffic.
type TrafficSummaryReader = ports.TrafficSummaryReader

// recordView runs only after HTML rendering and conditional-cache handling.
// Match registered routes, not URL prefixes: fragments and downloads must not
// become views when new routes are added underneath a repository page.
func (s *Server) recordView(ctx context.Context, r *http.Request, status int) {
	if s.cfg.Traffic == nil || status != http.StatusOK || r.Method != http.MethodGet || SignedIn(ctx) || isHX(r) {
		return
	}
	switch r.Pattern {
	case "GET /{repo}", "GET /{repo}/{$}",
		"GET /{repo}/tree", "GET /{repo}/tree/{rest...}", "GET /{repo}/blob/{rest...}",
		"GET /{repo}/commits", "GET /{repo}/commits/{rest...}", "GET /{repo}/commit/{sha}",
		"GET /{repo}/refs", "GET /{repo}/compare/{rest...}",
		"GET /{repo}/issues", "GET /{repo}/issues/{$}", "GET /{repo}/issues/{n}",
		"GET /{repo}/merge-requests", "GET /{repo}/merge-requests/{$}", "GET /{repo}/merge-requests/{n}",
		"GET /{repo}/releases", "GET /{repo}/releases/{$}", "GET /{repo}/releases/tag/{tag...}",
		"GET /{repo}/actions", "GET /{repo}/actions/{$}", "GET /{repo}/actions/runs/{id}",
		"GET /{repo}/actions/runs/{id}/job/{job}":
	default:
		return
	}
	name, err := domain.ParseRepoName(r.PathValue("repo"))
	if err != nil {
		return
	}
	ip, _ := netip.ParseAddr(clientlimit.ClientAddr(r, s.cfg.TrustedProxies))
	s.cfg.Traffic.Hit(name, domain.TrafficView, ip, r.UserAgent(), r.Referer())
}

// TrafficForgeReader supplies owner-only traffic across the forge.
type TrafficForgeReader = ports.TrafficForgeReader
