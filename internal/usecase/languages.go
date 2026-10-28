// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

const (
	languagesJobTimeout  = 2 * time.Minute
	defaultLanguagesPace = 50 * time.Millisecond
	languagesListRetries = 5
)

// LanguagesConfig holds the dependencies of the language worker.
type LanguagesConfig struct {
	Meta   ports.RepoMetaStore
	Store  ports.LanguageStore
	Reader ports.RepoReader
	Files  ports.TreeFileLister
	Clock  ports.Clock
	// Pace is the wait before each job of the startup backfill; 0 means 50ms
	// and a negative value means none (tests).
	Pace time.Duration
}

// Languages counts the languages of the default branch of each repository in
// the background, with one worker goroutine. A repository is counted again when
// its default-branch head moved; the result is derived data, so a failed job is
// logged and dropped (the next push or restart tries again).
type Languages struct {
	cfg    LanguagesConfig
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu             sync.Mutex
	hot, cold      []domain.RepoName // FIFOs: marked by a trigger, then by the backfill
	queued         map[domain.RepoName]bool
	running, again map[domain.RepoName]bool
	wake           chan struct{}
}

var _ ports.LanguageTrigger = (*Languages)(nil)

// NewLanguages builds a worker whose lifetime is controlled by Close.
func NewLanguages(ctx context.Context, cfg LanguagesConfig) *Languages {
	if cfg.Pace == 0 {
		cfg.Pace = defaultLanguagesPace
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	return &Languages{cfg: cfg, ctx: ctx, cancel: cancel,
		queued: map[domain.RepoName]bool{}, running: map[domain.RepoName]bool{}, again: map[domain.RepoName]bool{},
		wake: make(chan struct{}, 1)}
}

// Start starts the worker and queues every repository for a backfill.
func (l *Languages) Start() {
	l.wg.Add(2)
	go l.backfill()
	go l.loop()
}

// Close cancels the running job and waits for the worker.
func (l *Languages) Close(_ context.Context) {
	l.cancel()
	l.wg.Wait()
}

// OnRepoChanged implements ports.LanguageTrigger: it marks the repository and
// returns. Any push may call it; the job skips a head that did not move.
func (l *Languages) OnRepoChanged(_ context.Context, repo domain.RepoName) { l.markHot(repo) }

func (l *Languages) signal() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *Languages) markHot(repo domain.RepoName) {
	l.mu.Lock()
	switch {
	case l.running[repo]:
		l.again[repo] = true
	case l.queued[repo]:
		// Already waiting: move it from the backfill to the front queue.
		for i, r := range l.cold {
			if r == repo {
				l.cold = append(l.cold[:i], l.cold[i+1:]...)
				l.hot = append(l.hot, repo)
				break
			}
		}
	default:
		l.queued[repo] = true
		l.hot = append(l.hot, repo)
	}
	l.mu.Unlock()
	l.signal()
}

func (l *Languages) markCold(repo domain.RepoName) {
	l.mu.Lock()
	if !l.queued[repo] && !l.running[repo] {
		l.queued[repo] = true
		l.cold = append(l.cold, repo)
	}
	l.mu.Unlock()
	l.signal()
}

// next takes the oldest hot repository, else the oldest cold one.
func (l *Languages) next() (repo domain.RepoName, found bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case len(l.hot) > 0:
		repo, l.hot = l.hot[0], l.hot[1:]
	case len(l.cold) > 0:
		repo, l.cold = l.cold[0], l.cold[1:]
	default:
		return domain.RepoName{}, false
	}
	delete(l.queued, repo)
	l.running[repo] = true
	return repo, true
}

func (l *Languages) pending() (hot, cold bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.hot) > 0, len(l.cold) > 0
}

func (l *Languages) loop() {
	defer l.wg.Done()
	for l.ctx.Err() == nil {
		hot, cold := l.pending()
		if !hot && !cold {
			select {
			case <-l.ctx.Done():
				return
			case <-l.wake:
			}
			continue
		}
		if !hot && l.cfg.Pace > 0 { // pace the backfill only
			select {
			case <-l.ctx.Done():
				return
			case <-time.After(l.cfg.Pace):
			}
		}
		repo, found := l.next()
		if !found {
			continue
		}
		l.runJob(repo)
		l.mu.Lock()
		delete(l.running, repo)
		if l.again[repo] {
			delete(l.again, repo)
			l.queued[repo] = true
			l.hot = append(l.hot, repo)
		}
		l.mu.Unlock()
	}
}

// runJob runs one job under a timeout; a failure or a panic is logged and
// leaves the worker running.
func (l *Languages) runJob(repo domain.RepoName) {
	ctx, cancel := context.WithTimeout(l.ctx, languagesJobTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			ctxLog(ctx).Error().Str("repo", repo.String()).Str("panic", fmt.Sprint(r)).Bytes("stack", debug.Stack()).Msg("languages: job panicked")
		}
	}()
	if err := l.compute(ctx, repo); err != nil && l.ctx.Err() == nil && !errors.Is(err, domain.ErrNotFound) {
		ctxLog(ctx).Error().Err(err).Str("repo", repo.String()).Msg("languages: count the languages")
	}
}

func (l *Languages) backfill() {
	defer l.wg.Done()
	for attempt := 0; ; attempt++ {
		repos, err := l.cfg.Meta.List(l.ctx)
		if err == nil {
			for _, r := range repos {
				l.markCold(r.Name)
			}
			return
		}
		if l.ctx.Err() != nil || attempt == languagesListRetries {
			if l.ctx.Err() == nil {
				ctxLog(l.ctx).Error().Err(err).Msg("languages: list the repositories at startup")
			}
			return
		}
		select {
		case <-l.ctx.Done():
			return
		case <-time.After(mergeBackoff(attempt + 1)):
		}
	}
}

// compute counts the languages of the default branch head of repo unless the
// stored count is for that head already.
func (l *Languages) compute(ctx context.Context, repo domain.RepoName) error {
	r, err := l.cfg.Meta.Get(ctx, repo)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get the repository: %w", err)
	}
	head, err := l.cfg.Reader.ResolveRevision(ctx, repo, domain.Revision{})
	if errors.Is(err, domain.ErrNotFound) { // empty, or HEAD names no branch
		return l.cfg.Store.Delete(ctx, repo)
	}
	if err != nil {
		return fmt.Errorf("resolve HEAD: %w", err)
	}
	old, ok, err := l.cfg.Store.Get(ctx, repo)
	if err != nil {
		return fmt.Errorf("read the stored count: %w", err)
	}
	if ok && old.Commit == head.CommitID && old.Version == domain.LanguagesVersion && old.RepoCreatedAt.Equal(r.CreatedAt) {
		return nil
	}
	// Read at the commit id, so that a moving HEAD cannot mix two trees.
	files, err := l.cfg.Files.ListFileSizes(ctx, repo, head.CommitID, domain.MaxLanguageFiles)
	if err != nil {
		return fmt.Errorf("list the files: %w", err)
	}
	rec := domain.CountLanguages(files)
	rec.Repo, rec.RepoCreatedAt, rec.Commit = repo, r.CreatedAt, head.CommitID
	rec.Version, rec.ComputedAt = domain.LanguagesVersion, l.cfg.Clock.Now().UTC()

	again, err := l.cfg.Reader.ResolveRevision(ctx, repo, domain.Revision{})
	if errors.Is(err, domain.ErrNotFound) { // the branch vanished meanwhile
		return l.cfg.Store.Delete(ctx, repo)
	}
	if err != nil {
		return fmt.Errorf("resolve HEAD again: %w", err)
	}
	if again.CommitID != head.CommitID {
		l.markHot(repo) // the head moved: drop this result
		return nil
	}
	if err := l.cfg.Store.Put(ctx, rec); err != nil {
		return fmt.Errorf("store the count: %w", err)
	}
	return nil
}
