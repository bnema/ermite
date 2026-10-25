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
	"sync"

	"golang.org/x/mod/semver"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

const (
	// depsBranch is the branch of Bernard's merge request, in its namespace.
	depsBranch = "bernard/deps"
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
func (b *Bernard) Open(ctx context.Context, p domain.Principal, repoName domain.RepoName, expected []domain.DepRef) (domain.RepoDeps, error) {
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
	if err := depsPrecondition(d, expected); err != nil {
		return domain.RepoDeps{}, err
	}
	if len(d.Updates) == 0 {
		return domain.RepoDeps{}, fmt.Errorf("%w: no dependency update is pending", domain.ErrPreconditionFailed)
	}
	var runnerSide bool
	for _, u := range d.Updates {
		eco := b.ecosystem(u.Ecosystem)
		if eco == nil {
			return domain.RepoDeps{}, fmt.Errorf("%w: unknown ecosystem %q", domain.ErrPreconditionFailed, u.Ecosystem)
		}
		if !eco.ApplyInServer() {
			if u.Ecosystem != domain.EcosystemGoMod {
				return domain.RepoDeps{}, fmt.Errorf("%w: %s updates cannot be applied by a runner", domain.ErrPreconditionFailed, u.Ecosystem)
			}
			runnerSide = true
		}
	}
	var rec domain.RepoDeps
	if runnerSide {
		rec, err = b.startRunnerJob(ctx, repoName, d, expected)
	} else {
		rec, err = b.apply(ctx, repoName, d.Updates, d.MR, func(r domain.RepoDeps) error { return movedPrecondition(r, expected) })
	}
	if err != nil {
		if !errors.Is(err, errRecordMoved) { // the record is not ours to fail
			b.fail(ctx, repoName, err)
		}
		return domain.RepoDeps{}, err
	}
	return rec, nil
}

// errRecordMoved marks a transition refused because the record changed since
// Open read it.
var errRecordMoved = errors.New("the dependency record changed")

func movedPrecondition(d domain.RepoDeps, expected []domain.DepRef) error {
	if err := depsPrecondition(d, expected); err != nil {
		return fmt.Errorf("%w: %w", errRecordMoved, err)
	}
	return nil
}

// startRunnerJob starts the CI run of the runner-side updates at the tip of the
// default branch and marks the record Applying with it. The caller holds the
// repository lock.
func (b *Bernard) startRunnerJob(ctx context.Context, name domain.RepoName, d domain.RepoDeps, expected []domain.DepRef) (domain.RepoDeps, error) {
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
	var runnerUps []domain.DepUpdate
	for _, u := range d.Updates {
		if !b.ecosystem(u.Ecosystem).ApplyInServer() {
			runnerUps = append(runnerUps, u)
		}
	}
	goVersion, err := b.goVersionOf(ctx, name, tip, runnerUps)
	if err != nil {
		return domain.RepoDeps{}, err
	}
	if cfg.Runs == nil {
		return domain.RepoDeps{}, fmt.Errorf("%w: dependency jobs are not available", domain.ErrPreconditionFailed)
	}
	refs := make([]domain.DepRef, len(runnerUps))
	for i, u := range runnerUps {
		refs[i] = u.DepRef
	}
	run, err := cfg.Runs.StartDepsRun(ctx, ports.DepsRunParams{
		Repo: name, Ref: "refs/heads/" + target, Sha: tip.String(), Title: domain.BotCommitSubject(len(d.Updates)),
		Job: domain.CIDepsJob{Updates: refs, GoVersion: goVersion}, Label: cfg.RunnerLabel, Timeout: cfg.JobTimeout,
	})
	if err != nil {
		return domain.RepoDeps{}, fmt.Errorf("start the dependency run: %w", err)
	}
	snapshot := slices.Clone(d.Updates)
	rec, err := cfg.Store.Mutate(ctx, name, func(r *domain.RepoDeps) (bool, error) {
		if err := movedPrecondition(*r, expected); err != nil {
			return false, err
		}
		r.Repo, r.State, r.RunID, r.RunUpdates, r.Error = name, domain.DepsApplying, run.ID, snapshot, ""
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

// goVersionOf returns the highest Go version the go.mod files of the updates'
// module directories ask for; the root go.mod does not count unless an update
// is in the root.
func (b *Bernard) goVersionOf(ctx context.Context, name domain.RepoName, tip domain.ObjectID, ups []domain.DepUpdate) (string, error) {
	var dirs []string
	for _, u := range ups {
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

// fail records the failure on the card: its first line, bounded.
func (b *Bernard) fail(ctx context.Context, name domain.RepoName, cause error) {
	msg := oneLine(cause.Error())
	_, err := b.cfg.Store.Mutate(context.WithoutCancel(ctx), name, func(d *domain.RepoDeps) (bool, error) {
		d.Repo, d.State, d.Error = name, domain.DepsFailed, msg
		return true, nil
	})
	if err != nil {
		ctxLog(ctx).Error().Err(err).Str("repo", name.String()).Msg("bernard: record the failure")
		return
	}
	b.changed(ctx, name)
}

// apply builds the commit of the updates on the tip of the default branch,
// moves bernard/deps to it and opens the merge request when there is none. The
// caller holds the repository lock. guard validates the record at the final
// transition.
func (b *Bernard) apply(ctx context.Context, name domain.RepoName, ups []domain.DepUpdate, storedMR int64, guard func(domain.RepoDeps) error) (domain.RepoDeps, error) {
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
	changes, err := b.changes(ctx, name, tip, ups)
	if err != nil {
		return domain.RepoDeps{}, err
	}
	return b.publish(ctx, name, target, tip, changes, ups, storedMR, guard)
}

// publish commits changes on base, moves bernard/deps to the commit and opens
// the merge request into target when there is none. ups are all the updates the
// commit holds. The caller holds the repository lock; guard is checked on the
// record inside the final Mutate and its error stops the transition.
func (b *Bernard) publish(ctx context.Context, name domain.RepoName, target string, tip domain.ObjectID, changes map[domain.TreePath][]byte, ups []domain.DepUpdate, storedMR int64, guard func(domain.RepoDeps) error) (domain.RepoDeps, error) {
	cfg := b.cfg
	tree, err := cfg.Git.WriteTree(ctx, name, tip, changes)
	if err != nil {
		return domain.RepoDeps{}, fmt.Errorf("write the tree: %w", err)
	}
	commit := domain.NewBotCommit(tree, tip, ups, domain.Identity{}, cfg.Committer, cfg.Clock.Now())
	armored, err := cfg.Signer.Sign(commit.Payload())
	if err != nil {
		return domain.RepoDeps{}, fmt.Errorf("sign the commit: %w", err)
	}
	head, err := cfg.Git.WriteCommit(ctx, name, commit.Signed(armored))
	if err != nil {
		return domain.RepoDeps{}, fmt.Errorf("write the commit: %w", err)
	}

	// Bernard's namespace: whatever bernard/deps holds is replaced.
	old, err := cfg.Git.BranchHead(ctx, name, depsBranch)
	if errors.Is(err, domain.ErrNotFound) {
		old, err = domain.ObjectID{}, nil
	}
	if err != nil {
		return domain.RepoDeps{}, fmt.Errorf("read %s: %w", depsBranch, err)
	}
	oldStr := domain.ZeroOID
	if !old.IsZero() {
		oldStr = old.String()
	}
	upd := []domain.RefUpdate{{Old: oldStr, New: head.String(), Ref: "refs/heads/" + depsBranch}}
	bot := domain.Bot(name)
	if err := recordRefTips(ctx, cfg.Tips, name, upd, domain.AuthorName(bot), domain.PusherOf(bot), false); err != nil {
		return domain.RepoDeps{}, err
	}
	if err := cfg.Git.UpdateBranch(ctx, name, depsBranch, old, head); err != nil {
		return domain.RepoDeps{}, fmt.Errorf("move %s: %w", depsBranch, err)
	}
	if cfg.CI != nil {
		cfg.CI.OnPush(ctx, name, upd, bot)
	}

	number, err := b.ensureMR(ctx, name, target, storedMR, ups)
	if err != nil {
		return domain.RepoDeps{}, err
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
		d.Repo, d.MR, d.MRVersions = name, number, slices.Clone(refs)
		d.State, d.Error, d.RunUpdates = domain.DepsIdle, "", nil // RunID stays: the link to the last run
		return true, nil
	})
	if err != nil {
		return domain.RepoDeps{}, fmt.Errorf("record the merge request: %w", err)
	}
	// After the final mutation, so the view that MergeRequestOpened woke sees
	// the record Idle with its merge request.
	b.changed(ctx, name)
	return rec, nil
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
		if err := eco.Validate(before, after, list); err != nil {
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
// when the stored one is gone or no longer open.
func (b *Bernard) ensureMR(ctx context.Context, name domain.RepoName, target string, stored int64, ups []domain.DepUpdate) (int64, error) {
	if stored != 0 {
		mr, err := b.cfg.MRStore.Get(ctx, name, stored)
		if err == nil && mr.IsOpen() {
			b.refreshMR(ctx, name, stored, mr, ups)
			return stored, nil
		}
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return 0, fmt.Errorf("read !%d: %w", stored, err)
		}
	}
	// An open request from bernard/deps may exist without being stored (a
	// lost record, a request opened by hand): adopt it.
	if n, err := b.findOpenMR(ctx, name, target); err != nil || n != 0 {
		if err == nil {
			if mr, gerr := b.cfg.MRStore.Get(ctx, name, n); gerr == nil {
				b.refreshMR(ctx, name, n, mr, ups)
			}
		}
		return n, err
	}
	mr, err := b.cfg.MRs.Create(ctx, domain.Bot(name), name, ports.CreateMergeRequestParams{
		Title:       domain.BotCommitSubject(len(ups)),
		Description: domain.BotCommitBody(ups),
		Source:      depsBranch, Target: target,
	})
	if errors.Is(err, domain.ErrAlreadyExists) {
		n, ferr := b.findOpenMR(ctx, name, target)
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
// those generated for ups, as the bot (which holds mr:write on its repository).
// A failure is logged: the branch is already updated.
func (b *Bernard) refreshMR(ctx context.Context, name domain.RepoName, number int64, mr domain.MergeRequest, ups []domain.DepUpdate) {
	title, desc := domain.BotCommitSubject(len(ups)), domain.BotCommitBody(ups)
	if mr.Title.String() == title && mr.Description.String() == desc {
		return
	}
	if _, err := b.cfg.MRs.Edit(ctx, domain.Bot(name), name, number, ports.EditMergeRequestParams{Title: &title, Description: &desc}); err != nil {
		ctxLog(ctx).Error().Err(err).Str("repo", name.String()).Int64("mr", number).Msg("bernard: refresh the merge request")
	}
}

// findOpenMR returns the number of the open merge request from bernard/deps
// into target, zero when there is none.
func (b *Bernard) findOpenMR(ctx context.Context, name domain.RepoName, target string) (int64, error) {
	nums, _, err := b.cfg.MRStore.OpenBySource(ctx, name, []string{depsBranch}, 16)
	if err != nil {
		return 0, fmt.Errorf("look for an open merge request: %w", err)
	}
	for _, n := range nums {
		mr, err := b.cfg.MRStore.Get(ctx, name, n)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("read !%d: %w", n, err)
		}
		if mr.IsOpen() && mr.Source == depsBranch && mr.Target == target {
			return n, nil
		}
	}
	return 0, nil
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
		if d.MR == 0 || ctx.Err() != nil {
			continue
		}
		mr, err := b.cfg.MRStore.Get(ctx, d.Repo, d.MR)
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
	if d.MR == 0 || d.MR != mr.Number {
		return
	}
	versions := slices.Clone(d.MRVersions)
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
		if d.State == domain.DepsApplying && d.RunID != 0 && b.cfg.Runs != nil {
			if err := b.cfg.Runs.CancelDepsRun(ctx, mr.Repo, d.RunID); err != nil && !errors.Is(err, domain.ErrNotFound) {
				log.Error().Err(err).Str("repo", mr.Repo.String()).Int64("run_id", d.RunID).Msg("bernard: cancel the dependency run of the closed merge request")
			}
		}
	}
	_, err = b.cfg.Store.Mutate(ctx, mr.Repo, func(d *domain.RepoDeps) (bool, error) {
		if d.MR != mr.Number {
			return false, nil
		}
		d.MR, d.MRVersions = 0, nil
		if !merged && d.State == domain.DepsApplying {
			d.State, d.Error, d.RunUpdates = domain.DepsIdle, "", nil
		}
		if !merged {
			d.Updates = slices.DeleteFunc(slices.Clone(d.Updates), func(u domain.DepUpdate) bool { return drop[u.DepRef] })
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
			cctx, cancel := context.WithTimeout(b.ctx, depsCheckTimeout)
			defer cancel()
			if _, err := b.check(cctx, b.cfg.Registry(), mr.Repo); err != nil && cctx.Err() == nil {
				log.Error().Err(err).Str("repo", mr.Repo.String()).Msg("bernard: check after a merge")
			}
		}()
	}
}
