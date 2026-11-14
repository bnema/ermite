// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/zerowrap"
)

// TrafficConfig supplies storage, time and optional queue/batch tuning.
// Nonpositive sizes and intervals use 4096 hits, 512 hits and five seconds.
type TrafficConfig struct {
	Store      ports.TrafficStore
	Clock      ports.Clock
	SelfHost   string
	QueueSize  int
	FlushEvery time.Duration
	BatchSize  int
}

// Traffic records best-effort hits without storing IPs or persistent salts.
// Run must be called once, normally in a goroutine. Close stops admission,
// drains queued hits and waits for Run. Storage failures are logged, not retried.
type Traffic struct {
	cfg     TrafficConfig
	queue   chan domain.TrafficHit
	dropped atomic.Uint64
	mu      sync.Mutex
	day     time.Time
	salt    [32]byte
	closed  bool
	started bool
	stop    chan struct{}
	done    chan struct{}
}

// NewTraffic builds a recorder. Store and Clock are required.
func NewTraffic(cfg TrafficConfig) *Traffic {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 4096
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 512
	}
	if cfg.FlushEvery <= 0 {
		cfg.FlushEvery = 5 * time.Second
	}
	return &Traffic{cfg: cfg, queue: make(chan domain.TrafficHit, cfg.QueueSize), stop: make(chan struct{}), done: make(chan struct{})}
}

// Hit queues a hit without waiting for storage or queue space. Invalid input,
// entropy failures, closed admission and a full queue increment Dropped.
func (t *Traffic) Hit(repo domain.RepoName, kind domain.TrafficKind, ip netip.Addr, userAgent, referer string) {
	if repo.IsZero() || !ip.IsValid() || (kind != domain.TrafficView && kind != domain.TrafficClone) {
		t.dropped.Add(1)
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		t.dropped.Add(1)
		return
	}
	now := t.cfg.Clock.Now().UTC()
	day := now.Truncate(24 * time.Hour)
	if !day.Equal(t.day) {
		if _, err := rand.Read(t.salt[:]); err != nil {
			t.dropped.Add(1)
			return
		}
		t.day = day
	}
	input := append(t.salt[:len(t.salt):len(t.salt)], ip.Unmap().AsSlice()...)
	hash := sha256.Sum256(input)
	h := domain.TrafficHit{Repo: repo, Kind: kind, At: now, Bot: kind == domain.TrafficView && domain.IsBotUserAgent(userAgent), ReferrerDomain: domain.ReferrerDomain(referer, t.cfg.SelfHost)}
	copy(h.VisitorHash[:], hash[:16])
	select {
	case t.queue <- h:
	default:
		t.dropped.Add(1)
	}
}

// Dropped returns the number of hits rejected before storage.
func (t *Traffic) Dropped() uint64 { return t.dropped.Load() }

// Run flushes on the interval or batch size and prunes hourly. On shutdown it
// stops admission and drains with a fresh context bounded to ten seconds.
func (t *Traffic) Run(ctx context.Context) {
	t.mu.Lock()
	if t.started {
		t.mu.Unlock()
		return
	}
	t.started = true
	t.mu.Unlock()
	defer close(t.done)
	flushTimer := time.NewTicker(t.cfg.FlushEvery)
	defer flushTimer.Stop()
	pruneTimer := time.NewTicker(time.Hour)
	defer pruneTimer.Stop()
	batch := make([]domain.TrafficHit, 0, t.cfg.BatchSize)
	flush := func(ctx context.Context) {
		if len(batch) == 0 {
			return
		}
		if ctx.Err() != nil {
			return // Preserve pending hits for the uncancelled final flush.
		}
		if err := t.cfg.Store.Record(ctx, batch); err != nil {
			log := zerowrap.FromCtx(ctx)
			log.Error().Err(err).Msg("record traffic")
		}
		batch = batch[:0]
	}
	for {
		select {
		case h := <-t.queue:
			batch = append(batch, h)
			if len(batch) >= t.cfg.BatchSize {
				flush(ctx)
			}
		case <-flushTimer.C:
			flush(ctx)
		case <-pruneTimer.C:
			if _, err := t.cfg.Store.Prune(ctx, t.cfg.Clock.Now().UTC()); err != nil {
				log := zerowrap.FromCtx(ctx)
				log.Error().Err(err).Msg("prune traffic")
			}
		case <-ctx.Done():
			t.shutdown(ctx, batch)
			return
		case <-t.stop:
			t.shutdown(ctx, batch)
			return
		}
	}
}

func (t *Traffic) shutdown(ctx context.Context, batch []domain.TrafficHit) {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if len(batch) > 0 {
		t.recordFinal(final, batch)
	}
	pending := make([]domain.TrafficHit, 0, t.cfg.BatchSize)
	for {
		select {
		case h := <-t.queue:
			pending = append(pending, h)
			if len(pending) < t.cfg.BatchSize {
				continue
			}
		default:
			if len(pending) > 0 {
				t.recordFinal(final, pending)
			}
			return
		}
		t.recordFinal(final, pending)
		pending = pending[:0]
	}
}
func (t *Traffic) recordFinal(ctx context.Context, hits []domain.TrafficHit) {
	if err := t.cfg.Store.Record(ctx, hits); err != nil {
		log := zerowrap.FromCtx(ctx)
		log.Error().Err(err).Msg("flush traffic on shutdown")
	}
}

// Close stops admission and waits for the final flush. It also works before Run.
func (t *Traffic) Close(ctx context.Context) {
	t.mu.Lock()
	if !t.closed {
		t.closed = true
		close(t.stop)
	}
	started := t.started
	t.mu.Unlock()
	if !started {
		t.Run(context.WithoutCancel(ctx))
	}
	<-t.done
}

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

func addTraffic(a *domain.TrafficCounts, b domain.TrafficCounts) {
	a.Views += b.Views
	a.ViewUniques += b.ViewUniques
	a.Clones += b.Clones
	a.CloneUniques += b.CloneUniques
	a.Bots += b.Bots
	a.Partial = a.Partial || b.Partial
}
func (t *Traffic) dayWindow(days int) (time.Time, time.Time) {
	to := t.cfg.Clock.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	return to.Add(-time.Duration(days) * 24 * time.Hour), to
}

// RepoSummary sums the current UTC day and preceding 29 days.
func (t *Traffic) RepoSummary(ctx context.Context, repo domain.RepoName) (domain.TrafficCounts, error) {
	from, to := t.dayWindow(30)
	buckets, err := t.cfg.Store.Daily(ctx, repo, from, to)
	var counts domain.TrafficCounts
	for _, b := range buckets {
		addTraffic(&counts, b.TrafficCounts)
	}
	return counts, err
}

// RepoDaily returns days zero-filled daily unique sums, including today, oldest first.
// days must be between 1 and 90, the daily retention limit.
func (t *Traffic) RepoDaily(ctx context.Context, repo domain.RepoName, days int) ([]uint64, error) {
	if days < 1 || days > 90 {
		return nil, domain.ErrInvalid
	}
	from, to := t.dayWindow(days)
	buckets, err := t.cfg.Store.Daily(ctx, repo, from, to)
	if err != nil {
		return nil, err
	}
	result := make([]uint64, days)
	for _, b := range buckets {
		i := int(b.Start.Sub(from) / (24 * time.Hour))
		if i >= 0 && i < days {
			result[i] = b.ViewUniques + b.CloneUniques
		}
	}
	return result, nil
}

// RepoHourly returns 24 zero-filled UTC hourly buckets including the current hour.
func (t *Traffic) RepoHourly(ctx context.Context, repo domain.RepoName) ([]domain.TrafficBucket, error) {
	to := t.cfg.Clock.Now().UTC().Truncate(time.Hour).Add(time.Hour)
	from := to.Add(-24 * time.Hour)
	buckets, err := t.cfg.Store.Hourly(ctx, repo, from, to)
	if err != nil {
		return nil, err
	}
	result := make([]domain.TrafficBucket, 24)
	for i := range result {
		result[i].Start = from.Add(time.Duration(i) * time.Hour)
	}
	for _, b := range buckets {
		i := int(b.Start.Sub(from) / time.Hour)
		if i >= 0 && i < 24 {
			result[i] = b
		}
	}
	return result, nil
}

// Forge returns ranked traffic over a positive window of at most 90 days.
// Exactly 24h uses the current and preceding 23 hourly buckets. Other windows
// use daily buckets covering the window. Referrers always use covering daily
// buckets, so 24h referrals have coarser time resolution than hourly counts.
func (t *Traffic) Forge(ctx context.Context, window time.Duration) (TrafficForge, error) {
	if window <= 0 || window > 90*24*time.Hour {
		return TrafficForge{}, domain.ErrInvalid
	}
	days := int((window-1)/(24*time.Hour)) + 1
	from, to := t.dayWindow(days)
	daily, err := t.cfg.Store.Summaries(ctx, from, to)
	if err != nil {
		return TrafficForge{}, err
	}
	rows := daily
	if window == 24*time.Hour {
		end := t.cfg.Clock.Now().UTC().Truncate(time.Hour).Add(time.Hour)
		rows, err = t.cfg.Store.HourlySummaries(ctx, end.Add(-24*time.Hour), end)
		if err != nil {
			return TrafficForge{}, err
		}
		// Hourly counts can cross two UTC days; referrals cover both days.
		daily, err = t.cfg.Store.Summaries(ctx, end.Add(-24*time.Hour).Truncate(24*time.Hour), to)
		if err != nil {
			return TrafficForge{}, err
		}
	}
	result := TrafficForge{Repos: rows}
	refs := map[string]uint64{}
	for _, r := range daily {
		for host, n := range r.Referrers {
			refs[host] += n
		}
	}
	for _, r := range rows {
		addTraffic(&result.Totals, r.TrafficCounts)
	}
	sort.Slice(result.Repos, func(i, j int) bool {
		a, b := result.Repos[i], result.Repos[j]
		if a.Views+a.Clones == b.Views+b.Clones {
			return a.Repo.String() < b.Repo.String()
		}
		return a.Views+a.Clones > b.Views+b.Clones
	})
	for host, n := range refs {
		result.Referrers = append(result.Referrers, TrafficReferrer{Domain: host, Count: n})
	}
	sort.Slice(result.Referrers, func(i, j int) bool {
		a, b := result.Referrers[i], result.Referrers[j]
		if a.Count == b.Count {
			return a.Domain < b.Domain
		}
		return a.Count > b.Count
	})
	if len(result.Referrers) > 20 {
		result.Referrers = result.Referrers[:20]
	}
	return result, nil
}
