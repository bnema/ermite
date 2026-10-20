// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// AutoMergeConfig holds dependencies of the asynchronous merge evaluator.
type AutoMergeConfig struct {
	MRs      *MergeRequests
	Store    ports.MergeRequestStore
	Meta     ports.RepoMetaStore
	Git      ports.MergeGit
	Checks   ports.CIChecksReader
	Activity ports.CIActivity
	Tokens   ports.TokenStore
	Clock    ports.Clock
	After    func(time.Duration) <-chan time.Time
}

// AutoMerge serializes evaluation within each repository. Triggers only enqueue
// work, including when invoked while CI or merge-request locks are held.
type AutoMerge struct {
	cfg            AutoMergeConfig
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	mu             sync.Mutex
	pending        map[domain.RepoName]bool
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

var _ ports.AutoMergeTrigger = (*AutoMerge)(nil)

// NewAutoMerge builds an evaluator; call Start once, and Close before MRs.Wait.
func NewAutoMerge(ctx context.Context, cfg AutoMergeConfig) *AutoMerge {
	if cfg.After == nil {
		cfg.After = time.After
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	return &AutoMerge{cfg: cfg, ctx: ctx, cancel: cancel,
		pending: map[domain.RepoName]bool{}, dirty: map[domain.RepoName]bool{}, running: map[domain.RepoName]bool{}, again: map[domain.RepoName]bool{}, wake: make(chan struct{}, 1),
		retries: map[domain.RepoName]int{}, scheduled: map[domain.RepoName]bool{}, retryKick: make(chan struct{}, 1)}
}

// Start starts workers and discovers repositories, retrying failed discovery.
func (s *AutoMerge) Start() {
	s.wg.Add(2 + defaultMergeabilityWorkers)
	go s.scheduler()
	go func() {
		defer s.wg.Done()
		for attempt := 0; attempt <= mergeabilityRetries; attempt++ {
			repos, err := s.cfg.Store.ListOpenAll(s.ctx)
			if err != nil && s.ctx.Err() == nil {
				ctxLog(s.ctx).Error().Err(err).Int("attempt", attempt+1).Msg("auto-merge: list the repositories at startup")
			}
			if err == nil {
				for _, repo := range repos {
					if repo.AutoMerge != nil {
						s.OnAutoMergeEnabled(s.ctx, repo.Repo)
					}
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
	for range defaultMergeabilityWorkers {
		go s.loop()
	}
}

// Close cancels workers and pending retry waits, then waits for them.
func (s *AutoMerge) Close(_ context.Context) {
	s.retryMu.Lock()
	s.closed = true
	s.retryMu.Unlock()
	s.cancel()
	s.wg.Wait()
}

func (s *AutoMerge) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *AutoMerge) markDirty(repo domain.RepoName) {
	s.mu.Lock()
	if !s.pending[repo] {
		s.mu.Unlock()
		return
	}
	if s.running[repo] {
		s.again[repo] = true
	} else if !s.dirty[repo] {
		s.dirty[repo] = true
		s.order = append(s.order, repo)
	}
	s.mu.Unlock()
	s.signal()
}

func (s *AutoMerge) next() (domain.RepoName, bool) {
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

func (s *AutoMerge) loop() {
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
		if s.evaluate(repo) {
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

// OnRepoChanged marks and wakes only; it never merges on the caller's stack.
func (s *AutoMerge) OnRepoChanged(_ context.Context, repo domain.RepoName) { s.markDirty(repo) }

// OnAutoMergeEnabled remembers an intent before queuing evaluation.
func (s *AutoMerge) OnAutoMergeEnabled(_ context.Context, repo domain.RepoName) {
	s.mu.Lock()
	s.pending[repo] = true
	if s.running[repo] {
		s.again[repo] = true
	}
	s.mu.Unlock()
	s.markDirty(repo)
}

func (s *AutoMerge) evaluate(repo domain.RepoName) bool {
	err := s.evaluateRepo(repo)
	if err != nil && s.ctx.Err() == nil {
		ctxLog(s.ctx).Error().Err(err).Str("repo", repo.String()).Msg("auto-merge: evaluate repository")
	}
	return err == nil || s.ctx.Err() != nil
}

func (s *AutoMerge) evaluateRepo(repo domain.RepoName) error {
	ctx := s.ctx
	meta, err := s.cfg.Meta.Get(ctx, repo)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	mrs, err := s.cfg.Store.ListOpen(ctx, repo)
	if err != nil {
		return err
	}
	hasIntent := slices.ContainsFunc(mrs, func(mr domain.MergeRequest) bool { return mr.AutoMerge != nil })
	if !hasIntent {
		s.mu.Lock()
		// An enable arriving during this scan requests another pass. Do not
		// erase its mark based on the older store snapshot.
		if !s.again[repo] {
			delete(s.pending, repo)
		}
		s.mu.Unlock()
	}
	var failures []error
	for _, mr := range mrs {
		if ctx.Err() != nil {
			return nil
		}
		if mr.AutoMerge == nil {
			continue
		}
		if err := s.evaluateMR(ctx, meta, mr); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *AutoMerge) evaluateMR(ctx context.Context, repo domain.Repo, mr domain.MergeRequest) error {
	intent := mr.AutoMerge
	settings := repo.EffectiveSettings()
	cancel := func(reason string) error { return s.cfg.MRs.cancelAuto(ctx, mr, reason) }
	if !settings.AllowAutoMerge {
		return cancel("auto-merge was turned off in the repository settings")
	}
	if !settings.Allows(intent.Mode) {
		return cancel(fmt.Sprintf("the %s merge mode is no longer allowed", intent.Mode))
	}
	head, err := s.cfg.Git.BranchHead(ctx, mr.Repo, mr.Source)
	if errors.Is(err, domain.ErrNotFound) || (err == nil && head.IsZero()) {
		return cancel("the source branch was deleted")
	}
	if err != nil {
		return err
	}
	if head != intent.HeadID {
		return cancel("the branch moved to " + head.String()[:7])
	}
	// An enabler that can no longer merge cancels at once, even while CI is
	// pending: the intent must not stay shown as enabled. This check only
	// cancels; the merge itself resolves the enabler again under the merge lock.
	if _, err := enablerCanMerge(ctx, s.cfg.Tokens, intent, mr, s.cfg.Clock.Now()); err != nil {
		if errors.Is(err, domain.ErrForbidden) {
			return cancel(autoMergeReason(err))
		}
		return err
	}
	if s.cfg.Activity != nil && !s.cfg.Activity.Settled(ctx, mr.Repo, mr.Number) {
		return nil
	}
	checks, err := s.cfg.Checks.CommitChecks(ctx, mr.Repo, head.String())
	if err != nil {
		return err
	}
	if !hasSuccessfulCheck(checks) {
		return nil
	}
	_, err = s.cfg.MRs.mergeAs(ctx, domain.Principal{}, mr.Repo, mr.Number, ports.MergeParams{Mode: intent.Mode, HeadID: intent.HeadID}, mergeOpts{auto: &autoMergeOpts{intent: intent, tokens: s.cfg.Tokens, activity: s.cfg.Activity}})
	switch {
	case err == nil, errors.Is(err, errAutoMergeWait):
		return nil
	case errors.Is(err, domain.ErrPreconditionFailed), errors.Is(err, domain.ErrForbidden), errors.Is(err, domain.ErrUnauthenticated):
		return cancel(autoMergeReason(err))
	default:
		return err
	}
}

// Removing a known sentinel's presentation prefix is display formatting, not
// error classification (which always uses errors.Is).
func autoMergeReason(err error) string {
	text := err.Error()
	for _, sentinel := range []error{domain.ErrPreconditionFailed, domain.ErrForbidden, domain.ErrUnauthenticated} {
		text = strings.TrimPrefix(text, sentinel.Error()+": ")
	}
	return text
}

func (s *AutoMerge) retry(key domain.RepoName) {
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
func (s *AutoMerge) scheduler() {
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
func (s *AutoMerge) popDue(waited retryItem) []domain.RepoName {
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
func (s *AutoMerge) retryDone(key domain.RepoName) {
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

func enablerCanMerge(ctx context.Context, tokens ports.TokenStore, intent *domain.AutoMerge, mr domain.MergeRequest, now time.Time) (domain.Principal, error) {
	fresh, valid, err := principalOfPusher(ctx, tokens, intent.EnabledBy, now)
	if err != nil {
		return domain.Principal{}, err
	}
	if !valid || !fresh.Allows(domain.ScopeMRMerge, mr.Repo) || !fresh.AllowsRef("refs/heads/"+mr.Target) {
		return domain.Principal{}, fmt.Errorf("%w: the principal that enabled auto-merge can no longer merge %s", domain.ErrForbidden, mr.Target)
	}
	return fresh, nil
}
