// SPDX-License-Identifier: AGPL-3.0-or-later

package clientlimit

import (
	"context"
	"slices"
	"sync"
	"time"

	"bnema.dev/ermite/internal/ports"
)

// Submission windows of the public issue form.
const (
	// HourWindow and DayWindow are the sliding windows of the limits
	// PerHour and PerDay.
	HourWindow = time.Hour
	DayWindow  = 24 * time.Hour

	// PerHour and PerDay are the submissions one client may make in each
	// window. They are the limits of the form (PerHour and
	// PerDay), repeated here because this package may not import
	// the domain; a test keeps them equal.
	PerHour = 3
	PerDay  = 10

	submissionSweepInterval = HourWindow / 4
)

// SubmissionLimiter limits the submissions of the public issue form per client
// (an IPv4 address or an IPv6 /64, see Key): PerHour in a sliding
// hour and PerDay in a sliding day. It also counts the
// submissions of all clients in the last hour. It lives in memory, is bounded
// like Limiter, and is safe for concurrent use.
type SubmissionLimiter struct {
	now func() time.Time

	mu        sync.Mutex
	subs      map[string][]time.Time // oldest first, all within DayWindow
	global    []time.Time            // oldest first, all within HourWindow
	lastSweep time.Time
}

var _ ports.SubmissionLimiter = (*SubmissionLimiter)(nil)

// NewSubmissionLimiter builds a limiter. now defaults to time.Now.
func NewSubmissionLimiter(now func() time.Time) *SubmissionLimiter {
	if now == nil {
		now = time.Now
	}
	return &SubmissionLimiter{now: now, subs: make(map[string][]time.Time)}
}

// Allow reports whether addr may submit now. When it may not, retryAfter is
// how long until the oldest submission that counts leaves its window.
func (l *SubmissionLimiter) Allow(_ context.Context, addr string) (bool, time.Duration) {
	key := limiterKey(addr)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := l.prune(key, now)
	var wait time.Duration
	if h := withinWindow(ts, now, HourWindow); len(h) >= PerHour {
		wait = h[len(h)-PerHour].Add(HourWindow).Sub(now)
	}
	if len(ts) >= PerDay {
		wait = max(wait, ts[len(ts)-PerDay].Add(DayWindow).Sub(now))
	}
	if wait <= 0 {
		return true, 0
	}
	return false, max(wait, time.Second)
}

// Record counts a submission from addr.
func (l *SubmissionLimiter) Record(_ context.Context, addr string) {
	key := limiterKey(addr)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maintain(now)
	ts := append(l.prune(key, now), now)
	if len(ts) > PerDay*2 {
		ts = ts[len(ts)-PerDay*2:] // enough to compute the wait
	}
	l.subs[key] = ts
	l.global = append(l.pruneGlobal(now), now)
	if len(l.global) > maxTracked {
		l.global = l.global[len(l.global)-maxTracked:] // bound the memory
	}
}

// Recent returns the submissions of addr and of all clients in the last hour.
func (l *SubmissionLimiter) Recent(_ context.Context, addr string) (perAddr, global int) {
	key := limiterKey(addr)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(withinWindow(l.prune(key, now), now, HourWindow)), len(l.pruneGlobal(now))
}

// withinWindow returns the tail of ts (oldest first) that is within window.
func withinWindow(ts []time.Time, now time.Time, window time.Duration) []time.Time {
	cut := now.Add(-window)
	i, _ := slices.BinarySearchFunc(ts, cut, func(t, c time.Time) int {
		if t.After(c) {
			return 1
		}
		return -1
	})
	return ts[i:]
}

// prune drops the submissions of key older than DayWindow. The caller holds mu.
func (l *SubmissionLimiter) prune(key string, now time.Time) []time.Time {
	ts := withinWindow(l.subs[key], now, DayWindow)
	if len(ts) == 0 {
		delete(l.subs, key)
		return nil
	}
	l.subs[key] = ts
	return ts
}

// pruneGlobal drops the global submissions older than HourWindow. The caller
// holds mu.
func (l *SubmissionLimiter) pruneGlobal(now time.Time) []time.Time {
	l.global = withinWindow(l.global, now, HourWindow)
	return l.global
}

// maintain keeps the table small: expired entries go at most once per
// submissionSweepInterval, and a table at its bound loses its oldest tenth.
// The caller holds mu.
func (l *SubmissionLimiter) maintain(now time.Time) {
	if now.Sub(l.lastSweep) >= submissionSweepInterval {
		l.lastSweep = now
		for key := range l.subs {
			l.prune(key, now)
		}
	}
	if len(l.subs) < maxTracked {
		return
	}
	drop := len(l.subs) - (maxTracked - maxTracked/evictFraction)
	type entry struct {
		key  string
		last time.Time
	}
	all := make([]entry, 0, len(l.subs))
	for key, ts := range l.subs {
		all = append(all, entry{key, ts[len(ts)-1]})
	}
	slices.SortFunc(all, func(a, b entry) int { return a.last.Compare(b.last) })
	for _, e := range all[:drop] {
		delete(l.subs, e.key)
	}
}
