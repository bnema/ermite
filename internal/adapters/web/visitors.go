// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"slices"
	"sync"
	"time"

	"bnema.dev/ermite/internal/ports"
)

// visitorsFresh is how long a visitor series is served from the cache. The
// home page renders every row on each request, including 304s; this keeps
// the traffic store to one read per repository every few seconds.
const visitorsFresh = 30 * time.Second

// maxVisitors bounds visitorsCache; one entry per repository and day.
const maxVisitors = 4096

// visitorsCache keeps the recent daily visitors of the home rows. Keyed by
// the repository name, creation time and UTC day: the traffic of a repository is the
// same for every reader, and a new day shifts the series. Failed reads are
// not kept. Bounded, cleared when full.
type visitorsCache struct {
	mu sync.Mutex
	m  map[visitorsKey]visitorsEntry
}

type visitorsKey struct {
	repo      string
	createdAt time.Time
	day       string // UTC, YYYY-MM-DD
}

type visitorsEntry struct {
	at    time.Time
	daily []uint64
}

// get returns the visitors of repo over the activity window, from the cache
// when they were read less than visitorsFresh ago. The slice returned is the
// caller's own copy.
func (c *visitorsCache) get(ctx context.Context, traffic TrafficDailyReader, repo ports.RepoView, now time.Time) ([]uint64, error) {
	key := visitorsKey{repo: repo.Name.String(), createdAt: repo.CreatedAt.UTC(), day: now.UTC().Format("2006-01-02")}
	c.mu.Lock()
	e, ok := c.m[key]
	c.mu.Unlock()
	if ok && now.Sub(e.at) >= 0 && now.Sub(e.at) < visitorsFresh {
		return slices.Clone(e.daily), nil
	}
	daily, err := traffic.RepoDaily(ctx, repo.Name, ports.ActivityDays)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.m == nil || len(c.m) >= maxVisitors {
		c.m = make(map[visitorsKey]visitorsEntry)
	}
	c.m[key] = visitorsEntry{at: now, daily: slices.Clone(daily)}
	c.mu.Unlock()
	return slices.Clone(daily), nil
}
