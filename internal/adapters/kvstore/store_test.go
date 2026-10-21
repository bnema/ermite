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

	b := domain.Repo{Name: name(t, "b"), Description: "second", Topics: []string{"cli", "go"}, Visibility: domain.VisibilityPublic, CreatedAt: created, Settings: domain.DefaultRepoSettings()}
	a := domain.Repo{Name: name(t, "a"), Visibility: domain.VisibilityPrivate, CreatedAt: created, PushedAt: created.Add(time.Hour), ActiveAt: created.Add(2 * time.Hour), Settings: domain.DefaultRepoSettings()}
	require.NoError(t, s.Create(ctx, b))
	require.NoError(t, s.Create(ctx, a))

	got, err := s.Get(ctx, b.Name)
	require.NoError(t, err)
	assert.Equal(t, b, got)
	got, err = s.Get(ctx, a.Name)
	require.NoError(t, err)
	assert.Equal(t, a, got)
	assert.True(t, got.PushedAt.Equal(a.PushedAt))
	assert.True(t, got.ActiveAt.Equal(a.ActiveAt))

	list, err := s.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []domain.Repo{a, b}, list)

	err = s.Create(ctx, b)
	assert.ErrorIs(t, err, domain.ErrAlreadyExists)

	b.Description = "changed"
	b.Topics = []string{"forge"}
	b.Visibility = domain.VisibilityPrivate
	_, err = s.Mutate(ctx, b.Name, func(r *domain.Repo) (bool, error) { *r = b; return true, nil })
	require.NoError(t, err)
	got, err = s.Get(ctx, b.Name)
	require.NoError(t, err)
	assert.Equal(t, b, got)

	b.Topics = nil
	_, err = s.Mutate(ctx, b.Name, func(r *domain.Repo) (bool, error) { *r = b; return true, nil })
	require.NoError(t, err)
	got, err = s.Get(ctx, b.Name)
	require.NoError(t, err)
	assert.Nil(t, got.Topics, "cleared topics read back as nil")

	require.NoError(t, s.Delete(ctx, b.Name))
	_, err = s.Get(ctx, b.Name)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	require.NoError(t, s.Delete(ctx, b.Name), "deleting a missing repository is not an error")
	_, err = s.Mutate(ctx, b.Name, func(r *domain.Repo) (bool, error) { return true, nil })
	assert.ErrorIs(t, err, domain.ErrNotFound)

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
	r := domain.Repo{Name: name(t, "keep"), Visibility: domain.VisibilityPublic, CreatedAt: created, Settings: domain.DefaultRepoSettings()}
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

	// The caller's own cancellation is reported, not mistaken for health.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	assert.ErrorIs(t, s.Check(cancelled), context.Canceled)

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

	t.Run("missing is written as the current version", func(t *testing.T) {
		dir := t.TempDir()
		s, err := kvstore.Open(ctx, dir)
		require.NoError(t, err)
		require.NoError(t, s.Close(ctx))
		assert.Equal(t, uint32(kvstore.SchemaVersion), readSchema(t, dir))
	})

	t.Run("current is accepted and kept", func(t *testing.T) {
		dir := t.TempDir()
		s, err := kvstore.Open(ctx, dir)
		require.NoError(t, err)
		require.NoError(t, s.Close(ctx))
		s, err = kvstore.Open(ctx, dir)
		require.NoError(t, err)
		require.NoError(t, s.Close(ctx))
		assert.Equal(t, uint32(kvstore.SchemaVersion), readSchema(t, dir))
	})

	t.Run("version 1 is upgraded to the current one, its data kept", func(t *testing.T) {
		dir := t.TempDir()
		putRaw(t, dir, "meta/schema", mustProto(t, &storagepb.SchemaVersion{Version: 1}))
		putRaw(t, dir, "repo/old", mustProto(t, &storagepb.Repo{Name: "old", Visibility: storagepb.Visibility_VISIBILITY_PUBLIC}))
		s, err := kvstore.Open(ctx, dir)
		require.NoError(t, err)
		got, err := s.Get(ctx, name(t, "old"))
		require.NoError(t, err)
		assert.Equal(t, "old", got.Name.String())
		require.NoError(t, s.Close(ctx))
		assert.Equal(t, uint32(kvstore.SchemaVersion), readSchema(t, dir))
	})

	t.Run("version 2 is upgraded to the current one, its runs kept", func(t *testing.T) {
		dir := t.TempDir()
		putRaw(t, dir, "meta/schema", mustProto(t, &storagepb.SchemaVersion{Version: 2}))
		putRaw(t, dir, "repo/old", mustProto(t, &storagepb.Repo{Name: "old", Visibility: storagepb.Visibility_VISIBILITY_PUBLIC}))
		// A run written by a schema 2 build (no head sha), and a retained pull_request
		// run that has one: both must read back after the upgrade.
		putRaw(t, dir, "run/old/00000000000000000001", mustProto(t, &storagepb.CIRun{
			Repo: "old", Id: 1, Number: 1, Attempt: 1, Event: "push", Ref: "refs/heads/main", Sha: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Status: storagepb.CIStatus_CI_STATUS_COMPLETED,
		}))
		putRaw(t, dir, "run/old/00000000000000000002", mustProto(t, &storagepb.CIRun{
			Repo: "old", Id: 2, Number: 2, Attempt: 1, Event: "pull_request", Ref: "refs/pull/1/merge",
			Sha: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", HeadSha: "cccccccccccccccccccccccccccccccccccccccc",
			Status: storagepb.CIStatus_CI_STATUS_COMPLETED,
		}))
		s, err := kvstore.Open(ctx, dir)
		require.NoError(t, err)
		got, err := s.Get(ctx, name(t, "old"))
		require.NoError(t, err)
		assert.Equal(t, "old", got.Name.String())
		old, err := s.CI().GetRun(ctx, name(t, "old"), 1)
		require.NoError(t, err)
		assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", old.Sha)
		assert.Empty(t, old.HeadSha)
		assert.Equal(t, old.Sha, old.CheckSha(), "a run without a head sha keeps its checks on its commit")
		pr, err := s.CI().GetRun(ctx, name(t, "old"), 2)
		require.NoError(t, err)
		assert.Equal(t, "pull_request", pr.Event)
		assert.Equal(t, "cccccccccccccccccccccccccccccccccccccccc", pr.HeadSha)
		assert.Equal(t, pr.HeadSha, pr.CheckSha(), "the checks of a pull_request run are on the head")
		require.NoError(t, s.Close(ctx))
		assert.Equal(t, uint32(kvstore.SchemaVersion), readSchema(t, dir))
	})

	t.Run("version 2 is upgraded to the current one, its data kept", func(t *testing.T) {
		dir := t.TempDir()
		putRaw(t, dir, "meta/schema", mustProto(t, &storagepb.SchemaVersion{Version: 2}))
		putRaw(t, dir, "repo/old", mustProto(t, &storagepb.Repo{Name: "old", Visibility: storagepb.Visibility_VISIBILITY_PUBLIC}))
		s, err := kvstore.Open(ctx, dir)
		require.NoError(t, err)
		_, err = s.Get(ctx, name(t, "old"))
		require.NoError(t, err)
		require.NoError(t, s.Close(ctx))
		assert.Equal(t, uint32(kvstore.SchemaVersion), readSchema(t, dir))
	})

	// Downgrade guard: once a build with reusable workflows has opened the data
	// (and may have stored calls), a build that knows schema 2 only must refuse
	// it; it does so for any version above its own (ErrSchemaTooNew), which is
	// the version 3 this build writes.
	t.Run("data written by this build is above schema 2", func(t *testing.T) {
		dir := t.TempDir()
		s, err := kvstore.Open(ctx, dir)
		require.NoError(t, err)
		require.NoError(t, s.Close(ctx))
		assert.Greater(t, readSchema(t, dir), uint32(2), "a build that supports 2 refuses it")
	})

	t.Run("newer stops startup and releases the lock", func(t *testing.T) {
		dir := t.TempDir()
		putRaw(t, dir, "meta/schema", mustProto(t, &storagepb.SchemaVersion{Version: kvstore.SchemaVersion + 1}))
		_, err := kvstore.Open(ctx, dir)
		require.ErrorIs(t, err, kvstore.ErrSchemaTooNew)
		assert.Equal(t, uint32(kvstore.SchemaVersion+1), readSchema(t, dir), "the lock was released and the value kept")
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

func TestRepoWithoutTopicsReadsNil(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	putRaw(t, dir, "repo/old", mustProto(t, &storagepb.Repo{Name: "old", Visibility: storagepb.Visibility_VISIBILITY_PUBLIC}))
	putRaw(t, dir, "repo/new", mustProto(t, &storagepb.Repo{Name: "new", Visibility: storagepb.Visibility_VISIBILITY_PUBLIC, Topics: []string{"a", "b"}}))
	s := openStore(t, dir)

	old, err := s.Get(ctx, name(t, "old"))
	require.NoError(t, err)
	assert.Nil(t, old.Topics)
	got, err := s.Get(ctx, name(t, "new"))
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, got.Topics)
}

func TestSchema6TokenUpgrade(t *testing.T) {
	ctx := context.Background()
	for _, from := range []uint32{3, 4, 5} {
		t.Run(fmt.Sprint(from), func(t *testing.T) {
			dir := t.TempDir()
			putRaw(t, dir, "meta/schema", mustProto(t, &storagepb.SchemaVersion{Version: from}))
			all := domain.AllScopes()
			scopes := make([]string, 0, len(all)-1)
			for _, sc := range all {
				if sc != domain.ScopeReleaseWrite {
					scopes = append(scopes, sc.String())
				}
			}
			for _, tc := range []struct {
				id              string
				scopes          []string
				repos, prefixes []string
				upgraded        bool
			}{
				{id: "full", scopes: scopes, upgraded: true},
				{id: "partial", scopes: []string{"repo:read"}},
				{id: "missing-review", scopes: func() []string {
					var out []string
					for _, s := range scopes {
						if s != "mr:review" {
							out = append(out, s)
						}
					}
					return out
				}(), upgraded: from < 4},
				{id: "repo", scopes: scopes, repos: []string{"a"}},
				{id: "branch", scopes: scopes, prefixes: []string{"bot/"}},
			} {
				putRaw(t, dir, "token/"+tc.id, mustProto(t, &storagepb.Token{Id: tc.id, Scopes: tc.scopes, Repos: tc.repos, BranchPrefixes: tc.prefixes}))
			}
			s, err := kvstore.Open(ctx, dir)
			require.NoError(t, err)
			require.NoError(t, s.Close(ctx))
			assert.Equal(t, uint32(kvstore.SchemaVersion), readSchema(t, dir))
			db, err := kv.Open(ctx, dir, kv.Options{})
			require.NoError(t, err)
			defer func() { _ = db.Close(ctx) }()
			for _, id := range []string{"full", "partial", "missing-review", "repo", "branch"} {
				item, ok := db.Get(ctx, []byte("token/"+id))
				require.True(t, ok)
				var pb storagepb.Token
				require.NoError(t, proto.Unmarshal(item.Value, &pb))
				if id == "full" || (id == "missing-review" && from < 4) {
					assert.Contains(t, pb.Scopes, "release:write")
				} else {
					assert.NotContains(t, pb.Scopes, "release:write")
				}
			}
		})
	}
}
