// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"net/url"
	"strings"
	"time"
)

// TrafficKind identifies the operation counted by a traffic hit.
type TrafficKind string

const (
	TrafficClone TrafficKind = "clone"
	TrafficView  TrafficKind = "view"
)

// TrafficHit is a privacy-preserving observation. VisitorHash is a truncated
// SHA-256 hash of a random, in-memory daily salt followed by the visitor IP.
// It must never contain the IP itself. Bot applies only to views.
type TrafficHit struct {
	Repo           RepoName
	Kind           TrafficKind
	At             time.Time
	VisitorHash    [16]byte
	Bot            bool
	ReferrerDomain string
}

// TrafficCounts holds additive counters. Uniques are distinct within each
// bucket, not across buckets: summing daily uniques counts visitor-days.
// When Partial is true, a bounded unique set filled and uniques are a lower
// bound. Bots are excluded from Views and ViewUniques.
type TrafficCounts struct {
	Views        uint64
	ViewUniques  uint64
	Clones       uint64
	CloneUniques uint64
	Bots         uint64
	Partial      bool
}

// TrafficBucket holds counters for an interval beginning at Start in UTC.
// Referrers is populated only for daily buckets and contains domain counts.
type TrafficBucket struct {
	Start time.Time
	TrafficCounts
	Referrers map[string]uint64
}

// TrafficDay is a daily UTC traffic bucket.
type TrafficDay = TrafficBucket

// TrafficSeries contains oldest-first buckets for one repository.
type TrafficSeries struct {
	Repo    RepoName
	Buckets []TrafficBucket
}

// TrafficSummary contains totals for a repository over a requested window.
// Unique counters are sums of bucket uniques, not window-wide distinct users.
type TrafficSummary struct {
	Repo RepoName
	TrafficCounts
	Referrers map[string]uint64
}

// IsBotUserAgent classifies view user agents. An empty agent is a bot;
// curl and wget are not. Clone callers must not use this classification.
func IsBotUserAgent(ua string) bool {
	ua = strings.ToLower(strings.TrimSpace(ua))
	if ua == "" {
		return true
	}
	for _, token := range []string{"bot", "crawl", "spider", "slurp", "facebookexternalhit", "ahrefs", "semrush", "yandex", "bingpreview", "headless"} {
		if strings.Contains(ua, token) {
			return true
		}
	}
	return false
}

// ReferrerDomain returns only the normalized hostname of an absolute HTTP(S)
// referrer. Empty, malformed and same-host referrers are ignored. selfHost is
// the forge's public hostname, optionally with a port, not a URL.
func ReferrerDomain(refererURL, selfHost string) string {
	u, err := url.Parse(refererURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return ""
	}
	normalize := func(host string) string {
		return strings.TrimPrefix(strings.ToLower(strings.TrimSuffix(host, ".")), "www.")
	}
	host := normalize(u.Hostname())
	self, err := url.Parse("//" + selfHost)
	if err == nil && host == normalize(self.Hostname()) {
		return ""
	}
	return host
}
