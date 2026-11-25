// SPDX-License-Identifier: AGPL-3.0-or-later

// Traffic-fixture adds synthetic traffic to existing repositories for local previews.
// The database takes an exclusive directory lock: stop the server first (even
// for -dry-run). From the dev compose directory:
//
//	docker compose stop ermite
//	docker compose run --rm --no-deps ermite go run ./internal/app/trafficfixture
//	docker compose up -d ermite
//
// Outside Docker: GOWORK=off go run ./internal/app/trafficfixture -data /path/to/data
// Flags: -days 90 -scale 1.0 -repos a,b -dry-run. The data root contains kv/,
// exactly as on the server. Dry-run skips traffic writes, but opening the store
// can initialize/upgrade metadata. Repeated runs ADD counts (uniques too, once a
// bucket's unique keys are pruned); use dev data only.
package main

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/domain"
)

// clonePool is the number of distinct clone visitors per repository and UTC day.
const clonePool = 3

type options struct {
	days  int
	scale float64
	repos string
	dry   bool
}

func main() {
	data := flag.String("data", os.Getenv("ERMITE_DATA_DIR"), "server data root (defaults to ERMITE_DATA_DIR)")
	var o options
	flag.IntVar(&o.days, "days", 90, "days of history (1-90)")
	flag.Float64Var(&o.scale, "scale", 1, "positive traffic multiplier")
	flag.StringVar(&o.repos, "repos", "", "comma-separated repository names (default all)")
	flag.BoolVar(&o.dry, "dry-run", false, "generate summaries without recording traffic")
	flag.Parse()
	if err := run(context.Background(), *data, o, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, data string, o options, out io.Writer) (err error) {
	if data == "" {
		return fmt.Errorf("-data or ERMITE_DATA_DIR is required")
	}
	if o.days < 1 || o.days > 90 || o.scale <= 0 || math.IsNaN(o.scale) || math.IsInf(o.scale, 0) {
		return fmt.Errorf("days must be 1-90 and scale must be finite and positive")
	}
	store, err := kvstore.Open(ctx, filepath.Join(data, "kv"))
	if err != nil {
		return err
	}
	defer func() {
		if e := store.Close(context.WithoutCancel(ctx)); err == nil {
			err = e
		}
	}()
	repos, err := store.List(ctx)
	if err != nil {
		return err
	}
	filter := map[string]bool{}
	if o.repos != "" {
		for _, n := range strings.Split(o.repos, ",") {
			filter[strings.TrimSpace(n)] = true
		}
	}
	for n := range filter {
		found := false
		for _, r := range repos {
			if r.Name.String() == n {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("repository %q not found", n)
		}
	}
	now := time.Now().UTC()
	for _, r := range repos {
		if len(filter) > 0 && !filter[r.Name.String()] {
			continue
		}
		if err := seed(ctx, store, r.Name, now, o, out); err != nil {
			return err
		}
	}
	return nil
}

// seed streams bounded batches through ImportHistory, which keeps hits dated
// before the repository was created; daily windows end at now so no hit is future
// dated or older than the 90-day retention. Uniform timestamps within each
// window also populate the last 48 hourly buckets without double-counting.
func seed(ctx context.Context, store *kvstore.Store, repo domain.RepoName, now time.Time, o options, out io.Writer) error {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(repo.String()))
	rng := rand.New(rand.NewSource(int64(hash.Sum64())))
	baseline := 45 + rng.Intn(130)
	spike1, spike2 := rng.Intn(o.days), rng.Intn(o.days)
	refs := []string{"", "github.com", "google.com", "news.ycombinator.com", "reddit.com", "lobste.rs", "duckduckgo.com", "x.com"}
	batch := make([]domain.TrafficHit, 0, 512)
	views, clones, bots := 0, 0, 0
	flush := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !o.dry {
			if err := store.Traffic().ImportHistory(ctx, batch); err != nil {
				return err
			}
		}
		batch = batch[:0]
		return nil
	}
	for day := o.days - 1; day >= 0; day-- {
		end := now.Add(-time.Duration(day) * 24 * time.Hour)
		rhythm := 1.0
		if end.Weekday() == time.Saturday || end.Weekday() == time.Sunday {
			rhythm = 0.55
		}
		n := math.Ceil(float64(baseline) * rhythm * (0.7 + rng.Float64()*0.6) * o.scale)
		spike := day == spike1 || day == spike2
		if spike {
			n *= 5
		}
		if n > float64(1_000_000) {
			return fmt.Errorf("scale generates over 1 million hits/day for %s; reduce it", repo)
		}
		count := int(n)
		pool := max(1, count/4)
		for i := 0; i < count; i++ {
			h := domain.TrafficHit{Repo: repo, Kind: domain.TrafficView, At: end.Add(-time.Duration(rng.Int63n(int64(24 * time.Hour))))}
			if i%15 == 0 {
				// Clone visitors come from a small pool: every 15th hit is a
				// clone, and its ordinal among clones picks the visitor.
				h.Kind = domain.TrafficClone
				h.VisitorHash = visitorHash("clone", repo, h.At, (i/15)%clonePool)
				clones++
			} else {
				h.VisitorHash = visitorHash("view", repo, h.At, rng.Intn(pool))
				h.Bot = i%11 == 0
				if h.Bot {
					bots++
				} else {
					views++
				}
				h.ReferrerDomain = refs[rng.Intn(len(refs))]
				if spike && i%2 == 0 {
					h.ReferrerDomain = "news.ycombinator.com"
				}
			}
			batch = append(batch, h)
			if len(batch) == cap(batch) {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
	if len(batch) > 0 {
		if err := flush(); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(out, "%s: views=%d clones=%d bots=%d days=%d dry-run=%t\n", repo, views, clones, bots, o.days, o.dry)
	return err
}

// visitorHash derives a full 16-byte visitor hash from the visitor's identity
// (kind and ordinal), the repository and the UTC day of at.
func visitorHash(kind string, repo domain.RepoName, at time.Time, ordinal int) [16]byte {
	sum := sha256.Sum256([]byte(fmt.Sprintf("fixture:%s:%s:%s:%d", kind, repo, at.UTC().Format("2006-01-02"), ordinal)))
	var h [16]byte
	copy(h[:], sum[:16])
	return h
}
