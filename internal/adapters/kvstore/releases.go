// SPDX-License-Identifier: AGPL-3.0-or-later
package kvstore

import (
	kv "bnema.dev/ermite-kv"
	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"context"
	"errors"
	"fmt"
	"github.com/bnema/zerowrap"
	"google.golang.org/protobuf/proto"
	"slices"
	"strconv"
)

// Releases implements ports.ReleaseStore over the shared metadata database.
type Releases struct{ db *kv.DB }

var _ ports.ReleaseStore = (*Releases)(nil)

// Releases returns the release metadata store.
func (s *Store) Releases() *Releases { return &Releases{db: s.db} }
func relKey(repo domain.RepoName, id int64) []byte {
	return []byte("rel/" + repo.String() + "/" + fmt.Sprintf(idFormat, id))
}
func relIDKey(id int64) []byte { return []byte("relid/" + fmt.Sprintf(idFormat, id)) }
func relTagKey(repo domain.RepoName, tag string) []byte {
	return []byte("reltag/" + repo.String() + "/" + tag)
}
func relAssetKey(id int64) []byte { return []byte("relasset/" + fmt.Sprintf(idFormat, id)) }
func relNamePrefix(repo domain.RepoName, id int64) string {
	return "relassets/" + repo.String() + "/" + fmt.Sprintf(idFormat, id) + "/"
}
func relNameKey(a domain.ReleaseAsset) []byte {
	return []byte(relNamePrefix(a.Repo, a.ReleaseID) + a.Name)
}
func relLatestKey(repo domain.RepoName) []byte { return []byte("rellatest/" + repo.String()) }
func relUseKey(repo domain.RepoName) []byte    { return []byte("reluse/" + repo.String()) }

// GetLatestRelease resolves the index and release from the same snapshot.
func (c *Releases) GetLatestRelease(ctx context.Context, repo domain.RepoName) (domain.Release, error) {
	snap := c.db.Snapshot(ctx)
	item, ok := snap.Get(ctx, relLatestKey(repo))
	if !ok {
		return domain.Release{}, domain.ErrNotFound
	}
	id, err := strconv.ParseInt(string(item.Value), 10, 64)
	if err != nil {
		return domain.Release{}, err
	}
	item, ok = snap.Get(ctx, relKey(repo, id))
	if !ok {
		return domain.Release{}, domain.ErrNotFound
	}
	return decodeRelease(item)
}
func encodeRelease(r domain.Release) []byte {
	return mustMarshal(&storagepb.Release{
		Id:              r.ID,
		Repo:            r.Repo.String(),
		Tag:             r.Tag,
		Title:           r.Title,
		Notes:           r.Notes,
		Draft:           r.Draft,
		Prerelease:      r.Prerelease,
		MakeLatest:      r.MakeLatest,
		TargetCommitish: r.TargetCommitish,
		Author:          r.Author,
		CreatedAt:       encodeTime(r.CreatedAt),
		PublishedAt:     encodeTime(r.PublishedAt),
		UpdatedAt:       encodeTime(r.UpdatedAt),
	})
}
func decodeRelease(item kv.Item) (domain.Release, error) {
	var pb storagepb.Release
	if err := proto.Unmarshal(item.Value, &pb); err != nil {
		return domain.Release{}, err
	}
	repo, err := domain.ParseRepoName(pb.GetRepo())
	if err != nil {
		return domain.Release{}, err
	}
	return domain.Release{Version: item.Version,
		ID:              pb.GetId(),
		Repo:            repo,
		Tag:             pb.GetTag(),
		Title:           pb.GetTitle(),
		Notes:           pb.GetNotes(),
		Draft:           pb.GetDraft(),
		Prerelease:      pb.GetPrerelease(),
		MakeLatest:      pb.GetMakeLatest(),
		TargetCommitish: pb.GetTargetCommitish(),
		Author:          pb.GetAuthor(),
		CreatedAt:       decodeTime(pb.GetCreatedAt()),
		PublishedAt:     decodeTime(pb.GetPublishedAt()),
		UpdatedAt:       decodeTime(pb.GetUpdatedAt()),
	}, nil
}
func encodeReleaseAsset(r domain.ReleaseAsset) []byte {
	return mustMarshal(&storagepb.ReleaseAsset{
		Id:          r.ID,
		ReleaseId:   r.ReleaseID,
		Repo:        r.Repo.String(),
		Name:        r.Name,
		Label:       r.Label,
		ContentType: r.ContentType,
		Size:        r.Size,
		Digest:      r.Digest,
		State:       string(r.State),
		Uploader:    r.Uploader,
		Charged:     r.Charged,
		MultipartId: r.MultipartID,
		CreatedAt:   encodeTime(r.CreatedAt),
		UpdatedAt:   encodeTime(r.UpdatedAt),
	})
}
func decodeReleaseAsset(item kv.Item) (domain.ReleaseAsset, error) {
	var pb storagepb.ReleaseAsset
	if err := proto.Unmarshal(item.Value, &pb); err != nil {
		return domain.ReleaseAsset{}, err
	}
	repo, err := domain.ParseRepoName(pb.GetRepo())
	if err != nil {
		return domain.ReleaseAsset{}, err
	}
	return domain.ReleaseAsset{Version: item.Version,
		ID:          pb.GetId(),
		ReleaseID:   pb.GetReleaseId(),
		Repo:        repo,
		Name:        pb.GetName(),
		Label:       pb.GetLabel(),
		ContentType: pb.GetContentType(),
		Size:        pb.GetSize(),
		Digest:      pb.GetDigest(),
		State:       domain.ReleaseAssetState(pb.GetState()),
		Uploader:    pb.GetUploader(),
		Charged:     pb.GetCharged(),
		MultipartID: pb.GetMultipartId(),
		CreatedAt:   decodeTime(pb.GetCreatedAt()),
		UpdatedAt:   decodeTime(pb.GetUpdatedAt()),
	}, nil
}

// NextReleaseID allocates a global monotonically increasing id.
func (c *Releases) NextReleaseID(ctx context.Context) (int64, error) {
	for range artifactIDTries {
		last, item, ok, err := (&CI{db: c.db}).readSeq(ctx, []byte("relseq"))
		if err != nil {
			return 0, err
		}
		var b kv.Batch
		stageSeq(&b, []byte("relseq"), item, ok, last+1)
		_, err = c.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue
		}
		if err != nil {
			return 0, err
		}
		return last + 1, nil
	}
	return 0, domain.ErrConflict
}

// ReleaseUsage reads the repository's quota gate.
func (c *Releases) ReleaseUsage(ctx context.Context, repo domain.RepoName) (int64, uint64, error) {
	item, ok := c.db.Get(ctx, relUseKey(repo))
	if !ok {
		return 0, 0, nil
	}
	var pb storagepb.ReleaseUsage
	if err := proto.Unmarshal(item.Value, &pb); err != nil {
		return 0, 0, err
	}
	return pb.GetUsed(), item.Version, nil
}

// GetRelease reads a release in its repository.
func (c *Releases) GetRelease(ctx context.Context, repo domain.RepoName, id int64) (domain.Release, error) {
	item, ok := c.db.Get(ctx, relKey(repo, id))
	if !ok {
		return domain.Release{}, domain.ErrNotFound
	}
	return decodeRelease(item)
}

// ReleaseRepo resolves the global id index.
func (c *Releases) ReleaseRepo(ctx context.Context, id int64) (domain.RepoName, error) {
	item, ok := c.db.Get(ctx, relIDKey(id))
	if !ok {
		return domain.RepoName{}, domain.ErrNotFound
	}
	return domain.ParseRepoName(string(item.Value))
}

// GetReleaseByTag resolves a repository tag.
func (c *Releases) GetReleaseByTag(ctx context.Context, repo domain.RepoName, tag string) (domain.Release, error) {
	snap := c.db.Snapshot(ctx)
	idx, ok := snap.Get(ctx, relTagKey(repo, tag))
	if !ok {
		return domain.Release{}, domain.ErrNotFound
	}
	id, err := strconv.ParseInt(string(idx.Value), 10, 64)
	if err != nil {
		return domain.Release{}, err
	}
	item, ok := snap.Get(ctx, relKey(repo, id))
	if !ok {
		return domain.Release{}, domain.ErrNotFound
	}
	return decodeRelease(item)
}

// ListReleases returns all releases by descending id.
func (c *Releases) ListReleases(ctx context.Context, repo domain.RepoName) ([]domain.Release, error) {
	var out []domain.Release
	for _, item := range c.db.Snapshot(ctx).Prefix(ctx, []byte("rel/"+repo.String()+"/")) {
		r, err := decodeRelease(item)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	slices.Reverse(out)
	return out, nil
}

// GetAsset reads asset metadata in every state.
func (c *Releases) GetAsset(ctx context.Context, id int64) (domain.ReleaseAsset, error) {
	item, ok := c.db.Get(ctx, relAssetKey(id))
	if !ok {
		return domain.ReleaseAsset{}, domain.ErrNotFound
	}
	return decodeReleaseAsset(item)
}

// GetAssetByName resolves the unique name index.
func (c *Releases) GetAssetByName(ctx context.Context, repo domain.RepoName, releaseID int64, name string) (domain.ReleaseAsset, error) {
	snap := c.db.Snapshot(ctx)
	idx, ok := snap.Get(ctx, relNameKey(domain.ReleaseAsset{Repo: repo, ReleaseID: releaseID, Name: name}))
	if !ok {
		return domain.ReleaseAsset{}, domain.ErrNotFound
	}
	id, err := strconv.ParseInt(string(idx.Value), 10, 64)
	if err != nil {
		return domain.ReleaseAsset{}, err
	}
	item, ok := snap.Get(ctx, relAssetKey(id))
	if !ok {
		return domain.ReleaseAsset{}, domain.ErrNotFound
	}
	return decodeReleaseAsset(item)
}

// ListAssets returns every state in name order.
func (c *Releases) ListAssets(ctx context.Context, repo domain.RepoName, releaseID int64) ([]domain.ReleaseAsset, error) {
	snap := c.db.Snapshot(ctx)
	var out []domain.ReleaseAsset
	for _, idx := range snap.Prefix(ctx, []byte(relNamePrefix(repo, releaseID))) {
		id, err := strconv.ParseInt(string(idx.Value), 10, 64)
		if err != nil {
			return nil, err
		}
		item, ok := snap.Get(ctx, relAssetKey(id))
		if !ok {
			return nil, domain.ErrNotFound
		}
		a, err := decodeReleaseAsset(item)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// AllAssets returns every asset for garbage collection.
func (c *Releases) AllAssets(ctx context.Context) ([]domain.ReleaseAsset, error) {
	var out []domain.ReleaseAsset
	for _, item := range c.db.Snapshot(ctx).Prefix(ctx, []byte("relasset/")) {
		a, err := decodeReleaseAsset(item)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// ApplyReleases atomically changes metadata and its quota gate.
//
//hexcheck:ignore suspicious-business-logic-in-adapter enforces storage version preconditions and unique secondary indexes, not release policy
func (c *Releases) ApplyReleases(ctx context.Context, w ports.ReleaseWrite) error {
	log := zerowrap.FromCtx(ctx)
	var b kv.Batch
	if err := guardRepoKey(ctx, c.db, &b, w.Repo); err != nil {
		return err
	}
	if w.UsageVersion == 0 {
		b.RequireAbsent(relUseKey(w.Repo))
	} else {
		b.RequireVersion(relUseKey(w.Repo), w.UsageVersion)
	}
	b.Put(relUseKey(w.Repo), mustMarshal(&storagepb.ReleaseUsage{Used: w.Used}))
	if w.SetLatest {
		if w.LatestID > 0 {
			b.Put(relLatestKey(w.Repo), []byte(strconv.FormatInt(w.LatestID, 10)))
		} else {
			b.Delete(relLatestKey(w.Repo))
		}
	}
	snap := c.db.Snapshot(ctx)
	for _, r := range w.DeleteReleases {
		if r.Repo != w.Repo || r.Version == 0 {
			return domain.ErrInvalid
		}
		b.RequireVersion(relKey(r.Repo, r.ID), r.Version)
		b.Delete(relKey(r.Repo, r.ID))
		b.Delete(relTagKey(r.Repo, r.Tag))
		b.Delete(relIDKey(r.ID))
	}
	for _, r := range w.DeleteAssets {
		if r.Repo != w.Repo || r.Version == 0 {
			return domain.ErrInvalid
		}
		b.RequireVersion(relAssetKey(r.ID), r.Version)
		b.Delete(relAssetKey(r.ID))
		b.Delete(relNameKey(r))
	}
	seenReleases := map[string]int64{}
	for _, r := range w.PutReleases {
		if r.Repo != w.Repo || r.ID < 1 {
			return domain.ErrInvalid
		}
		idxKey := relTagKey(r.Repo, r.Tag)
		if id, ok := seenReleases[string(idxKey)]; ok && id != r.ID {
			return domain.ErrAlreadyExists
		}
		seenReleases[string(idxKey)] = r.ID
		if item, ok := snap.Get(ctx, idxKey); ok && string(item.Value) != fmt.Sprintf(idFormat, r.ID) {
			removed := false
			for _, d := range w.DeleteReleases {
				if string(relTagKey(d.Repo, d.Tag)) == string(idxKey) {
					removed = true
				}
			}
			if !removed {
				return domain.ErrAlreadyExists
			}
		}
		if old, ok := snap.Get(ctx, relKey(r.Repo, r.ID)); ok {
			previous, err := decodeRelease(old)
			if err != nil {
				return err
			}
			oldKey := relTagKey(previous.Repo, previous.Tag)
			if string(oldKey) != string(idxKey) {
				b.Delete(oldKey)
			}
		}
		stage(&b, relKey(r.Repo, r.ID), encodeRelease(r), r.Version)
		b.Put(idxKey, []byte(fmt.Sprintf(idFormat, r.ID)))
		if r.Version == 0 {
			b.RequireAbsent(relIDKey(r.ID))
		}
		b.Put(relIDKey(r.ID), []byte(r.Repo.String()))
	}
	seenAssets := map[string]int64{}
	for _, r := range w.PutAssets {
		if r.Repo != w.Repo || r.ID < 1 {
			return domain.ErrInvalid
		}
		idxKey := relNameKey(r)
		if id, ok := seenAssets[string(idxKey)]; ok && id != r.ID {
			return domain.ErrAlreadyExists
		}
		seenAssets[string(idxKey)] = r.ID
		if item, ok := snap.Get(ctx, idxKey); ok && string(item.Value) != fmt.Sprintf(idFormat, r.ID) {
			removed := false
			for _, d := range w.DeleteAssets {
				if string(relNameKey(d)) == string(idxKey) {
					removed = true
				}
			}
			if !removed {
				return domain.ErrAlreadyExists
			}
		}
		if old, ok := snap.Get(ctx, relAssetKey(r.ID)); ok {
			previous, err := decodeReleaseAsset(old)
			if err != nil {
				return err
			}
			oldKey := relNameKey(previous)
			if string(oldKey) != string(idxKey) {
				b.Delete(oldKey)
			}
		}
		stage(&b, relAssetKey(r.ID), encodeReleaseAsset(r), r.Version)
		b.Put(idxKey, []byte(fmt.Sprintf(idFormat, r.ID)))
	}
	_, err := c.db.Apply(ctx, &b)
	if errors.Is(err, kv.ErrConflict) {
		return domain.ErrConflict
	}
	if err != nil {
		return fmt.Errorf("write releases: %w", err)
	}
	log.Debug().Str("repo", w.Repo.String()).Msg("release metadata updated")
	return nil
}

// releaseKeys collects all repository-owned release keys, including global indexes.
func releaseKeys(ctx context.Context, snap *kv.Snapshot, repo domain.RepoName) [][]byte {
	keys := [][]byte{relUseKey(repo), relLatestKey(repo)}
	for _, prefix := range []string{"rel/", "reltag/", "relassets/"} {
		for key, item := range snap.Prefix(ctx, []byte(prefix+repo.String()+"/")) {
			keys = append(keys, slices.Clone(key))
			if prefix == "rel/" {
				if r, err := decodeRelease(item); err == nil {
					keys = append(keys, relIDKey(r.ID))
				}
			}
		}
	}
	for key, item := range snap.Prefix(ctx, []byte("relasset/")) {
		if a, err := decodeReleaseAsset(item); err == nil && a.Repo == repo {
			keys = append(keys, slices.Clone(key))
		}
	}
	return keys
}
