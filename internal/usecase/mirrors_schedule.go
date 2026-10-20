// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"time"

	"bnema.dev/ermite/internal/domain"
	"github.com/bnema/zerowrap"
)

// Start starts the bounded worker pool and UTC scheduler exactly once.
func (m *Mirrors) Start() {
	m.start.Do(func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.closed {
			return
		}
		m.wg.Add(m.cfg.Workers + 1)
		for i := 0; i < m.cfg.Workers; i++ {
			go func() {
				defer m.wg.Done()
				for {
					select {
					case <-m.ctx.Done():
						return
					case repo := <-m.jobs:
						m.run(repo)
					}
				}
			}()
		}
		go func() {
			defer m.wg.Done()
			ticker := time.NewTicker(m.cfg.ScheduleTick)
			defer ticker.Stop()
			for {
				select {
				case <-m.ctx.Done():
					return
				case <-ticker.C:
					m.ScheduleNow(m.ctx)
				}
			}
		}()
	})
}

// Close cancels workers and waits until they stop or the caller's deadline expires.
func (m *Mirrors) Close(ctx context.Context) {
	m.mu.Lock()
	m.closed = true
	m.cancel()
	m.mu.Unlock()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-ctx.Done():
	case <-done:
	}
}

// OnPush marks the repository dirty without I/O or waiting for a worker.
func (m *Mirrors) OnPush(_ context.Context, repo domain.RepoName, _ []domain.RefUpdate) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		g := m.gens[repo]
		g.dirty++
		m.gens[repo] = g
	}
}

// ScheduleNow examines the current minute; missed minutes are not replayed. A repository that
// was due but found the queue full is kept and retried on the next calls, so a full queue
// delays a sync instead of skipping a period. Workers must be running (Start).
func (m *Mirrors) ScheduleNow(ctx context.Context) {
	m.scheduleMu.Lock()
	defer m.scheduleMu.Unlock()
	now := m.cfg.Clock.Now().UTC()
	minute := now.Truncate(time.Minute)
	newMinute := minute.After(m.lastMinute)
	if !newMinute && len(m.deferred) == 0 {
		return
	}
	log := zerowrap.FromCtx(ctx)
	repos, err := m.cfg.Meta.List(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("mirror schedule: cannot list repositories")
		return
	}
	if newMinute {
		m.lastMinute = minute
	}
	live := make(map[domain.RepoName]bool, len(repos))
	for _, r := range repos {
		if ctx.Err() != nil {
			return
		}
		live[r.Name] = true
		_, wasDeferred := m.deferred[r.Name]
		if !newMinute && !wasDeferred {
			continue
		}
		delete(m.deferred, r.Name)
		t, err := m.target(ctx, r.Name)
		if err != nil || !t.Enabled {
			continue
		}
		s, err := m.status(ctx, r.Name)
		if err != nil || s.State == domain.MirrorPaused {
			continue
		}
		due := wasDeferred
		if !due {
			if s.State == domain.MirrorFailing {
				due = !now.Before(s.NextRetryAt)
			} else {
				due = (domain.CronSchedule{Cron: t.Cron, Loc: time.UTC}).Due(minute)
			}
		}
		if !due {
			continue
		}
		m.mu.Lock()
		g := m.gens[r.Name]
		m.mu.Unlock()
		if s.State == domain.MirrorOK && g.cleanKnown && g.clean == g.dirty {
			snapshot, e := m.cfg.Store.GetSnapshot(ctx, r.Name)
			if e == nil && snapshot.URL == t.URL && snapshot.AllBranches == t.AllBranches {
				continue
			}
		}
		if _, _, err := m.enqueue(r.Name, mirrorReq{}); errors.Is(err, domain.ErrResourceExhausted) {
			m.deferred[r.Name] = struct{}{}
			log.Warn().Str("repo", r.Name.String()).Msg("mirror queue full: sync deferred")
		}
	}
	for repo := range m.deferred {
		if !live[repo] {
			delete(m.deferred, repo)
		}
	}
	m.mu.Lock()
	for repo := range m.gens {
		if !live[repo] {
			delete(m.gens, repo)
		}
	}
	m.mu.Unlock()
}
