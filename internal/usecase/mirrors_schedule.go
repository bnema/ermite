// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
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
		m.dirty[repo]++
	}
}

// ScheduleNow examines one current minute; missed minutes are not replayed.
func (m *Mirrors) ScheduleNow(ctx context.Context) {
	m.Start()
	m.scheduleMu.Lock()
	defer m.scheduleMu.Unlock()
	now := m.cfg.Clock.Now().UTC()
	minute := now.Truncate(time.Minute)
	if !minute.After(m.lastMinute) {
		return
	}
	m.lastMinute = minute
	repos, err := m.cfg.Meta.List(ctx)
	if err != nil {
		return
	}
	live := make(map[domain.RepoName]bool, len(repos))
	for _, r := range repos {
		if ctx.Err() != nil {
			return
		}
		live[r.Name] = true
		t, err := m.target(ctx, r.Name)
		if err != nil || !t.Enabled {
			continue
		}
		s, err := m.status(ctx, r.Name)
		if err != nil || s.State == domain.MirrorPaused {
			continue
		}
		due := false
		if s.State == domain.MirrorFailing {
			due = !now.Before(s.NextRetryAt)
		} else {
			due = (domain.CronSchedule{Cron: t.Cron, Loc: time.UTC}).Due(minute)
		}
		if !due {
			continue
		}
		m.mu.Lock()
		clean, known := m.clean[r.Name]
		dirty := m.dirty[r.Name]
		m.mu.Unlock()
		if s.State == domain.MirrorOK && known && clean == dirty {
			snapshot, e := m.cfg.Store.GetSnapshot(ctx, r.Name)
			if e == nil && snapshot.URL == t.URL && snapshot.AllBranches == t.AllBranches {
				continue
			}
		}
		if _, _, err := m.enqueue(r.Name, false, false); err != nil {
			log := zerowrap.FromCtx(ctx)
			log.Warn().Str("repo", r.Name.String()).Msg("mirror queue full or closed")
		}
	}
	m.mu.Lock()
	for repo := range m.dirty {
		if !live[repo] {
			delete(m.dirty, repo)
			delete(m.clean, repo)
		}
	}
	m.mu.Unlock()
}
