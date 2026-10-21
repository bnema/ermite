// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

func pendingArtifact(repo domain.RepoName, run, id int64, name string) domain.CIArtifact {
	return domain.CIArtifact{
		ID: id, Repo: repo, RunID: run, Attempt: 1, JobID: "1-0-0", Assignment: domain.HashTokenSecret("ermitejob_x"),
		Name: name, MediaType: domain.CIArtifactZip, State: domain.CIArtifactPending,
		CreatedAt: created, ExpiresAt: created.Add(24 * time.Hour),
	}
}

func TestCIArtifactsRoundTripAndIndexes(t *testing.T) {
	ctx := context.Background()
	s, _, n := ciRepo(t)
	arts := s.Artifacts()

	used, ver, err := arts.ArtifactUsage(ctx, n)
	require.NoError(t, err)
	assert.Zero(t, used)
	assert.Zero(t, ver)

	id1, err := arts.NextArtifactID(ctx)
	require.NoError(t, err)
	id2, err := arts.NextArtifactID(ctx)
	require.NoError(t, err)
	assert.Greater(t, id2, id1, "ids increase")

	a := pendingArtifact(n, 7, id1, "dist")
	a.Store = domain.CIArtifactStoreBlob
	b := pendingArtifact(n, 7, id2, "logs")
	require.NoError(t, arts.ApplyArtifacts(ctx, ports.CIArtifactWrite{Repo: n, Put: []domain.CIArtifact{a, b}}))

	got, err := arts.GetArtifact(ctx, id1)
	require.NoError(t, err)
	assert.Equal(t, "dist", got.Name)
	assert.Equal(t, domain.CIArtifactStoreBlob, got.Store)
	legacy, err := arts.GetArtifact(ctx, id2)
	require.NoError(t, err)
	assert.Equal(t, domain.CIArtifactStoreLegacy, legacy.Store, "an absent proto field keeps the legacy layout")
	assert.Equal(t, a.Assignment, got.Assignment)
	assert.Equal(t, domain.CIArtifactPending, got.State)
	assert.True(t, got.ExpiresAt.Equal(a.ExpiresAt))
	assert.NotZero(t, got.Version)

	list, err := arts.RunArtifacts(ctx, n, 7)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, []int64{id1, id2}, []int64{list[0].ID, list[1].ID}, "by id")
	other, err := arts.RunArtifacts(ctx, n, 8)
	require.NoError(t, err)
	assert.Empty(t, other)

	// A second create of the same id is a conflict.
	err = arts.ApplyArtifacts(ctx, ports.CIArtifactWrite{Repo: n, UsageVersion: 0, Put: []domain.CIArtifact{a}})
	assert.ErrorIs(t, err, domain.ErrConflict)

	// Blocks, and the usage precondition.
	_, ver, err = arts.ArtifactUsage(ctx, n)
	require.NoError(t, err)
	got.Charged = 10
	blk := ports.CIArtifactBlock{ArtifactID: id1, ID: []byte{0, 1, 0xff}, Size: 10, File: "0001ff.ab"}
	require.NoError(t, arts.ApplyArtifacts(ctx, ports.CIArtifactWrite{Repo: n, Used: 10, UsageVersion: ver, Put: []domain.CIArtifact{got}, PutBlocks: []ports.CIArtifactBlock{blk}}))
	err = arts.ApplyArtifacts(ctx, ports.CIArtifactWrite{Repo: n, Used: 99, UsageVersion: ver})
	assert.ErrorIs(t, err, domain.ErrConflict, "the usage moved since it was read")
	used, _, err = arts.ArtifactUsage(ctx, n)
	require.NoError(t, err)
	assert.EqualValues(t, 10, used)
	blocks, err := arts.ArtifactBlocks(ctx, id1)
	require.NoError(t, err)
	assert.Equal(t, []ports.CIArtifactBlock{blk}, blocks)
	ids, err := arts.BlockArtifactIDs(ctx)
	require.NoError(t, err)
	assert.Equal(t, []int64{id1}, ids)

	// A stale record version is a conflict too.
	_, ver, _ = arts.ArtifactUsage(ctx, n)
	err = arts.ApplyArtifacts(ctx, ports.CIArtifactWrite{Repo: n, Used: 10, UsageVersion: ver, Put: []domain.CIArtifact{got}})
	assert.ErrorIs(t, err, domain.ErrConflict)

	// Delete takes the record, its index and its blocks.
	cur, err := arts.GetArtifact(ctx, id1)
	require.NoError(t, err)
	_, ver, _ = arts.ArtifactUsage(ctx, n)
	require.NoError(t, arts.ApplyArtifacts(ctx, ports.CIArtifactWrite{Repo: n, Used: 0, UsageVersion: ver, Delete: []domain.CIArtifact{cur}}))
	_, err = arts.GetArtifact(ctx, id1)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	blocks, err = arts.ArtifactBlocks(ctx, id1)
	require.NoError(t, err)
	assert.Empty(t, blocks)
	all, err := arts.AllArtifacts(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, id2, all[0].ID)

	_, err = arts.GetArtifact(ctx, 0)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestCIArtifactIDsAreUniqueUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	s, _, _ := ciRepo(t)
	arts := s.Artifacts()
	var mu sync.Mutex
	seen := map[int64]bool{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 10 {
				id, err := arts.NextArtifactID(ctx)
				assert.NoError(t, err)
				mu.Lock()
				assert.False(t, seen[id], "id %d given twice", id)
				seen[id] = true
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	assert.Len(t, seen, 80)
}

func TestCIArtifactsGoWithTheRepository(t *testing.T) {
	ctx := context.Background()
	s, _, n := ciRepo(t)
	arts := s.Artifacts()
	a := pendingArtifact(n, 3, 1, "dist")
	blk := ports.CIArtifactBlock{ArtifactID: 1, ID: []byte("b"), Size: 4, File: "62.00"}
	require.NoError(t, arts.ApplyArtifacts(ctx, ports.CIArtifactWrite{Repo: n, Used: 4, Put: []domain.CIArtifact{a}, PutBlocks: []ports.CIArtifactBlock{blk}}))

	require.NoError(t, s.Delete(ctx, n))
	_, err := arts.GetArtifact(ctx, 1)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	all, err := arts.AllArtifacts(ctx)
	require.NoError(t, err)
	assert.Empty(t, all)
	ids, err := arts.BlockArtifactIDs(ctx)
	require.NoError(t, err)
	assert.Empty(t, ids)
	used, ver, err := arts.ArtifactUsage(ctx, n)
	require.NoError(t, err)
	assert.Zero(t, used)
	assert.Zero(t, ver)

	// No write recreates keys under the deleted repository.
	err = arts.ApplyArtifacts(ctx, ports.CIArtifactWrite{Repo: n, Put: []domain.CIArtifact{pendingArtifact(n, 3, 2, "x")}})
	assert.ErrorIs(t, err, domain.ErrNotFound)
	all, _ = arts.AllArtifacts(ctx)
	assert.Empty(t, all)
}

func TestCIArtifactDeleteBlocks(t *testing.T) {
	ctx := context.Background()
	s, _, n := ciRepo(t)
	arts := s.Artifacts()
	a := pendingArtifact(n, 3, 5, "dist")
	blks := []ports.CIArtifactBlock{{ArtifactID: 5, ID: []byte("a"), Size: 1, File: "61.00"}, {ArtifactID: 5, ID: []byte("b"), Size: 1, File: "62.00"}}
	require.NoError(t, arts.ApplyArtifacts(ctx, ports.CIArtifactWrite{Repo: n, Used: 2, Put: []domain.CIArtifact{a}, PutBlocks: blks}))
	require.NoError(t, arts.DeleteBlocks(ctx, 5))
	got, err := arts.ArtifactBlocks(ctx, 5)
	require.NoError(t, err)
	assert.Empty(t, got)
	require.NoError(t, arts.DeleteBlocks(ctx, 5), "nothing left is fine")
}
