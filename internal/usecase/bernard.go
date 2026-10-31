// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"golang.org/x/mod/semver"
)

const (
	// maxDepsManifestBytes bounds a manifest Bernard reads.
	maxDepsManifestBytes = 1 << 20
	// depsCheckTimeout bounds a synchronous check of one repository and the
	// check of a runner report when its job completes.
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
	// JobTimeout is the time budget of a dependency job once a runner has it
	// (rounded up to the minute); ten minutes when zero. The wait for a runner
	// is CI's queue timeout.
	JobTimeout time.Duration
	// Runs starts and cancels the CI runs of the dependency jobs; may be nil
	// in a Bernard that does not open merge requests.
	Runs ports.DepsRunner
	// Runners lists registered runner capabilities for Open; required with Runs.
	Runners ports.RunnerLister
	// RunnerLabel is the runner label that serves the dependency jobs.
	RunnerLabel string
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
	cfg.RunnerLabel = strings.TrimSpace(cfg.RunnerLabel)
	if cfg.Runs != nil && cfg.RunnerLabel == "" {
		return nil, fmt.Errorf("%w: bernard needs a runner label for its dependency jobs", domain.ErrInvalid)
	}
	if cfg.Runs != nil && cfg.Runners == nil {
		return nil, fmt.Errorf("%w: bernard needs the runner list for its dependency jobs", domain.ErrInvalid)
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
		b.reconcileRuns(b.ctx, 0)         // the runs are the truth: settle the records left applying
		b.wg.Add(2)
		go b.loop()
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
	b.reconcileRuns(ctx, depsReconcileGrace) // a run that ended unseen would keep its record applying for ever
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
		base, majors := domain.SplitBatches(ups)
		next := []domain.DepBatch{{Updates: base}}
		for _, u := range majors {
			next = append(next, domain.DepBatch{Key: domain.MajorBatchKey(u), Updates: []domain.DepUpdate{u}})
		}
		fresh, moved = 0, false
		for i := range next {
			batch := &next[i]
			old, _, exists := d.Batch(batch.Key)
			if !domain.SameRefs(old.Refs(), batch.Refs()) {
				moved = true
			}
			updates := batch.Updates
			if exists {
				*batch = old
			}
			batch.Updates = slices.Clone(updates)
			for j := range batch.Updates {
				batch.Updates[j].New = !slices.Contains(old.Refs(), batch.Updates[j].DepRef)
				if batch.Updates[j].New {
					fresh++
				}
			}
		}
		for _, old := range d.Batches {
			if slices.ContainsFunc(next, func(batch domain.DepBatch) bool { return batch.Key == old.Key }) {
				continue
			}
			if old.MR != 0 || old.State == domain.DepsApplying {
				// Kept for its merge request or run: a change only when it
				// loses its updates.
				moved = moved || len(old.Updates) != 0
				old.Updates = nil
				next = append(next, old)
			} else {
				moved = true // dropped
			}
		}
		slices.SortFunc(next[1:], func(a, b domain.DepBatch) int { return strings.Compare(string(a.Key), string(b.Key)) })
		d.Repo, d.Batches = name, next
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
	var cbErr error // first error of the PickUpdates callbacks
	var out []domain.DepUpdate
	for _, eco := range b.cfg.Ecosystems {
		files := map[domain.TreePath][]byte{}
		for _, in := range eco.Manifests(paths) {
			in.Optional = true // the listing and the read are not atomic
			more, _, err := b.readInputs(ctx, name, rev, []ports.DepsInput{in}, nil)
			if errors.Is(err, errDepsOversize) {
				log.Warn().Str("repo", name.String()).Str("path", in.Path.String()).Msg("bernard: manifest larger than 1 MiB skipped")
				continue
			}
			if err != nil {
				return nil, err
			}
			maps.Copy(files, more)
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
				at, err := b.cfg.Store.FirstSeen(ctx, c.Ecosystem, versionName(c.Name, v.Path), v.Version, v.SHA, now)
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
			ups := domain.PickUpdates(c, now, b.cfg.Cooldown, firstSeen, ignored)
			if cbErr != nil {
				return nil, cbErr
			}

			// Fit same-major and cross-major candidates independently, preserving
			// fallback to an older fitting version in each lane.
			for _, major := range []bool{false, true} {
				lane := slices.DeleteFunc(slices.Clone(ups), func(u domain.DepUpdate) bool { return (semver.Major(u.Version) != semver.Major(u.Current)) != major })
				up, ok, err := fittingUpdate(ctx, reg, name, c, lane)
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
				mal, err := reg.Malicious(ctx, up.Ecosystem, versionName(up.Name, up.NewName), up.Version)
				if err != nil {
					log.Warn().Err(err).Str("repo", name.String()).Str("name", up.Name).Str("version", up.Version).Msg("bernard: advisory lookup failed, update skipped")
					continue
				}
				if !mal {
					out = append(out, up)
				}
			}

		}
	}
	return out, nil
}

// fittingUpdate returns the first of ups, highest first, that can be applied:
// for a Go module, the first whose go.mod go directive fits the one of the
// module in c.Directory. A version whose go.mod cannot be read is skipped; a done ctx is an error.
func fittingUpdate(ctx context.Context, reg ports.DepsRegistry, name domain.RepoName, c domain.DepCandidate, ups []domain.DepUpdate) (domain.DepUpdate, bool, error) {
	if c.Ecosystem != domain.EcosystemGoMod {
		if len(ups) == 0 {
			return domain.DepUpdate{}, false, nil
		}
		return ups[0], true, nil
	}
	for _, up := range ups {
		if err := ctx.Err(); err != nil {
			return domain.DepUpdate{}, false, err
		}
		dep, err := reg.GoDirective(ctx, versionName(up.Name, up.NewName), up.Version)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return domain.DepUpdate{}, false, cerr
			}
			ctxLog(ctx).Warn().Err(err).Str("repo", name.String()).Str("name", up.Name).Str("version", up.Version).Msg("bernard: go.mod lookup failed, version skipped")
			continue
		}
		if domain.GoDirectiveFits(dep, c.GoDirective) {
			return up, true, nil
		}
	}
	return domain.DepUpdate{}, false, nil
}

// errDepsOversize marks a full input larger than its limit.
var errDepsOversize = errors.New("dependency file too large")

// readInputs reads the inputs of one ecosystem and the original content of
// the changed report paths. Full inputs may be up to maxDepsManifestBytes;
// header inputs hold at most ports.DepsHeaderBytes. Changed paths are
// optional (new files) and read in full up to maxReportFileBytes; beyond, an
// opaque one (not a full input) keeps only its header. A path requested
// several ways is read the most complete way. partial lists the paths whose
// content is only a header. Absent optional inputs are left out.
// Errors: domain.ErrNotFound (absent required input), errDepsOversize, or the
// read failure.
func (b *Bernard) readInputs(ctx context.Context, name domain.RepoName, rev domain.Revision, inputs []ports.DepsInput, changed []domain.TreePath) (files map[domain.TreePath][]byte, partial map[domain.TreePath]bool, err error) {
	type want struct{ optional, header, full, changed bool }
	wants := map[domain.TreePath]*want{}
	var order []domain.TreePath
	add := func(tp domain.TreePath, w want) {
		prev, ok := wants[tp]
		if !ok {
			wants[tp] = &w
			order = append(order, tp)
			return
		}
		prev.optional = prev.optional && w.optional
		prev.header = prev.header && w.header
		prev.full = prev.full || w.full
		prev.changed = prev.changed || w.changed
	}
	for _, in := range inputs {
		add(in.Path, want{optional: in.Optional, header: in.Header, full: !in.Header})
	}
	for _, tp := range changed {
		add(tp, want{optional: true, changed: true})
	}
	files, partial = map[domain.TreePath][]byte{}, map[domain.TreePath]bool{}
	for _, tp := range order {
		w := wants[tp]
		var content []byte
		var ok bool
		switch {
		case w.changed:
			content, ok, err = b.readDepsFile(ctx, name, rev, tp, maxReportFileBytes)
			if err == nil && !ok {
				if w.full {
					err = fmt.Errorf("%w: %s is larger than %d bytes", errDepsOversize, tp, maxReportFileBytes)
				} else {
					content, err = b.readDepsHeader(ctx, name, rev, tp)
					partial[tp] = true
				}
			}
		case w.header:
			content, err = b.readDepsHeader(ctx, name, rev, tp)
			partial[tp] = true
		default:
			content, ok, err = b.readDepsFile(ctx, name, rev, tp, maxDepsManifestBytes)
			if err == nil && !ok {
				err = fmt.Errorf("%w: %s is larger than %d bytes", errDepsOversize, tp, maxDepsManifestBytes)
			}
		}
		if errors.Is(err, domain.ErrNotFound) && w.optional {
			delete(partial, tp)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		files[tp] = content
	}
	return files, partial, nil
}

// readDepsHeader reads at most ports.DepsHeaderBytes of a file of any size.
func (b *Bernard) readDepsHeader(ctx context.Context, name domain.RepoName, rev domain.Revision, tp domain.TreePath) ([]byte, error) {
	_, rc, err := b.cfg.Reader.OpenBlob(ctx, name, rev, tp)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", tp, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, ports.DepsHeaderBytes))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", tp, err)
	}
	return data, nil
}

// readManifest reads a file; ok is false when it is larger than the limit.
func (b *Bernard) readManifest(ctx context.Context, name domain.RepoName, rev domain.Revision, tp domain.TreePath) ([]byte, bool, error) {
	return b.readDepsFile(ctx, name, rev, tp, maxDepsManifestBytes)
}

func (b *Bernard) readDepsFile(ctx context.Context, name domain.RepoName, rev domain.Revision, tp domain.TreePath, limit int) ([]byte, bool, error) {
	info, rc, err := b.cfg.Reader.OpenBlob(ctx, name, rev, tp)
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", tp, err)
	}
	defer func() { _ = rc.Close() }()
	if info.Size > int64(limit) {
		return nil, false, nil
	}
	data, err := io.ReadAll(io.LimitReader(rc, int64(limit)+1))
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", tp, err)
	}
	if len(data) > limit {
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
		if !slices.ContainsFunc(d.Batches, domain.DepBatch.Visible) || (!repo.IsZero() && d.Repo != repo) {
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
func (b *Bernard) Ignore(ctx context.Context, p domain.Principal, repo domain.RepoName, key domain.DepBatchKey, expected []domain.DepRef) (domain.RepoDeps, error) {
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
	if err := depsPrecondition(d, key, expected); err != nil {
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
		if err := depsPrecondition(*d, key, expected); err != nil {
			return false, err // the record moved since it was read
		}
		_, i, _ := d.Batch(key)
		batch := &d.Batches[i]
		batch.Updates = slices.DeleteFunc(slices.Clone(batch.Updates), func(u domain.DepUpdate) bool { return drop[u.DepRef] })
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
func depsPrecondition(d domain.RepoDeps, key domain.DepBatchKey, expected []domain.DepRef) error {
	batch, _, ok := d.Batch(key)
	if !ok {
		return domain.ErrNotFound
	}
	if batch.State == domain.DepsApplying {
		return fmt.Errorf("%w: dependency updates are being applied", domain.ErrPreconditionFailed)
	}
	if !domain.SameRefs(expected, batch.Refs()) {
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

func versionName(name, newName string) string {
	if newName != "" && newName != name {
		return newName
	}
	return name
}
