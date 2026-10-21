// SPDX-License-Identifier: AGPL-3.0-or-later
package kvstore_test

import (
	"context"
	"sync"
	"testing"

	kv "bnema.dev/ermite-kv"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

func TestReleaseStorage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openStore(t, dir)
	repo := name(t, "release-test")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, Visibility: domain.VisibilityPublic, CreatedAt: created, Settings: domain.DefaultRepoSettings()}))
	st := s.Releases()
	id, err := st.NextReleaseID(ctx)
	require.NoError(t, err)
	r := domain.Release{ID: id, Repo: repo, Tag: "v1", Title: "title", Notes: "notes", MakeLatest: true, TargetCommitish: "abc", Author: "owner", CreatedAt: created, PublishedAt: created, UpdatedAt: created}
	assetID, err := st.NextReleaseID(ctx)
	require.NoError(t, err)
	a := domain.ReleaseAsset{ID: assetID, ReleaseID: id, Repo: repo, Name: "binary", Label: "label", ContentType: "application/octet-stream", Size: 3, Digest: "sha256:abc", State: domain.ReleaseAssetUploading, Uploader: "owner", Charged: 3, MultipartID: "multipart", CreatedAt: created, UpdatedAt: created}
	w := ports.ReleaseWrite{Repo: repo, Used: 3, PutReleases: []domain.Release{r}, PutAssets: []domain.ReleaseAsset{a}}
	require.NoError(t, st.ApplyReleases(ctx, w))
	got, err := st.GetReleaseByTag(ctx, repo, "v1")
	require.NoError(t, err)
	r.Version = got.Version
	require.Equal(t, r, got)
	ar, err := st.GetAssetByName(ctx, repo, id, "binary")
	require.NoError(t, err)
	a.Version = ar.Version
	require.Equal(t, a, ar)
	rr, err := st.ReleaseRepo(ctx, id)
	require.NoError(t, err)
	require.Equal(t, repo, rr)
	rs, err := st.ListReleases(ctx, repo)
	require.NoError(t, err)
	require.Equal(t, []domain.Release{r}, rs)
	as, err := st.ListAssets(ctx, repo, id)
	require.NoError(t, err)
	require.Equal(t, []domain.ReleaseAsset{a}, as)
	all, err := st.AllAssets(ctx)
	require.NoError(t, err)
	require.Equal(t, as, all)
	require.ErrorIs(t, st.ApplyReleases(ctx, w), domain.ErrConflict)
	used, version, err := st.ReleaseUsage(ctx, repo)
	require.NoError(t, err)
	require.EqualValues(t, 3, used)
	duplicate := r
	duplicate.ID += 10
	duplicate.Version = 0
	require.ErrorIs(t, st.ApplyReleases(ctx, ports.ReleaseWrite{Repo: repo, UsageVersion: version, PutReleases: []domain.Release{duplicate}}), domain.ErrAlreadyExists)
	da := a
	da.ID += 10
	da.Version = 0
	require.ErrorIs(t, st.ApplyReleases(ctx, ports.ReleaseWrite{Repo: repo, UsageVersion: version, PutAssets: []domain.ReleaseAsset{da}}), domain.ErrAlreadyExists)
	stale := r
	stale.Version++
	require.ErrorIs(t, st.ApplyReleases(ctx, ports.ReleaseWrite{Repo: repo, UsageVersion: version, PutReleases: []domain.Release{stale}}), domain.ErrConflict)
	staleAsset := a
	staleAsset.Version++
	require.ErrorIs(t, st.ApplyReleases(ctx, ports.ReleaseWrite{Repo: repo, UsageVersion: version, DeleteAssets: []domain.ReleaseAsset{staleAsset}}), domain.ErrConflict)
	r.Tag = "v2"
	a.Name = "renamed"
	require.NoError(t, st.ApplyReleases(ctx, ports.ReleaseWrite{Repo: repo, UsageVersion: version, Used: 3, PutReleases: []domain.Release{r}, PutAssets: []domain.ReleaseAsset{a}}))
	_, err = st.GetReleaseByTag(ctx, repo, "v1")
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = st.GetAssetByName(ctx, repo, id, "binary")
	require.ErrorIs(t, err, domain.ErrNotFound)
	renamedRelease, err := st.GetReleaseByTag(ctx, repo, "v2")
	require.NoError(t, err)
	renamedAsset, err := st.GetAssetByName(ctx, repo, id, "renamed")
	require.NoError(t, err)
	_, version, err = st.ReleaseUsage(ctx, repo)
	require.NoError(t, err)
	require.NoError(t, st.ApplyReleases(ctx, ports.ReleaseWrite{Repo: repo, UsageVersion: version, DeleteReleases: []domain.Release{renamedRelease}, DeleteAssets: []domain.ReleaseAsset{renamedAsset}}))
	_, err = st.GetRelease(ctx, repo, id)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = st.GetAsset(ctx, a.ID)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, version, err = st.ReleaseUsage(ctx, repo)
	require.NoError(t, err)
	r.Version, a.Version = 0, 0
	require.NoError(t, st.ApplyReleases(ctx, ports.ReleaseWrite{Repo: repo, UsageVersion: version, Used: 3, PutReleases: []domain.Release{r}, PutAssets: []domain.ReleaseAsset{a}}))
	require.NoError(t, s.Delete(ctx, repo))
	_, err = st.GetRelease(ctx, repo, id)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = st.ReleaseRepo(ctx, id)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = st.GetAsset(ctx, a.ID)
	require.ErrorIs(t, err, domain.ErrNotFound)
	all, err = st.AllAssets(ctx)
	require.NoError(t, err)
	require.Empty(t, all)
	used, version, err = st.ReleaseUsage(ctx, repo)
	require.NoError(t, err)
	require.Zero(t, used)
	require.Zero(t, version)
	require.ErrorIs(t, st.ApplyReleases(ctx, ports.ReleaseWrite{Repo: repo}), domain.ErrNotFound)
	require.NoError(t, s.Close(ctx))
	db, err := kv.Open(ctx, dir, kv.Options{})
	require.NoError(t, err)
	defer func() { _ = db.Close(ctx) }()
	for _, prefix := range []string{"rel/", "reltag/", "relid/", "relasset/", "relassets/", "reluse/"} {
		for key := range db.Snapshot(ctx).Prefix(ctx, []byte(prefix)) {
			t.Errorf("repository deletion left release key %q", key)
		}
	}
}
func TestConcurrentReleaseIDs(t *testing.T) {
	st := openStore(t, t.TempDir()).Releases()
	var wg sync.WaitGroup
	ids := make(chan int64, 32)
	for range 32 {
		wg.Go(func() { id, err := st.NextReleaseID(context.Background()); require.NoError(t, err); ids <- id })
	}
	wg.Wait()
	close(ids)
	seen := map[int64]bool{}
	for id := range ids {
		require.False(t, seen[id])
		seen[id] = true
	}
	require.Len(t, seen, 32)
}
