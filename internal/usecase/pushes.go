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

// pushRetries bounds the retries of a metadata update that loses a race.
const pushRetries = 5

// Pushes are the use cases around a Git push.
type Pushes struct {
	meta  ports.RepoMetaStore
	repos *Repos
	git   ports.GitRepos
	bus   ports.EventBus
	clock ports.Clock
	maint ports.MaintenanceScheduler
	auth  ports.Revalidator
	fresh ports.RepoFreshener   // nil: the reader needs no telling
	ci    ports.WorkflowTrigger // nil: no CI
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

	if !create || !canWrite(p, name) {
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

// Check is the pre-receive decision. v1 accepts every push of a principal that
// may write the repository; protected branches and limits will refuse here by
// returning an error that matches domain.ErrRejected.
//
// The token of the push was valid when the request began, but a push can run
// for long: it is re-validated here, so a token revoked or expired meanwhile
// stops the push before Git applies it. A failure to tell refuses it too.
func (u *Pushes) Check(ctx context.Context, p domain.Principal, name domain.RepoName, updates []domain.RefUpdate) error {
	if err := u.auth.Revalidate(ctx, p); err != nil {
		if errors.Is(err, domain.ErrUnauthenticated) {
			return fmt.Errorf("%w: the token is no longer valid", domain.ErrRejected)
		}
		return fmt.Errorf("revalidate token: %w", err)
	}
	if !canWrite(p, name) {
		return fmt.Errorf("%w: no write access to %s", domain.ErrRejected, name)
	}
	log := zerowrap.FromCtx(ctx)
	log.Debug().Str("repo", name.String()).Int("refs", len(updates)).Msg("push checked")
	return nil
}

// Record is the post-receive step of an accepted push: it sets pushed_at,
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
	u.bus.Publish(ctx, domain.PushEvent{
		Repo: name, Updates: append([]domain.RefUpdate(nil), updates...), At: now,
		Visibility: repo.Visibility, CreatedAt: repo.CreatedAt,
	})
	u.maint.Schedule(ctx, name)
	log.Info().
		Str("repo", name.String()).Int("refs", len(updates)).
		Str("principal", p.Label()).Str("token_id", p.TokenID()).
		Msg("push received")
	return errors.Join(errs...)
}

// touchPushedAt stores pushed_at, retrying when another write got in between.
// A repository deleted meanwhile is not an error. It returns the repository
// as read, or the zero value when it is unknown. A push (Pushes.Record) and a
// merge, which moves a branch too, both record through it.
func touchPushedAt(ctx context.Context, meta ports.RepoMetaStore, name domain.RepoName, now time.Time) (domain.Repo, error) {
	for range pushRetries {
		repo, err := meta.Get(ctx, name)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.Repo{}, nil
		}
		if err != nil {
			return domain.Repo{}, fmt.Errorf("get repository %q: %w", name, err)
		}
		if !now.After(repo.PushedAt) {
			return repo, nil // a later push already recorded
		}
		repo.PushedAt = now
		err = meta.Update(ctx, repo)
		if errors.Is(err, domain.ErrConflict) {
			continue
		}
		if errors.Is(err, domain.ErrNotFound) {
			return domain.Repo{}, nil
		}
		if err != nil {
			return repo, fmt.Errorf("update repository %q: %w", name, err)
		}
		return repo, nil
	}
	return domain.Repo{}, fmt.Errorf("update pushed_at of %q: %w", name, domain.ErrConflict)
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
