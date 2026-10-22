// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Releases manages release metadata and blob contents. Construct once per server.
type Releases struct {
	store   ports.ReleaseStore
	blobs   ports.BlobStore
	repos   ports.RepoMetaStore
	reader  ports.RepoReader
	tags    ports.ReleaseTagWriter
	pushes  ports.PushHookService
	bus     ports.EventBus
	clock   ports.Clock
	limits  ports.ReleaseLimits
	uploads chan struct{}
	sweepMu sync.Mutex
	misses  map[int64]int
}

var _ ports.ReleaseService = (*Releases)(nil)
var _ ports.ReleaseTagTrigger = (*Releases)(nil)
var _ ports.ReleaseRepoCleaner = (*Releases)(nil)
var _ ports.ReleaseSweeper = (*Releases)(nil)

// NewReleases constructs the release service. Zero limits select the defaults.
// pushes must be the shared Pushes instance, so created tags notify every hook.
func NewReleases(store ports.ReleaseStore, blobs ports.BlobStore, repos ports.RepoMetaStore, reader ports.RepoReader, tags ports.ReleaseTagWriter, pushes ports.PushHookService, bus ports.EventBus, clock ports.Clock, limits ports.ReleaseLimits) *Releases {
	if limits.AssetMax == 0 {
		limits.AssetMax = domain.DefaultReleaseAssetMax
	}
	if limits.AssetsMax == 0 {
		limits.AssetsMax = domain.DefaultReleaseAssetsMax
	}
	if limits.Quota == 0 {
		limits.Quota = domain.DefaultReleaseQuota
	}
	return &Releases{store: store, blobs: blobs, repos: repos, reader: reader, tags: tags, pushes: pushes, bus: bus, clock: clock, limits: limits, uploads: make(chan struct{}, 16), misses: make(map[int64]int)}
}

func (r *Releases) readable(ctx context.Context, p domain.Principal, name domain.RepoName) (domain.Repo, error) {
	repo, err := lookupRepo(ctx, r.repos, name)
	if err == nil && !canRead(p, repo) {
		err = domain.ErrNotFound
	}
	return repo, err
}
func releaseWriter(p domain.Principal, name domain.RepoName) bool {
	return !p.IsBranchLimited() && (p.Allows(domain.ScopeReleaseWrite, name) || p.JobPermits(domain.PermContents, domain.PermissionWrite, name))
}
func (r *Releases) writable(ctx context.Context, p domain.Principal, name domain.RepoName) (domain.Repo, error) {
	repo, err := r.readable(ctx, p, name)
	if err != nil {
		return repo, err
	}
	if p.IsBranchLimited() {
		return repo, fmt.Errorf("%w: a branch-limited token cannot manage releases", domain.ErrForbidden)
	}
	if releaseWriter(p, name) {
		return repo, nil
	}
	if p.IsAnonymous() {
		return repo, domain.ErrUnauthenticated
	}
	return repo, domain.ErrForbidden
}
func (r *Releases) updateReleases(ctx context.Context, repo domain.RepoName, fn func(*ports.ReleaseWrite) error) error {
	const releaseTries = 256
	for range releaseTries {
		if err := ctx.Err(); err != nil {
			return err
		}
		used, v, err := r.store.ReleaseUsage(ctx, repo)
		if err != nil {
			return err
		}
		w := ports.ReleaseWrite{Repo: repo, Used: used, UsageVersion: v}
		if err = fn(&w); err != nil {
			return err
		}
		if len(w.PutReleases) > 0 || len(w.DeleteReleases) > 0 {
			w.SetLatest = true
			all, err := r.store.ListReleases(ctx, repo)
			if err != nil {
				return err
			}
			byID := make(map[int64]domain.Release, len(all)+len(w.PutReleases))
			for _, rel := range all {
				byID[rel.ID] = rel
			}
			for _, rel := range w.DeleteReleases {
				delete(byID, rel.ID)
			}
			for _, rel := range w.PutReleases {
				byID[rel.ID] = rel
			}
			all = all[:0]
			for _, rel := range byID {
				all = append(all, rel)
			}
			if latest, ok := domain.LatestRelease(all); ok {
				w.LatestID = latest.ID
			}
		}
		err = r.store.ApplyReleases(ctx, w)
		if errors.Is(err, domain.ErrConflict) {
			continue
		}
		return err
	}
	return fmt.Errorf("%w: releases of %s keep changing, try again", domain.ErrUnavailable, repo)
}
func (r *Releases) resolve(ctx context.Context, repo domain.RepoName, rev string) (domain.ObjectID, error) {
	v, err := domain.ParseRevision(rev)
	if err != nil {
		return domain.ObjectID{}, err
	}
	got, err := r.reader.ResolveRevision(ctx, repo, v)
	return got.CommitID, err
}
func (r *Releases) bind(ctx context.Context, p domain.Principal, rel *domain.Release, target string, create bool) error {
	oid, err := r.resolve(ctx, rel.Repo, "refs/tags/"+rel.Tag)
	if errors.Is(err, domain.ErrNotFound) {
		if rel.Draft {
			rel.TargetCommitish = ""
			return nil
		}
		if !create || target == "" {
			return fmt.Errorf("%w: tag %q does not exist; give target_commitish to create it", domain.ErrPreconditionFailed, rel.Tag)
		}
		if r.pushes == nil {
			return fmt.Errorf("%w: release tag notifications are not configured", domain.ErrUnavailable)
		}
		oid, err = r.resolve(ctx, rel.Repo, target)
		if err != nil {
			return err
		}
		err = r.tags.CreateTag(ctx, rel.Repo, rel.Tag, oid)
		if errors.Is(err, domain.ErrAlreadyExists) {
			oid, err = r.resolve(ctx, rel.Repo, "refs/tags/"+rel.Tag)
		} else if err == nil {
			err = r.pushes.Record(ctx, p, rel.Repo, []domain.RefUpdate{{Ref: "refs/tags/" + rel.Tag, Old: domain.ZeroOID, New: oid.String()}})
		}
	}
	if err != nil {
		return err
	}
	rel.TargetCommitish = oid.String()
	return nil
}
func (r *Releases) pin(ctx context.Context, w *ports.ReleaseWrite, rel domain.Release) error {
	if !rel.MakeLatest {
		return nil
	}
	all, err := r.store.ListReleases(ctx, rel.Repo)
	if err != nil {
		return err
	}
	for _, other := range all {
		if other.ID != rel.ID && other.MakeLatest {
			other.MakeLatest = false
			w.PutReleases = append(w.PutReleases, other)
		}
	}
	return nil
}
func (r *Releases) emit(ctx context.Context, repo domain.Repo, rel domain.Release, kind string) {
	at := r.clock.Now()
	var ev domain.Event
	switch kind {
	case "published":
		ev = domain.ReleasePublished{Release: rel, At: at, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt}
	case "updated":
		ev = domain.ReleaseUpdated{Release: rel, At: at, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt}
	case "deleted":
		ev = domain.ReleaseDeleted{Release: rel, At: at, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt}
	case "unpublished":
		ev = domain.ReleaseUnpublished{Release: rel, At: at, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt}
	}
	r.bus.Publish(ctx, ev)
}

// CreateRelease creates a release, optionally creating its tag when publishing.
func (r *Releases) CreateRelease(ctx context.Context, p domain.Principal, name domain.RepoName, params ports.CreateReleaseParams) (domain.Release, error) {
	repo, err := r.writable(ctx, p, name)
	if err != nil {
		return domain.Release{}, err
	}
	rel := domain.Release{Repo: name, Draft: params.Draft, Prerelease: params.Prerelease, MakeLatest: params.MakeLatest, Author: domain.AuthorName(p), CreatedAt: r.clock.Now(), UpdatedAt: r.clock.Now()}
	if rel.Tag, err = domain.ParseReleaseTag(params.Tag); err != nil {
		return rel, err
	}
	if rel.Title, err = domain.ParseReleaseTitle(params.Title); err != nil {
		return rel, err
	}
	if rel.Notes, err = domain.ParseReleaseNotes(params.Notes); err != nil {
		return rel, err
	}
	if _, err = r.store.GetReleaseByTag(ctx, name, rel.Tag); err == nil {
		return rel, domain.ErrAlreadyExists
	} else if !errors.Is(err, domain.ErrNotFound) {
		return rel, err
	}
	if err = r.bind(ctx, p, &rel, params.TargetCommitish, true); err != nil {
		return rel, err
	}
	if rel.Draft {
		rel.MakeLatest = false
	} else {
		rel.PublishedAt = r.clock.Now()
	}
	rel.ID, err = r.store.NextReleaseID(ctx)
	if err != nil {
		return rel, err
	}
	err = r.updateReleases(ctx, name, func(w *ports.ReleaseWrite) error {
		if err := r.pin(ctx, w, rel); err != nil {
			return err
		}
		w.PutReleases = append(w.PutReleases, rel)
		return nil
	})
	if err != nil {
		return domain.Release{}, err
	}
	if !rel.Draft {
		r.emit(ctx, repo, rel, "published")
	}
	return r.store.GetRelease(ctx, name, rel.ID)
}

// GetRelease resolves a visible release by exactly one selector.
func (r *Releases) GetRelease(ctx context.Context, p domain.Principal, name domain.RepoName, ref ports.ReleaseRef) (domain.Release, error) {
	if _, err := r.readable(ctx, p, name); err != nil {
		return domain.Release{}, err
	}
	n := 0
	if ref.ID != 0 {
		n++
	}
	if ref.Tag != "" {
		n++
	}
	if ref.Latest {
		n++
	}
	if n != 1 {
		return domain.Release{}, domain.ErrInvalid
	}
	var rel domain.Release
	var err error
	switch {
	case ref.ID != 0:
		rel, err = r.store.GetRelease(ctx, name, ref.ID)
	case ref.Tag != "":
		rel, err = r.store.GetReleaseByTag(ctx, name, ref.Tag)
	default:
		rel, err = r.store.GetLatestRelease(ctx, name)
	}
	if err == nil && rel.Draft && !releaseWriter(p, name) {
		err = domain.ErrNotFound
	}
	return rel, err
}

// ListReleases lists visible releases newest first, with an ID cursor.
func (r *Releases) ListReleases(ctx context.Context, p domain.Principal, name domain.RepoName, size int, token string) (ports.ReleasePage, error) {
	var out ports.ReleasePage
	if _, err := r.readable(ctx, p, name); err != nil {
		return out, err
	}
	if size == 0 {
		size = 30
	}
	n, err := pageSize(size)
	if err != nil {
		return out, err
	}
	var cursor int64
	if token != "" {
		cursor, err = strconv.ParseInt(token, 10, 64)
		if err != nil || cursor <= 0 {
			return out, domain.ErrInvalidPageToken
		}
	}
	all, err := r.store.ListReleases(ctx, name)
	if err != nil {
		return out, err
	}
	slices.SortFunc(all, func(a, b domain.Release) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		if a.ID > b.ID {
			return -1
		}
		if a.ID < b.ID {
			return 1
		}
		return 0
	})
	start := token == ""
	writer := releaseWriter(p, name)
	for _, rel := range all {
		if rel.Draft && !writer {
			continue // a hidden draft is not even a valid cursor
		}
		if !start {
			if rel.ID == cursor {
				start = true
			}
			continue
		}
		if len(out.Releases) == n {
			out.NextPageToken = strconv.FormatInt(out.Releases[n-1].ID, 10)
			break
		}
		out.Releases = append(out.Releases, rel)
	}
	if !start {
		return out, domain.ErrInvalidPageToken
	}
	return out, nil
}

// UpdateRelease changes only fields whose pointers are non-nil.
func (r *Releases) UpdateRelease(ctx context.Context, p domain.Principal, name domain.RepoName, id int64, params ports.UpdateReleaseParams) (domain.Release, error) {
	repo, err := r.writable(ctx, p, name)
	if err != nil {
		return domain.Release{}, err
	}
	var rel domain.Release
	kind := "updated"
	err = r.updateReleases(ctx, name, func(w *ports.ReleaseWrite) error {
		var err error
		rel, err = r.store.GetRelease(ctx, name, id)
		if err != nil {
			return err
		}
		wasDraft := rel.Draft
		if params.Tag != nil {
			rel.Tag, err = domain.ParseReleaseTag(*params.Tag)
			if err != nil {
				return err
			}
		}
		if params.Title != nil {
			rel.Title, err = domain.ParseReleaseTitle(*params.Title)
			if err != nil {
				return err
			}
		}
		if params.Notes != nil {
			rel.Notes, err = domain.ParseReleaseNotes(*params.Notes)
			if err != nil {
				return err
			}
		}
		if params.Draft != nil {
			rel.Draft = *params.Draft
		}
		if params.Prerelease != nil {
			rel.Prerelease = *params.Prerelease
		}
		if params.MakeLatest != nil {
			rel.MakeLatest = *params.MakeLatest
		}
		target := ""
		if params.TargetCommitish != nil {
			target = *params.TargetCommitish
		}
		if err = r.bind(ctx, p, &rel, target, target != ""); err != nil {
			return err
		}
		kind = "updated"
		if rel.Draft {
			rel.PublishedAt = time.Time{}
			rel.MakeLatest = false
		} else if wasDraft {
			rel.PublishedAt = r.clock.Now()
			kind = "published"
		}
		rel.UpdatedAt = r.clock.Now()
		if err = r.pin(ctx, w, rel); err != nil {
			return err
		}
		w.PutReleases = append(w.PutReleases, rel)
		return nil
	})
	if err != nil {
		return domain.Release{}, err
	}
	r.emit(ctx, repo, rel, kind)
	return r.store.GetRelease(ctx, name, id)
}

// DeleteRelease removes metadata first, then its contents.
func (r *Releases) DeleteRelease(ctx context.Context, p domain.Principal, name domain.RepoName, id int64) error {
	repo, err := r.writable(ctx, p, name)
	if err != nil {
		return err
	}
	var rel domain.Release
	var assets []domain.ReleaseAsset
	err = r.updateReleases(ctx, name, func(w *ports.ReleaseWrite) error {
		var err error
		rel, err = r.store.GetRelease(ctx, name, id)
		if err != nil {
			return err
		}
		assets, err = r.store.ListAssets(ctx, name, id)
		if err != nil {
			return err
		}
		w.DeleteReleases = []domain.Release{rel}
		w.DeleteAssets = assets
		for _, a := range assets {
			w.Used -= a.Charged
		}
		return nil
	})
	if err != nil {
		return err
	}
	r.removeAssetBlobs(context.WithoutCancel(ctx), assets)
	r.emit(ctx, repo, rel, "deleted")
	return nil
}

// OnRefsMoved unpublishes releases of deleted tags; recreated tags stay drafts.
func (r *Releases) OnRefsMoved(ctx context.Context, name domain.RepoName, updates []domain.RefUpdate) {
	for _, u := range updates {
		if !strings.HasPrefix(u.Ref, "refs/tags/") || u.New != domain.ZeroOID {
			continue
		}
		var changed bool
		var rel domain.Release
		err := r.updateReleases(ctx, name, func(w *ports.ReleaseWrite) error {
			changed = false
			var err error
			rel, err = r.store.GetReleaseByTag(ctx, name, strings.TrimPrefix(u.Ref, "refs/tags/"))
			if err != nil {
				return err
			}
			if rel.Draft {
				return nil
			}
			rel.Draft = true
			rel.MakeLatest = false
			rel.PublishedAt = time.Time{}
			rel.UpdatedAt = r.clock.Now()
			w.PutReleases = []domain.Release{rel}
			changed = true
			return nil
		})
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			ctxLog(ctx).Error().Err(err).Msg("unpublish release after tag deletion")
			continue
		}
		if changed {
			repo, err := lookupRepo(ctx, r.repos, name)
			if err == nil {
				r.emit(ctx, repo, rel, "unpublished")
			}
		}
	}
}

// AssetsOf snapshots assets before the repository's metadata is removed.
func (r *Releases) AssetsOf(ctx context.Context, name domain.RepoName) ([]domain.ReleaseAsset, error) {
	all, err := r.store.AllAssets(ctx)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(a domain.ReleaseAsset) bool { return a.Repo != name }), nil
}

// OnRepoDeleted removes contents after repository metadata has been deleted.
func (r *Releases) OnRepoDeleted(ctx context.Context, _ domain.RepoName, assets []domain.ReleaseAsset) {
	r.removeAssetBlobs(ctx, assets)
}
func (r *Releases) removeAssetBlobs(ctx context.Context, assets []domain.ReleaseAsset) {
	for _, a := range assets {
		if a.MultipartID != "" {
			if err := r.blobs.AbortMultipart(ctx, a.BlobKey(), a.MultipartID); err != nil {
				ctxLog(ctx).Error().Err(err).Int64("asset", a.ID).Msg("abort release asset multipart")
			}
		}
		if err := r.blobs.Delete(ctx, a.BlobKey()); err != nil {
			ctxLog(ctx).Error().Err(err).Int64("asset", a.ID).Msg("remove release asset blob")
		}
	}
}
