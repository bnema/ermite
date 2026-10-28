// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func fullLanguages(t *testing.T, n domain.RepoName) domain.RepoLanguages {
	t.Helper()
	commit, err := domain.ParseObjectID(strings.Repeat("ab", 20))
	require.NoError(t, err)
	return domain.RepoLanguages{
		Repo:          n,
		RepoCreatedAt: created,
		Commit:        commit,
		Version:       domain.LanguagesVersion,
		ComputedAt:    created.Add(time.Hour),
		Languages:     []domain.LanguageBytes{{Name: "Go", Bytes: 3000}, {Name: "Shell", Bytes: 1000}},
		Truncated:     true,
	}
}

func TestLanguagesRoundTrip(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	l := s.Languages()
	n := name(t, "proj")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: n, Visibility: domain.VisibilityPrivate, CreatedAt: created}))

	_, ok, err := l.Get(ctx, n)
	require.NoError(t, err)
	require.False(t, ok)

	want := fullLanguages(t, n)
	require.NoError(t, l.Put(ctx, want))
	got, ok, err := l.Get(ctx, n)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, want, got)

	require.NoError(t, l.Delete(ctx, n))
	_, ok, err = l.Get(ctx, n)
	require.NoError(t, err)
	require.False(t, ok)
	require.NoError(t, l.Delete(ctx, n)) // missing: not an error
}

func TestLanguagesPutRefusesMissingOrRecreatedRepo(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	l := s.Languages()
	n := name(t, "proj")

	require.ErrorIs(t, l.Put(ctx, fullLanguages(t, n)), domain.ErrNotFound)

	require.NoError(t, s.Create(ctx, domain.Repo{Name: n, Visibility: domain.VisibilityPrivate, CreatedAt: created.Add(time.Minute)}))
	require.ErrorIs(t, l.Put(ctx, fullLanguages(t, n)), domain.ErrNotFound)
	_, ok, err := l.Get(ctx, n)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestLanguagesDeletedWithRepository(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	l := s.Languages()
	n, keep := name(t, "proj"), name(t, "keep")
	for _, r := range []domain.RepoName{n, keep} {
		require.NoError(t, s.Create(ctx, domain.Repo{Name: r, Visibility: domain.VisibilityPrivate, CreatedAt: created}))
		require.NoError(t, l.Put(ctx, fullLanguages(t, r)))
	}
	require.NoError(t, s.Delete(ctx, n))
	_, ok, err := l.Get(ctx, n)
	require.NoError(t, err)
	require.False(t, ok)
	_, ok, err = l.Get(ctx, keep)
	require.NoError(t, err)
	require.True(t, ok)
}

func TestLanguagesPutRetriesAgainstConcurrentMutate(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	l := s.Languages()
	n := name(t, "proj")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: n, Visibility: domain.VisibilityPrivate, CreatedAt: created}))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// Few rounds: both sides rewrite the repository key and retry a bounded number of times.
		for i := range 3 {
			_, err := s.Mutate(ctx, n, func(r *domain.Repo) (bool, error) {
				r.PushedAt = created.Add(time.Duration(i+1) * time.Second)
				return true, nil
			})
			require.NoError(t, err)
		}
	}()
	go func() {
		defer wg.Done()
		for range 3 {
			require.NoError(t, l.Put(ctx, fullLanguages(t, n)))
		}
	}()
	wg.Wait()
	_, ok, err := l.Get(ctx, n)
	require.NoError(t, err)
	require.True(t, ok)
}
