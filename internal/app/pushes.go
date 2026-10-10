// SPDX-License-Identifier: AGPL-3.0-or-later

package app

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

// MaintenanceScheduler queues repository maintenance. *Maintainer fits.
type MaintenanceScheduler interface {
	Schedule(name domain.RepoName)
}

// Pushes are the use cases around a Git push.
type Pushes struct {
	meta  ports.RepoMetaStore
	repos *Repos
	git   ports.GitRepos
	bus   ports.EventBus
	clock ports.Clock
	maint MaintenanceScheduler
}

// NewPushes builds the push use cases. maint may be nil, which disables
// maintenance after a push.
func NewPushes(meta ports.RepoMetaStore, repos *Repos, git ports.GitRepos, bus ports.EventBus, clock ports.Clock, maint MaintenanceScheduler) *Pushes {
	return &Pushes{meta: meta, repos: repos, git: git, bus: bus, clock: clock, maint: maint}
}

// Open decides whether the principal may push to the repository and, with
// create, creates it when it does not exist yet (private, published as
// RepoCreated). create is for the first request of a push only.
//
// Anonymous callers get domain.ErrUnauthenticated. A repository the caller
// cannot read and cannot write, or a missing one it may not create, answers
// domain.ErrNotFound, so that a private repository never shows itself. One it
// can read but not write answers domain.ErrForbidden.
func (u *Pushes) Open(ctx context.Context, p domain.Principal, name domain.RepoName, create bool) (domain.Repo, bool, error) {
	if p.IsAnonymous() {
		return domain.Repo{}, false, domain.ErrUnauthenticated
	}
	repo, err := u.meta.Get(ctx, name)
	switch {
	case err == nil:
		return u.openExisting(p, repo)
	case !errors.Is(err, domain.ErrNotFound):
		return domain.Repo{}, false, fmt.Errorf("get repository %q: %w", name, err)
	}

	if !create || !canWrite(p, name) {
		return domain.Repo{}, false, domain.ErrNotFound
	}
	repo, err = u.repos.Create(ctx, p, CreateParams{Name: name, Visibility: domain.VisibilityPrivate})
	switch {
	case err == nil:
		u.bus.Publish(ctx, domain.RepoCreated{Repo: repo.Name, Visibility: repo.Visibility, At: repo.CreatedAt})
		return repo, true, nil
	case errors.Is(err, domain.ErrAlreadyExists):
		// A concurrent first push won the race: this one pushes to its repository.
		repo, gerr := u.meta.Get(ctx, name)
		if gerr != nil {
			return domain.Repo{}, false, fmt.Errorf("get repository %q: %w", name, gerr)
		}
		return u.openExisting(p, repo)
	}
	return domain.Repo{}, false, err
}

func (u *Pushes) openExisting(p domain.Principal, repo domain.Repo) (domain.Repo, bool, error) {
	switch {
	case canWrite(p, repo.Name):
		return repo, false, nil
	case canRead(p, repo):
		return domain.Repo{}, false, domain.ErrForbidden
	}
	return domain.Repo{}, false, domain.ErrNotFound
}

// Check is the pre-receive decision. v1 accepts every push of a principal that
// may write the repository; protected branches and limits will refuse here by
// returning an error that matches domain.ErrRejected.
func (u *Pushes) Check(ctx context.Context, p domain.Principal, name domain.RepoName, updates []domain.RefUpdate) error {
	if !canWrite(p, name) {
		return fmt.Errorf("%w: no write access to %s", domain.ErrRejected, name)
	}
	log := zerowrap.FromCtx(ctx)
	log.Debug().Str("repo", name.String()).Int("refs", len(updates)).Msg("push checked")
	return nil
}

// Record is the post-receive step of an accepted push: it sets pushed_at,
// repairs HEAD when it points to a missing branch, publishes a PushEvent and
// schedules maintenance. Every step runs even when an earlier one fails; the
// failures are returned together and the push itself is not affected.
func (u *Pushes) Record(ctx context.Context, p domain.Principal, name domain.RepoName, updates []domain.RefUpdate) error {
	log := zerowrap.FromCtx(ctx)
	now := u.clock.Now().UTC()
	var errs []error

	if err := u.touchPushedAt(ctx, name, now); err != nil {
		errs = append(errs, err)
	}
	if err := u.fixDefaultBranch(ctx, name, updates); err != nil {
		errs = append(errs, err)
	}
	u.bus.Publish(ctx, domain.PushEvent{Repo: name, Updates: append([]domain.RefUpdate(nil), updates...), At: now})
	if u.maint != nil {
		u.maint.Schedule(name)
	}
	log.Info().
		Str("repo", name.String()).Int("refs", len(updates)).
		Str("principal", p.Label()).Str("token_id", p.TokenID()).
		Msg("push received")
	return errors.Join(errs...)
}

// touchPushedAt stores pushed_at, retrying when another write got in between.
// A repository deleted meanwhile is not an error.
func (u *Pushes) touchPushedAt(ctx context.Context, name domain.RepoName, now time.Time) error {
	for range pushRetries {
		repo, err := u.meta.Get(ctx, name)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("get repository %q: %w", name, err)
		}
		if !now.After(repo.PushedAt) {
			return nil // a later push already recorded
		}
		repo.PushedAt = now
		err = u.meta.Update(ctx, repo)
		if errors.Is(err, domain.ErrConflict) {
			continue
		}
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("update repository %q: %w", name, err)
		}
		return nil
	}
	return fmt.Errorf("update pushed_at of %q: %w", name, domain.ErrConflict)
}

// fixDefaultBranch points HEAD to a branch that exists when it points to a
// missing one: main when it was pushed, else the first pushed branch.
func (u *Pushes) fixDefaultBranch(ctx context.Context, name domain.RepoName, updates []domain.RefUpdate) error {
	branch, exists, err := u.git.DefaultBranch(ctx, name)
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
	if err := u.git.SetDefaultBranch(ctx, name, target); err != nil {
		return fmt.Errorf("set default branch of %q to %q: %w", name, target, err)
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("repo", name.String()).Str("branch", target).Msg("default branch set")
	return nil
}
