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

const trafficPrefix = "traffic/"
const trafficUniqueCap = 10000
const trafficReferrerCap = 50

// Traffic stores JSON bucket records and bounded unique hash sets in the shared database.
type Traffic struct{ db *kv.DB }

var _ ports.TrafficStore = (*Traffic)(nil)

// Traffic returns a traffic store valid until the Store is closed.
func (s *Store) Traffic() *Traffic { return &Traffic{db: s.db} }

type trafficRecord struct {
	Bucket domain.TrafficBucket
	Views  map[string]bool
	Clones map[string]bool
}

func trafficRepoPrefix(repo domain.RepoName) string { return trafficPrefix + repo.String() + "/" }
func trafficKey(repo domain.RepoName, kind string, at time.Time) []byte {
	return []byte(trafficRepoPrefix(repo) + kind + "/" + at.Format("20060102T150405Z"))
}

// trafficKeys lists every traffic record owned by repo for repository deletion.
func trafficKeys(ctx context.Context, snap *kv.Snapshot, repo domain.RepoName) [][]byte {
	prefix := trafficRepoPrefix(repo)
	var keys [][]byte
	for key := range snap.Range(ctx, []byte(prefix), []byte(prefix+"\xff")) {
		keys = append(keys, append([]byte(nil), key...))
	}
	return keys
}

// Record merges a batch atomically into UTC daily and hourly buckets. Hits for
// missing repositories are discarded. Invalid hits reject the entire batch.
func (t *Traffic) Record(ctx context.Context, hits []domain.TrafficHit) error {
	for _, h := range hits {
		if h.Repo.IsZero() || (h.Kind != domain.TrafficView && h.Kind != domain.TrafficClone) || h.At.IsZero() {
			return fmt.Errorf("traffic hit: %w", domain.ErrInvalid)
		}
	}
	for range repoMutationTries {
		if err := ctx.Err(); err != nil {
			return err
		}
		var b kv.Batch
		records := map[string]*trafficRecord{}
		repos := map[domain.RepoName]bool{}
		for _, h := range hits {
			exists, seen := repos[h.Repo]
			if !seen {
				err := guardRepoKey(ctx, t.db, &b, h.Repo)
				if err != nil && !errors.Is(err, domain.ErrNotFound) {
					return err
				}
				exists = err == nil
				repos[h.Repo] = exists
			}
			if !exists {
				continue
			}
			for _, kind := range []string{"d", "h"} {
				width := time.Hour
				if kind == "d" {
					width = 24 * time.Hour
				}
				start := h.At.UTC().Truncate(width)
				key := trafficKey(h.Repo, kind, start)
				r := records[string(key)]
				if r == nil {
					r = &trafficRecord{Bucket: domain.TrafficBucket{Start: start}}
					item, ok := t.db.Get(ctx, key)
					if ok {
						if err := json.Unmarshal(item.Value, r); err != nil {
							return fmt.Errorf("decode traffic: %w", err)
						}
						b.RequireVersion(key, item.Version)
					} else {
						b.RequireAbsent(key)
					}
					records[string(key)] = r
				}
				r.add(h, kind == "d")
			}
		}
		for key, r := range records {
			value, err := json.Marshal(r)
			if err != nil {
				return err
			}
			b.Put([]byte(key), value)
		}
		if b.Len() == 0 {
			return ctx.Err()
		}
		if _, err := t.db.Apply(ctx, &b); err != nil {
			if errors.Is(err, kv.ErrConflict) {
				continue
			}
			return err
		}
		return nil
	}
	return domain.ErrConflict
}

func (r *trafficRecord) add(h domain.TrafficHit, daily bool) {
	if h.Kind == domain.TrafficView && h.Bot {
		r.Bucket.Bots++
		return
	}
	hashes, count := &r.Views, &r.Bucket.ViewUniques
	if h.Kind == domain.TrafficClone {
		hashes, count = &r.Clones, &r.Bucket.CloneUniques
		r.Bucket.Clones++
	} else {
		r.Bucket.Views++
	}
	if *hashes == nil {
		*hashes = map[string]bool{}
	}
	hash := hex.EncodeToString(h.VisitorHash[:])
	if !(*hashes)[hash] {
		if len(*hashes) < trafficUniqueCap {
			(*hashes)[hash] = true
			*count++
		} else {
			r.Bucket.Partial = true
		}
	}
	// The port may be called without the HTTP normalization path. Only store
	// already-normalized, valid domains, never arbitrary caller strings.
	if daily && h.ReferrerDomain != "" && len(h.ReferrerDomain) <= 253 && domain.ReferrerDomain("https://"+h.ReferrerDomain, "") == h.ReferrerDomain {
		if r.Bucket.Referrers == nil {
			r.Bucket.Referrers = map[string]uint64{}
		}
		if _, ok := r.Bucket.Referrers[h.ReferrerDomain]; ok || len(r.Bucket.Referrers) < trafficReferrerCap {
			r.Bucket.Referrers[h.ReferrerDomain]++
		}
	}
}

// Daily returns stored daily buckets with start in [from, to), oldest first.
func (t *Traffic) Daily(ctx context.Context, repo domain.RepoName, from, to time.Time) ([]domain.TrafficBucket, error) {
	return t.buckets(ctx, repo, "d", from, to)
}

// Hourly returns stored hourly buckets with start in [from, to), oldest first.
func (t *Traffic) Hourly(ctx context.Context, repo domain.RepoName, from, to time.Time) ([]domain.TrafficBucket, error) {
	return t.buckets(ctx, repo, "h", from, to)
}

func (t *Traffic) buckets(ctx context.Context, repo domain.RepoName, kind string, from, to time.Time) ([]domain.TrafficBucket, error) {
	var buckets []domain.TrafficBucket
	prefix := trafficRepoPrefix(repo) + kind + "/"
	err := t.scan(ctx, prefix, func(_ string, r trafficRecord) {
		if !r.Bucket.Start.Before(from) && r.Bucket.Start.Before(to) {
			buckets = append(buckets, r.Bucket)
		}
	})
	return buckets, err
}

func (t *Traffic) scan(ctx context.Context, prefix string, fn func(string, trafficRecord)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for key, item := range t.db.Snapshot(ctx).Range(ctx, []byte(prefix), []byte(prefix+"\xff")) {
		if err := ctx.Err(); err != nil {
			return err
		}
		var r trafficRecord
		if err := json.Unmarshal(item.Value, &r); err != nil {
			return fmt.Errorf("decode traffic: %w", err)
		}
		fn(string(key), r)
	}
	return ctx.Err()
}

// Summaries sums daily buckets in [from, to), ordered by repository name.
// Unique counts are sums of daily uniques, not window-wide distinct visitors.
func (t *Traffic) Summaries(ctx context.Context, from, to time.Time) ([]domain.TrafficSummary, error) {
	return t.summaries(ctx, "d", from, to)
}

// HourlySummaries sums hourly buckets, without referrers, for short windows.
func (t *Traffic) HourlySummaries(ctx context.Context, from, to time.Time) ([]domain.TrafficSummary, error) {
	return t.summaries(ctx, "h", from, to)
}

func (t *Traffic) summaries(ctx context.Context, kind string, from, to time.Time) ([]domain.TrafficSummary, error) {
	rows := map[string]*domain.TrafficSummary{}
	var parseErr error
	err := t.scan(ctx, trafficPrefix, func(key string, r trafficRecord) {
		parts := strings.Split(strings.TrimPrefix(key, trafficPrefix), "/")
		if len(parts) != 3 || parts[1] != kind || r.Bucket.Start.Before(from) || !r.Bucket.Start.Before(to) {
			return
		}
		repo, err := domain.ParseRepoName(parts[0])
		if err != nil {
			parseErr = err
			return
		}
		row := rows[parts[0]]
		if row == nil {
			row = &domain.TrafficSummary{Repo: repo, Referrers: map[string]uint64{}}
			rows[parts[0]] = row
		}
		c := r.Bucket
		row.Views += c.Views
		row.ViewUniques += c.ViewUniques
		row.Clones += c.Clones
		row.CloneUniques += c.CloneUniques
		row.Bots += c.Bots
		row.Partial = row.Partial || c.Partial
		for host, n := range c.Referrers {
			row.Referrers[host] += n
		}
	})
	if err != nil {
		return nil, err
	}
	if parseErr != nil {
		return nil, parseErr
	}
	result := make([]domain.TrafficSummary, 0, len(rows))
	for _, row := range rows {
		result = append(result, *row)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Repo.String() < result[j].Repo.String() })
	return result, nil
}

// Prune removes buckets starting strictly before now minus their retention:
// 90 days for daily buckets and 48 hours for hourly buckets. It returns the
// number deleted. Version guards prevent deleting a concurrently updated bucket.
func (t *Traffic) Prune(ctx context.Context, now time.Time) (int, error) {
	done := 0
	if err := ctx.Err(); err != nil {
		return done, err
	}
	for key, item := range t.db.Snapshot(ctx).Range(ctx, []byte(trafficPrefix), []byte(trafficPrefix+"\xff")) {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		var r trafficRecord
		if err := json.Unmarshal(item.Value, &r); err != nil {
			return done, err
		}
		retention := 48 * time.Hour
		if strings.Contains(string(key), "/d/") {
			retention = 90 * 24 * time.Hour
		}
		if !r.Bucket.Start.Before(now.Add(-retention)) {
			continue
		}
		var b kv.Batch
		b.RequireVersion(key, item.Version)
		b.Delete(key)
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
