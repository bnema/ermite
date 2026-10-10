// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"sync"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// MaintenanceDelay is how long maintenance waits after the first push of a
// burst, so that pushes in quick succession share one run.
const MaintenanceDelay = 5 * time.Second

// Maintainer runs repository maintenance after pushes, debounced per
// repository: the first Schedule starts a timer, further ones before it fires
// join the same run, and a repository is maintained by one run at a time. A
// Schedule during a run queues exactly one more run.
//
// Its goroutines use the logger of the context given to NewMaintainer and
// live until Close. A run is cancelled by Close, and Git bounds it otherwise
// (gitcli applies its command timeout to a context without deadline).
type Maintainer struct {
	git   ports.GitRepos
	delay time.Duration
	ctx   context.Context
	stop  context.CancelFunc

	mu      sync.Mutex
	closed  bool
	entries map[domain.RepoName]*maintEntry
	wg      sync.WaitGroup
}

type maintEntry struct {
	timer   *time.Timer // pending run, nil while running or idle
	running bool
	again   bool // scheduled during a run
}

// NewMaintainer builds a maintainer. ctx carries the logger; its cancellation
// is not followed, call Close. A zero delay means MaintenanceDelay.
func NewMaintainer(ctx context.Context, git ports.GitRepos, delay time.Duration) *Maintainer {
	if delay <= 0 {
		delay = MaintenanceDelay
	}
	run, stop := context.WithCancel(context.WithoutCancel(ctx))
	return &Maintainer{git: git, delay: delay, ctx: run, stop: stop, entries: make(map[domain.RepoName]*maintEntry)}
}

// Schedule queues maintenance of the repository.
func (m *Maintainer) Schedule(ctx context.Context, name domain.RepoName) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	e := m.entries[name]
	switch {
	case e == nil:
		e = &maintEntry{}
		m.entries[name] = e
		m.arm(name, e)
		log := zerowrap.FromCtx(ctx)
		log.Debug().Str("repo", name.String()).Dur("delay", m.delay).Msg("repository maintenance scheduled")
	case e.running:
		e.again = true
	}
	// A timer is already pending: this push joins it.
}

// arm starts the timer of e. The caller holds mu.
func (m *Maintainer) arm(name domain.RepoName, e *maintEntry) {
	m.wg.Add(1) // released when the timer fires or is stopped by Close
	e.timer = time.AfterFunc(m.delay, func() { m.run(name, e) })
}

func (m *Maintainer) run(name domain.RepoName, e *maintEntry) {
	defer m.wg.Done()
	m.mu.Lock()
	if m.closed {
		delete(m.entries, name)
		m.mu.Unlock()
		return
	}
	e.timer, e.running = nil, true
	m.mu.Unlock()

	log := zerowrap.FromCtx(m.ctx)
	start := time.Now()
	if err := m.git.Maintain(m.ctx, name); err != nil {
		log.Warn().Err(err).Str("repo", name.String()).Msg("repository maintenance failed")
	} else {
		log.Debug().Str("repo", name.String()).Dur("duration", time.Since(start)).Msg("repository maintained")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	e.running = false
	if e.again && !m.closed {
		e.again = false
		m.arm(name, e)
		return
	}
	delete(m.entries, name)
}

// Close stops pending runs, cancels the one in progress and waits for it.
func (m *Maintainer) Close(_ context.Context) {
	m.mu.Lock()
	m.closed = true
	for _, e := range m.entries {
		if e.timer != nil && e.timer.Stop() {
			m.wg.Done() // the timer will never fire
			e.timer = nil
		}
	}
	m.mu.Unlock()
	m.stop()
	m.wg.Wait()
}
