// SPDX-License-Identifier: AGPL-3.0-or-later

package ports

import "bnema.dev/ermite/internal/domain"

// TrafficReferrer is a domain-only referral count.
type TrafficReferrer struct {
	Domain string
	Count  uint64
}

// TrafficForge contains ranked repository rows, totals and top referrers.
type TrafficForge struct {
	Repos     []domain.TrafficSummary
	Totals    domain.TrafficCounts
	Referrers []TrafficReferrer
}

// TrafficReport contains window totals and points, and top referrers over 30 days.
type TrafficReport struct {
	Totals    domain.TrafficCounts
	Points    []domain.TrafficBucket
	Referrers []TrafficReferrer
}
