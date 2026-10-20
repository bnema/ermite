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
	mergeabilityRetries        = 5
	mergeabilityRetryInitial   = time.Second
	mergeabilityRetryMax       = time.Minute
)

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

// MergeabilityConfig holds the dependencies of the repository reconciler.
type MergeabilityConfig struct {
	AutoMerge ports.AutoMergeTrigger
	Store     ports.MergeRequestStore
	Meta      ports.RepoMetaStore
	Git       ports.MergeGit
	Bus       ports.EventBus
	Clock     ports.Clock
	Workers   int // defaults to 2
	// After is injectable to drive backoff without wall-clock waits in tests.
	After func(time.Duration) <-chan time.Time
}

// Mergeability reconciles derived records, with one writer per repository.
// Its work and retry state are bounded by the number of repositories.
type Mergeability struct {
	cfg            MergeabilityConfig
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	mu             sync.Mutex
	dirty          map[domain.RepoName]bool
	order          []domain.RepoName
	running, again map[domain.RepoName]bool
	wake           chan struct{}
	retryMu        sync.Mutex
	retries        map[domain.RepoName]int
	scheduled      map[domain.RepoName]bool
	heap           []retryItem
	retryKick      chan struct{}
	closed         bool
}

// MergeabilityHook is the unchanged trigger and reader contract.
type MergeabilityHook interface {
	ports.MergeabilityTrigger
	ports.MergeabilityReader
}

var _ MergeabilityHook = (*Mergeability)(nil)

// NewMergeability builds a reconciler whose lifetime is controlled by Close.
func NewMergeability(ctx context.Context, cfg MergeabilityConfig) *Mergeability {
	if cfg.Workers <= 0 {
		cfg.Workers = defaultMergeabilityWorkers
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	return &Mergeability{cfg: cfg, ctx: ctx, cancel: cancel,
		dirty: map[domain.RepoName]bool{}, running: map[domain.RepoName]bool{}, again: map[domain.RepoName]bool{}, wake: make(chan struct{}, 1),
		retries: map[domain.RepoName]int{}, scheduled: map[domain.RepoName]bool{}, retryKick: make(chan struct{}, 1)}
}

// Start starts workers and discovers repositories, retrying failed discovery.
func (s *Mergeability) Start() {
	s.wg.Add(2 + s.cfg.Workers)
	go s.scheduler()
	go func() {
		defer s.wg.Done()
		for attempt := 0; attempt <= mergeabilityRetries; attempt++ {
			repos, err := s.cfg.Meta.List(s.ctx)
			if err != nil && s.ctx.Err() == nil {
				ctxLog(s.ctx).Error().Err(err).Int("attempt", attempt+1).Msg("mergeability: list the repositories at startup")
			}
			if err == nil {
				for _, repo := range repos {
					s.markDirty(repo.Name)
				}
				return
			}
			if attempt == mergeabilityRetries {
				return
			}
			select {
			case <-s.ctx.Done():
				return
			case <-s.cfg.After(mergeBackoff(attempt + 1)):
			}
		}
	}()
	for range s.cfg.Workers {
		go s.loop()
	}
}

// Close cancels workers and pending retry waits, then waits for them.
func (s *Mergeability) Close(_ context.Context) {
	s.retryMu.Lock()
	s.closed = true
	s.retryMu.Unlock()
	s.cancel()
	s.wg.Wait()
}

func (s *Mergeability) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Mergeability) markDirty(repo domain.RepoName) {
	s.mu.Lock()
	if s.running[repo] {
		s.again[repo] = true
	} else if !s.dirty[repo] {
		s.dirty[repo] = true
		s.order = append(s.order, repo)
	}
	s.mu.Unlock()
	s.signal()
}

func (s *Mergeability) next() (domain.RepoName, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.order) == 0 {
		return domain.RepoName{}, false
	}
	repo := s.order[0]
	s.order = s.order[1:]
	delete(s.dirty, repo)
	s.running[repo] = true
	if len(s.order) > 0 {
		s.signal()
	}
	return repo, true
}

func (s *Mergeability) loop() {
	defer s.wg.Done()
	for s.ctx.Err() == nil {
		repo, found := s.next()
		if !found {
			select {
			case <-s.ctx.Done():
				return
			case <-s.wake:
			}
			continue
		}
		if s.reconcile(repo) {
			s.retryDone(repo)
		} else {
			s.retry(repo)
		}
		s.mu.Lock()
		delete(s.running, repo)
		if s.again[repo] {
			delete(s.again, repo)
			s.dirty[repo] = true
			s.order = append(s.order, repo)
			s.signal()
		}
		s.mu.Unlock()
	}
}

// OnOpened implements ports.MergeabilityTrigger.
func (s *Mergeability) OnOpened(_ context.Context, mr domain.MergeRequest) { s.markDirty(mr.Repo) }

// OnBranchesMoved dirties the repository for any branch update.
func (s *Mergeability) OnBranchesMoved(_ context.Context, repo domain.RepoName, updates []domain.RefUpdate) {
	for _, update := range updates {
		if strings.HasPrefix(update.Ref, "refs/heads/") {
			s.markDirty(repo)
			return
		}
	}
}

// Read returns a current record, or checking and requests reconciliation.
func (s *Mergeability) Read(ctx context.Context, mr domain.MergeRequest, targetTip domain.ObjectID) domain.Mergeability {
	if !mr.HeadID.IsZero() && !targetTip.IsZero() {
		rec, err := s.cfg.Store.GetMergeability(ctx, mr.Repo, mr.Number)
		if err == nil && rec.IsFor(targetTip, mr.HeadID) {
			return rec
		}
	}
	s.markDirty(mr.Repo)
	return domain.Mergeability{Repo: mr.Repo, Number: mr.Number, State: domain.MergeabilityChecking}
}

// reconcile settles each open request for the tips read in this pass. A push
// during computation requests another pass; old-tip records are harmless.
func (s *Mergeability) reconcile(repo domain.RepoName) bool {
	ctx := s.ctx
	meta, err := s.cfg.Meta.Get(ctx, repo)
	if errors.Is(err, domain.ErrNotFound) {
		return true
	}
	if err != nil {
		s.logFailure(ctx, err, "get the repository", repo, 0)
		return false
	}
	mrs, err := s.cfg.Store.ListOpen(ctx, repo)
	if err != nil {
		s.logFailure(ctx, err, "list the open merge requests", repo, 0)
		return false
	}
	ok := true
	changed := false
	for _, mr := range mrs {
		if ctx.Err() != nil {
			return true
		}
		head, tip, found, transient := s.tips(ctx, mr)
		if !found {
			ok = ok && !transient
			continue
		}
		prev, err := s.cfg.Store.GetMergeability(ctx, repo, mr.Number)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			s.logFailure(ctx, err, "get the mergeability", repo, mr.Number)
			ok = false
			continue
		}
		if prev.IsFor(tip, head) {
			continue
		}
		res, err := s.compute(ctx, repo, mr.Number, tip, head)
		if err != nil {
			s.logFailure(ctx, err, "compute the mergeability", repo, mr.Number)
			ok = false
			continue
		}
		res.Version = prev.Version
		err = s.cfg.Store.PutMergeability(ctx, res)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			if !errors.Is(err, domain.ErrConflict) { // retried by the next pass
				s.logFailure(ctx, err, "store the mergeability", repo, mr.Number)
			}
			ok = false
			continue
		}
		s.cfg.Bus.Publish(ctx, domain.MergeRequestMergeability{
			Repo: repo, Number: mr.Number, Mergeability: res, At: res.CheckedAt,
			Visibility: meta.Visibility, CreatedAt: meta.CreatedAt,
		})
		changed = true
	}
	if changed && s.cfg.AutoMerge != nil {
		s.cfg.AutoMerge.OnRepoChanged(ctx, repo)
	}
	return ok
}

// logFailure logs an unexpected failure; cancellation is expected on Close.
func (s *Mergeability) logFailure(ctx context.Context, err error, op string, repo domain.RepoName, number int64) {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, domain.ErrNotFound) {
		return
	}
	e := ctxLog(ctx).Error().Err(err).Str("repo", repo.String())
	if number != 0 {
		e = e.Int64("number", number)
	}
	e.Msg("mergeability: " + op)
}

// retryItem is one scheduled retry.
type retryItem struct {
	due time.Time
	key domain.RepoName
}

// retry schedules a retry of key after a bounded exponential backoff. It is
// deduplicated per key, gives up after mergeabilityRetries attempts and stops
// on Close. All retries share one scheduler goroutine.
func (s *Mergeability) retry(key domain.RepoName) {
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
		for _, k := range s.popDue(waited) {
			if s.ctx.Err() != nil {
				return
			}
			s.markDirty(k)
		}
	}
}

// popDue removes the waited item and the ones due as well. The waited item is
// removed regardless of the clock only when both its key and its deadline
// match: a replacement retry of the same repository is kept until it is due.
func (s *Mergeability) popDue(waited retryItem) []domain.RepoName {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	now := s.cfg.Clock.Now()
	var out []domain.RepoName
	s.heap = slices.DeleteFunc(s.heap, func(it retryItem) bool {
		if (it.key != waited.key || !it.due.Equal(waited.due)) && it.due.After(now) {
			return false
		}
		out = append(out, it.key)
		delete(s.scheduled, it.key)
		return true
	})
	return out
}

// retryDone forgets the failures of key after a success.
func (s *Mergeability) retryDone(key domain.RepoName) {
	s.retryMu.Lock()
	delete(s.retries, key)
	delete(s.scheduled, key)
	s.heap = slices.DeleteFunc(s.heap, func(it retryItem) bool { return it.key == key })
	select {
	case s.retryKick <- struct{}{}:
	default:
	}
	s.retryMu.Unlock()
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
func (s *Mergeability) compute(ctx context.Context, repo domain.RepoName, number int64, tip, head domain.ObjectID) (domain.Mergeability, error) {
	m := domain.Mergeability{Repo: repo, Number: number, TargetID: tip, HeadID: head, CheckedAt: s.cfg.Clock.Now().UTC()}
	ancestor := tip == head
	if !ancestor {
		var err error
		if ancestor, err = s.cfg.Git.IsAncestor(ctx, repo, tip, head); err != nil {
			return m, err
		}
	}
	if ancestor {
		m.State = domain.MergeabilityUpToDate
		return m, nil
	}
	tree, err := s.cfg.Git.MergeTree(ctx, repo, tip, head)
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
	n, err := s.cfg.Git.CountBehind(ctx, repo, head, tip, domain.BehindLimit)
	if err != nil {
		return m, err
	}
	m.Behind, m.BehindCapped = domain.BoundBehind(n)
	return m, nil
}

// WithAutoMerge installs the evaluator before Start.
func (s *Mergeability) WithAutoMerge(t ports.AutoMergeTrigger) *Mergeability {
	s.cfg.AutoMerge = t
	return s
}
