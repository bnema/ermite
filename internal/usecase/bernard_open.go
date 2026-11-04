// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

const (
	// maxDepsError bounds the error kept on a failed record.
	maxDepsError = 2 << 10
)

// repoLock takes the lock of a repository and returns its release.
func (b *Bernard) repoLock(name domain.RepoName) func() {
	b.lockMu.Lock()
	if b.locks == nil {
		b.locks = map[domain.RepoName]*sync.Mutex{}
	}
	mu := b.locks[name]
	if mu == nil {
		mu = &sync.Mutex{}
		b.locks[name] = mu
	}
	b.lockMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

func (b *Bernard) canOpen() bool {
	c := b.cfg
	return c.Git != nil && c.Repos != nil && c.Signer != nil && c.MRs != nil && c.MRStore != nil
}

// Open opens the Bernard merge request for the expected updates, or rebuilds
// its branch from the target when it is open already. The updates of an
// ecosystem that cannot be applied in the server (Go modules) go to a runner
// as a job: the record is Applying until the runner reports (applyRunnerReport).
// The others are applied in the server, at once or when the report arrives.
func (b *Bernard) Open(ctx context.Context, p domain.Principal, repoName domain.RepoName, key domain.DepBatchKey, expected []domain.DepRef) (domain.RepoDeps, error) {
	if !p.HasOwnerRights() {
		return domain.RepoDeps{}, domain.ErrNotFound
	}
	if !b.canOpen() {
		return domain.RepoDeps{}, fmt.Errorf("%w: opening dependency updates is not available", domain.ErrPreconditionFailed)
	}
	repo, err := lookupRepo(ctx, b.cfg.Meta, repoName)
	if err != nil {
		return domain.RepoDeps{}, err
	}
	if repo.Archived {
		return domain.RepoDeps{}, domain.ErrArchived
	}
	defer b.repoLock(repoName)()
	d, err := b.cfg.Store.Get(ctx, repoName)
	if err != nil {
		return domain.RepoDeps{}, fmt.Errorf("read the dependency record: %w", err)
	}
	if err := depsPrecondition(d, key, expected); err != nil {
		return domain.RepoDeps{}, err
	}
	batch, _, _ := d.Batch(key)
	if len(batch.Updates) == 0 {
		return domain.RepoDeps{}, fmt.Errorf("%w: no dependency update is pending", domain.ErrPreconditionFailed)
	}
	if batch.Adopted {
		return domain.RepoDeps{}, errBranchAdopted
	}
	if batch.Queued() {
		return domain.RepoDeps{}, fmt.Errorf("%w: the dependency updates are queued already", domain.ErrPreconditionFailed)
	}
	for _, u := range batch.Updates {
		if b.ecosystem(u.Ecosystem) == nil {
			return domain.RepoDeps{}, fmt.Errorf("%w: unknown ecosystem %q", domain.ErrPreconditionFailed, u.Ecosystem)
		}
	}
	// The queue does the work (bernard_queue.go): the versions accepted are
	// kept so that it can tell whether they changed meanwhile.
	now := b.cfg.Clock.Now()
	rec, err := b.cfg.Store.Mutate(ctx, repoName, func(r *domain.RepoDeps) (bool, error) {
		if err := depsPrecondition(*r, key, expected); err != nil {
			return false, err
		}
		_, i, _ := r.Batch(key)
		stored := &r.Batches[i]
		if stored.Queued() {
			return false, fmt.Errorf("%w: the dependency updates are queued already", domain.ErrPreconditionFailed)
		}
		stored.State, stored.QueuedAt, stored.Manual = domain.DepsQueued, now, true
		stored.Error, stored.Repro, stored.RunUpdates = "", nil, slices.Clone(stored.Updates)
		return true, nil
	})
	if err != nil {
		return domain.RepoDeps{}, fmt.Errorf("queue the dependency updates: %w", err)
	}
	b.changed(ctx, repoName)
	b.wakeQueue()
	return rec, nil
}

// markAdopted records that a human changed the branch of the batch: Bernard
// never rewrites it again, and its merge request body says so. idle ends a
// draft in progress (a run still applying finishes and only refreshes the
// body). The caller holds the repository lock.
func (b *Bernard) markAdopted(ctx context.Context, name domain.RepoName, key domain.DepBatchKey, idle bool) {
	ctx = context.WithoutCancel(ctx)
	rec, err := b.cfg.Store.Mutate(ctx, name, func(d *domain.RepoDeps) (bool, error) {
		_, i, ok := d.Batch(key)
		if !ok {
			return false, nil
		}
		stored := &d.Batches[i]
		stored.Adopted = true
		stored.Pending, stored.PendingPrev = domain.ObjectID{}, domain.ObjectID{}
		if idle && stored.State != domain.DepsApplying {
			stored.State, stored.Error, stored.Repro = domain.DepsIdle, "", nil
		}
		return true, nil
	})
	if err != nil {
		ctxLog(ctx).Error().Err(err).Str("repo", name.String()).Msg("bernard: record the adoption of the branch")
		return
	}
	if batch, _, ok := rec.Batch(key); ok {
		b.noteAdopted(ctx, name, batch)
	}
	b.changed(ctx, name)
}

// noteAdopted refreshes only the description of the merge request of an
// adopted batch: its title and its branch are the human's.
func (b *Bernard) noteAdopted(ctx context.Context, name domain.RepoName, batch domain.DepBatch) {
	if batch.MR == 0 || !b.canOpen() {
		return
	}
	mr, err := b.cfg.MRStore.Get(ctx, name, batch.MR)
	if err != nil || !mr.IsOpen() {
		return
	}
	ups := batch.MRUpdates
	if len(ups) == 0 {
		ups = batch.Updates
	}
	desc := domain.BotMRBodyFor(batch, ups, nil)
	if mr.Description.String() == desc {
		return
	}
	if _, err := b.cfg.MRs.Edit(ctx, domain.Bot(name), name, batch.MR, ports.EditMergeRequestParams{Description: &desc}); err != nil {
		ctxLog(ctx).Error().Err(err).Str("repo", name.String()).Int64("mr", batch.MR).Msg("bernard: note the adoption on the merge request")
	}
}

// Held implements ports.CIHold: every draft head Bernard pushed on a batch
// branch stays held whatever the batch became since (CI plans late: a failed
// or replaced draft never starts its runs). Only draft heads are held: a final
// commit never is, whatever the state of the record when CI plans it. An
// adopted branch holds nothing. It reads the record without the repository lock: CI asks
// while Bernard publishes.
func (b *Bernard) Held(ctx context.Context, repo domain.RepoName, branch string, sha domain.ObjectID) bool {
	d, err := b.cfg.Store.Get(ctx, repo)
	if err != nil {
		return false
	}
	for _, batch := range d.Batches {
		if batch.BranchName() != branch || batch.Adopted {
			continue
		}
		if _, ok := batch.Draft(sha); ok {
			return true
		}
	}
	return false
}

var _ ports.CIHold = (*Bernard)(nil)

// onPush marks adopted the batches whose branch a human push moved away from
// the head Bernard wrote. Bernard's own writes publish no PushEvent.
func (b *Bernard) onPush(e domain.PushEvent) {
	ctx := b.ctx
	defer func() {
		if r := recover(); r != nil {
			ctxLog(ctx).Error().Interface("panic", r).Bytes("stack", debug.Stack()).Msg("bernard: push event panicked")
		}
	}()
	var branches []string
	for _, u := range e.Updates {
		branch, ok := strings.CutPrefix(u.Ref, "refs/heads/")
		if ok && u.New != domain.ZeroOID && strings.HasPrefix(branch, "bernard/") {
			branches = append(branches, branch)
		}
	}
	if len(branches) == 0 {
		return
	}
	defer b.repoLock(e.Repo)()
	d, err := b.cfg.Store.Get(ctx, e.Repo)
	if err != nil {
		ctxLog(ctx).Error().Err(err).Str("repo", e.Repo.String()).Msg("bernard: read the record")
		return
	}
	for _, u := range e.Updates {
		for _, batch := range d.Batches {
			if batch.Adopted || batch.MR == 0 || "refs/heads/"+batch.BranchName() != u.Ref || u.New == domain.ZeroOID {
				continue
			}
			// An open request with no head recorded (from before heads were)
			// is adopted by any push: Bernard's own publish no event.
			sha, err := domain.ParseObjectID(u.New)
			switch {
			case err != nil:
			case !batch.Pending.IsZero():
				if _, err := b.settlePending(ctx, e.Repo, batch.Key, &sha, false); err != nil {
					ctxLog(ctx).Error().Err(err).Str("repo", e.Repo.String()).Msg("bernard: settle the pending head")
				}
			case sha != batch.Head:
				b.markAdopted(ctx, e.Repo, batch.Key, false)
			}
		}
	}
}

// errRecordMoved marks a transition refused because the record changed since
// Open read it.
var errRecordMoved = errors.New("the dependency record changed")

func movedPrecondition(d domain.RepoDeps, key domain.DepBatchKey, expected []domain.DepRef) error {
	if err := depsPrecondition(d, key, expected); err != nil {
		return fmt.Errorf("%w: %w", errRecordMoved, err)
	}
	return nil
}

// missingRunnerKind returns "" when one runner with label advertises every
// required kind. Otherwise it names the first required kind no runner with
// label advertises or, when each kind is advertised by some runner but none
// has them all, the first kind the first such runner lacks.
func missingRunnerKind(runners []domain.CIRunner, label string, requires []string) string {
	var labelled []domain.CIRunner
	for _, r := range runners {
		if domain.LabelsMatch([]string{label}, r.Labels) {
			if supportsDeps(r.Kinds, requires) {
				return ""
			}
			labelled = append(labelled, r)
		}
	}
	for _, kind := range requires {
		if !slices.ContainsFunc(labelled, func(r domain.CIRunner) bool { return supportsDeps(r.Kinds, []string{kind}) }) {
			return kind
		}
	}
	for _, kind := range requires {
		if !supportsDeps(labelled[0].Kinds, []string{kind}) {
			return kind
		}
	}
	return requires[0]
}

// runParams checks that a runner can take the runner-side updates of batch and
// returns the parameters of their run at the tip of the default branch.
func (b *Bernard) runParams(ctx context.Context, name domain.RepoName, batch domain.DepBatch) (ports.DepsRunParams, error) {
	cfg := b.cfg
	var err error
	var runnerUps []domain.DepUpdate
	for _, u := range batch.Updates {
		if !b.ecosystem(u.Ecosystem).ApplyInServer() {
			runnerUps = append(runnerUps, u)
		}
	}
	details := make([]domain.CIDepsUpdate, len(runnerUps))
	requires := []string{"deps"}
	for i, u := range runnerUps {
		details[i] = domain.CIDepsUpdate{Ref: u.DepRef, NewName: u.NewName}
		if u.Ecosystem == domain.EcosystemNPM && !slices.Contains(requires, "deps.npm") {
			requires = append(requires, "deps.npm")
		}
		if u.NewName != "" && !slices.Contains(requires, "deps.gomod-path-majors") {
			requires = append(requires, "deps.gomod-path-majors")
		}
	}
	if cfg.Runners == nil {
		return ports.DepsRunParams{}, fmt.Errorf("%w: dependency jobs are not available", domain.ErrPreconditionFailed)
	}
	runners, err := cfg.Runners.ListRunners(ctx)
	if err != nil {
		return ports.DepsRunParams{}, err
	}
	if missing := missingRunnerKind(runners, cfg.RunnerLabel, requires); missing != "" {
		return ports.DepsRunParams{}, fmt.Errorf("%w: no runner supports %s updates", domain.ErrPreconditionFailed, missing)
	}
	target, exists, err := cfg.Repos.DefaultBranch(ctx, name)
	if err != nil {
		return ports.DepsRunParams{}, err
	}
	if !exists {
		return ports.DepsRunParams{}, fmt.Errorf("%w: %s has no default branch", domain.ErrPreconditionFailed, name)
	}
	tip, err := cfg.Git.BranchHead(ctx, name, target)
	if err != nil {
		return ports.DepsRunParams{}, fmt.Errorf("read %s: %w", target, err)
	}
	goVersion, err := b.goVersionOf(ctx, name, tip, runnerUps)
	if err != nil {
		return ports.DepsRunParams{}, err
	}
	if cfg.Runs == nil {
		return ports.DepsRunParams{}, fmt.Errorf("%w: dependency jobs are not available", domain.ErrPreconditionFailed)
	}
	nodeVersion := ""
	for _, eco := range cfg.Ecosystems {
		preparer, ok := eco.(ports.DepsJobPreparer)
		if !ok {
			continue
		}
		var own []domain.DepUpdate
		for _, u := range runnerUps {
			if u.Ecosystem == eco.Name() {
				own = append(own, u)
			}
		}
		if len(own) == 0 {
			continue
		}
		files, _, err := b.readInputs(ctx, name, domain.RevisionOf(tip), eco.Inputs(own, nil), nil)
		if errors.Is(err, domain.ErrNotFound) || errors.Is(err, errDepsOversize) {
			return ports.DepsRunParams{}, fmt.Errorf("%w: read the %s files: %v", domain.ErrPreconditionFailed, eco.Name(), err)
		}
		if err != nil {
			return ports.DepsRunParams{}, err
		}
		more, node, err := preparer.RunnerDetails(files, own)
		if err != nil {
			return ports.DepsRunParams{}, err
		}
		for i, d := range details {
			for _, m := range more {
				if d.Ref == m.Ref {
					details[i] = m
				}
			}
		}
		nodeVersion = node
	}
	return ports.DepsRunParams{
		Repo: name, Ref: "refs/heads/" + target, Sha: tip.String(), Title: domain.BotSubject(batch),
		Job: domain.CIDepsJob{Details: details, GoVersion: goVersion, NodeVersion: nodeVersion, Requires: requires}, Label: cfg.RunnerLabel, Timeout: cfg.JobTimeout,
	}, nil
}

// startRun starts the runner job of the batch key of rec, which holds its
// draft merge request, and marks the batch Applying. It is the seam a run
// queue calls once a slot is free. The caller holds the repository lock.
func (b *Bernard) startRun(ctx context.Context, rec domain.RepoDeps, key domain.DepBatchKey) (domain.RepoDeps, error) {
	batch, _, ok := rec.Batch(key)
	if !ok {
		return domain.RepoDeps{}, errRecordMoved
	}
	params, err := b.runParams(ctx, rec.Repo, batch)
	if err != nil {
		return domain.RepoDeps{}, err
	}
	return b.startRunWith(ctx, rec.Repo, batch, batch.Refs(), params)
}

// startRunWith starts the run of params and marks the batch Applying with it.
func (b *Bernard) startRunWith(ctx context.Context, name domain.RepoName, batch domain.DepBatch, expected []domain.DepRef, params ports.DepsRunParams) (domain.RepoDeps, error) {
	cfg := b.cfg
	run, err := cfg.Runs.StartDepsRun(ctx, params)
	if err != nil {
		return domain.RepoDeps{}, fmt.Errorf("start the dependency run: %w", err)
	}
	snapshot := slices.Clone(batch.Updates)
	rec, err := cfg.Store.Mutate(ctx, name, func(r *domain.RepoDeps) (bool, error) {
		if err := movedPrecondition(*r, batch.Key, expected); err != nil {
			return false, err
		}
		_, i, ok := r.Batch(batch.Key)
		if !ok {
			return false, errRecordMoved
		}
		stored := &r.Batches[i]
		stored.State, stored.RunID, stored.RunUpdates, stored.Error, stored.Repro = domain.DepsApplying, run.ID, snapshot, "", nil
		return true, nil
	})
	if err != nil {
		if cerr := cfg.Runs.CancelDepsRun(context.WithoutCancel(ctx), name, run.ID); cerr != nil {
			ctxLog(ctx).Error().Err(cerr).Str("repo", name.String()).Int64("run_id", run.ID).Msg("bernard: cancel a run no record holds")
		}
		return domain.RepoDeps{}, fmt.Errorf("record the dependency run: %w", err)
	}
	b.changed(ctx, name)
	return rec, nil
}

// writeDraft writes the manifest bump of batch on its branch at the tip of the
// default branch and opens or refreshes its merge request as a draft: the
// batch is Drafted, waiting for a run slot (runStep). The caller holds the
// repository lock.
func (b *Bernard) writeDraft(ctx context.Context, name domain.RepoName, batch domain.DepBatch, expected []domain.DepRef) (domain.RepoDeps, error) {
	params, err := b.runParams(ctx, name, batch)
	if err != nil {
		return domain.RepoDeps{}, err
	}
	target := strings.TrimPrefix(params.Ref, "refs/heads/")
	tip, err := domain.ParseObjectID(params.Sha)
	if err != nil {
		return domain.RepoDeps{}, err
	}
	changes, err := b.bumpChanges(ctx, name, tip, batch.Updates)
	if err != nil {
		return domain.RepoDeps{}, err
	}
	draft := batch
	draft.State, draft.Error, draft.Repro = domain.DepsDrafted, "", nil
	// The record holds the draft head before the branch moves: CI holds the
	// runs of that head (Held) whenever it plans them.
	_, _, err = b.writeBranch(ctx, name, draft, tip, tip, batch.Head, changes, func(r domain.RepoDeps, stored *domain.DepBatch, head domain.ObjectID, created bool) error {
		if err := movedPrecondition(r, batch.Key, expected); err != nil {
			return err
		}
		stored.AddDraft(head, created)
		stored.State, stored.Error, stored.Repro = domain.DepsDrafted, "", nil
		return nil
	})
	if err != nil {
		return domain.RepoDeps{}, err
	}
	number, err := b.ensureMR(ctx, name, target, draft, nil, true)
	if err != nil {
		return domain.RepoDeps{}, err
	}
	rec, err := b.cfg.Store.Mutate(ctx, name, func(r *domain.RepoDeps) (bool, error) {
		if err := movedPrecondition(*r, batch.Key, expected); err != nil {
			return false, err
		}
		_, i, ok := r.Batch(batch.Key)
		if !ok {
			return false, errRecordMoved
		}
		stored := &r.Batches[i]
		stored.MR = number
		stored.MRVersions, stored.MRUpdates = slices.Clone(batch.Refs()), slices.Clone(batch.Updates)
		return true, nil
	})
	if err != nil {
		return domain.RepoDeps{}, fmt.Errorf("record the draft merge request: %w", err)
	}
	b.changed(ctx, name)
	return rec, nil
}

// bumpChanges is the draft content of ups at tip: the manifest bump of the
// runner-side updates and the server-side updates applied in full.
func (b *Bernard) bumpChanges(ctx context.Context, name domain.RepoName, tip domain.ObjectID, ups []domain.DepUpdate) (map[domain.TreePath][]byte, error) {
	rev := domain.RevisionOf(tip)
	var serverUps []domain.DepUpdate
	changes := map[domain.TreePath][]byte{}
	for _, eco := range b.cfg.Ecosystems {
		var own []domain.DepUpdate
		for _, u := range ups {
			if u.Ecosystem == eco.Name() {
				own = append(own, u)
			}
		}
		if len(own) == 0 {
			continue
		}
		if eco.ApplyInServer() {
			serverUps = append(serverUps, own...)
			continue
		}
		files, partial, err := b.readInputs(ctx, name, rev, eco.Inputs(own, nil), nil)
		if errors.Is(err, domain.ErrNotFound) || errors.Is(err, errDepsOversize) {
			return nil, fmt.Errorf("%w: read the %s files: %v", domain.ErrPreconditionFailed, eco.Name(), err)
		}
		if err != nil {
			return nil, err
		}
		for tp := range partial {
			delete(files, tp) // a header is no file to edit
		}
		after, err := eco.Bump(files, own)
		if err != nil {
			return nil, fmt.Errorf("bump the %s manifests: %w", eco.Name(), err)
		}
		for tp, content := range after {
			if old, ok := files[tp]; !ok || !bytes.Equal(old, content) {
				changes[tp] = content
			}
		}
	}
	if len(serverUps) > 0 {
		more, err := b.changes(ctx, name, tip, serverUps)
		if err != nil {
			return nil, err
		}
		for tp, content := range more {
			if _, clash := changes[tp]; clash {
				return nil, fmt.Errorf("%w: %s is changed by two ecosystems", domain.ErrPreconditionFailed, tp)
			}
			changes[tp] = content
		}
	}
	return changes, nil
}

// goVersionOf returns the highest Go version the go.mod files of the updates'
// module directories ask for; the root go.mod does not count unless an update
// is in the root.
func (b *Bernard) goVersionOf(ctx context.Context, name domain.RepoName, tip domain.ObjectID, ups []domain.DepUpdate) (string, error) {
	var dirs []string
	for _, u := range ups {
		if u.Ecosystem != domain.EcosystemGoMod {
			continue
		}
		if !slices.Contains(dirs, u.Directory) {
			dirs = append(dirs, u.Directory)
		}
	}
	best := ""
	for _, dir := range dirs {
		tp, err := domain.ParseTreePath(path.Join(dir, "go.mod"))
		if err != nil {
			return "", fmt.Errorf("%w: %v", domain.ErrPreconditionFailed, err)
		}
		content, ok, err := b.readManifest(ctx, name, domain.RevisionOf(tip), tp)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("%w: %s is larger than %d bytes", domain.ErrPreconditionFailed, tp, maxDepsManifestBytes)
		}
		v, err := domain.GoVersionOf(content)
		if err != nil {
			return "", err
		}
		if best == "" || semver.Compare("v"+v, "v"+best) > 0 {
			best = v
		}
	}
	return best, nil
}

func (b *Bernard) ecosystem(name domain.DepsEcosystem) ports.DepsEcosystem {
	for _, e := range b.cfg.Ecosystems {
		if e.Name() == name {
			return e
		}
	}
	return nil
}

// fail records the failure on the card: a bounded excerpt.
func (b *Bernard) fail(ctx context.Context, name domain.RepoName, key domain.DepBatchKey, cause error) {
	msg := keepDepsError(cause.Error())
	rec, err := b.cfg.Store.Mutate(context.WithoutCancel(ctx), name, func(d *domain.RepoDeps) (bool, error) {
		_, i, ok := d.Batch(key)
		if !ok {
			return false, nil
		}
		d.Batches[i].State, d.Batches[i].Error = domain.DepsFailed, msg
		if !d.Batches[i].Head.IsZero() { // a draft is open: say how to redo it
			d.Batches[i].Repro = domain.DepsRepro(d.Batches[i].Updates)
		}
		return true, nil
	})
	if err != nil {
		ctxLog(ctx).Error().Err(err).Str("repo", name.String()).Msg("bernard: record the failure")
		return
	}
	if batch, _, ok := rec.Batch(key); ok {
		b.refreshDraft(context.WithoutCancel(ctx), name, batch)
	}
	b.changed(ctx, name)
}

// apply builds the commit of the updates on the tip of the default branch,
// moves bernard/deps to it and opens the merge request when there is none. The
// caller holds the repository lock. guard validates the record at the final
// transition.
func (b *Bernard) apply(ctx context.Context, name domain.RepoName, batch domain.DepBatch, guard func(domain.RepoDeps) error) (domain.RepoDeps, error) {
	cfg := b.cfg
	target, exists, err := cfg.Repos.DefaultBranch(ctx, name)
	if err != nil {
		return domain.RepoDeps{}, err
	}
	if !exists {
		return domain.RepoDeps{}, fmt.Errorf("%w: %s has no default branch", domain.ErrPreconditionFailed, name)
	}
	tip, err := cfg.Git.BranchHead(ctx, name, target)
	if err != nil {
		return domain.RepoDeps{}, fmt.Errorf("read %s: %w", target, err)
	}
	changes, err := b.changes(ctx, name, tip, batch.Updates)
	if err != nil {
		return domain.RepoDeps{}, err
	}
	return b.publish(ctx, name, target, tip, tip, batch.Head, changes, batch, guard, nil)
}

// publish commits changes on base (with parent as the commit's parent), moves
// the batch branch from old (read from the branch when zero) to the commit and
// opens the merge request into target, ready, when there is none. ups are all
// the updates the commit holds. The caller holds the repository lock; guard is
// checked on the record inside the final Mutate and its error stops the
// transition.
func (b *Bernard) publish(ctx context.Context, name domain.RepoName, target string, base, parent, old domain.ObjectID, changes map[domain.TreePath][]byte, batch domain.DepBatch, guard func(domain.RepoDeps) error, effects []domain.DepSideEffect) (domain.RepoDeps, error) {
	cfg := b.cfg
	ups := batch.Updates
	// A kept merge request (the draft) is made ready before the branch moves:
	// the pull_request runs the push starts see it ready, with its title.
	var number int64
	var err error
	if batch.MR != 0 {
		// The branch must still be Bernard's before its request changes.
		cur, err := cfg.Git.BranchHead(ctx, name, batch.BranchName())
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return domain.RepoDeps{}, fmt.Errorf("read %s: %w", batch.BranchName(), err)
		}
		if !old.IsZero() && cur != old {
			return domain.RepoDeps{}, errBranchAdopted
		}
		if old.IsZero() && !cur.IsZero() {
			if bot, err := b.botTip(ctx, name, cur); err != nil {
				return domain.RepoDeps{}, err
			} else if !bot {
				return domain.RepoDeps{}, errBranchAdopted
			}
		}
	}
	if batch.MR != 0 {
		if number, err = b.ensureMR(ctx, name, target, batch, effects, false); err != nil {
			return domain.RepoDeps{}, err
		}
	}
	head, moved, err := b.writeBranch(ctx, name, batch, base, parent, old, changes, nil)
	var draft domain.DepDraft
	if err != nil {
		return domain.RepoDeps{}, err
	}
	if number == 0 {
		if number, err = b.ensureMR(ctx, name, target, batch, effects, false); err != nil {
			return domain.RepoDeps{}, err
		}
	}
	refs := make([]domain.DepRef, len(ups))
	for i, u := range ups {
		refs[i] = u.DepRef
	}
	rec, err := cfg.Store.Mutate(ctx, name, func(d *domain.RepoDeps) (bool, error) {
		if guard != nil {
			if err := guard(*d); err != nil {
				return false, err
			}
		}
		_, i, ok := d.Batch(batch.Key)
		if !ok {
			return false, errRecordMoved
		}
		stored := &d.Batches[i]
		stored.MR, stored.MRVersions, stored.MRUpdates = number, slices.Clone(refs), slices.Clone(ups)
		stored.State, stored.Error, stored.RunUpdates, stored.Repro = domain.DepsIdle, "", nil, nil // RunID stays: the link to the last run
		if stored.Branch == "" {
			stored.Branch = batch.BranchName()
		}
		stored.Head = head
		if !moved {
			// The draft head is the final one: no longer held.
			draft, _ = stored.Draft(head)
			stored.RemoveDraft(head)
		}
		return true, nil
	})
	if err != nil {
		return domain.RepoDeps{}, fmt.Errorf("record the merge request: %w", err)
	}
	// After the final mutation, so the view that MergeRequestOpened woke sees
	// the record Idle with its merge request.
	b.changed(ctx, name)
	if !moved && cfg.Hold != nil {
		// The draft head is the final one: the runs it held start now.
		if err := cfg.Hold.ReleaseHold(ctx, name, batch.BranchName(), head, draft.Created); err != nil {
			ctxLog(ctx).Error().Err(err).Str("repo", name.String()).Msg("bernard: release the runs of the head")
		}
	}
	return rec, nil
}

// errBranchAdopted is the error of a write to a batch branch that a human
// changed since Bernard last wrote it.
var errBranchAdopted = fmt.Errorf("%w: the branch was changed by hand", domain.ErrPreconditionFailed)

// writeBranch commits changes on base with parent and moves the batch branch
// to the commit with a compare-and-swap from old. A branch that is no longer
// at a non-zero old was changed by hand: errBranchAdopted. A zero old (no head
// recorded) creates the branch; an existing one is taken over only when its
// tip is a Bernard commit (its head is recorded from it), otherwise it is a
// human's: errBranchAdopted.
//
// The record names the commit as pending (DepBatch.Pending, from the head the
// branch moves from) before the branch moves, and as the head once it moved;
// a failure of the move clears it, and settlePending resolves one a crash
// left. before, when set, edits the batch in that same write (told whether
// the move creates the branch), so that CI, told of the push, sees it. When
// parent is old and the tree is parent's, nothing is written and the parent
// is returned with moved false.
func (b *Bernard) writeBranch(ctx context.Context, name domain.RepoName, batch domain.DepBatch, base, parent, old domain.ObjectID, changes map[domain.TreePath][]byte, before func(r domain.RepoDeps, stored *domain.DepBatch, head domain.ObjectID, created bool) error) (head domain.ObjectID, moved bool, err error) {
	cfg := b.cfg
	branch := batch.BranchName()
	cur, err := cfg.Git.BranchHead(ctx, name, branch)
	if errors.Is(err, domain.ErrNotFound) {
		cur, err = domain.ObjectID{}, nil
	}
	if err != nil {
		return domain.ObjectID{}, false, fmt.Errorf("read %s: %w", branch, err)
	}
	switch {
	case !old.IsZero() && cur != old:
		return domain.ObjectID{}, false, errBranchAdopted
	case old.IsZero() && !cur.IsZero():
		bot, err := b.botTip(ctx, name, cur)
		if err != nil {
			return domain.ObjectID{}, false, err
		}
		if !bot {
			return domain.ObjectID{}, false, errBranchAdopted
		}
		old = cur
	}
	tree, err := cfg.Git.WriteTree(ctx, name, base, changes)
	if err != nil {
		return domain.ObjectID{}, false, fmt.Errorf("write the tree: %w", err)
	}
	if !old.IsZero() && parent == old {
		if same, err := cfg.Git.CommitTree(ctx, name, parent); err == nil && same == tree {
			return parent, false, nil
		}
	}
	commit := domain.NewBotCommit(tree, parent, domain.BotSubject(batch), batch.Updates, domain.Identity{}, cfg.Committer, cfg.Clock.Now())
	armored, err := cfg.Signer.Sign(commit.Payload())
	if err != nil {
		return domain.ObjectID{}, false, fmt.Errorf("sign the commit: %w", err)
	}
	head, err = cfg.Git.WriteCommit(ctx, name, commit.Signed(armored))
	if err != nil {
		return domain.ObjectID{}, false, fmt.Errorf("write the commit: %w", err)
	}
	// The branch was at old: a move meanwhile is a human's.
	defer func() {
		if errors.Is(err, domain.ErrConflict) {
			err = errBranchAdopted
		}
	}()
	if _, err := cfg.Store.Mutate(ctx, name, func(r *domain.RepoDeps) (bool, error) {
		_, i, ok := r.Batch(batch.Key)
		if !ok {
			return false, errRecordMoved
		}
		stored := &r.Batches[i]
		if before != nil {
			if err := before(*r, stored, head, old.IsZero()); err != nil {
				return false, err
			}
		}
		if stored.Branch == "" {
			stored.Branch = branch
		}
		if stored.Head.IsZero() {
			stored.Head = old // a Bernard tip taken over
		}
		stored.Pending, stored.PendingPrev = head, old
		return true, nil
	}); err != nil {
		return domain.ObjectID{}, false, err
	}
	head, err = b.moveBranch(ctx, name, batch.Key, branch, old, head)
	if err != nil {
		return domain.ObjectID{}, false, err
	}
	return head, true, nil
}

// moveBranch moves the branch from old to the pending head and records it as
// the head; a move that demonstrably did not happen clears the pending head.
func (b *Bernard) moveBranch(ctx context.Context, name domain.RepoName, key domain.DepBatchKey, branch string, old, head domain.ObjectID) (domain.ObjectID, error) {
	cfg := b.cfg
	settle := func(moved bool) error {
		_, err := cfg.Store.Mutate(context.WithoutCancel(ctx), name, func(r *domain.RepoDeps) (bool, error) {
			_, i, ok := r.Batch(key)
			if !ok || r.Batches[i].Pending != head {
				return false, nil
			}
			stored := &r.Batches[i]
			if moved {
				stored.Head = head
			}
			stored.Pending, stored.PendingPrev = domain.ObjectID{}, domain.ObjectID{}
			return true, nil
		})
		return err
	}
	oldStr := domain.ZeroOID
	if !old.IsZero() {
		oldStr = old.String()
	}
	upd := []domain.RefUpdate{{Old: oldStr, New: head.String(), Ref: "refs/heads/" + branch}}
	bot := domain.Bot(name)
	if err := recordRefTips(ctx, cfg.Tips, name, upd, domain.AuthorName(bot), domain.PusherOf(bot), false); err != nil {
		if serr := settle(false); serr != nil {
			ctxLog(ctx).Error().Err(serr).Str("repo", name.String()).Msg("bernard: clear the pending head")
		}
		return domain.ObjectID{}, err
	}
	if err := cfg.Git.UpdateBranch(ctx, name, branch, old, head); err != nil {
		// The move may have happened despite the error (a cancelled context
		// after update-ref): the branch tells. Unread, the pending head stays
		// for settlePending.
		tip, rerr := cfg.Git.BranchHead(context.WithoutCancel(ctx), name, branch)
		if errors.Is(rerr, domain.ErrNotFound) {
			tip, rerr = domain.ObjectID{}, nil
		}
		switch {
		case rerr != nil:
			return domain.ObjectID{}, fmt.Errorf("move %s: %w", branch, err)
		case tip == head:
			// Moved: go on as a success.
		case tip == old:
			if serr := settle(false); serr != nil {
				ctxLog(ctx).Error().Err(serr).Str("repo", name.String()).Msg("bernard: clear the pending head")
			}
			return domain.ObjectID{}, fmt.Errorf("move %s: %w", branch, err)
		default:
			return domain.ObjectID{}, errBranchAdopted // markAdopted clears the pending head
		}
	}
	// Recorded before CI hears of the push. Should it fail, the record keeps
	// the pending head, which settlePending promotes.
	if err := settle(true); err != nil {
		ctxLog(ctx).Error().Err(err).Str("repo", name.String()).Msg("bernard: record the head of the branch")
	}
	if cfg.CI != nil {
		cfg.CI.OnPush(ctx, name, upd, bot)
	}
	return head, nil
}

// botTip reports whether the tip of a branch is a Bernard commit: its author
// is the one NewBotCommit writes.
func (b *Bernard) botTip(ctx context.Context, name domain.RepoName, tip domain.ObjectID) (bool, error) {
	cs, _, err := b.cfg.Git.ListRange(ctx, name, domain.ObjectID{}, tip, 1)
	if err != nil {
		return false, fmt.Errorf("read the tip of the branch: %w", err)
	}
	if len(cs) != 1 {
		return false, nil
	}
	want := domain.NewBotCommit(domain.ObjectID{}, domain.ObjectID{}, "", nil, domain.Identity{}, b.cfg.Committer, time.Time{}).Author
	return cs[0].Author.Name == want.Name && cs[0].Author.Email == want.Email, nil
}

// settlePending resolves the pending head of a batch against the tip of its
// branch (read when tip is nil): at the pending head it becomes the head, at
// the head it moved from it is dropped, anywhere else a human moved the
// branch and the batch is adopted (idle as for markAdopted). It returns the
// record. The caller holds the repository lock.
func (b *Bernard) settlePending(ctx context.Context, name domain.RepoName, key domain.DepBatchKey, tip *domain.ObjectID, idle bool) (domain.RepoDeps, error) {
	d, err := b.cfg.Store.Get(ctx, name)
	if err != nil {
		return domain.RepoDeps{}, err
	}
	batch, _, ok := d.Batch(key)
	if !ok || batch.Pending.IsZero() {
		return d, nil
	}
	var cur domain.ObjectID
	if tip != nil {
		cur = *tip
	} else if cur, err = b.cfg.Git.BranchHead(ctx, name, batch.BranchName()); errors.Is(err, domain.ErrNotFound) {
		cur = domain.ObjectID{}
	} else if err != nil {
		return domain.RepoDeps{}, fmt.Errorf("read %s: %w", batch.BranchName(), err)
	}
	human := false
	d, err = b.cfg.Store.Mutate(ctx, name, func(r *domain.RepoDeps) (bool, error) {
		_, i, ok := r.Batch(key)
		if !ok || r.Batches[i].Pending != batch.Pending {
			return false, nil
		}
		human = !r.Batches[i].SettlePending(cur)
		return true, nil
	})
	if err != nil {
		return domain.RepoDeps{}, fmt.Errorf("settle the pending head: %w", err)
	}
	if human {
		b.markAdopted(ctx, name, key, idle)
		return b.cfg.Store.Get(ctx, name)
	}
	return d, nil
}

// settleAllPending resolves the pending heads a previous process left.
func (b *Bernard) settleAllPending(ctx context.Context) {
	recs, err := b.cfg.Store.List(ctx)
	if err != nil {
		ctxLog(ctx).Error().Err(err).Msg("bernard: list the records")
		return
	}
	for _, r := range recs {
		for _, batch := range r.Batches {
			if batch.Pending.IsZero() || b.cfg.Git == nil {
				continue
			}
			func() {
				defer b.repoLock(r.Repo)()
				if _, err := b.settlePending(ctx, r.Repo, batch.Key, nil, true); err != nil {
					ctxLog(ctx).Error().Err(err).Str("repo", r.Repo.String()).Msg("bernard: settle the pending head")
				}
			}()
		}
	}
}

// changes reads the files the updates touch at the tip, applies the updates
// with their ecosystem and validates the result.
func (b *Bernard) changes(ctx context.Context, name domain.RepoName, tip domain.ObjectID, ups []domain.DepUpdate) (map[domain.TreePath][]byte, error) {
	rev := domain.RevisionOf(tip)
	byEco := map[domain.DepsEcosystem][]domain.DepUpdate{}
	var order []domain.DepsEcosystem
	for _, u := range ups {
		if _, ok := byEco[u.Ecosystem]; !ok {
			order = append(order, u.Ecosystem)
		}
		byEco[u.Ecosystem] = append(byEco[u.Ecosystem], u)
	}
	changes := map[domain.TreePath][]byte{}
	for _, ecoName := range order {
		eco := b.ecosystem(ecoName)
		list := byEco[ecoName]
		before := map[domain.TreePath][]byte{}
		for _, u := range list {
			tp, err := domain.ParseTreePath(u.Directory)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", domain.ErrPreconditionFailed, err)
			}
			if _, ok := before[tp]; ok {
				continue
			}
			content, ok, err := b.readManifest(ctx, name, rev, tp)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("%w: %s is larger than %d bytes", domain.ErrPreconditionFailed, tp, maxDepsManifestBytes)
			}
			before[tp] = content
		}
		after, err := eco.Apply(before, list)
		if err != nil {
			return nil, fmt.Errorf("apply the %s updates: %w", ecoName, err)
		}
		if _, err := eco.Validate(before, after, list); err != nil {
			return nil, fmt.Errorf("check the %s updates: %w", ecoName, err)
		}
		for tp, content := range after {
			if !bytes.Equal(content, before[tp]) {
				changes[tp] = content
			}
		}
	}
	return changes, nil
}

// ensureMR returns the number of the open Bernard merge request, creating it
// when the stored one is gone or no longer open. draft keeps it a draft (the
// runner has not finished); otherwise a draft is marked ready.
func (b *Bernard) ensureMR(ctx context.Context, name domain.RepoName, target string, batch domain.DepBatch, effects []domain.DepSideEffect, draft bool) (int64, error) {
	stored, branch := batch.MR, batch.BranchName()
	if stored != 0 {
		mr, err := b.cfg.MRStore.Get(ctx, name, stored)
		if err == nil && mr.IsOpen() {
			if err := b.refreshMR(ctx, name, stored, mr, batch, effects, draft); err != nil {
				return 0, err
			}
			return stored, nil
		}
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return 0, fmt.Errorf("read !%d: %w", stored, err)
		}
	}
	// An open request from the branch may exist without being stored (a
	// lost record, a request opened by hand): adopt it.
	if mr, err := b.findOpenMRRecord(ctx, name, target, branch); err != nil {
		return 0, err
	} else if mr.Number != 0 {
		if err := b.refreshMR(ctx, name, mr.Number, mr, batch, effects, draft); err != nil {
			return 0, err
		}
		return mr.Number, nil
	}
	title := domain.BotSubject(batch)
	if draft {
		title = domain.BotDraftTitle(batch)
	}
	mr, err := b.cfg.MRs.Create(ctx, domain.Bot(name), name, ports.CreateMergeRequestParams{
		Title:       title,
		Description: domain.BotMRBodyFor(batch, batch.Updates, effects),
		Source:      branch, Target: target,
	})
	if errors.Is(err, domain.ErrAlreadyExists) {
		n, ferr := b.findOpenMR(ctx, name, target, branch)
		if ferr != nil {
			return 0, ferr
		}
		if n != 0 {
			return n, nil
		}
	}
	if err != nil {
		return 0, fmt.Errorf("open the merge request: %w", err)
	}
	return mr.Number, nil
}

// refreshMR sets the title and the description of the kept merge request to
// those generated for the batch, as the bot (which holds mr:write on its
// repository): the draft title while draft, otherwise a draft is marked ready
// and gets the plain subject. A failure is returned: the caller does not
// record the merge request as holding the batch, so the step is retried.
func (b *Bernard) refreshMR(ctx context.Context, name domain.RepoName, number int64, mr domain.MergeRequest, batch domain.DepBatch, effects []domain.DepSideEffect, draft bool) error {
	title, desc := domain.BotSubject(batch), domain.BotMRBodyFor(batch, batch.Updates, effects)
	if draft {
		title = domain.BotDraftTitle(batch)
	} else if mr.IsDraft() {
		ready, err := b.cfg.MRs.MarkReady(ctx, domain.Bot(name), name, number)
		switch {
		case err == nil:
			mr.Title = ready.Title
		case errors.Is(err, domain.ErrPreconditionFailed): // ready already
		default:
			return fmt.Errorf("mark !%d ready: %w", number, err)
		}
	}
	if mr.Title.String() == title && mr.Description.String() == desc {
		return nil
	}
	params := ports.EditMergeRequestParams{Description: &desc}
	if mr.Title.String() != title {
		params.Title = &title
	}
	if _, err := b.cfg.MRs.Edit(ctx, domain.Bot(name), name, number, params); err != nil {
		return fmt.Errorf("refresh !%d: %w", number, err)
	}
	return nil
}

// findOpenMR returns the number of the open merge request from bernard/deps
// into target, zero when there is none.
func (b *Bernard) findOpenMR(ctx context.Context, name domain.RepoName, target, branch string) (int64, error) {
	mr, err := b.findOpenMRRecord(ctx, name, target, branch)
	return mr.Number, err
}

// findOpenMRRecord is findOpenMR returning the merge request read, zero when
// there is none.
func (b *Bernard) findOpenMRRecord(ctx context.Context, name domain.RepoName, target, branch string) (domain.MergeRequest, error) {
	nums, _, err := b.cfg.MRStore.OpenBySource(ctx, name, []string{branch}, 16)
	if err != nil {
		return domain.MergeRequest{}, fmt.Errorf("look for an open merge request: %w", err)
	}
	for _, n := range nums {
		mr, err := b.cfg.MRStore.Get(ctx, name, n)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			return domain.MergeRequest{}, fmt.Errorf("read !%d: %w", n, err)
		}
		if mr.IsOpen() && mr.Source == branch && mr.Target == target {
			mr.Number = n
			return mr, nil
		}
	}
	return domain.MergeRequest{}, nil
}

// watchMergeRequests applies the close and the merge of Bernard's merge
// request until the bus or Bernard stops. At the start and after a gap in the
// events it reconciles the records with the merge requests, which may have
// ended unseen.
func (b *Bernard) watchMergeRequests(sub <-chan domain.Event) {
	defer b.wg.Done()
	b.reconcileMergeRequests()
	for {
		select {
		case <-b.ctx.Done():
			return
		case ev, ok := <-sub:
			if !ok {
				return
			}
			switch e := ev.(type) {
			case domain.EventGap:
				b.reconcileMergeRequests()
			case domain.MergeRequestClosed:
				b.onMRDone(e.MergeRequest, false)
			case domain.MergeRequestMerged:
				b.onMRDone(e.MergeRequest, true)
			case domain.PushEvent:
				b.onPush(e)
			case domain.RunUpdated, domain.JobUpdated:
				b.wakeQueue() // a slot or a runner may be free
			}
		}
	}
}

// reconcileMergeRequests applies the end of every stored merge request that is
// no longer open: closed as a close, merged as a merge.
func (b *Bernard) reconcileMergeRequests() {
	if b.cfg.MRStore == nil {
		return
	}
	ctx := b.ctx
	log := ctxLog(ctx)
	recs, err := b.cfg.Store.List(ctx)
	if err != nil {
		log.Error().Err(err).Msg("bernard: list the records to reconcile")
		return
	}
	for _, d := range recs {
		for _, batch := range d.Batches {
			if batch.MR == 0 || ctx.Err() != nil {
				continue
			}
			mr, err := b.cfg.MRStore.Get(ctx, d.Repo, batch.MR)
			switch {
			case errors.Is(err, domain.ErrNotFound):
				continue
			case err != nil:
				log.Error().Err(err).Str("repo", d.Repo.String()).Msg("bernard: read the merge request to reconcile")
				continue
			}
			switch mr.State {
			case domain.MRStateClosed:
				b.onMRDone(mr, false)
			case domain.MRStateMerged:
				b.onMRDone(mr, true)
			}
		}
	}
}

// onMRDone handles the end of a merge request: when it is Bernard's, a close
// ignores the versions it held and a merge triggers a check.
func (b *Bernard) onMRDone(mr domain.MergeRequest, merged bool) {
	ctx := b.ctx
	log := ctxLog(ctx)
	defer func() {
		if r := recover(); r != nil {
			log.Error().Interface("panic", r).Bytes("stack", debug.Stack()).Msg("bernard: merge request event panicked")
		}
	}()
	defer b.repoLock(mr.Repo)()
	d, err := b.cfg.Store.Get(ctx, mr.Repo)
	if err != nil {
		log.Error().Err(err).Str("repo", mr.Repo.String()).Msg("bernard: read the record")
		return
	}
	batch := mrBatch(&d, mr.Number)
	if batch == nil {
		return
	}
	versions := slices.Clone(batch.MRVersions)
	if !merged && len(versions) > 0 {
		if err := b.cfg.Store.Ignore(ctx, mr.Repo, versions); err != nil {
			log.Error().Err(err).Str("repo", mr.Repo.String()).Msg("bernard: ignore the versions of the closed merge request")
			return
		}
	}
	drop := make(map[domain.DepRef]bool, len(versions))
	for _, v := range versions {
		drop[v] = true
	}
	if !merged {
		// A run still working on the closed request must not publish what the
		// owner just refused: it is cancelled, and its end finds the record
		// moved on.
		if batch.State == domain.DepsApplying && batch.RunID != 0 && b.cfg.Runs != nil {
			if err := b.cfg.Runs.CancelDepsRun(ctx, mr.Repo, batch.RunID); err != nil && !errors.Is(err, domain.ErrNotFound) {
				log.Error().Err(err).Str("repo", mr.Repo.String()).Int64("run_id", batch.RunID).Msg("bernard: cancel the dependency run of the closed merge request")
			}
		}
	}
	_, err = b.cfg.Store.Mutate(ctx, mr.Repo, func(d *domain.RepoDeps) (bool, error) {
		batch := mrBatch(d, mr.Number)
		if batch == nil {
			return false, nil
		}
		batch.MR, batch.MRVersions, batch.MRUpdates = 0, nil, nil
		batch.Adopted, batch.Head, batch.Drafts = false, domain.ObjectID{}, nil // the branch is Bernard's again
		batch.Pending, batch.PendingPrev = domain.ObjectID{}, domain.ObjectID{}
		if batch.Queued() || (!merged && batch.State == domain.DepsApplying) {
			batch.State, batch.Error, batch.RunUpdates = domain.DepsIdle, "", nil
		}
		if !merged {
			batch.Updates = slices.DeleteFunc(slices.Clone(batch.Updates), func(u domain.DepUpdate) bool { return drop[u.DepRef] })
		}
		if batch.Key != "" && len(batch.Updates) == 0 {
			_, i, _ := d.Batch(batch.Key)
			d.Batches = slices.Delete(slices.Clone(d.Batches), i, i+1)
		}
		return true, nil
	})
	if err != nil {
		log.Error().Err(err).Str("repo", mr.Repo.String()).Msg("bernard: clear the merge request")
		return
	}
	b.changed(ctx, mr.Repo)
	if merged {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			cctx, cancel := context.WithTimeout(b.ctx, depsBackgroundCheckTimeout)
			defer cancel()
			if _, err := b.check(cctx, b.cfg.Registry(), mr.Repo); err != nil && cctx.Err() == nil {
				log.Error().Err(err).Str("repo", mr.Repo.String()).Msg("bernard: check after a merge")
			}
		}()
	}
}

func mrBatch(d *domain.RepoDeps, number int64) *domain.DepBatch {
	if number == 0 {
		return nil
	}
	for i := range d.Batches {
		if d.Batches[i].MR == number {
			return &d.Batches[i]
		}
	}
	return nil
}
