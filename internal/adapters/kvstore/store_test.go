// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	kv "bnema.dev/ermite-kv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/domain"
)

func openStore(t *testing.T, dir string) *kvstore.Store {
	t.Helper()
	ctx := context.Background()
	s, err := kvstore.Open(ctx, dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(ctx) })
	return s
}

func name(t *testing.T, s string) domain.RepoName {
	t.Helper()
	n, err := domain.ParseRepoName(s)
	require.NoError(t, err)
	return n
}

var created = time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)

func TestCreateGetListUpdateDelete(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())

	b := domain.Repo{Name: name(t, "b"), Description: "second", Visibility: domain.VisibilityPublic, CreatedAt: created}
	a := domain.Repo{Name: name(t, "a"), Visibility: domain.VisibilityPrivate, CreatedAt: created, PushedAt: created.Add(time.Hour)}
	require.NoError(t, s.Create(ctx, b))
	require.NoError(t, s.Create(ctx, a))

	got, err := s.Get(ctx, b.Name)
	require.NoError(t, err)
	assert.Equal(t, b, got)
	got, err = s.Get(ctx, a.Name)
	require.NoError(t, err)
	assert.Equal(t, a, got)
	assert.True(t, got.PushedAt.Equal(a.PushedAt))

	list, err := s.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []domain.Repo{a, b}, list)

	err = s.Create(ctx, b)
	assert.ErrorIs(t, err, domain.ErrAlreadyExists)

	b.Description = "changed"
	b.Visibility = domain.VisibilityPrivate
	require.NoError(t, s.Update(ctx, b))
	got, err = s.Get(ctx, b.Name)
	require.NoError(t, err)
	assert.Equal(t, b, got)

	require.NoError(t, s.Delete(ctx, b.Name))
	_, err = s.Get(ctx, b.Name)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	require.NoError(t, s.Delete(ctx, b.Name), "deleting a missing repository is not an error")
	assert.ErrorIs(t, s.Update(ctx, b), domain.ErrNotFound)

	_, err = s.Get(ctx, name(t, "never"))
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestCreateRejectsInvalidValues(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	n := name(t, "x")

	assert.ErrorIs(t, s.Create(ctx, domain.Repo{Name: n, Visibility: "secret"}), domain.ErrInvalidVisibility)
	assert.ErrorIs(t, s.Create(ctx, domain.Repo{Visibility: domain.VisibilityPublic}), domain.ErrInvalidName)
	assert.Error(t, s.Create(ctx, domain.Repo{Name: n, Visibility: domain.VisibilityPublic, Description: "\xff"}))
	list, err := s.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestConcurrentCreateSameName(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	n := name(t, "tools")

	const workers = 16
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Create(ctx, domain.Repo{Name: n, Description: fmt.Sprint(i), Visibility: domain.VisibilityPrivate, CreatedAt: created})
		}()
	}
	wg.Wait()
	close(errs)
	ok, exists := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, domain.ErrAlreadyExists):
			exists++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, ok)
	assert.Equal(t, workers-1, exists)
}

func TestPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := kvstore.Open(ctx, dir)
	require.NoError(t, err)
	r := domain.Repo{Name: name(t, "keep"), Visibility: domain.VisibilityPublic, CreatedAt: created}
	require.NoError(t, s.Create(ctx, r))
	require.NoError(t, s.Close(ctx))

	s2 := openStore(t, dir)
	got, err := s2.Get(ctx, r.Name)
	require.NoError(t, err)
	assert.Equal(t, r, got)
}

func TestCheck(t *testing.T) {
	ctx := context.Background()
	s, err := kvstore.Open(ctx, t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.Check(ctx))
	require.NoError(t, s.Close(ctx))
	assert.ErrorIs(t, s.Check(ctx), kv.ErrClosed)
}

func TestOpenLockedDirectory(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_ = openStore(t, dir)
	_, err := kvstore.Open(ctx, dir)
	assert.ErrorIs(t, err, kv.ErrLocked)
}

// putRaw writes a value with ermite-kv directly, bypassing the store.
func putRaw(t *testing.T, dir, key string, val []byte) {
	t.Helper()
	ctx := context.Background()
	db, err := kv.Open(ctx, dir, kv.Options{})
	require.NoError(t, err)
	var b kv.Batch
	b.Put([]byte(key), val)
	_, err = db.Apply(ctx, &b)
	require.NoError(t, err)
	require.NoError(t, db.Close(ctx))
}

func readSchema(t *testing.T, dir string) uint32 {
	t.Helper()
	ctx := context.Background()
	db, err := kv.Open(ctx, dir, kv.Options{})
	require.NoError(t, err)
	defer func() { _ = db.Close(ctx) }()
	item, ok := db.Get(ctx, []byte("meta/schema"))
	require.True(t, ok, "meta/schema missing")
	var sv storagepb.SchemaVersion
	require.NoError(t, proto.Unmarshal(item.Value, &sv))
	return sv.GetVersion()
}

func TestSchema(t *testing.T) {
	ctx := context.Background()

	t.Run("missing is written as 1", func(t *testing.T) {
		dir := t.TempDir()
		s, err := kvstore.Open(ctx, dir)
		require.NoError(t, err)
		require.NoError(t, s.Close(ctx))
		assert.Equal(t, uint32(1), readSchema(t, dir))
	})

	t.Run("current is accepted and kept", func(t *testing.T) {
		dir := t.TempDir()
		s, err := kvstore.Open(ctx, dir)
		require.NoError(t, err)
		require.NoError(t, s.Close(ctx))
		s, err = kvstore.Open(ctx, dir)
		require.NoError(t, err)
		require.NoError(t, s.Close(ctx))
		assert.Equal(t, uint32(1), readSchema(t, dir))
	})

	t.Run("newer stops startup and releases the lock", func(t *testing.T) {
		dir := t.TempDir()
		putRaw(t, dir, "meta/schema", mustProto(t, &storagepb.SchemaVersion{Version: 2}))
		_, err := kvstore.Open(ctx, dir)
		require.ErrorIs(t, err, kvstore.ErrSchemaTooNew)
		assert.Equal(t, uint32(2), readSchema(t, dir), "the lock was released and the value kept")
	})

	t.Run("garbage and zero are errors", func(t *testing.T) {
		dir := t.TempDir()
		putRaw(t, dir, "meta/schema", []byte{0xff, 0xff, 0xff})
		_, err := kvstore.Open(ctx, dir)
		assert.Error(t, err)

		dir = t.TempDir()
		putRaw(t, dir, "meta/schema", mustProto(t, &storagepb.SchemaVersion{Version: 0}))
		_, err = kvstore.Open(ctx, dir)
		assert.Error(t, err)
	})
}

func mustProto(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	require.NoError(t, err)
	return b
}

func TestUnspecifiedVisibilityReadsPrivate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	putRaw(t, dir, "repo/odd", mustProto(t, &storagepb.Repo{Name: "odd"})) // visibility unset
	putRaw(t, dir, "repo/future", mustProto(t, &storagepb.Repo{Name: "future", Visibility: storagepb.Visibility(42)}))
	s := openStore(t, dir)

	for _, n := range []string{"odd", "future"} {
		got, err := s.Get(ctx, name(t, n))
		require.NoError(t, err)
		assert.Equal(t, domain.VisibilityPrivate, got.Visibility, n)
	}
}

func TestCorruptRepoValue(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	putRaw(t, dir, "repo/bad", []byte{0xff, 0xff})
	putRaw(t, dir, "repo/mismatch", mustProto(t, &storagepb.Repo{Name: "Not Valid"}))
	s := openStore(t, dir)

	_, err := s.Get(ctx, name(t, "bad"))
	assert.Error(t, err)
	_, err = s.Get(ctx, name(t, "mismatch"))
	assert.ErrorIs(t, err, domain.ErrInvalidName)
	_, err = s.List(ctx)
	assert.Error(t, err)
}
