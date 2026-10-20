// SPDX-License-Identifier: AGPL-3.0-or-later
package domain

import (
	"sync"
	"time"
)

// cronGapCache memoizes CronMinGap per parsed entry: a scan covers two years of minutes.
const cronGapCacheMax = 256

var cronGapCache = struct {
	sync.Mutex
	m map[Cron]time.Duration
}{m: make(map[Cron]time.Duration)}

// CronMinGap is the smallest gap between two firings, 0 when the entry fires fewer than twice.
// It scans a leap year, the following non-leap year and the January after, so that Feb 29,
// year-end and Feb 28/Mar 1 neighbours are all compared.
func CronMinGap(c Cron) time.Duration {
	cronGapCache.Lock()
	gap, ok := cronGapCache.m[c]
	cronGapCache.Unlock()
	if ok {
		return gap
	}
	gap = scanCronMinGap(c)
	cronGapCache.Lock()
	if len(cronGapCache.m) >= cronGapCacheMax {
		clear(cronGapCache.m)
	}
	cronGapCache.m[c] = gap
	cronGapCache.Unlock()
	return gap
}

func scanCronMinGap(c Cron) time.Duration {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC) // leap
	end := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)   // 2025 is not leap; include Jan 2026
	var prev time.Time
	var gap time.Duration
	for t := start; t.Before(end); t = t.Add(time.Minute) {
		if !c.dayMatches(t) {
			t = t.Truncate(24 * time.Hour).Add(24*time.Hour - time.Minute)
			continue
		}
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

// dayMatches is the day-level part of Matches (month, day of month, day of week), UTC.
func (c Cron) dayMatches(t time.Time) bool {
	t = t.UTC()
	if c.month&(1<<uint(t.Month())) == 0 {
		return false
	}
	domOK := c.dom&(1<<uint(t.Day())) != 0
	dowOK := c.dow&(1<<uint(t.Weekday())) != 0
	if !c.domStar && !c.dowStar {
		return domOK || dowOK
	}
	return domOK && dowOK
}

// nextCronHorizon bounds NextCron: a year of minutes.
const nextCronHorizon = 366 * 24 * time.Hour

// NextCron is the first firing after t, within a year. UTC schedules skip the days that cannot match.
func NextCron(s CronSchedule, t time.Time) (time.Time, bool) {
	end := t.Add(nextCronHorizon)
	utc := s.Loc == nil || s.Loc == time.UTC
	for n := t.Truncate(time.Minute).Add(time.Minute); !n.After(end); n = n.Add(time.Minute) {
		if utc && !s.Cron.dayMatches(n) {
			n = n.Truncate(24 * time.Hour).Add(24*time.Hour - time.Minute)
			continue
		}
		if s.Due(n) {
			return n, true
		}
	}
	return time.Time{}, false
}
