// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

const (
	defaultMergeabilityWorkers = 2
	defaultMergeabilityQueue   = 1024
	// mergeabilityAttempts bounds the computations of one check that lose a
	// race with a moving branch or a concurrent write; the check is then queued
	// again.
	mergeabilityAttempts = 3
	// Failed checks (Git or store errors) are retried with an exponential
	// backoff, up to mergeabilityRetries times, then dropped until the next
	// trigger.
	mergeabilityRetries      = 5
	mergeabilityRetryInitial = time.Second
	mergeabilityRetryMax     = time.Minute
)

// retryKey names the work a scheduled retry redoes: the rescan of one
// repository, or (all) the rescan of every repository. Retry state is thus
// bounded by the number of repositories plus one.
type retryKey struct {
	repo domain.RepoName
	all  bool
}

func repoKey(repo domain.RepoName) retryKey { return retryKey{repo: repo} }

var globalKey = retryKey{all: true}

// mergeBackoff is the delay before retry number attempt (1-based).
func mergeBackoff(attempt int) time.Duration {
	d := mergeabilityRetryInitial
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= mergeabilityRetryMax {
			return mergeabilityRetryMax
		}
	}
	return d
}

// MergeabilityConfig holds what the mergeability service needs. Workers and
// Queue default to 2 and 1024.
type MergeabilityConfig struct {
	Store ports.MergeRequestStore
	Meta  ports.RepoMetaStore
	Git   ports.MergeGit
	Bus   ports.EventBus
	Clock ports.Clock
	// Workers is the number of checks that run at once, Queue the number of
	// checks that may wait; past it a repository is rescanned.
	Workers, Queue int
	// After returns a channel that fires after d; it defaults to time.After.
	// Tests inject it to drive the retry backoff.
	After func(d time.Duration) <-chan time.Time
}

// Mergeability keeps up to date whether each open merge request merges with
// its target (see domain.Mergeability). It is independent of CI: it never
// touches refs/pull, workflows or runs. It computes with Git reads only, holds
// no repository lock, and stores its result with a compare-and-swap.
type Mergeability struct {
	cfg    MergeabilityConfig
	q      *mergeQueue
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	retryMu   sync.Mutex
	retries   map[retryKey]int // attempts so far of the scheduled or failing work
	scheduled map[retryKey]bool
	heap      []retryItem // sorted by due time
	retryKick chan struct{}
	closed    bool
}

// MergeabilityHook is what the merge request use cases need from the
// mergeability service: queue checks, read results and drop the record of a
// request that is no longer open.
type MergeabilityHook interface {
	ports.MergeabilityTrigger
	ports.MergeabilityReader
	// OnClosed deletes the record of a merge request that left the open state.
	OnClosed(ctx context.Context, mr domain.MergeRequest)
}

var _ MergeabilityHook = (*Mergeability)(nil)

// NewMergeability builds the service. Call Start to run it, Close to stop it.
func NewMergeability(ctx context.Context, cfg MergeabilityConfig) *Mergeability {
	if cfg.Workers <= 0 {
		cfg.Workers = defaultMergeabilityWorkers
	}
	if cfg.Queue <= 0 {
		cfg.Queue = defaultMergeabilityQueue
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	// The workers outlive the cancellation of ctx: Close controls their life.
	wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	return &Mergeability{
		cfg: cfg, q: newMergeQueue(cfg.Queue), ctx: wctx, cancel: cancel,
		retries: map[retryKey]int{}, scheduled: map[retryKey]bool{}, retryKick: make(chan struct{}, 1),
	}
}

// Start rescans the open merge requests of every repository (the records may
// be missing or stale after a restart) and starts the workers.
func (s *Mergeability) Start() {
	s.wg.Add(1)
	go s.scheduler()
	s.rescanAll()
	for range s.cfg.Workers {
		s.wg.Add(1)
		go s.loop()
	}
}

// Close stops the workers and waits for them.
func (s *Mergeability) Close(_ context.Context) {
	s.retryMu.Lock()
	s.closed = true
	s.retryMu.Unlock()
	s.cancel()
	s.wg.Wait()
}

// retryItem is one scheduled retry.
type retryItem struct {
	due time.Time
	key retryKey
}

// retry schedules a retry of key after a bounded exponential backoff. It is
// deduplicated per key, gives up after mergeabilityRetries attempts and stops
// on Close. All retries share one scheduler goroutine.
func (s *Mergeability) retry(key retryKey) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	if s.closed || s.scheduled[key] {
		return
	}
	s.retries[key]++
	n := s.retries[key]
	if n > mergeabilityRetries {
		delete(s.retries, key)
		return
	}
	s.scheduled[key] = true
	it := retryItem{due: s.cfg.Clock.Now().Add(mergeBackoff(n)), key: key}
	i, _ := slices.BinarySearchFunc(s.heap, it, func(a, b retryItem) int {
		if a.due.After(b.due) {
			return 1
		}
		return -1 // equal dues keep their insertion order
	})
	s.heap = slices.Insert(s.heap, i, it)
	if i == 0 {
		select {
		case s.retryKick <- struct{}{}:
		default:
		}
	}
}

// scheduler is the only goroutine that waits for retries: it sleeps until the
// earliest is due and puts it back in the normal queue.
func (s *Mergeability) scheduler() {
	defer s.wg.Done()
	// The timer armed for waited; a kick re-arms it only when the earliest
	// retry changed, so a stale kick never starts a second timer.
	var (
		after  <-chan time.Time
		waited retryItem
	)
	for {
		s.retryMu.Lock()
		var head retryItem
		has := len(s.heap) > 0
		if has {
			head = s.heap[0]
		}
		s.retryMu.Unlock()
		if !has {
			after = nil
			select {
			case <-s.ctx.Done():
				return
			case <-s.retryKick:
				continue
			}
		}
		if after == nil || head.key != waited.key || !head.due.Equal(waited.due) {
			after, waited = s.cfg.After(max(0, head.due.Sub(s.cfg.Clock.Now()))), head
		}
		select {
		case <-s.ctx.Done():
			return
		case <-s.retryKick:
			continue // an earlier retry may have arrived
		case <-after:
			after = nil
		}
		for _, k := range s.popDue(head.key) {
			if s.ctx.Err() != nil {
				return
			}
			s.fire(k)
		}
	}
}

// popDue removes the waited item and the ones due as well.
func (s *Mergeability) popDue(waited retryKey) []retryKey {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	now := s.cfg.Clock.Now()
	var out []retryKey
	s.heap = slices.DeleteFunc(s.heap, func(it retryItem) bool {
		if it.key != waited && it.due.After(now) {
			return false
		}
		out = append(out, it.key)
		delete(s.scheduled, it.key)
		return true
	})
	return out
}

// fire puts the work of a due retry back in the normal queue, so that the
// worker limits apply.
func (s *Mergeability) fire(k retryKey) {
	if k.all {
		s.rescanAll()
		return
	}
	s.q.markDirty(k.repo)
}

// retryDone forgets the failures of key after a success.
func (s *Mergeability) retryDone(key retryKey) {
	s.retryMu.Lock()
	delete(s.retries, key)
	s.retryMu.Unlock()
}

func (s *Mergeability) rescanAll() {
	repos, err := s.cfg.Meta.List(s.ctx)
	if err != nil {
		ctxLog(s.ctx).Error().Err(err).Msg("mergeability: list the repositories to rescan")
		s.retry(globalKey)
		return
	}
	s.retryDone(globalKey)
	for _, r := range repos {
		n, err := s.cfg.Store.CountOpen(s.ctx, r.Name)
		if err != nil {
			ctxLog(s.ctx).Error().Err(err).Str("repo", r.Name.String()).Msg("mergeability: count the open merge requests")
			s.q.markDirty(r.Name)
			continue
		}
		if n > 0 {
			s.q.markDirty(r.Name)
		}
	}
}

// OnOpened implements ports.MergeabilityTrigger.
func (s *Mergeability) OnOpened(_ context.Context, mr domain.MergeRequest) {
	s.q.add(mergeKey{mr.Repo, mr.Number})
}

// OnClosed deletes the record of a merge request that is no longer open. The
// store already drops it when the request leaves the open state; this makes it
// explicit and is idempotent.
func (s *Mergeability) OnClosed(ctx context.Context, mr domain.MergeRequest) {
	if err := s.cfg.Store.DeleteMergeability(context.WithoutCancel(ctx), mr.Repo, mr.Number); err != nil {
		ctxLog(ctx).Error().Err(err).Str("repo", mr.Repo.String()).Int64("number", mr.Number).Msg("mergeability: delete the record of a closed merge request")
	}
	s.q.takeSeen(mergeKey{mr.Repo, mr.Number})
}

// OnBranchesMoved implements ports.MergeabilityTrigger: the open merge
// requests whose source or target is one of the branches are queued. Past the
// bound, or when the lookup fails, the repository is rescanned.
func (s *Mergeability) OnBranchesMoved(ctx context.Context, repo domain.RepoName, updates []domain.RefUpdate) {
	var branches []string
	for _, u := range updates {
		if b, ok := strings.CutPrefix(u.Ref, "refs/heads/"); ok && !slices.Contains(branches, b) {
			branches = append(branches, b)
		}
	}
	if len(branches) == 0 {
		return
	}
	lookups := []func(context.Context, domain.RepoName, []string, int) ([]int64, bool, error){
		s.cfg.Store.OpenBySource, s.cfg.Store.OpenByTarget,
	}
	for _, lookup := range lookups {
		numbers, truncated, err := lookup(ctx, repo, branches, s.cfg.Queue)
		if err != nil || truncated {
			if err != nil {
				ctxLog(ctx).Error().Err(err).Str("repo", repo.String()).Msg("mergeability: look up the merge requests of moved branches: the repository is rescanned")
			}
			s.q.markDirty(repo)
		}
		for _, n := range numbers {
			s.q.add(mergeKey{repo, n})
		}
	}
}

// Read implements ports.MergeabilityReader.
func (s *Mergeability) Read(ctx context.Context, mr domain.MergeRequest, targetTip domain.ObjectID) domain.Mergeability {
	checking := domain.Mergeability{Repo: mr.Repo, Number: mr.Number, State: domain.MergeabilityChecking}
	if mr.HeadID.IsZero() || targetTip.IsZero() {
		return checking // a branch is gone: there is nothing to check
	}
	rec, err := s.cfg.Store.GetMergeability(ctx, mr.Repo, mr.Number)
	if err == nil && rec.IsFor(targetTip, mr.HeadID) {
		return rec
	}
	k := mergeKey{mr.Repo, mr.Number}
	s.q.markSeen(k)
	s.q.add(k)
	return checking
}

func (s *Mergeability) loop() {
	defer s.wg.Done()
	for {
		key, dirty := s.q.next()
		switch {
		case key != nil:
			if !s.runKey(*key) {
				s.retry(repoKey(key.repo))
			}
		case dirty != nil:
			s.rescan(*dirty)
		default:
			select {
			case <-s.ctx.Done():
				return
			case <-s.q.wake:
			}
		}
		if s.ctx.Err() != nil {
			return
		}
	}
}

// runKey checks k, which is marked running, as often as it was queued again
// meanwhile. It reports false when a check failed transiently; the caller
// then retries the repository.
func (s *Mergeability) runKey(k mergeKey) bool {
	ok := true
	for {
		ok = s.check(k) && ok
		if !s.q.finish(k) || s.ctx.Err() != nil {
			return ok
		}
	}
}

// rescan checks every open merge request of a repository, here and not through
// the bounded queue, so that a rescan never marks its repository dirty again.
// It succeeds, and resets the retry budget of the repository, only if the
// listing and every check succeeded; otherwise the repository is retried once.
func (s *Mergeability) rescan(repo domain.RepoName) {
	mrs, err := s.cfg.Store.ListOpen(s.ctx, repo)
	if err != nil {
		ctxLog(s.ctx).Error().Err(err).Str("repo", repo.String()).Msg("mergeability: list the open merge requests to rescan")
		s.retry(repoKey(repo))
		return
	}
	ok := true
	for _, mr := range mrs {
		if s.ctx.Err() != nil {
			return
		}
		k := mergeKey{repo, mr.Number}
		if s.q.tryRun(k) {
			ok = s.runKey(k) && ok
		}
	}
	if ok {
		s.retryDone(repoKey(repo))
	} else {
		s.retry(repoKey(repo))
	}
}

// Check computes the mergeability of a merge request now, on the calling
// goroutine, as a worker does. It is for tests; the server never calls it.
func (s *Mergeability) Check(repo domain.RepoName, number int64) {
	k := mergeKey{repo, number}
	if s.q.tryRun(k) && !s.runKey(k) {
		s.retry(repoKey(k.repo))
	}
}

// check brings the record of k up to date. It reports false when a transient
// error (store, Git) dropped the work, which is then retried.
func (s *Mergeability) check(k mergeKey) (done bool) {
	ctx := s.ctx
	log := ctxLog(ctx).With().Str("repo", k.repo.String()).Int64("number", k.number).Logger()
	for range mergeabilityAttempts {
		mr, err := s.cfg.Store.Get(ctx, k.repo, k.number)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				s.q.takeSeen(k)
				return true
			}
			if ctx.Err() != nil {
				return true
			}
			log.Error().Err(err).Msg("mergeability: read the merge request")
			return false
		}
		if !mr.IsOpen() {
			s.q.takeSeen(k)
			return true // the store dropped the record when it left the open state
		}
		head, tip, ok, transient := s.tips(ctx, mr)
		if !ok {
			return !transient
		}
		prev, err := s.cfg.Store.GetMergeability(ctx, k.repo, k.number)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			if ctx.Err() != nil {
				return true
			}
			log.Error().Err(err).Msg("mergeability: read the record")
			return false
		}
		if prev.IsFor(tip, head) {
			if s.q.takeSeen(k) && s.publish(ctx, prev) != nil { // a reader saw "checking" before this record
				s.q.markSeen(k)
				return false
			}
			return true
		}
		res, err := s.compute(ctx, k, tip, head)
		if err != nil {
			if ctx.Err() != nil {
				return true
			}
			log.Error().Err(err).Msg("mergeability: compute")
			return false
		}
		// The branches may have moved during the computation: the result is
		// then for tips nobody asks about, and is dropped.
		if h2, t2, ok, transient := s.tips(ctx, mr); !ok {
			return !transient
		} else if h2 != head || t2 != tip {
			continue
		}
		res.Version = prev.Version
		switch err := s.cfg.Store.PutMergeability(ctx, res); {
		case errors.Is(err, domain.ErrConflict):
			continue
		case errors.Is(err, domain.ErrNotFound):
			s.q.takeSeen(k)
			return true // closed meanwhile
		case err != nil:
			if ctx.Err() != nil {
				return true
			}
			log.Error().Err(err).Msg("mergeability: store the record")
			return false
		}
		seen := s.q.takeSeen(k)
		if seen || prev.State == "" || changedMergeability(prev, res) {
			if s.publish(ctx, res) != nil {
				s.q.markSeen(k) // the next check publishes it
				return false
			}
		}
		return true
	}
	s.q.add(k) // kept losing races: try again later
	return true
}

// tips reads the heads of the source and the target of mr. ok is false when
// the check is over: a branch is gone (the record is deleted) or Git failed,
// which is transient.
func (s *Mergeability) tips(ctx context.Context, mr domain.MergeRequest) (head, tip domain.ObjectID, ok, transient bool) {
	head, herr := s.cfg.Git.BranchHead(ctx, mr.Repo, mr.Source)
	tip, terr := s.cfg.Git.BranchHead(ctx, mr.Repo, mr.Target)
	switch {
	case errors.Is(herr, domain.ErrNotFound) || errors.Is(terr, domain.ErrNotFound):
		if err := s.cfg.Store.DeleteMergeability(ctx, mr.Repo, mr.Number); err != nil {
			if ctx.Err() == nil {
				ctxLog(ctx).Error().Err(err).Str("repo", mr.Repo.String()).Int64("number", mr.Number).Msg("mergeability: delete the record of a merge request without a branch")
			}
			transient = ctx.Err() == nil
		}
		s.q.takeSeen(mergeKey{mr.Repo, mr.Number})
		return domain.ObjectID{}, domain.ObjectID{}, false, transient
	case herr != nil || terr != nil:
		if ctx.Err() != nil {
			return domain.ObjectID{}, domain.ObjectID{}, false, false
		}
		ctxLog(ctx).Error().Err(errors.Join(herr, terr)).Str("repo", mr.Repo.String()).Int64("number", mr.Number).Msg("mergeability: read the branches")
		return domain.ObjectID{}, domain.ObjectID{}, false, true
	}
	return head, tip, true, false
}

// compute runs the Git reads that decide the state of head against tip.
func (s *Mergeability) compute(ctx context.Context, k mergeKey, tip, head domain.ObjectID) (domain.Mergeability, error) {
	m := domain.Mergeability{Repo: k.repo, Number: k.number, TargetID: tip, HeadID: head, CheckedAt: s.cfg.Clock.Now().UTC()}
	ancestor := tip == head
	if !ancestor {
		var err error
		if ancestor, err = s.cfg.Git.IsAncestor(ctx, k.repo, tip, head); err != nil {
			return m, err
		}
	}
	if ancestor {
		m.State = domain.MergeabilityUpToDate
		return m, nil
	}
	tree, err := s.cfg.Git.MergeTree(ctx, k.repo, tip, head)
	if err != nil {
		return m, err
	}
	if !tree.Clean {
		m.State = domain.MergeabilityConflict
		tree = tree.Bounded()
		m.ConflictPaths, m.ConflictPathsTruncated = tree.ConflictPaths, tree.Truncated
		return m, nil
	}
	m.State = domain.MergeabilityBehind
	n, err := s.cfg.Git.CountBehind(ctx, k.repo, head, tip, domain.BehindLimit)
	if err != nil {
		return m, err
	}
	m.Behind, m.BehindCapped = domain.BoundBehind(n)
	return m, nil
}

// changedMergeability reports whether what a reader shows differs.
func changedMergeability(a, b domain.Mergeability) bool {
	return a.State != b.State || a.Behind != b.Behind || a.BehindCapped != b.BehindCapped ||
		a.ConflictPathsTruncated != b.ConflictPathsTruncated || !slices.Equal(a.ConflictPaths, b.ConflictPaths)
}

// publish sends the result on the bus. It fails on a transient error; a
// repository that is gone is terminal and not an error.
func (s *Mergeability) publish(ctx context.Context, m domain.Mergeability) error {
	repo, err := s.cfg.Meta.Get(ctx, m.Repo)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) || ctx.Err() != nil {
			return nil
		}
		ctxLog(ctx).Error().Err(err).Str("repo", m.Repo.String()).Msg("mergeability: read the repository to publish a result")
		return err
	}
	s.cfg.Bus.Publish(ctx, domain.MergeRequestMergeability{
		Repo: m.Repo, Number: m.Number, Mergeability: m, At: m.CheckedAt,
		Visibility: repo.Visibility, CreatedAt: repo.CreatedAt,
	})
	return nil
}
