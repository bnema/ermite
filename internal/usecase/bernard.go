// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

const (
	// maxDepsManifestBytes bounds a manifest Bernard reads.
	maxDepsManifestBytes = 1 << 20
	// depsCheckTimeout bounds a synchronous check of one repository.
	depsCheckTimeout = 60 * time.Second
	// defaultBernardTick is how often the daily schedule looks at the clock.
	defaultBernardTick = time.Minute
)

// BernardConfig configures Bernard.
type BernardConfig struct {
	Store      ports.DepsStore
	Meta       ports.RepoMetaStore
	Reader     ports.RepoReader
	Ecosystems []ports.DepsEcosystem
	// Registry returns a fresh registry, with its own cache, for each run.
	Registry func() ports.DepsRegistry
	Bus      ports.EventBus
	Clock    ports.Clock
	// CheckAt is the cron expression (UTC) of the daily check.
	CheckAt string
	// Cooldown is how old a version must be to be proposed.
	Cooldown time.Duration
	// Snooze is how long a snooze hides a repository.
	Snooze time.Duration
	// JobTimeout is the deadline of a runner job from its assignment; ten
	// minutes when zero.
	JobTimeout time.Duration
	// RevokeJob deletes the repository token of a deps job that left the
	// queue; may be nil.
	RevokeJob func(ctx context.Context, jobID string)
	// Tick is how often the schedule looks at the clock; one minute when zero.
	Tick time.Duration

	// The ports below are for Open and may be nil in a Bernard that only checks.

	// Repos gives the default branch.
	Repos ports.GitRepos
	// Git reads the branches and writes the tree, the commit and the branch.
	Git ports.MergeGit
	// Signer signs Bernard's commits with the instance key.
	Signer ports.CommitSigner
	// Committer is the instance identity.
	Committer domain.Identity
	// Tips records who pushes bernard/deps; may be nil.
	Tips ports.CIRefTipStore
	// CI starts the runs of a push; may be nil.
	CI ports.WorkflowTrigger
	// MRs opens the merge request as Bernard.
	MRs ports.MergeRequestService
	// MRStore tells whether the stored merge request is still open.
	MRStore ports.MergeRequestStore
}

// Bernard checks the dependencies of the repositories every day and keeps the
// pending updates. It implements ports.DepsService.
type Bernard struct {
	cfg   BernardConfig
	cron  domain.CronSchedule
	ctx   context.Context
	stop  context.CancelFunc
	wg    sync.WaitGroup
	start sync.Once

	mu     sync.Mutex
	closed bool

	runMu sync.Mutex // serialises ScheduleNow
	last  time.Time  // last minute looked at

	jobMu    sync.Mutex // guards jobs and jobOrder (bernard_jobs.go)
	jobs     map[string]*depsJob
	jobOrder []string

	lockMu sync.Mutex
	locks  map[domain.RepoName]*sync.Mutex // one lock per repository, for Open and the merge request events
}

var _ ports.DepsService = (*Bernard)(nil)

// NewBernard builds Bernard. It returns domain.ErrInvalid for a missing port or
// a bad cron expression.
func NewBernard(cfg BernardConfig) (*Bernard, error) {
	if cfg.Store == nil || cfg.Meta == nil || cfg.Reader == nil || cfg.Registry == nil || cfg.Bus == nil || cfg.Clock == nil {
		return nil, fmt.Errorf("%w: bernard needs a store, meta, reader, registry, bus and clock", domain.ErrInvalid)
	}
	cron, err := domain.ParseCron(cfg.CheckAt)
	if err != nil {
		return nil, fmt.Errorf("bernard check schedule: %w", err)
	}
	if cfg.Tick <= 0 {
		cfg.Tick = defaultBernardTick
	}
	if cfg.JobTimeout <= 0 {
		cfg.JobTimeout = defaultDepsJobTimeout
	}
	ctx, stop := context.WithCancel(context.Background())
	return &Bernard{cfg: cfg, cron: domain.CronSchedule{Cron: cron, Loc: time.UTC}, ctx: ctx, stop: stop}, nil
}

// Start starts the daily schedule, once.
func (b *Bernard) Start() {
	b.start.Do(func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.closed {
			return
		}
		sub := b.cfg.Bus.Subscribe(b.ctx) // before returning: no event is missed
		b.failInterrupted(b.ctx)          // the queue is in memory: applying records lost their job
		b.wg.Add(3)
		go b.loop()
		go b.expiryLoop()
		go b.watchMergeRequests(sub)
	})
}

// Close stops the schedule and waits for it until ctx ends.
func (b *Bernard) Close(ctx context.Context) {
	b.mu.Lock()
	b.closed = true
	b.stop()
	b.mu.Unlock()
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-ctx.Done():
	case <-done:
	}
}

func (b *Bernard) loop() {
	defer b.wg.Done()
	t := time.NewTicker(b.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-t.C:
		}
		b.ScheduleNow(b.ctx)
	}
}

// expiryLoop fails the jobs past their deadline on its own ticker, so a long
// scheduled scan does not delay it.
func (b *Bernard) expiryLoop() {
	defer b.wg.Done()
	t := time.NewTicker(b.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-t.C:
		}
		b.expireJobs(b.ctx)
	}
}

// ScheduleNow looks at the clock once: when a new minute is due by the cron
// expression it checks every repository, one after the other, with one
// registry. A panic is logged, not propagated. It is for the schedule and
// tests.
func (b *Bernard) ScheduleNow(ctx context.Context) {
	log := ctxLog(ctx)
	defer func() {
		if r := recover(); r != nil {
			log.Error().Interface("panic", r).Bytes("stack", debug.Stack()).Msg("bernard scheduler panicked")
		}
	}()
	b.runMu.Lock()
	defer b.runMu.Unlock()
	minute := b.cfg.Clock.Now().UTC().Truncate(time.Minute)
	if !minute.After(b.last) {
		return
	}
	b.last = minute
	if !b.cron.Due(minute) {
		return
	}
	repos, err := b.cfg.Meta.List(ctx)
	if err != nil {
		log.Error().Err(err).Msg("bernard: list the repositories")
		return
	}
	reg := b.cfg.Registry()
	for _, r := range repos {
		if ctx.Err() != nil {
			return
		}
		if r.Archived {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, depsCheckTimeout)
		_, err := b.check(rctx, reg, r.Name)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Error().Err(err).Str("repo", r.Name.String()).Msg("bernard check failed")
		}
	}
}

// check looks for updates in one repository and stores them. An archived
// repository is skipped and its stored record returned.
func (b *Bernard) check(ctx context.Context, reg ports.DepsRegistry, name domain.RepoName) (domain.RepoDeps, error) {
	repo, err := lookupRepo(ctx, b.cfg.Meta, name)
	if err != nil {
		return domain.RepoDeps{}, err
	}
	if repo.Archived {
		return b.cfg.Store.Get(ctx, name)
	}
	ups, err := b.detect(ctx, reg, name)
	if err != nil {
		return domain.RepoDeps{}, err
	}
	slices.SortFunc(ups, func(a, c domain.DepUpdate) int {
		return cmpStrings(string(a.Ecosystem), string(c.Ecosystem), a.Directory, c.Directory, a.Name, c.Name)
	})
	// The result is installed under the repository lock, with the ignores as
	// they are now: an Ignore that ran during the detection is not undone.
	defer b.repoLock(name)()
	kept := ups[:0:0]
	for _, u := range ups {
		ignored, err := b.cfg.Store.IsIgnored(ctx, name, u.DepRef)
		if err != nil {
			return domain.RepoDeps{}, fmt.Errorf("ignored %s@%s: %w", u.Name, u.Version, err)
		}
		if !ignored {
			kept = append(kept, u)
		}
	}
	ups = kept
	now := b.cfg.Clock.Now()
	fresh := 0
	moved := false
	rec, err := b.cfg.Store.Mutate(ctx, name, func(d *domain.RepoDeps) (bool, error) {
		moved = !domain.SameRefs(d.Refs(), refsOf(ups))
		prev := make(map[domain.DepRef]bool, len(d.Updates))
		for _, u := range d.Updates {
			prev[u.DepRef] = true
		}
		fresh = 0
		next := make([]domain.DepUpdate, len(ups))
		for i, u := range ups {
			u.New = !prev[u.DepRef]
			if u.New {
				fresh++
			}
			next[i] = u
		}
		d.Repo = name
		d.Updates = next
		d.CheckedAt = now
		return true, nil
	})
	if err != nil {
		return domain.RepoDeps{}, fmt.Errorf("store the updates: %w", err)
	}
	if fresh > 0 {
		b.cfg.Bus.Publish(ctx, domain.DependencyUpdatesFound{Repo: name, Count: fresh, At: now})
	}
	if moved {
		b.changed(ctx, name)
	}
	return rec, nil
}

// changed tells the owner's live view that the record of name changed. It is
// called after the store mutation, never inside it.
func (b *Bernard) changed(ctx context.Context, name domain.RepoName) {
	b.cfg.Bus.Publish(context.WithoutCancel(ctx), domain.DependencyUpdatesChanged{Repo: name, At: b.cfg.Clock.Now()})
}

func refsOf(ups []domain.DepUpdate) []domain.DepRef {
	refs := make([]domain.DepRef, len(ups))
	for i, u := range ups {
		refs[i] = u.DepRef
	}
	return refs
}

func cmpStrings(pairs ...string) int {
	for i := 0; i+1 < len(pairs); i += 2 {
		if c := strings.Compare(pairs[i], pairs[i+1]); c != 0 {
			return c
		}
	}
	return 0
}

// detect returns the updates to propose at the tip of the default branch.
func (b *Bernard) detect(ctx context.Context, reg ports.DepsRegistry, name domain.RepoName) ([]domain.DepUpdate, error) {
	log := ctxLog(ctx)
	res, err := b.cfg.Reader.ResolveRevision(ctx, name, domain.Revision{})
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil // no repository directory, no default branch: nothing to check
	}
	if err != nil {
		return nil, fmt.Errorf("resolve the default branch: %w", err)
	}
	rev := domain.RevisionOf(res.CommitID)
	paths, err := b.cfg.Reader.ListPaths(ctx, name, rev, domain.MaxPathEntries, domain.MaxPathBytes)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list the paths: %w", err)
	}
	now := b.cfg.Clock.Now()
	var cbErr error // first error of the PickUpdate callbacks
	var out []domain.DepUpdate
	for _, eco := range b.cfg.Ecosystems {
		files := map[domain.TreePath][]byte{}
		for _, tp := range eco.Manifests(paths) {
			content, ok, err := b.readManifest(ctx, name, rev, tp)
			if err != nil {
				return nil, err
			}
			if !ok {
				log.Warn().Str("repo", name.String()).Str("path", tp.String()).Msg("bernard: manifest larger than 1 MiB skipped")
				continue
			}
			files[tp] = content
		}
		if len(files) == 0 {
			continue
		}
		cands, err := eco.Detect(ctx, files, reg)
		if err != nil {
			return nil, fmt.Errorf("detect %s dependencies: %w", eco.Name(), err)
		}
		for _, c := range cands {
			firstSeen := func(v domain.DepVersion) time.Time {
				at, err := b.cfg.Store.FirstSeen(ctx, c.Ecosystem, c.Name, v.Version, v.SHA, now)
				if err != nil {
					if cbErr == nil {
						cbErr = fmt.Errorf("first seen of %s@%s: %w", c.Name, v.Version, err)
					}
					return time.Time{}
				}
				return at
			}
			ignored := func(ref domain.DepRef) bool {
				ok, err := b.cfg.Store.IsIgnored(ctx, name, ref)
				if err != nil {
					if cbErr == nil {
						cbErr = fmt.Errorf("ignored %s@%s: %w", ref.Name, ref.Version, err)
					}
					return true
				}
				return ok
			}
			up, ok := domain.PickUpdate(c, now, b.cfg.Cooldown, firstSeen, ignored)
			if cbErr != nil {
				return nil, cbErr
			}
			if !ok {
				continue
			}
			mal, err := reg.Malicious(ctx, up.Ecosystem, up.Name, up.Version)
			if err != nil {
				log.Warn().Err(err).Str("repo", name.String()).Str("name", up.Name).Str("version", up.Version).Msg("bernard: advisory lookup failed, update skipped")
				continue
			}
			if mal {
				continue
			}
			out = append(out, up)
		}
	}
	return out, nil
}

// readManifest reads a file; ok is false when it is larger than the limit.
func (b *Bernard) readManifest(ctx context.Context, name domain.RepoName, rev domain.Revision, tp domain.TreePath) ([]byte, bool, error) {
	info, rc, err := b.cfg.Reader.OpenBlob(ctx, name, rev, tp)
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", tp, err)
	}
	defer func() { _ = rc.Close() }()
	if info.Size > maxDepsManifestBytes {
		return nil, false, nil
	}
	data, err := io.ReadAll(io.LimitReader(rc, maxDepsManifestBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", tp, err)
	}
	if len(data) > maxDepsManifestBytes {
		return nil, false, nil
	}
	return data, true, nil
}

// List returns the records with pending updates, or only repo's.
func (b *Bernard) List(ctx context.Context, p domain.Principal, repo domain.RepoName) ([]domain.RepoDeps, error) {
	if !p.HasOwnerRights() {
		return nil, domain.ErrNotFound
	}
	all, err := b.cfg.Store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list the dependency records: %w", err)
	}
	var out []domain.RepoDeps
	for _, d := range all {
		if len(d.Updates) == 0 || (!repo.IsZero() && d.Repo != repo) {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// Check runs the check of one repository now and returns its stored record.
func (b *Bernard) Check(ctx context.Context, p domain.Principal, repo domain.RepoName) (domain.RepoDeps, error) {
	if !p.HasOwnerRights() {
		return domain.RepoDeps{}, domain.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, depsCheckTimeout)
	defer cancel()
	return b.check(ctx, b.cfg.Registry(), repo)
}

// Ignore hides exactly the expected versions from now on.
func (b *Bernard) Ignore(ctx context.Context, p domain.Principal, repo domain.RepoName, expected []domain.DepRef) (domain.RepoDeps, error) {
	if !p.HasOwnerRights() {
		return domain.RepoDeps{}, domain.ErrNotFound
	}
	if _, err := lookupRepo(ctx, b.cfg.Meta, repo); err != nil {
		return domain.RepoDeps{}, err
	}
	defer b.repoLock(repo)()
	d, err := b.cfg.Store.Get(ctx, repo)
	if err != nil {
		return domain.RepoDeps{}, fmt.Errorf("read the dependency record: %w", err)
	}
	if err := depsPrecondition(d, expected); err != nil {
		return domain.RepoDeps{}, err
	}
	if err := b.cfg.Store.Ignore(ctx, repo, expected); err != nil {
		return domain.RepoDeps{}, fmt.Errorf("ignore the updates: %w", err)
	}
	drop := make(map[domain.DepRef]bool, len(expected))
	for _, r := range expected {
		drop[r] = true
	}
	rec, err := b.cfg.Store.Mutate(ctx, repo, func(d *domain.RepoDeps) (bool, error) {
		if err := depsPrecondition(*d, expected); err != nil {
			return false, err // the record moved since it was read
		}
		d.Updates = slices.DeleteFunc(slices.Clone(d.Updates), func(u domain.DepUpdate) bool { return drop[u.DepRef] })
		return true, nil
	})
	if err != nil {
		return domain.RepoDeps{}, fmt.Errorf("remove the ignored updates: %w", err)
	}
	b.changed(ctx, repo)
	return rec, nil
}

// depsPrecondition checks that the client saw the pending versions and that no
// job is applying them.
func depsPrecondition(d domain.RepoDeps, expected []domain.DepRef) error {
	if d.State == domain.DepsApplying {
		return fmt.Errorf("%w: dependency updates are being applied", domain.ErrPreconditionFailed)
	}
	if !domain.SameRefs(expected, d.Refs()) {
		return fmt.Errorf("%w: the pending dependency updates changed, reload and retry", domain.ErrPreconditionFailed)
	}
	return nil
}

// Snooze hides the repository's updates for the configured time.
func (b *Bernard) Snooze(ctx context.Context, p domain.Principal, repo domain.RepoName) (domain.RepoDeps, error) {
	if !p.HasOwnerRights() {
		return domain.RepoDeps{}, domain.ErrNotFound
	}
	if _, err := lookupRepo(ctx, b.cfg.Meta, repo); err != nil {
		return domain.RepoDeps{}, err
	}
	defer b.repoLock(repo)()
	until := b.cfg.Clock.Now().Add(b.cfg.Snooze)
	rec, err := b.cfg.Store.Mutate(ctx, repo, func(d *domain.RepoDeps) (bool, error) {
		d.Repo = repo
		d.SnoozedUntil = until
		return true, nil
	})
	if err != nil {
		return domain.RepoDeps{}, fmt.Errorf("snooze: %w", err)
	}
	b.changed(ctx, repo)
	return rec, nil
}
