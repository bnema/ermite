// SPDX-License-Identifier: AGPL-3.0-or-later

// Package usecase holds the use cases of ermite. It depends on the domain and on
// the ports only.
package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Repos are the repository use cases.
type Repos struct {
	meta  ports.RepoMetaStore
	git   ports.GitRepos
	bus   ports.EventBus
	clock ports.Clock
	ci    ports.CIRunCleaner // nil: no CI logs to remove
}

var _ ports.RepoService = (*Repos)(nil)

// NewRepos builds the repository use cases over their ports. Changes are
// published on bus.
func NewRepos(meta ports.RepoMetaStore, git ports.GitRepos, bus ports.EventBus, clock ports.Clock) *Repos {
	return &Repos{meta: meta, git: git, bus: bus, clock: clock}
}

// WithCIRunCleaner makes Delete remove the CI logs of the repository. Call it
// before the first request; it returns r.
func (r *Repos) WithCIRunCleaner(c ports.CIRunCleaner) *Repos {
	r.ci = c
	return r
}

// Create makes a repository and publishes RepoCreated: it stages a bare repository, records the
// metadata (which fails with domain.ErrAlreadyExists if the name is taken,
// also when a concurrent Create wins) and publishes the directory. A failure
// after the metadata is written removes it again and discards the staged
// repository.
//
// The principal needs repo:write on the name and no branch prefixes: anonymous
// callers get domain.ErrUnauthenticated, other callers without it (a token
// limited to some branches included) domain.ErrForbidden.
func (r *Repos) Create(ctx context.Context, p domain.Principal, params ports.CreateRepoParams) (domain.Repo, error) {
	if params.Name.IsZero() {
		return domain.Repo{}, fmt.Errorf("%w: empty name", domain.ErrInvalidName)
	}
	if p.IsAnonymous() {
		return domain.Repo{}, domain.ErrUnauthenticated
	}
	if !canCreateRepo(p, params.Name) {
		return domain.Repo{}, domain.ErrForbidden
	}
	if err := validateDescription(params.Description); err != nil {
		return domain.Repo{}, err
	}
	vis := params.Visibility
	if vis == "" {
		vis = domain.VisibilityPrivate
	}
	if _, err := domain.ParseVisibility(vis.String()); err != nil {
		return domain.Repo{}, err
	}
	repo := domain.Repo{
		Name:        params.Name,
		Description: params.Description,
		Visibility:  vis,
		CreatedAt:   r.clock.Now().UTC(),
		Settings:    domain.DefaultRepoSettings(),
	}

	log := zerowrap.FromCtx(ctx)
	// Cleanup must finish even when the caller gives up.
	cleanup := context.WithoutCancel(ctx)

	id, err := r.git.Stage(ctx)
	if err != nil {
		return domain.Repo{}, fmt.Errorf("stage repository %q: %w", repo.Name, err)
	}
	if err := r.meta.Create(ctx, repo); err != nil {
		r.discard(cleanup, id)
		if errors.Is(err, domain.ErrAlreadyExists) {
			return domain.Repo{}, fmt.Errorf("repository %q: %w", repo.Name, domain.ErrAlreadyExists)
		}
		return domain.Repo{}, fmt.Errorf("store repository %q: %w", repo.Name, err)
	}
	if err := r.git.Publish(ctx, id, repo.Name); err != nil {
		if derr := r.meta.Delete(cleanup, repo.Name); derr != nil {
			log.Error().Err(derr).Str("repo", repo.Name.String()).Msg("remove metadata after failed publish")
		}
		r.discard(cleanup, id)
		if errors.Is(err, domain.ErrAlreadyExists) {
			return domain.Repo{}, fmt.Errorf("repository %q: directory %w", repo.Name, domain.ErrAlreadyExists)
		}
		return domain.Repo{}, fmt.Errorf("publish repository %q: %w", repo.Name, err)
	}
	log.Info().Str("repo", repo.Name.String()).Str("visibility", vis.String()).Msg("repository created")
	r.bus.Publish(ctx, domain.RepoCreated{Repo: repo.Name, Visibility: repo.Visibility, At: repo.CreatedAt})
	return repo, nil
}

func (r *Repos) discard(ctx context.Context, id ports.StagedID) {
	if err := r.git.Discard(ctx, id); err != nil {
		log := zerowrap.FromCtx(ctx)
		log.Error().Err(err).Str("staged", string(id)).Msg("discard staged repository")
	}
}

// lookupRepo reads the metadata of a repository: domain.ErrInvalidName for the
// zero name, domain.ErrNotFound for a missing repository. Access rules are the
// caller's.
func lookupRepo(ctx context.Context, meta ports.RepoMetaStore, name domain.RepoName) (domain.Repo, error) {
	if name.IsZero() {
		return domain.Repo{}, fmt.Errorf("%w: empty name", domain.ErrInvalidName)
	}
	return meta.Get(ctx, name)
}

// Get returns a repository. A repository the principal may not read answers
// domain.ErrNotFound, like a missing one.
func (r *Repos) Get(ctx context.Context, p domain.Principal, name domain.RepoName) (domain.Repo, error) {
	repo, err := lookupRepo(ctx, r.meta, name)
	if err != nil {
		return domain.Repo{}, err
	}
	if !canRead(p, repo) {
		return domain.Repo{}, domain.ErrNotFound
	}
	return repo, nil
}

// List returns the repositories the principal may read, ordered by name.
func (r *Repos) List(ctx context.Context, p domain.Principal) ([]domain.Repo, error) {
	all, err := r.meta.List(ctx)
	if err != nil {
		return nil, err
	}
	visible := make([]domain.Repo, 0, len(all))
	for _, repo := range all {
		if canRead(p, repo) {
			visible = append(visible, repo)
		}
	}
	return visible, nil
}

// ReconcileReport tells what Reconcile found.
type ReconcileReport struct {
	// Registered are directories without metadata, now registered private.
	Registered []domain.RepoName
	// MissingDirs are repositories with metadata and no directory. They are
	// left untouched.
	MissingDirs []domain.RepoName
	// InvalidDirs are directory entries that cannot be repositories.
	InvalidDirs []string
}

// Reconcile aligns the repository directories with the metadata at startup:
// it removes staging leftovers, registers a directory without metadata as
// private, skips invalid directory names, and reports metadata without a
// directory at error level without touching it.
func (r *Repos) Reconcile(ctx context.Context) (ReconcileReport, error) {
	log := zerowrap.FromCtx(ctx)
	var rep ReconcileReport

	if err := r.git.PurgeStaging(ctx); err != nil {
		return rep, fmt.Errorf("purge staging: %w", err)
	}
	dirs, err := r.git.List(ctx)
	if err != nil {
		return rep, fmt.Errorf("list repository directories: %w", err)
	}
	metas, err := r.meta.List(ctx)
	if err != nil {
		return rep, fmt.Errorf("list repository metadata: %w", err)
	}

	for _, entry := range dirs.Invalid {
		log.Warn().Str("entry", entry).Msg("skip directory that is not a valid repository name")
	}
	rep.InvalidDirs = dirs.Invalid

	hasMeta := make(map[domain.RepoName]bool, len(metas))
	for _, m := range metas {
		hasMeta[m.Name] = true
	}
	hasDir := make(map[domain.RepoName]bool, len(dirs.Names))
	for _, name := range dirs.Names {
		hasDir[name] = true
	}

	for _, name := range dirs.Names {
		if hasMeta[name] {
			continue
		}
		repo := domain.Repo{
			Name:       name,
			Visibility: domain.VisibilityPrivate,
			CreatedAt:  r.clock.Now().UTC(),
			Settings:   domain.DefaultRepoSettings(),
		}
		if err := r.meta.Create(ctx, repo); err != nil && !errors.Is(err, domain.ErrAlreadyExists) {
			return rep, fmt.Errorf("register repository %q: %w", name, err)
		}
		log.Warn().Str("repo", name.String()).Msg("registered repository directory without metadata as private")
		rep.Registered = append(rep.Registered, name)
	}
	for _, m := range metas {
		if !hasDir[m.Name] {
			log.Error().Str("repo", m.Name.String()).Msg("repository metadata without a directory, left untouched")
			rep.MissingDirs = append(rep.MissingDirs, m.Name)
		}
	}
	return rep, nil
}
