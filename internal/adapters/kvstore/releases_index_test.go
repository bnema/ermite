// SPDX-License-Identifier: AGPL-3.0-or-later
package kvstore

import (
	"context"
	"testing"
	"time"

	kv "bnema.dev/ermite-kv"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"github.com/stretchr/testify/require"
)

func TestReleaseLatestIndexBackfill(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s, err := Open(ctx, dir)
	require.NoError(t, err)
	var repos []domain.RepoName
	for _, raw := range []string{"backfill", "empty", "newest"} {
		repo, err := domain.ParseRepoName(raw)
		require.NoError(t, err)
		repos = append(repos, repo)
	}
	for _, repo := range repos {
		require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, Visibility: domain.VisibilityPublic}))
	}
	var b kv.Batch
	for i, repo := range repos {
		for j := 1; j <= 5; j++ {
			rel := domain.Release{ID: int64(i*10 + j), Repo: repo, Tag: string(rune('a' + j)), CreatedAt: time.Unix(int64(j), 0)}
			rel.Draft = j == 5 || i == 1
			rel.Prerelease = j == 4
			rel.MakeLatest = j == 1 && i == 0
			b.Put(relKey(repo, rel.ID), encodeRelease(rel))
		}
		// A stale key must also be removed for a repo with no eligible release.
		b.Put(relLatestKey(repo), []byte("999"))
	}
	b.Delete([]byte(releaseLatestIndexMarker))
	_, err = s.db.Apply(ctx, &b)
	require.NoError(t, err)
	require.NoError(t, s.Close(context.Background()))
	s, err = Open(ctx, dir)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close(context.Background())) }()
	for i, want := range []int64{1, 0, 23} {
		got, err := s.Releases().GetLatestRelease(ctx, repos[i])
		if want == 0 {
			require.ErrorIs(t, err, domain.ErrNotFound)
		} else {
			require.NoError(t, err)
			require.Equal(t, want, got.ID)
		}
	}
	require.NoError(t, s.buildReleaseLatestIndex(ctx))
	got, err := s.Releases().GetLatestRelease(ctx, repos[0])
	require.NoError(t, err)
	require.EqualValues(t, 1, got.ID)
	require.NoError(t, s.Delete(ctx, repos[0]))
	_, err = s.Releases().GetLatestRelease(ctx, repos[0])
	require.ErrorIs(t, err, domain.ErrNotFound)
}

func TestGetLatestReleaseAtomicUpdate(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, t.TempDir())
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close(context.Background())) }()
	repo, err := domain.ParseRepoName("atomic")
	require.NoError(t, err)
	require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, Visibility: domain.VisibilityPublic}))
	a := domain.Release{ID: 1, Repo: repo, Tag: "a"}
	b := domain.Release{ID: 2, Repo: repo, Tag: "b"}
	store := s.Releases()
	require.NoError(t, store.ApplyReleases(ctx, ports.ReleaseWrite{Repo: repo, PutReleases: []domain.Release{a, b}, SetLatest: true, LatestID: a.ID}))
	a, err = store.GetLatestRelease(ctx, repo)
	require.NoError(t, err)
	require.EqualValues(t, 1, a.ID)
	_, version, err := store.ReleaseUsage(ctx, repo)
	require.NoError(t, err)
	a.Prerelease = true
	require.NoError(t, store.ApplyReleases(ctx, ports.ReleaseWrite{Repo: repo, UsageVersion: version, PutReleases: []domain.Release{a}, SetLatest: true, LatestID: b.ID}))
	got, err := store.GetLatestRelease(ctx, repo)
	require.NoError(t, err)
	require.EqualValues(t, 2, got.ID)
	// An asset-only/usage write leaves the index untouched, including its version.
	before, ok := s.db.Get(ctx, relLatestKey(repo))
	require.True(t, ok)
	_, version, err = store.ReleaseUsage(ctx, repo)
	require.NoError(t, err)
	require.NoError(t, store.ApplyReleases(ctx, ports.ReleaseWrite{Repo: repo, UsageVersion: version, Used: 10}))
	after, ok := s.db.Get(ctx, relLatestKey(repo))
	require.True(t, ok)
	require.Equal(t, before, after)
}
