// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ermitegit implements ports.RepoReader over the ermite-git library.
//
// The adapter wraps the gitcli reader. Each method has a route, a constant
// below: the methods that match gitcli in the shared conformance table and
// beat it in the benchmarks are served by ermite-git, the others (and
// GetCommit) by the wrapped reader. A repository that ermite-git cannot read
// (ErrUnsupported: SHA-256, reftable, alternates, replace refs...) is served
// by the wrapped reader for every method.
//
// Any other error of the library is returned, never hidden by a fallback.
// The exceptions are decisions about the data, not failures: a commit whose
// message is not in UTF-8 (git converts it, the library does not) and an
// object larger than the library's in-memory bound are read by the wrapped
// reader.
//
// Freshness: a read that begins after a push has returned sees the push,
// whichever path made it, because the library reads refs from disk on each
// call (loose refs always, packed-refs after a stat) and rescans the packs
// when an object is missing (TestFreshness proves it with no Refresh at all).
// The adapter still calls Refresh to release replaced packs, pick up a new
// commit-graph and notice a repository that stopped being supported: at once
// when ermite itself wrote (the push and the maintenance tell it through
// Refresh; a merge request merge waits for its maintenance), and as a safety
// net at most once per Config.RefreshEvery for each repository while it is
// read, by one reader at a time. A Refresh stats the files of the repository,
// too much to pay on every read of a method that itself costs 2 µs.
//
// Memory: the cache of open repositories is bounded (LRU) and drops the ones
// idle longer than Config.IdleTTL; each repository holds at most
// Config.DeltaCacheSize of delta bases and reads objects up to
// Config.MaxObjectSize into memory. gitcli's Trash tells the adapter to drop
// a deleted repository (see Forget).
package ermitegit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	git "bnema.dev/ermite-git"
	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

var (
	_ ports.RepoReader    = (*Adapter)(nil)
	_ ports.RepoFreshener = (*Adapter)(nil)
)

// ErrClosed is returned by reads after Close.
var ErrClosed = errors.New("ermitegit: reader is closed")

// Backend says which reader serves a method.
type Backend uint8

// Backends.
const (
	// CLI is the git binary, through the gitcli reader.
	CLI Backend = iota
	// ErmiteGit is the ermite-git library.
	ErmiteGit
)

// String returns "cli" or "ermite-git".
func (b Backend) String() string {
	if b == ErmiteGit {
		return "ermite-git"
	}
	return "cli"
}

// Routes says which reader serves each method. GetCommit is not here: it is
// always served by the wrapped reader.
type Routes struct {
	ListRefs Backend
	// ResolveRevision is a revision that is a name or a full id.
	ResolveRevision Backend
	// ResolveRevisionAbbrev is a revision that is an abbreviated id.
	ResolveRevisionAbbrev Backend
	ResolveRefPath        Backend
	// GetTree is a directory listing without last commits.
	GetTree Backend
	// GetTreeLastCommits is the listing of the root with the last commit of
	// each entry.
	GetTreeLastCommits Backend
	// GetTreeSubdirLastCommits is the same for a subdirectory, which costs a
	// much longer history walk (195 ms with git on lazygit).
	GetTreeSubdirLastCommits Backend
	OpenBlob                 Backend
	// ListCommits is a page of history of a revision.
	ListCommits Backend
	// ListCommitsPath is a page of the history of a file or a directory.
	ListCommitsPath Backend
	// ListPaths is the recursive list of the file paths of a revision.
	ListPaths Backend
}

// tree picks the route of a directory listing.
func (r Routes) tree(path domain.TreePath, lastCommits bool) Backend {
	switch {
	case !lastCommits:
		return r.GetTree
	case path.IsRoot():
		return r.GetTreeLastCommits
	default:
		return r.GetTreeSubdirLastCommits
	}
}

// AllRoutes returns routes that send every method to b. The tests and the
// benchmarks use it to exercise a backend whatever the production routes are.
func AllRoutes(b Backend) Routes {
	return Routes{b, b, b, b, b, b, b, b, b, b, b}
}

// Config configures an Adapter.
type Config struct {
	// Fallback serves the methods that are not routed to ermite-git and the
	// repositories ermite-git cannot read. Required.
	Fallback ports.RepoReader
	// RepoDir returns the directory of a repository. It does not check that it
	// exists. Required.
	RepoDir func(domain.RepoName) string
	// RefreshEvery is the longest a repository goes without a Refresh of the
	// library while it is read. Zero means DefaultRefreshEvery; a negative
	// value refreshes before every read. It does not affect what a read
	// sees: see the package comment.
	RefreshEvery time.Duration
	// MaxRepos bounds the repositories kept open; zero means DefaultMaxRepos.
	MaxRepos int
	// IdleTTL is how long a repository may go unused before it is closed.
	// Zero means DefaultIdleTTL; a negative value keeps repositories open
	// until evicted.
	IdleTTL time.Duration
	// DeltaCacheSize is the budget in bytes of the cache of delta bases of
	// each open repository. Zero means DefaultDeltaCacheSize; a negative value
	// disables the cache.
	DeltaCacheSize int64
	// MaxObjectSize is the size in bytes above which an object that must be
	// held in memory is read by the wrapped reader instead. Zero means
	// DefaultMaxObjectSize; a negative value means the library default
	// (128 MiB).
	MaxObjectSize int64
	// Routes overrides DefaultRoutes when not nil.
	Routes *Routes
}

// Adapter is a ports.RepoReader that routes each method to ermite-git or to
// the wrapped reader.
type Adapter struct {
	cli    ports.RepoReader
	dirOf  func(domain.RepoName) string
	routes Routes
	cache  *cache

	logged sync.Map // dir -> struct{}: the unsupported repositories already logged
}

// New builds an adapter.
func New(cfg Config) *Adapter {
	routes := DefaultRoutes()
	if cfg.Routes != nil {
		routes = *cfg.Routes
	}
	refreshEvery := cfg.RefreshEvery
	if refreshEvery == 0 {
		refreshEvery = DefaultRefreshEvery
	}
	idleTTL := cfg.IdleTTL
	if idleTTL == 0 {
		idleTTL = DefaultIdleTTL
	}
	deltaCache := cfg.DeltaCacheSize
	if deltaCache == 0 {
		deltaCache = DefaultDeltaCacheSize
	}
	maxObject := cfg.MaxObjectSize
	if maxObject == 0 {
		maxObject = DefaultMaxObjectSize
	}
	a := &Adapter{cli: cfg.Fallback, dirOf: cfg.RepoDir, routes: routes}
	a.cache = newCache(cacheConfig{
		max: cfg.MaxRepos, refreshEvery: refreshEvery, idleTTL: idleTTL,
		openOpts:    []git.Option{git.WithDeltaCacheSize(deltaCache), git.WithMaxObjectSize(maxObject)},
		onUnsupport: a.logUnsupported,
	})
	return a
}

// Close closes the open repositories. Reads after Close fail with ErrClosed;
// blob streams already returned stay valid until they are closed. The wrapped
// reader is not closed.
func (a *Adapter) Close() { a.cache.close() }

// Forget drops the open repository of dir, which was moved away or deleted.
func (a *Adapter) Forget(dir string) {
	a.cache.forget(dir)
	a.logged.Delete(dir)
}

// Refresh tells the adapter that ermite wrote to the repository (a push, the
// maintenance): the library refreshes it now, if it is open, instead of at the
// next read after RefreshEvery. A repository that is not open has nothing to
// refresh. A failure is logged, not returned: reads do not depend on it.
func (a *Adapter) Refresh(ctx context.Context, repo domain.RepoName) {
	if err := a.cache.refreshNow(ctx, repo, a.dirOf(repo)); err != nil {
		log := zerowrap.FromCtx(ctx)
		log.Warn().Err(err).Str("repo", repo.String()).Msg("refresh of the open repository failed")
	}
}

// OpenRepos returns the number of repositories in the cache, for tests.
func (a *Adapter) OpenRepos() int { return a.cache.size() }

// logUnsupported logs, once per directory, that a repository is served by git.
func (a *Adapter) logUnsupported(ctx context.Context, dir string, name domain.RepoName, cause error) {
	if _, seen := a.logged.LoadOrStore(dir, struct{}{}); seen {
		return
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("repo", name.String()).Str("reason", cause.Error()).Msg("repository is not readable by ermite-git, served by git")
}

// errFallback makes serve hand the call to the wrapped reader.
var errFallback = errors.New("ermitegit: served by git")

// serve runs fn on the open repository, or cli when the repository is
// unsupported, when fn asks for it, or when the library refuses on a bound.
func serve[T any](ctx context.Context, a *Adapter, repo domain.RepoName, fn func(*git.Repo) (T, error), cli func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	h, err := a.cache.get(ctx, repo, a.dirOf(repo))
	if err != nil {
		return zero, err
	}
	defer h.release()
	if h.repo == nil {
		return cli()
	}
	v, err := fn(h.repo)
	switch {
	case err == nil:
		return v, nil
	case errors.Is(err, errFallback), errors.Is(err, git.ErrTooLarge):
		return cli()
	case errors.Is(err, git.ErrUnsupported):
		h.fallBack(ctx, repo, err)
		return cli()
	}
	return zero, mapErr(ctx, err)
}

// mapErr turns the errors of the library into the ones of the port. A
// cancelled context stays itself.
func mapErr(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil && errors.Is(err, ctx.Err()):
		return err
	case errors.Is(err, domain.ErrNotFound), errors.Is(err, domain.ErrNotFile), errors.Is(err, domain.ErrNotDirectory):
		return err // already one of the port's
	case errors.Is(err, git.ErrNotFound), errors.Is(err, git.ErrAmbiguous):
		// gitcli reads an ambiguous abbreviation as a missing object.
		return fmt.Errorf("%w: %w", domain.ErrNotFound, err)
	case errors.Is(err, git.ErrInvalidPath):
		return fmt.Errorf("%w: %w", domain.ErrInvalidPath, err)
	case errors.Is(err, git.ErrInvalidID):
		return fmt.Errorf("%w: %w", domain.ErrInvalidObjectID, err)
	case errors.Is(err, git.ErrClosed):
		return fmt.Errorf("%w: %w", ErrClosed, err)
	}
	return err
}

// ListRefs lists HEAD, the branches and the tags.
func (a *Adapter) ListRefs(ctx context.Context, repo domain.RepoName) ([]domain.Ref, error) {
	cli := func() ([]domain.Ref, error) { return a.cli.ListRefs(ctx, repo) }
	if a.routes.ListRefs == CLI {
		return cli()
	}
	return serve(ctx, a, repo, func(r *git.Repo) ([]domain.Ref, error) { return listRefs(r) }, cli)
}

// ResolveRevision turns a revision into a commit.
//
//hexcheck:ignore suspicious-business-logic-in-adapter routes a call by a code constant; rules and limits come from domain and ports
func (a *Adapter) ResolveRevision(ctx context.Context, repo domain.RepoName, rev domain.Revision) (domain.ResolvedRevision, error) {
	cli := func() (domain.ResolvedRevision, error) { return a.cli.ResolveRevision(ctx, repo, rev) }
	route := a.routes.ResolveRevision
	if rev.IsAbbrevID() {
		route = a.routes.ResolveRevisionAbbrev
	}
	if route == CLI {
		return cli()
	}
	return serve(ctx, a, repo, func(r *git.Repo) (domain.ResolvedRevision, error) {
		res, err := resolveRevision(r, rev)
		if err != nil {
			return domain.ResolvedRevision{}, fmt.Errorf("resolve %q in %q: %w", rev, repo, err)
		}
		return res.domain()
	}, cli)
}

// ResolveRefPath splits a path into a revision and the rest.
//
//hexcheck:ignore suspicious-business-logic-in-adapter routes a call by a code constant; rules and limits come from domain and ports
func (a *Adapter) ResolveRefPath(ctx context.Context, repo domain.RepoName, path string) (domain.RefPath, error) {
	cli := func() (domain.RefPath, error) { return a.cli.ResolveRefPath(ctx, repo, path) }
	if a.routes.ResolveRefPath == CLI {
		return cli()
	}
	return serve(ctx, a, repo, func(r *git.Repo) (domain.RefPath, error) {
		out, err := domain.SplitRefPath(path, func(rev domain.Revision) (domain.ResolvedRevision, error) {
			res, err := resolveRevision(r, rev)
			if err != nil {
				return domain.ResolvedRevision{}, mapErr(ctx, err)
			}
			return res.domain()
		})
		if err != nil {
			return domain.RefPath{}, fmt.Errorf("resolve path %q in %q: %w", path, repo, err)
		}
		return out, nil
	}, cli)
}

// ListPaths lists the file and symbolic link paths of a revision.
func (a *Adapter) ListPaths(ctx context.Context, repo domain.RepoName, rev domain.Revision, maxPaths, maxBytes int) (domain.PathList, error) {
	cli := func() (domain.PathList, error) { return a.cli.ListPaths(ctx, repo, rev, maxPaths, maxBytes) }
	if a.routes.ListPaths == CLI {
		return cli()
	}
	return serve(ctx, a, repo, func(r *git.Repo) (domain.PathList, error) {
		list, err := listPaths(ctx, r, rev, maxPaths, maxBytes)
		if err != nil {
			return domain.PathList{}, fmt.Errorf("paths of %q: %w", repo, err)
		}
		return list, nil
	}, cli)
}

// GetTree lists a directory.
func (a *Adapter) GetTree(ctx context.Context, repo domain.RepoName, rev domain.Revision, path domain.TreePath, includeLastCommit bool) (domain.Tree, error) {
	cli := func() (domain.Tree, error) { return a.cli.GetTree(ctx, repo, rev, path, includeLastCommit) }
	if a.routes.tree(path, includeLastCommit) == CLI {
		return cli()
	}
	return serve(ctx, a, repo, func(r *git.Repo) (domain.Tree, error) {
		tree, err := getTree(ctx, r, rev, path, includeLastCommit)
		if err != nil {
			return domain.Tree{}, fmt.Errorf("tree %q of %q: %w", path, repo, err)
		}
		return tree, nil
	}, cli)
}

// OpenBlob describes a file and returns its content as a stream.
func (a *Adapter) OpenBlob(ctx context.Context, repo domain.RepoName, rev domain.Revision, path domain.TreePath) (domain.BlobInfo, io.ReadCloser, error) {
	type result struct {
		info domain.BlobInfo
		rc   io.ReadCloser
	}
	cli := func() (result, error) {
		info, rc, err := a.cli.OpenBlob(ctx, repo, rev, path)
		return result{info, rc}, err
	}
	if a.routes.OpenBlob == CLI {
		res, err := cli()
		return res.info, res.rc, err
	}
	res, err := serve(ctx, a, repo, func(r *git.Repo) (result, error) {
		info, rc, err := openBlob(ctx, r, rev, path)
		if err != nil {
			return result{}, fmt.Errorf("blob %q of %q: %w", path, repo, err)
		}
		return result{info, rc}, nil
	}, cli)
	return res.info, res.rc, err
}

// ListCommits returns a page of history.
//
//hexcheck:ignore suspicious-business-logic-in-adapter routes a call by a code constant; rules and limits come from domain and ports
func (a *Adapter) ListCommits(ctx context.Context, repo domain.RepoName, q ports.CommitQuery) (ports.CommitPage, error) {
	if err := q.Validate(); err != nil {
		return ports.CommitPage{}, err
	}
	cli := func() (ports.CommitPage, error) { return a.cli.ListCommits(ctx, repo, q) }
	route := a.routes.ListCommits
	if !q.Path.IsRoot() {
		route = a.routes.ListCommitsPath
	}
	if route == CLI {
		return cli()
	}
	return serve(ctx, a, repo, func(r *git.Repo) (ports.CommitPage, error) {
		page, err := listCommits(ctx, r, q)
		if err != nil {
			return ports.CommitPage{}, fmt.Errorf("history of %q: %w", repo, err)
		}
		return page, nil
	}, cli)
}

// GetCommit is served by the wrapped reader: the diff stays on git.
func (a *Adapter) GetCommit(ctx context.Context, repo domain.RepoName, rev domain.Revision, limits domain.DiffLimits) (domain.CommitDetail, error) {
	return a.cli.GetCommit(ctx, repo, rev, limits)
}

// CountCommits is served by the wrapped reader: git counts from the commit
// graph.
func (a *Adapter) CountCommits(ctx context.Context, repo domain.RepoName, head domain.ObjectID, since time.Time) (int, error) {
	return a.cli.CountCommits(ctx, repo, head, since)
}

// LatestTag is served by the wrapped reader.
func (a *Adapter) LatestTag(ctx context.Context, repo domain.RepoName) (domain.TagInfo, bool, error) {
	return a.cli.LatestTag(ctx, repo)
}
