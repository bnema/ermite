// SPDX-License-Identifier: AGPL-3.0-or-later
package domain

import "time"

func CronMinGap(c Cron) time.Duration {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	// Include the next January so adjacent December/January runs are compared.
	end := start.AddDate(1, 1, 0)
	var prev time.Time
	var gap time.Duration
	for t := start; t.Before(end); t = t.Add(time.Minute) {
		if c.Matches(t) {
			if !prev.IsZero() {
				d := t.Sub(prev)
				if gap == 0 || d < gap {
					gap = d
				}
			}
			prev = t
		}
	}
	return gap
}
func NextCron(s CronSchedule, t time.Time) (time.Time, bool) {
	end := t.Add(400 * 24 * time.Hour)
	for n := t.Truncate(time.Minute).Add(time.Minute); !n.After(end); n = n.Add(time.Minute) {
		if s.Due(n) {
			return n, true
		}
	}
	return time.Time{}, false
}
