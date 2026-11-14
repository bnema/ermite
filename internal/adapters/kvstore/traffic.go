// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	kv "bnema.dev/ermite-kv"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Traffic keys. Every bucket has one small aggregate key, and every distinct
// visitor of a bucket has one empty unique key:
//
//	traffic/a/<repo>/<d|h>/<start>                     aggregate (JSON counts and referrers)
//	traffic/u/<repo>/<d|h>/<start>/<v|c>/<hash hex>    unique visitor marker (empty value)
//
// d is a daily bucket, h an hourly one; v marks a view visitor, c a clone
// visitor. <start> is the UTC bucket start as 20060102T150405Z, so the keys of
// one repository and bucket width sort by time and range reads use key bounds.
// Repository names cannot contain "/", so "<repo>/" never prefixes another
// repository's keys.
const (
	trafficPrefix       = "traffic/"
	trafficAggPrefix    = trafficPrefix + "a/"
	trafficUniquePrefix = trafficPrefix + "u/"
	trafficTimeLayout   = "20060102T150405Z"
	trafficDaily        = "d"
	trafficHourly       = "h"
)

const trafficUniqueCap = 10000
const trafficReferrerCap = 50
const trafficReferrerMaxLen = 253

// trafficBatchBudget bounds the encoded ops of one Record sub-batch, well
// under the 16 MiB ermite-kv batch limit. Record adds a hit to a sub-batch
// only when the sub-batch stays within the budget assuming the hit's worst
// case (see trafficWrite.cost), so a sub-batch exceeds it only when its first
// hit alone does, which needs a repository record of several MiB.
const trafficBatchBudget = 4 << 20

// trafficAggregateMaxBytes bounds one encoded aggregate put, key included:
// six uint64 counters and Partial (about 200 bytes of JSON), plus at most 50
// referrers of at most 253 bytes with a uint64 count (about 14 KB), plus a key
// of at most 132 bytes. TestTrafficAggregateBound checks it.
const trafficAggregateMaxBytes = 16 << 10

// Traffic stores small aggregate bucket records and one key per unique visitor.
type Traffic struct{ db *kv.DB }

var _ ports.TrafficStore = (*Traffic)(nil)

// Traffic returns a traffic store valid until the Store is closed.
func (s *Store) Traffic() *Traffic { return &Traffic{db: s.db} }

// trafficAggregate is the stored value of an aggregate key. The bucket start
// lives in the key.
type trafficAggregate struct {
	domain.TrafficCounts
	Referrers map[string]uint64 `json:",omitempty"`
}

func trafficAggRepoPrefix(repo domain.RepoName) string {
	return trafficAggPrefix + repo.String() + "/"
}

func trafficAggKindPrefix(repo domain.RepoName, width string) string {
	return trafficAggRepoPrefix(repo) + width + "/"
}

func trafficAggKey(repo domain.RepoName, width string, start time.Time) string {
	return trafficAggKindPrefix(repo, width) + start.UTC().Format(trafficTimeLayout)
}

func trafficUniqueRepoPrefix(repo domain.RepoName) string {
	return trafficUniquePrefix + repo.String() + "/"
}

// trafficUniqueBucketPrefix prefixes every unique key of one bucket.
func trafficUniqueBucketPrefix(repo domain.RepoName, width string, start time.Time) string {
	return trafficUniqueRepoPrefix(repo) + width + "/" + start.UTC().Format(trafficTimeLayout) + "/"
}

func trafficUniqueKey(h domain.TrafficHit, width string, start time.Time) string {
	kind := "v"
	if h.Kind == domain.TrafficClone {
		kind = "c"
	}
	return trafficUniqueBucketPrefix(h.Repo, width, start) + kind + "/" + hex.EncodeToString(h.VisitorHash[:])
}

// trafficBound returns the key of the first bucket starting at or after t, so
// that [trafficBound(from), trafficBound(to)) selects starts in [from, to).
// Bucket starts are whole seconds, so t is rounded up to a whole second.
func trafficBound(repo domain.RepoName, width string, t time.Time) string {
	if s := t.Truncate(time.Second); !s.Equal(t) {
		t = s.Add(time.Second)
	}
	return trafficAggKey(repo, width, t)
}

// parseTrafficAggKey splits an aggregate key into repository, width and start.
func parseTrafficAggKey(key string) (domain.RepoName, string, time.Time, error) {
	parts := strings.Split(strings.TrimPrefix(key, trafficAggPrefix), "/")
	if len(parts) != 3 {
		return domain.RepoName{}, "", time.Time{}, fmt.Errorf("malformed traffic key %q", key)
	}
	repo, err := domain.ParseRepoName(parts[0])
	if err != nil {
		return domain.RepoName{}, "", time.Time{}, err
	}
	start, err := time.Parse(trafficTimeLayout, parts[2])
	if err != nil {
		return domain.RepoName{}, "", time.Time{}, fmt.Errorf("traffic key %q: %w", key, err)
	}
	return repo, parts[1], start, nil
}

// trafficKeys lists every aggregate and unique key owned by repo for repository deletion.
func trafficKeys(ctx context.Context, snap *kv.Snapshot, repo domain.RepoName) [][]byte {
	var keys [][]byte
	for _, prefix := range []string{trafficAggRepoPrefix(repo), trafficUniqueRepoPrefix(repo)} {
		for key := range snap.Prefix(ctx, []byte(prefix)) {
			keys = append(keys, append([]byte(nil), key...))
		}
	}
	return keys
}

// Record merges hits into UTC daily and hourly buckets. Hits for missing
// repositories are discarded, and so are hits admitted before the current
// repository record was created (At before its CreatedAt): they belong to a
// deleted repository of the same name. Invalid hits reject the whole call
// before any write. Hits are written in order in sub-batches of at most about
// trafficBatchBudget bytes; each sub-batch is atomic, keeps both buckets of a
// hit together and guards the repository record it checked, but an error
// leaves earlier sub-batches recorded.
func (t *Traffic) Record(ctx context.Context, hits []domain.TrafficHit) error {
	return t.record(ctx, hits, true)
}

// ImportHistory is Record without the creation-time fence: it keeps hits
// dated before their repository was created. It is for development fixtures
// that backfill synthetic history; servers must use Record.
func (t *Traffic) ImportHistory(ctx context.Context, hits []domain.TrafficHit) error {
	return t.record(ctx, hits, false)
}

func (t *Traffic) record(ctx context.Context, hits []domain.TrafficHit, fence bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, h := range hits {
		if h.Repo.IsZero() || (h.Kind != domain.TrafficView && h.Kind != domain.TrafficClone) || h.At.IsZero() {
			return fmt.Errorf("traffic hit: %w", domain.ErrInvalid)
		}
	}
	for len(hits) > 0 {
		n, err := t.recordPrefix(ctx, hits, fence)
		if err != nil {
			return err
		}
		hits = hits[n:]
	}
	return nil
}

// recordPrefix applies the longest prefix of hits that fits one sub-batch and
// returns its length, retrying on write conflicts.
func (t *Traffic) recordPrefix(ctx context.Context, hits []domain.TrafficHit, fence bool) (int, error) {
	for range repoMutationTries {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		w := newTrafficWrite(t.db.Snapshot(ctx), fence)
		n, err := w.addHits(ctx, hits)
		if err != nil {
			return 0, err
		}
		if err := w.finish(); err != nil {
			return 0, err
		}
		if w.b.Len() == 0 {
			return n, nil
		}
		if _, err := t.db.Apply(ctx, &w.b); err != nil {
			if errors.Is(err, kv.ErrConflict) {
				continue
			}
			return 0, err
		}
		return n, nil
	}
	return 0, domain.ErrConflict
}

// trafficRepo caches one repository read of a sub-batch.
type trafficRepo struct {
	item      kv.Item
	exists    bool
	createdAt time.Time
	guarded   bool
}

// trafficWrite builds one Record sub-batch from a single snapshot. Every
// aggregate it touches is version-guarded, and every unique key is written in
// the same batch as its aggregate, so a concurrent writer of the same bucket
// makes the batch conflict instead of counting a visitor twice.
type trafficWrite struct {
	snap    *kv.Snapshot
	fence   bool
	b       kv.Batch
	bytes   int
	repos   map[domain.RepoName]*trafficRepo
	aggs    map[string]*trafficAggregate
	uniques map[string]bool
}

func newTrafficWrite(snap *kv.Snapshot, fence bool) *trafficWrite {
	return &trafficWrite{snap: snap, fence: fence, repos: map[domain.RepoName]*trafficRepo{}, aggs: map[string]*trafficAggregate{}, uniques: map[string]bool{}}
}

// addHits adds hits until the next one could overflow the budget and returns
// how many were consumed. The first hit is always taken so Record progresses.
func (w *trafficWrite) addHits(ctx context.Context, hits []domain.TrafficHit) (int, error) {
	for i, h := range hits {
		r, err := w.repo(ctx, h.Repo)
		if err != nil {
			return 0, err
		}
		if !r.exists || (w.fence && h.At.Before(r.createdAt)) {
			continue
		}
		cost := w.cost(h, r)
		if i > 0 && w.bytes+cost > trafficBatchBudget {
			return i, nil
		}
		w.bytes += cost
		if !r.guarded {
			guardRepoItem(&w.b, h.Repo, r.item)
			r.guarded = true
		}
		if err := w.add(ctx, h); err != nil {
			return 0, err
		}
	}
	return len(hits), nil
}

// repo reads and caches the repository record of name from the snapshot.
func (w *trafficWrite) repo(ctx context.Context, name domain.RepoName) (*trafficRepo, error) {
	if r := w.repos[name]; r != nil {
		return r, nil
	}
	item, ok := w.snap.Get(ctx, repoKey(name))
	r := &trafficRepo{item: item, exists: ok}
	if ok {
		repo, err := decodeRepo(item.Value)
		if err != nil {
			return nil, fmt.Errorf("traffic repository %q: %w", name, err)
		}
		r.createdAt = repo.CreatedAt
	}
	w.repos[name] = r
	return r, nil
}

// guardRepoItem is guardRepoKey for a repository item read from a snapshot.
func guardRepoItem(b *kv.Batch, name domain.RepoName, item kv.Item) {
	b.RequireVersion(repoKey(name), item.Version)
	b.Put(repoKey(name), item.Value)
}

// cost is an upper bound on the encoded bytes h adds to the sub-batch: the
// repository guard the first time, an aggregate put per bucket not yet in the
// sub-batch, and a unique key put per bucket unless it is already pending.
func (w *trafficWrite) cost(h domain.TrafficHit, r *trafficRepo) int {
	n := 0
	if !r.guarded {
		n += putSize(len(repoKey(h.Repo)), len(r.item.Value))
	}
	for _, width := range []string{trafficDaily, trafficHourly} {
		start := trafficStart(h.At, width)
		if w.aggs[trafficAggKey(h.Repo, width, start)] == nil {
			n += trafficAggregateMaxBytes
		}
		if !isBotView(h) {
			if key := trafficUniqueKey(h, width, start); !w.uniques[key] {
				n += putSize(len(key), 0)
			}
		}
	}
	return n
}

// putSize is the encoded size of a put in an ermite-kv batch: an op byte and
// two uvarint length-prefixed byte strings.
func putSize(key, value int) int { return 1 + uvarintSize(key) + key + uvarintSize(value) + value }

func uvarintSize(v int) int {
	n := 1
	for ; v >= 0x80; v >>= 7 {
		n++
	}
	return n
}

func trafficStart(at time.Time, width string) time.Time {
	if width == trafficDaily {
		return at.UTC().Truncate(24 * time.Hour)
	}
	return at.UTC().Truncate(time.Hour)
}

func isBotView(h domain.TrafficHit) bool { return h.Kind == domain.TrafficView && h.Bot }

// add merges h into its daily and hourly aggregates and unique keys.
func (w *trafficWrite) add(ctx context.Context, h domain.TrafficHit) error {
	for _, width := range []string{trafficDaily, trafficHourly} {
		start := trafficStart(h.At, width)
		agg, err := w.aggregate(ctx, trafficAggKey(h.Repo, width, start))
		if err != nil {
			return err
		}
		if isBotView(h) {
			agg.Bots++
			continue
		}
		w.countVisitor(ctx, agg, h, trafficUniqueKey(h, width, start))
		if width == trafficDaily {
			agg.addReferrer(h.ReferrerDomain)
		}
	}
	return nil
}

// aggregate returns the pending aggregate of key, loading and guarding it on first use.
func (w *trafficWrite) aggregate(ctx context.Context, key string) (*trafficAggregate, error) {
	if agg := w.aggs[key]; agg != nil {
		return agg, nil
	}
	agg := &trafficAggregate{}
	item, ok := w.snap.Get(ctx, []byte(key))
	if ok {
		if err := json.Unmarshal(item.Value, agg); err != nil {
			return nil, fmt.Errorf("decode traffic %s: %w", key, err)
		}
		w.b.RequireVersion([]byte(key), item.Version)
	} else {
		w.b.RequireAbsent([]byte(key))
	}
	w.aggs[key] = agg
	return agg, nil
}

// countVisitor counts a view or clone and, for a visitor new to the bucket,
// writes its unique key and counts it, until the cap marks the bucket Partial.
func (w *trafficWrite) countVisitor(ctx context.Context, agg *trafficAggregate, h domain.TrafficHit, key string) {
	count := &agg.ViewUniques
	if h.Kind == domain.TrafficClone {
		count = &agg.CloneUniques
		agg.Clones++
	} else {
		agg.Views++
	}
	if w.uniques[key] {
		return
	}
	if _, ok := w.snap.Get(ctx, []byte(key)); ok {
		return
	}
	if *count >= trafficUniqueCap {
		agg.Partial = true
		return
	}
	w.b.Put([]byte(key), nil)
	w.uniques[key] = true
	*count++
}

// addReferrer counts an already-normalized referrer domain. The port may be
// called without the HTTP normalization path, so arbitrary strings are ignored.
// The first trafficReferrerCap domains are kept; later new domains are dropped.
func (agg *trafficAggregate) addReferrer(host string) {
	if host == "" || len(host) > trafficReferrerMaxLen || domain.ReferrerDomain("https://"+host, "") != host {
		return
	}
	if agg.Referrers == nil {
		agg.Referrers = map[string]uint64{}
	}
	if _, ok := agg.Referrers[host]; ok || len(agg.Referrers) < trafficReferrerCap {
		agg.Referrers[host]++
	}
}

// finish encodes the pending aggregates into the batch.
func (w *trafficWrite) finish() error {
	for key, agg := range w.aggs {
		value, err := json.Marshal(agg)
		if err != nil {
			return err
		}
		w.b.Put([]byte(key), value)
	}
	return nil
}

// Daily returns stored daily buckets with start in [from, to), oldest first.
func (t *Traffic) Daily(ctx context.Context, repo domain.RepoName, from, to time.Time) ([]domain.TrafficBucket, error) {
	return t.buckets(ctx, repo, trafficDaily, from, to)
}

// Hourly returns stored hourly buckets with start in [from, to), oldest first.
func (t *Traffic) Hourly(ctx context.Context, repo domain.RepoName, from, to time.Time) ([]domain.TrafficBucket, error) {
	return t.buckets(ctx, repo, trafficHourly, from, to)
}

func (t *Traffic) buckets(ctx context.Context, repo domain.RepoName, width string, from, to time.Time) ([]domain.TrafficBucket, error) {
	var buckets []domain.TrafficBucket
	err := scanTrafficRange(ctx, t.db.Snapshot(ctx), repo, width, from, to, func(b domain.TrafficBucket) {
		buckets = append(buckets, b)
	})
	return buckets, err
}

// scanTrafficRange decodes the aggregates of one repository and width with a
// start in [from, to), oldest first, reading only keys inside those bounds.
func scanTrafficRange(ctx context.Context, snap *kv.Snapshot, repo domain.RepoName, width string, from, to time.Time, fn func(domain.TrafficBucket)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !from.Before(to) {
		return nil
	}
	for key, item := range snap.Range(ctx, []byte(trafficBound(repo, width, from)), []byte(trafficBound(repo, width, to))) {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, _, start, err := parseTrafficAggKey(string(key))
		if err != nil {
			return err
		}
		var agg trafficAggregate
		if err := json.Unmarshal(item.Value, &agg); err != nil {
			return fmt.Errorf("decode traffic %s: %w", key, err)
		}
		fn(domain.TrafficBucket{Start: start, TrafficCounts: agg.TrafficCounts, Referrers: agg.Referrers})
	}
	return ctx.Err()
}

// trafficRepos lists, in name order, the repositories that have aggregates.
// It seeks past each repository's keys, so its cost grows with the number of
// repositories, not of buckets.
func trafficRepos(ctx context.Context, snap *kv.Snapshot) ([]domain.RepoName, error) {
	var repos []domain.RepoName
	start := trafficAggPrefix
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var next string
		for key := range snap.Range(ctx, []byte(start), []byte(trafficAggPrefix+"\xff")) {
			next = string(key)
			break
		}
		if next == "" {
			return repos, nil
		}
		repo, _, _, err := parseTrafficAggKey(next)
		if err != nil {
			return nil, err
		}
		repos = append(repos, repo)
		start = trafficAggRepoPrefix(repo) + "\xff"
	}
}

// Summaries sums daily buckets in [from, to), ordered by repository name.
// Unique counts are sums of daily uniques, not window-wide distinct visitors.
func (t *Traffic) Summaries(ctx context.Context, from, to time.Time) ([]domain.TrafficSummary, error) {
	return t.summaries(ctx, trafficDaily, from, to)
}

// HourlySummaries sums hourly buckets, without referrers, for short windows.
func (t *Traffic) HourlySummaries(ctx context.Context, from, to time.Time) ([]domain.TrafficSummary, error) {
	return t.summaries(ctx, trafficHourly, from, to)
}

func (t *Traffic) summaries(ctx context.Context, width string, from, to time.Time) ([]domain.TrafficSummary, error) {
	snap := t.db.Snapshot(ctx)
	repos, err := trafficRepos(ctx, snap)
	if err != nil {
		return nil, err
	}
	result := make([]domain.TrafficSummary, 0, len(repos))
	for _, repo := range repos {
		row := domain.TrafficSummary{Repo: repo, Referrers: map[string]uint64{}}
		found := false
		err := scanTrafficRange(ctx, snap, repo, width, from, to, func(b domain.TrafficBucket) {
			found = true
			addTrafficCounts(&row.TrafficCounts, b.TrafficCounts)
			for host, n := range b.Referrers {
				row.Referrers[host] += n
			}
		})
		if err != nil {
			return nil, err
		}
		if found {
			result = append(result, row)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Repo.String() < result[j].Repo.String() })
	return result, nil
}

func addTrafficCounts(a *domain.TrafficCounts, b domain.TrafficCounts) {
	a.Views += b.Views
	a.ViewUniques += b.ViewUniques
	a.Clones += b.Clones
	a.CloneUniques += b.CloneUniques
	a.Bots += b.Bots
	a.Partial = a.Partial || b.Partial
}

// Prune removes buckets starting strictly before now minus their retention:
// 90 days for daily buckets and 48 hours for hourly buckets, each with its
// unique keys in one batch (at most 2 × 10,000 deletes of under 170 bytes,
// about 3.4 MB). It returns the number of buckets deleted. A version guard
// skips a bucket updated concurrently; the next Prune retries it.
func (t *Traffic) Prune(ctx context.Context, now time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	snap := t.db.Snapshot(ctx)
	repos, err := trafficRepos(ctx, snap)
	if err != nil {
		return 0, err
	}
	done := 0
	retention := map[string]time.Duration{trafficDaily: 90 * 24 * time.Hour, trafficHourly: 48 * time.Hour}
	for _, repo := range repos {
		for _, width := range []string{trafficDaily, trafficHourly} {
			n, err := t.pruneRange(ctx, snap, repo, width, now.Add(-retention[width]))
			done += n
			if err != nil {
				return done, err
			}
		}
	}
	return done, nil
}

// pruneRange deletes the buckets of one repository and width starting before cutoff.
func (t *Traffic) pruneRange(ctx context.Context, snap *kv.Snapshot, repo domain.RepoName, width string, cutoff time.Time) (int, error) {
	done := 0
	start := trafficAggKindPrefix(repo, width)
	for key, item := range snap.Range(ctx, []byte(start), []byte(trafficBound(repo, width, cutoff))) {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		_, _, bucket, err := parseTrafficAggKey(string(key))
		if err != nil {
			return done, err
		}
		var b kv.Batch
		b.RequireVersion(key, item.Version)
		b.Delete(key)
		for ukey := range snap.Prefix(ctx, []byte(trafficUniqueBucketPrefix(repo, width, bucket))) {
			b.Delete(ukey)
		}
		if _, err := t.db.Apply(ctx, &b); err != nil {
			if errors.Is(err, kv.ErrConflict) {
				continue
			}
			return done, err
		}
		done++
	}
	return done, nil
}
