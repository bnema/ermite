// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

type mirrorFlight struct {
	pending, force, manual bool
	pendingForce           bool
	done                   chan struct{}
	completed              bool
	result                 ports.MirrorSyncResult
	err                    error
}

var (
	_ ports.MirrorService = (*Mirrors)(nil)
	_ ports.MirrorTrigger = (*Mirrors)(nil)
	_ ports.MirrorWarner  = (*Mirrors)(nil)
)

// Sync waits for its own run; overlapping requests coalesce into one sticky rerun.
func (m *Mirrors) Sync(ctx context.Context, p domain.Principal, repo domain.RepoName, force bool) (ports.MirrorSyncResult, error) {
	if err := m.admin(ctx, p, repo); err != nil {
		return ports.MirrorSyncResult{}, err
	}
	m.Start()
	f, queued, err := m.enqueue(repo, force, true)
	if err != nil {
		return ports.MirrorSyncResult{}, err
	}
	if queued {
		return ports.MirrorSyncResult{Queued: true}, nil
	}
	select {
	case <-m.ctx.Done():
		return ports.MirrorSyncResult{}, m.ctx.Err()
	case <-ctx.Done():
		return ports.MirrorSyncResult{}, ctx.Err()
	case <-f.done:
		return f.result, f.err
	}
}

// SyncAll requests a sync for every repository as an owner.
func (m *Mirrors) SyncAll(ctx context.Context, p domain.Principal, force bool) ([]ports.MirrorSyncResult, error) {
	if err := mirrorOwner(p); err != nil {
		return nil, err
	}
	repos, err := m.cfg.Meta.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ports.MirrorSyncResult, 0, len(repos))
	for _, r := range repos {
		v, err := m.Sync(ctx, p, r.Name, force)
		if err != nil {
			return out, err
		}
		out = append(out, v)
	}
	return out, nil
}
func (m *Mirrors) enqueue(repo domain.RepoName, force, manual bool) (*mirrorFlight, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, false, context.Canceled
	}
	if f := m.flights[repo]; f != nil {
		f.pending = true
		f.pendingForce = f.pendingForce || force
		return f, true, nil
	}
	f := &mirrorFlight{force: force, manual: manual, done: make(chan struct{})}
	m.flights[repo] = f
	select {
	case m.jobs <- repo:
		return f, false, nil
	default:
		delete(m.flights, repo)
		return nil, false, domain.ErrResourceExhausted
	}
}
func (m *Mirrors) run(repo domain.RepoName) {
	m.mu.Lock()
	f := m.flights[repo]
	m.mu.Unlock()
	for {
		result, err := m.attempt(m.ctx, repo, f.force, f.manual)
		m.mu.Lock()
		if !f.completed {
			f.result, f.err = result, err
			f.completed = true
			close(f.done)
		}
		if !f.pending || m.closed {
			delete(m.flights, repo)
			m.mu.Unlock()
			return
		}
		f.force, f.manual = f.pendingForce, false
		f.pending, f.pendingForce = false, false
		m.mu.Unlock()
	}
}

func (m *Mirrors) syncOnce(ctx context.Context, repo domain.RepoName, force bool) (ports.MirrorSyncResult, error) {
	return m.attempt(ctx, repo, force, true)
}
func (m *Mirrors) attempt(ctx context.Context, repo domain.RepoName, force, manual bool) (ports.MirrorSyncResult, error) {
	m.configMu.RLock()
	defer m.configMu.RUnlock()
	meta, err := m.cfg.Meta.Get(ctx, repo)
	if err != nil {
		return ports.MirrorSyncResult{}, err
	}
	t, err := m.target(ctx, repo)
	if err != nil {
		return ports.MirrorSyncResult{}, err
	}
	prev, err := m.status(ctx, repo)
	if err != nil {
		return ports.MirrorSyncResult{}, err
	}
	now := m.cfg.Clock.Now().UTC()
	if !t.Enabled {
		s := domain.MirrorDisable(prev, now)
		if prev.State != domain.MirrorDisabled {
			err = m.cfg.Store.PutStatus(ctx, s)
		}
		return ports.MirrorSyncResult{Status: s}, err
	}
	if !force && (prev.State == domain.MirrorPaused || (!manual && prev.State == domain.MirrorFailing && now.Before(prev.NextRetryAt))) {
		return ports.MirrorSyncResult{Status: prev}, nil
	}
	if manual && !force && !prev.LastAttemptAt.IsZero() && now.Sub(prev.LastAttemptAt) < domain.MirrorManualInterval {
		return ports.MirrorSyncResult{Status: prev}, fmt.Errorf("%w: last sync %s ago; wait 60s or use --force", domain.ErrResourceExhausted, now.Sub(prev.LastAttemptAt))
	}
	m.mu.Lock()
	generation := m.dirty[repo]
	m.mu.Unlock()
	refs, runErr := m.cfg.Reader.ListRefs(ctx, repo)
	var selected []domain.MirrorRef
	if runErr == nil {
		branch, _, e := m.cfg.Git.DefaultBranch(ctx, repo)
		runErr = e
		if e == nil {
			selected = domain.MirrorRefspecs(refs, branch, t.AllBranches)
		}
	}
	noOp := false
	if runErr == nil && !force {
		snapshot, e := m.cfg.Store.GetSnapshot(ctx, repo)
		if e != nil && !errors.Is(e, domain.ErrNotFound) {
			return ports.MirrorSyncResult{}, e
		}
		noOp = e == nil && snapshot.Equal(t.URL, selected, t.AllBranches)
	}
	if runErr == nil && !noOp {
		key, e := m.privateKey(ctx)
		runErr = e
		if e == nil {
			specs := make([]string, len(selected))
			for i, ref := range selected {
				specs[i] = ref.Name + ":" + ref.Name
			}
			// An empty ref list must not invoke git's implicit default push behavior.
			if len(specs) > 0 {
				_, runErr = m.cfg.Pusher.Push(ctx, ports.MirrorPush{Repo: repo, URL: t.URL, Refspecs: specs, PrivateKey: key})
			}
		}
	}
	if ctx.Err() != nil {
		return ports.MirrorSyncResult{}, ctx.Err()
	}
	var failure *domain.MirrorError
	if runErr != nil && !errors.As(runErr, &failure) {
		detail := "local mirror operation failed"
		if m.cfg.Sealer == nil {
			detail = "the server has no ERMITE_SECRETS_KEY, so it cannot store the mirror key"
		}
		failure = &domain.MirrorError{Class: domain.MirrorClassInternal, Detail: detail}
		runErr = failure
		if m.cfg.Sealer == nil {
			runErr = fmt.Errorf("%w: %s", domain.ErrPreconditionFailed, detail)
		}
	}
	transition := domain.ApplyMirrorOutcome(prev, domain.MirrorOutcome{Err: failure, NoOp: noOp}, now)
	if failure != nil && m.cfg.Sealer == nil {
		transition.Status.Reason = failure.Detail
	}
	if failure == nil {
		if err := m.cfg.Store.PutSnapshot(ctx, repo, domain.MirrorSnapshot{URL: t.URL, Refs: selected, AllBranches: t.AllBranches, At: now}); err != nil {
			return ports.MirrorSyncResult{}, err
		}
	}
	if err := m.cfg.Store.PutStatus(ctx, transition.Status); err != nil {
		return ports.MirrorSyncResult{}, err
	}
	if failure == nil {
		m.mu.Lock()
		m.clean[repo] = generation
		m.mu.Unlock()
	}
	if m.cfg.Bus != nil {
		if transition.FailedEvent {
			m.cfg.Bus.Publish(ctx, domain.MirrorFailed{Repo: repo, State: transition.Status.State, Class: transition.Status.Class, Reason: transition.Status.Reason, At: now, Visibility: meta.Visibility, CreatedAt: meta.CreatedAt})
		}
		if transition.Recovered {
			m.cfg.Bus.Publish(ctx, domain.MirrorRecovered{Repo: repo, At: now, Visibility: meta.Visibility, CreatedAt: meta.CreatedAt})
		}
	}
	return ports.MirrorSyncResult{Status: transition.Status, NoOp: noOp}, runErr
}
