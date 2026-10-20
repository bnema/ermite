// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// mirrorReq is what a sync request asks for. Requests that coalesce merge by OR.
type mirrorReq struct{ force, manual bool }

func (r mirrorReq) merge(o mirrorReq) mirrorReq {
	return mirrorReq{force: r.force || o.force, manual: r.manual || o.manual}
}

// mirrorFlight is the single in-flight sync of a repository: cur is running, next is the
// merged request that arrived meanwhile (one rerun, whatever the number of requests).
type mirrorFlight struct {
	cur       mirrorReq
	next      *mirrorReq
	done      chan struct{} // closed with the result of cur's first run
	completed bool
	result    ports.MirrorSyncResult
	err       error
}

var (
	_ ports.MirrorService = (*Mirrors)(nil)
	_ ports.MirrorTrigger = (*Mirrors)(nil)
	_ ports.MirrorWarner  = (*Mirrors)(nil)
)

// DefaultMirrorSyncWait is how long a manual sync holds its caller before answering "queued":
// under the 10 s the edge proxy allows a response, and well under the client timeout.
const DefaultMirrorSyncWait = 8 * time.Second

// Sync waits for its own run; overlapping requests coalesce into one sticky rerun. When the
// push outlasts the wait bound the result is Queued: the sync goes on in the background and
// its outcome is in the mirror status.
func (m *Mirrors) Sync(ctx context.Context, p domain.Principal, repo domain.RepoName, force bool) (ports.MirrorSyncResult, error) {
	if err := m.admin(ctx, p, repo); err != nil {
		return ports.MirrorSyncResult{}, err
	}
	m.Start()
	f, queued, err := m.enqueue(repo, mirrorReq{force: force, manual: true})
	if err != nil {
		return ports.MirrorSyncResult{}, err
	}
	if queued {
		return pendingSync(repo), nil
	}
	wctx, cancel := context.WithTimeout(ctx, m.cfg.SyncWait)
	defer cancel()
	return m.await(ctx, wctx, repo, f)
}

func pendingSync(repo domain.RepoName) ports.MirrorSyncResult {
	return ports.MirrorSyncResult{Repo: repo, Status: domain.MirrorStatus{Repo: repo}, Queued: true}
}

// await waits for the first run of f. wctx carries the wait bound and ctx the caller's own
// cancellation: the bound expiring alone yields a Queued result, not an error.
func (m *Mirrors) await(ctx, wctx context.Context, repo domain.RepoName, f *mirrorFlight) (ports.MirrorSyncResult, error) {
	select {
	case <-m.ctx.Done():
		return ports.MirrorSyncResult{}, m.ctx.Err()
	case <-wctx.Done():
		if err := ctx.Err(); err != nil {
			return ports.MirrorSyncResult{}, err
		}
		select { // a run that finished as the bound expired wins
		case <-f.done:
		default:
			return pendingSync(repo), nil
		}
	case <-f.done:
	}
	m.mu.Lock()
	res, err := f.result, f.err
	m.mu.Unlock()
	res.Repo = repo
	return res, err
}

// SyncAll syncs every repository whose mirror is enabled, as an owner. Every repository is
// enqueued first, then all outcomes are awaited together under one wait bound, so the total
// time is about one push, not one per repository. A failing repository does not stop the
// others: its outcome carries the error; one still running at the bound is Queued. The
// returned error is only for what prevents the whole request (authorization, listing,
// cancellation). Results are in listing order.
func (m *Mirrors) SyncAll(ctx context.Context, p domain.Principal, force bool) ([]ports.MirrorSyncResult, error) {
	if err := mirrorOwner(p); err != nil {
		return nil, err
	}
	repos, err := m.cfg.Meta.List(ctx)
	if err != nil {
		return nil, err
	}
	m.Start()
	type entry struct {
		res ports.MirrorSyncResult
		f   *mirrorFlight // nil once the outcome is known
	}
	entries := make([]entry, 0, len(repos))
	for _, r := range repos {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		t, err := m.target(ctx, r.Name)
		if err == nil && !t.Enabled {
			continue
		}
		e := entry{res: ports.MirrorSyncResult{Repo: r.Name, Err: err}}
		if err == nil {
			f, queued, qerr := m.enqueue(r.Name, mirrorReq{force: force, manual: true})
			switch {
			case qerr != nil:
				e.res.Err = qerr
			case queued:
				e.res = pendingSync(r.Name)
			default:
				e.f = f
			}
		}
		entries = append(entries, e)
	}
	wctx, cancel := context.WithTimeout(ctx, m.cfg.SyncWait)
	defer cancel()
	out := make([]ports.MirrorSyncResult, 0, len(entries))
	for _, e := range entries {
		if e.f != nil {
			res, err := m.await(ctx, wctx, e.res.Repo, e.f)
			if cerr := ctx.Err(); cerr != nil {
				return nil, cerr
			}
			e.res, e.res.Err = res, err
		}
		if e.res.Status.Repo == (domain.RepoName{}) {
			e.res.Status.Repo = e.res.Repo
		}
		out = append(out, e.res)
	}
	return out, nil
}

// Wait blocks until no sync is queued or running, or ctx is done. It is a test barrier: call it
// only while nothing enqueues new syncs and before Close (queued jobs are not drained by Close).
func (m *Mirrors) Wait(ctx context.Context) {
	done := make(chan struct{})
	go func() { m.flightWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (m *Mirrors) enqueue(repo domain.RepoName, req mirrorReq) (*mirrorFlight, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, false, context.Canceled
	}
	if f := m.flights[repo]; f != nil {
		next := req
		if f.next != nil {
			next = f.next.merge(req)
		}
		f.next = &next
		return f, true, nil
	}
	f := &mirrorFlight{cur: req, done: make(chan struct{})}
	select {
	case m.jobs <- repo:
		m.flights[repo] = f
		m.flightWG.Add(1)
		return f, false, nil
	default:
		return nil, false, domain.ErrResourceExhausted
	}
}

func (m *Mirrors) run(repo domain.RepoName) {
	m.mu.Lock()
	f := m.flights[repo]
	m.mu.Unlock()
	for {
		result, err := m.attempt(m.ctx, repo, f.cur)
		m.mu.Lock()
		if !f.completed {
			f.result, f.err = result, err
			f.completed = true
			close(f.done)
		}
		if f.next == nil || m.closed {
			delete(m.flights, repo)
			m.flightWG.Done()
			m.mu.Unlock()
			return
		}
		f.cur, f.next = *f.next, nil
		m.mu.Unlock()
	}
}

// mirrorPlan is what attempt decided under the configuration lock, before pushing.
type mirrorPlan struct {
	meta       domain.Repo
	target     domain.MirrorTarget
	prev       domain.MirrorStatus
	start      time.Time
	generation uint64
}

// plan reads the target and status and applies the skip and throttle rules. When done is set
// the attempt ends with result and err.
func (m *Mirrors) plan(ctx context.Context, repo domain.RepoName, req mirrorReq) (pl mirrorPlan, result ports.MirrorSyncResult, done bool, err error) {
	m.configMu.RLock()
	defer m.configMu.RUnlock()
	meta, err := m.cfg.Meta.Get(ctx, repo)
	if err != nil {
		return pl, result, true, err
	}
	t, err := m.target(ctx, repo)
	if err != nil {
		return pl, result, true, err
	}
	prev, err := m.status(ctx, repo)
	if err != nil {
		return pl, result, true, err
	}
	now := m.cfg.Clock.Now().UTC()
	if !t.Enabled {
		s := domain.MirrorDisable(prev, now)
		if prev.State != domain.MirrorDisabled {
			err = m.cfg.Store.PutStatus(ctx, s)
		}
		return pl, ports.MirrorSyncResult{Status: s}, true, err
	}
	if !req.force && (prev.State == domain.MirrorPaused || (!req.manual && prev.State == domain.MirrorFailing && now.Before(prev.NextRetryAt))) {
		return pl, ports.MirrorSyncResult{Status: prev}, true, nil
	}
	if req.manual && !req.force && !prev.LastAttemptAt.IsZero() && now.Sub(prev.LastAttemptAt) < domain.MirrorManualInterval {
		return pl, ports.MirrorSyncResult{Status: prev}, true, fmt.Errorf("%w: last sync %s ago; wait %s or use --force", domain.ErrResourceExhausted, now.Sub(prev.LastAttemptAt).Round(time.Second), domain.MirrorManualInterval)
	}
	m.mu.Lock()
	generation := m.gens[repo].dirty
	m.mu.Unlock()
	return mirrorPlan{meta: meta, target: t, prev: prev, start: now, generation: generation}, result, false, nil
}

// attempt runs one sync. The configuration lock is held to read and to record, never while
// pushing: a slow remote must not block configuration changes.
func (m *Mirrors) attempt(ctx context.Context, repo domain.RepoName, req mirrorReq) (ports.MirrorSyncResult, error) {
	pl, result, done, err := m.plan(ctx, repo, req)
	if done || err != nil {
		return result, err
	}
	t := pl.target
	// A missing sealer or an unreadable key fails the attempt before any repository I/O.
	key, runErr := m.privateKey(ctx)
	var refs []domain.Ref
	var selected []domain.MirrorRef
	if runErr == nil {
		refs, runErr = m.cfg.Reader.ListRefs(ctx, repo)
	}
	if runErr == nil {
		var branch string
		branch, _, runErr = m.cfg.Git.DefaultBranch(ctx, repo)
		if runErr == nil {
			selected = domain.MirrorRefspecs(refs, branch, t.AllBranches)
		}
	}
	noOp := false
	if runErr == nil && !req.force {
		snapshot, e := m.cfg.Store.GetSnapshot(ctx, repo)
		if e != nil && !errors.Is(e, domain.ErrNotFound) {
			return ports.MirrorSyncResult{}, e
		}
		noOp = e == nil && snapshot.Equal(t.URL, selected, t.AllBranches)
	}
	if runErr == nil && !noOp {
		specs := make([]string, len(selected))
		for i, ref := range selected {
			specs[i] = ref.Name + ":" + ref.Name
		}
		// An empty ref list must not invoke git's implicit default push behavior.
		if len(specs) > 0 {
			_, runErr = m.cfg.Pusher.Push(ctx, ports.MirrorPush{Repo: repo, URL: t.URL, Refspecs: specs, PrivateKey: key})
		}
	}
	if ctx.Err() != nil {
		return ports.MirrorSyncResult{}, ctx.Err()
	}
	var failure *domain.MirrorError
	if runErr != nil && !errors.As(runErr, &failure) {
		failure = &domain.MirrorError{Class: domain.MirrorClassInternal, Detail: "local mirror operation failed"}
		runErr = failure
	}
	return m.record(ctx, repo, pl, selected, noOp, failure, runErr)
}

// record stores the outcome, unless the effective target changed during the push: the result
// then describes a target that no longer applies and is discarded. The comparison includes the
// cron on purpose, like resetIfChanged: a schedule change during a push costs one extra push.
func (m *Mirrors) record(ctx context.Context, repo domain.RepoName, pl mirrorPlan, selected []domain.MirrorRef, noOp bool, failure *domain.MirrorError, runErr error) (ports.MirrorSyncResult, error) {
	m.configMu.RLock()
	defer m.configMu.RUnlock()
	t, err := m.target(ctx, repo)
	if err != nil {
		return ports.MirrorSyncResult{}, err
	}
	if !sameMirrorTarget(pl.target, t) {
		s, err := m.status(ctx, repo)
		return ports.MirrorSyncResult{Status: s}, err
	}
	// The push may be slow: completion, retry and success times come from the
	// clock after it returns; only LastAttemptAt marks when the attempt began.
	now := m.cfg.Clock.Now().UTC()
	transition := domain.ApplyMirrorOutcome(pl.prev, domain.MirrorOutcome{Err: failure, NoOp: noOp}, now)
	transition.Status.LastAttemptAt = pl.start
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
		g := m.gens[repo]
		g.clean, g.cleanKnown = pl.generation, true
		m.gens[repo] = g
		m.mu.Unlock()
	}
	if m.cfg.Bus != nil {
		if transition.FailedEvent {
			m.cfg.Bus.Publish(ctx, domain.MirrorFailed{Repo: repo, State: transition.Status.State, Class: transition.Status.Class, At: now, Visibility: pl.meta.Visibility, CreatedAt: pl.meta.CreatedAt})
		}
		if transition.Recovered {
			m.cfg.Bus.Publish(ctx, domain.MirrorRecovered{Repo: repo, At: now, Visibility: pl.meta.Visibility, CreatedAt: pl.meta.CreatedAt})
		}
	}
	return ports.MirrorSyncResult{Status: transition.Status, NoOp: noOp}, runErr
}
