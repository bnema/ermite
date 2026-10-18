// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"slices"
	"sync"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

const (
	// maxStatsEntries bounds the cache of figures; when full it is emptied,
	// and the entries of the current heads come back on the next read.
	maxStatsEntries = 1024
	// statsTTL is how long figures are served: the 30-day window moves
	// with time even when the repository does not.
	statsTTL = 10 * time.Minute
)

// statsKey names the figures of one state of a repository: its life (the
// creation time), its last push or merge (which every ref change, tags
// included, moves) and the head counted from.
type statsKey struct {
	repo    domain.RepoName
	created int64
	pushed  int64
	head    domain.ObjectID
}

type statsEntry struct {
	st  ports.RepoStats
	at  time.Time
	err error
	// done is closed when st and err are set; until then the entry is the
	// read in flight that the other callers wait for.
	done chan struct{}
}

// statsCache keeps the figures of the home page, so that a request reads Git
// once per repository state and per statsTTL, and concurrent requests share
// one read.
type statsCache struct {
	mu sync.Mutex
	m  map[statsKey]*statsEntry
}

// get returns the figures of key, computing them with read when they are
// missing, failed or older than statsTTL.
func (c *statsCache) get(ctx context.Context, key statsKey, now time.Time, read func() (ports.RepoStats, error)) (ports.RepoStats, error) {
	c.mu.Lock()
	e, ok := c.m[key]
	if ok {
		select {
		case <-e.done:
			if e.err != nil || now.Sub(e.at) >= statsTTL || now.Before(e.at) {
				ok = false
			}
		default: // in flight
		}
	}
	if !ok {
		if c.m == nil || len(c.m) >= maxStatsEntries {
			c.m = make(map[statsKey]*statsEntry)
		}
		e = &statsEntry{at: now, done: make(chan struct{})}
		c.m[key] = e
		c.mu.Unlock()
		e.st, e.err = read()
		close(e.done)
		return cloneStats(e.st), e.err
	}
	c.mu.Unlock()
	select {
	case <-e.done:
		return cloneStats(e.st), e.err
	case <-ctx.Done():
		return ports.RepoStats{}, ctx.Err()
	}
}

// activity counts the commits of the last ports.RecentDays days, and those
// of each of the last ports.ActivityDays days, oldest first. A day is the 24
// hours that end at now minus a whole number of days; a commit dated after
// now counts in the last day.
func activity(times []time.Time, now time.Time) (recent int, days []int) {
	days = make([]int, ports.ActivityDays)
	recentSince := daysBefore(now, ports.RecentDays)
	for _, t := range times {
		if !t.Before(recentSince) {
			recent++
		}
		ago := int(now.Sub(t) / (24 * time.Hour))
		if ago < 0 {
			ago = 0
		}
		if ago < ports.ActivityDays {
			days[ports.ActivityDays-1-ago]++
		}
	}
	return recent, days
}

// daysBefore is now minus n days of 24 hours: the days of activity are
// elapsed time, not calendar days, so that a change of daylight saving time
// neither drops nor doubles an hour.
func daysBefore(now time.Time, n int) time.Time {
	return now.Add(-time.Duration(n) * 24 * time.Hour)
}

func cloneStats(st ports.RepoStats) ports.RepoStats {
	st.Activity = slices.Clone(st.Activity)
	if st.LatestTag != nil {
		t := *st.LatestTag
		st.LatestTag = &t
	}
	return st
}

// RepoStats counts the commits reachable from head, all of them, those of
// the last ports.RecentDays days and those of each recent day, and names the
// latest tag. The answer is kept for statsTTL, or until the repository is
// pushed to: the home page and its live updates read it on every row.
func (b *Browse) RepoStats(ctx context.Context, p domain.Principal, repo domain.RepoName, head domain.ObjectID, now time.Time) (ports.RepoStats, error) {
	r, err := b.authorize(ctx, p, repo)
	if err != nil {
		return ports.RepoStats{}, err
	}
	key := statsKey{repo: r.Name, created: repoEpoch(r), pushed: r.PushedAt.UnixNano(), head: head}
	return b.stats.get(ctx, key, now, func() (ports.RepoStats, error) {
		// The read outlives a caller that gives up: the others wait for it.
		rctx := context.WithoutCancel(ctx)
		var st ports.RepoStats
		var err error
		if st.Commits, err = b.reader.CountCommits(rctx, repo, head); err != nil {
			return ports.RepoStats{}, err
		}
		// One walk gives both the recent count and the activity.
		span := max(ports.RecentDays, ports.ActivityDays)
		times, truncated, err := b.reader.CommitTimes(rctx, repo, head, daysBefore(now, span))
		if err != nil {
			return ports.RepoStats{}, err
		}
		st.Recent, st.Activity = activity(times, now)
		st.Partial = truncated
		tag, ok, err := b.reader.LatestTag(rctx, repo)
		if err != nil {
			return ports.RepoStats{}, err
		}
		if ok {
			st.LatestTag = &tag
		}
		return st, nil
	})
}
