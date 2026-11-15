// SPDX-License-Identifier: AGPL-3.0-or-later

package githttp

import (
	"sync"
	"time"
)

const (
	refusalLogInterval = time.Minute
	refusalLogMaxKeys  = 10_000
)

// refusalLog lets through one log line per key and interval, counting
// the refusals it swallowed. An entry evicted to make room loses its count.
// Safe for concurrent use.
type refusalLog struct {
	now       func() time.Time
	interval  time.Duration
	mu        sync.Mutex
	nextSweep time.Time
	seen      map[string]*refusalEntry
}

type refusalEntry struct {
	last    time.Time
	dropped int
}

// allow reports whether to log now and how many refusals were swallowed
// since the last line for key.
func (l *refusalLog) allow(key string) (ok bool, dropped int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if entry, exists := l.seen[key]; exists {
		if now.Sub(entry.last) < l.interval {
			entry.dropped++
			return false, 0
		}
		dropped = entry.dropped
		entry.last, entry.dropped = now, 0
		return true, dropped
	}
	if len(l.seen) >= refusalLogMaxKeys {
		if now.Before(l.nextSweep) {
			return false, 0
		}
		l.nextSweep = time.Time{}
		for k, entry := range l.seen {
			if now.Sub(entry.last) >= l.interval {
				delete(l.seen, k)
				continue
			}
			expires := entry.last.Add(l.interval)
			if l.nextSweep.IsZero() || expires.Before(l.nextSweep) {
				l.nextSweep = expires
			}
		}
		if len(l.seen) >= refusalLogMaxKeys {
			return false, 0
		}
	}
	l.seen[key] = &refusalEntry{last: now}
	return true, 0
}
