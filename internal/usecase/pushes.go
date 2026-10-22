// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Pushes are the use cases around a Git push.
type Pushes struct {
	releases ports.ReleaseTagTrigger
	links    ports.IssueLinkTrigger
	mirror   ports.MirrorTrigger // nil: no outbound mirror
	meta     ports.RepoMetaStore
	repos    *Repos
	git      ports.GitRepos
	bus      ports.EventBus
	clock    ports.Clock
	maint    ports.MaintenanceScheduler
	auth     ports.Revalidator
	fresh    ports.RepoFreshener       // nil: the reader needs no telling
	ci       ports.WorkflowTrigger     // nil: no CI
	trees    ports.PushTreeReader      // nil: pushes by job tokens are refused
	tips     ports.CIRefTipStore       // nil: who pushes a ref is not recorded
	mergeab  ports.MergeabilityTrigger // nil: mergeability is not tracked
}

var (
	_ ports.PushService     = (*Pushes)(nil)
	_ ports.PushHookService = (*Pushes)(nil)
)

// NewPushes builds the push use cases. Every argument is required: auth
// re-validates the token of a push when its pre-receive hook runs, maint
// queues the maintenance that follows a push.
func NewPushes(meta ports.RepoMetaStore, repos *Repos, git ports.GitRepos, bus ports.EventBus, clock ports.Clock, maint ports.MaintenanceScheduler, auth ports.Revalidator) *Pushes {
	return &Pushes{meta: meta, repos: repos, git: git, bus: bus, clock: clock, maint: maint, auth: auth}
}

// WithReleases enables synchronous release tag-deletion handling before the first push.
func (u *Pushes) WithReleases(t ports.ReleaseTagTrigger) *Pushes { u.releases = t; return u }

// WithIssueLinks enables synchronous best-effort issue indexing before the first push.
func (u *Pushes) WithIssueLinks(t ports.IssueLinkTrigger) *Pushes { u.links = t; return u }

// WithMirror makes Record notify outbound mirrors. Call before the first push.
func (u *Pushes) WithMirror(t ports.MirrorTrigger) *Pushes {
	u.mirror = t
	return u
}

// WithFreshener makes Record tell the reader that a repository changed, so that
// it serves the push at once. Call it before the first push; it returns u.
func (u *Pushes) WithFreshener(f ports.RepoFreshener) *Pushes {
	u.fresh = f
	return u
}

// WithWorkflowTrigger makes Record start the workflows that match the push.
// Call it before the first push; it returns u.
func (u *Pushes) WithWorkflowTrigger(t ports.WorkflowTrigger) *Pushes {
	u.ci = t
	return u
}

// WithMergeability makes Record queue the mergeability checks of the merge
// requests whose branches the push moved. Call it before the first push; it
// returns u.
func (u *Pushes) WithMergeability(t ports.MergeabilityTrigger) *Pushes {
	u.mergeab = t
	return u
}

// WithRefTips makes Check record who is about to move each branch and tag
// (domain.CIRefTip), before Git moves it. Call it before the first push; it
// returns u.
func (u *Pushes) WithRefTips(t ports.CIRefTipStore) *Pushes {
	u.tips = t
	return u
}

// WithPushTreeReader lets Check tell whether a push changes workflow files, which
// a push by a job token may not do. Without it a push by a job token is
// refused, as it cannot be judged. Call it before the first push; it returns u.
func (u *Pushes) WithPushTreeReader(g ports.PushTreeReader) *Pushes {
	u.trees = g
	return u
}

// Open decides whether the principal may push to the repository and, with
// create, creates it when it does not exist yet (private, published as
// RepoCreated by Repos.Create). create is for the first request of a push only.
//
// Anonymous callers get domain.ErrUnauthenticated. A repository the caller
// cannot read and cannot write, or a missing one it may not create, answers
// domain.ErrNotFound, so that a private repository never shows itself. One it
// can read but not write answers domain.ErrForbidden.
func (u *Pushes) Open(ctx context.Context, p domain.Principal, name domain.RepoName, create bool) error {
	if p.IsAnonymous() {
		return domain.ErrUnauthenticated
	}
	repo, err := u.meta.Get(ctx, name)
	switch {
	case err == nil:
		return openExisting(p, repo)
	case !errors.Is(err, domain.ErrNotFound):
		return fmt.Errorf("get repository %q: %w", name, err)
	}

	if !create || !canCreateRepo(p, name) {
		return domain.ErrNotFound
	}
	_, err = u.repos.Create(ctx, p, ports.CreateRepoParams{Name: name, Visibility: domain.VisibilityPrivate})
	switch {
	case err == nil:
		return nil // Create published RepoCreated
	case errors.Is(err, domain.ErrAlreadyExists):
		// A concurrent first push won the race: this one pushes to its repository.
		repo, gerr := u.meta.Get(ctx, name)
		if gerr != nil {
			return fmt.Errorf("get repository %q: %w", name, gerr)
		}
		return openExisting(p, repo)
	}
	return err
}

func openExisting(p domain.Principal, repo domain.Repo) error {
	switch {
	case canWrite(p, repo.Name):
		return nil
	case canRead(p, repo):
		return domain.ErrForbidden
	}
	return domain.ErrNotFound
}

// Check is the pre-receive decision. It accepts a push of a principal that
// may write the repository. A token limited to branch prefixes may only update
// (create, move or delete) branches under them: the first ref outside them
// refuses the whole push, tags and every other ref included. Further rules
// (protected branches, limits) will refuse here too, by returning an error
// that matches domain.ErrRejected.
//
// The prefix rule is domain.Principal.AllowsRef and shares its assumptions: no
// symbolic ref under refs/heads besides HEAD (ermite never creates one and
// receive-pack cannot), and case-sensitive ref storage (Linux). The hook sees
// every ref of the push, so a tag or any other ref is judged like a branch.
//
// The token of the push was valid when the request began, but a push can run
// for long: it is re-validated here, so a token revoked or expired meanwhile
// stops the push before Git applies it. A failure to tell refuses it too.
//
// The token of a CI job (GITHUB_TOKEN with contents: write) may not change
// workflows (CI-3, D3). The push is refused as a whole, on any ref, branch or
// tag, when a commit it brings changes .github/workflows (it has workflows that
// differ from those of every parent; removing them is allowed), or when a ref
// would end with workflows other than it had: an update with other than the old
// ones, a new ref with other than those of the default branch head (or any, when
// there is no head). A new branch cut from existing commits that change nothing
// is therefore allowed; deleting a ref is not a change. objects locate the
// objects of the push, which are not in the repository yet.
func (u *Pushes) Check(ctx context.Context, p domain.Principal, name domain.RepoName, updates []domain.RefUpdate, objects ports.PushObjects) error {
	if err := u.auth.Revalidate(ctx, p); err != nil {
		if errors.Is(err, domain.ErrUnauthenticated) {
			return fmt.Errorf("%w: the token is no longer valid", domain.ErrRejected)
		}
		return fmt.Errorf("revalidate token: %w", err)
	}
	if !canWrite(p, name) {
		return fmt.Errorf("%w: no write access to %s", domain.ErrRejected, name)
	}
	for _, up := range updates {
		// refs/pull/* belong to ermite (the head and the test merge of the merge
		// requests): nobody pushes there, whatever the token, the workflow
		// guard included.
		if domain.IsPullRefName(up.Ref) {
			return fmt.Errorf("%w: %s is maintained by ermite: refs/pull/* cannot be pushed", domain.ErrRejected, up.Ref)
		}
		if !p.AllowsRef(up.Ref) {
			return fmt.Errorf("%w: %s is outside the branches this token may push (%s)",
				domain.ErrRejected, up.Ref, strings.Join(p.BranchPrefixes(), ", "))
		}
	}
	if p.ViaJobToken() {
		if err := u.checkWorkflows(ctx, name, updates, objects); err != nil {
			return err
		}
	}
	// Last, once the push is accepted: who pushes the branches and tags is
	// recorded before Git moves them, so that a tip never has the record of an
	// earlier pusher at the same commit (domain.CIRefTip). A record that cannot
	// be written refuses the push, as it cannot be run later with a known trust.
	if err := recordRefTips(ctx, u.tips, name, updates, domain.AuthorName(p), domain.PusherOf(p), domain.TrustedActor(p)); err != nil {
		return err
	}
	log := zerowrap.FromCtx(ctx)
	log.Debug().Str("repo", name.String()).Int("refs", len(updates)).Msg("push checked")
	return nil
}

// workflowRefusal is GitHub's refusal of a push by the token of a job that
// changes a workflow (a push to a branch or a tag; the token lacks the
// "workflows" permission, which no job token can have).
const workflowRefusal = "refusing to allow a GitHub App to create or update workflow `%s` without `workflows` permission"

func (u *Pushes) checkWorkflows(ctx context.Context, name domain.RepoName, updates []domain.RefUpdate, objects ports.PushObjects) error {
	if u.trees == nil {
		return fmt.Errorf("%w: the token of a job may not push here", domain.ErrRejected)
	}
	t, err := u.trees.PushSubtrees(ctx, name, updates, objects, domain.WorkflowDir, domain.DefaultPushWalkBounds())
	if err != nil {
		return workflowReadError(name, err)
	}
	c, found, err := domain.WorkflowChangeOf(updates, t)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrRejected, err)
	}
	if !found {
		return nil
	}
	entry, err := u.trees.FirstDifferentEntry(ctx, name, objects, c.Old, c.New)
	if err != nil {
		return workflowReadError(name, err)
	}
	path := domain.WorkflowDir
	if entry != "" {
		path += "/" + entry
	}
	return fmt.Errorf("%w: "+workflowRefusal, domain.ErrRejected, path)
}

func workflowReadError(name domain.RepoName, err error) error {
	if errors.Is(err, domain.ErrInvalid) {
		return fmt.Errorf("%w: %v", domain.ErrRejected, err)
	}
	return fmt.Errorf("find the workflows changed by a push to %s: %w", name, err)
}

// Record is the post-receive step of an accepted push: it sets pushed_at and active_at,
// repairs HEAD when it points to a missing branch, tells the reader that the
// repository changed, queues the workflows the push triggers, publishes a
// PushEvent and schedules maintenance. Every
// step runs even when an earlier one fails; the failures are returned together
// and the push itself is not affected.
func (u *Pushes) Record(ctx context.Context, p domain.Principal, name domain.RepoName, updates []domain.RefUpdate) error {
	log := zerowrap.FromCtx(ctx)
	now := u.clock.Now().UTC()
	var errs []error

	repo, err := touchPushedAt(ctx, u.meta, name, now)
	if err != nil {
		errs = append(errs, err)
	}
	if err := u.fixDefaultBranch(ctx, name, updates); err != nil {
		errs = append(errs, err)
	}
	if u.fresh != nil {
		u.fresh.Refresh(ctx, name)
	}
	if u.ci != nil {
		u.ci.OnPush(ctx, name, updates, p) // queues only: never slows or fails the push
	}
	if u.mergeab != nil {
		u.mergeab.OnBranchesMoved(ctx, name, updates) // queues only
	}
	if u.links != nil {
		u.links.OnBranchesMoved(ctx, p, name, updates)
	}
	if u.releases != nil {
		u.releases.OnRefsMoved(ctx, name, updates)
	}
	u.bus.Publish(ctx, domain.PushEvent{
		Repo: name, Updates: append([]domain.RefUpdate(nil), updates...), At: now,
		Visibility: repo.Visibility, CreatedAt: repo.CreatedAt,
	})
	if u.mirror != nil {
		u.mirror.OnPush(ctx, name, updates) // marks only: never blocks
	}
	u.maint.Schedule(ctx, name)
	log.Info().
		Str("repo", name.String()).Int("refs", len(updates)).
		Str("principal", p.Label()).Str("token_id", p.TokenID()).
		Msg("push received")
	return errors.Join(errs...)
}

// touchPushedAt stores pushed_at and active_at, preserving later timestamps
// and retrying when another write got in between.
// A repository deleted meanwhile is not an error. It returns the repository
// as stored after mutation, or the zero value when it is unknown. A push
// (Pushes.Record) and a merge, which moves a branch too, both record through it.
func touchPushedAt(ctx context.Context, meta ports.RepoMetaStore, name domain.RepoName, now time.Time) (domain.Repo, error) {
	repo, err := meta.Mutate(ctx, name, func(repo *domain.Repo) (bool, error) {
		changed := false
		if now.After(repo.PushedAt) {
			repo.PushedAt = now
			changed = true
		}
		if now.After(repo.ActiveAt) {
			repo.ActiveAt = now
			changed = true
		}
		return changed, nil
	})
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Repo{}, nil
	}
	if err != nil {
		return repo, fmt.Errorf("update repository %q: %w", name, err)
	}
	return repo, nil
}

// fixDefaultBranch points HEAD to a branch that exists when it points to a
// missing one: main when it was pushed, else the first pushed branch.
func (u *Pushes) fixDefaultBranch(ctx context.Context, name domain.RepoName, updates []domain.RefUpdate) error {
	branch, exists, err := u.git.DefaultBranch(ctx, name)
	if errors.Is(err, domain.ErrNotFound) {
		return nil // deleted meanwhile
	}
	if err != nil {
		return fmt.Errorf("read default branch of %q: %w", name, err)
	}
	if exists {
		return nil
	}
	target := ""
	for _, up := range updates {
		b, ok := strings.CutPrefix(up.Ref, "refs/heads/")
		if !ok || up.IsDelete() {
			continue
		}
		if b == "main" {
			target = b
			break
		}
		if target == "" {
			target = b
		}
	}
	if target == "" || target == branch {
		return nil
	}
	err = u.git.SetDefaultBranch(ctx, name, target)
	if errors.Is(err, domain.ErrNotFound) {
		return nil // deleted meanwhile
	}
	if err != nil {
		return fmt.Errorf("set default branch of %q to %q: %w", name, target, err)
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("repo", name.String()).Str("branch", target).Msg("default branch set")
	return nil
}
