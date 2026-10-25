// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func fullDeps(n domain.RepoName) domain.RepoDeps {
	return domain.RepoDeps{
		Repo: n,
		Updates: []domain.DepUpdate{
			{DepRef: domain.DepRef{Ecosystem: domain.EcosystemGoMod, Directory: "", Name: "golang.org/x/mod", Version: "v0.2.0"}, Current: "v0.1.0", Kind: domain.DepMinor, New: true},
			{DepRef: domain.DepRef{Ecosystem: domain.EcosystemActions, Directory: ".github/workflows/ci.yml", Name: "actions/checkout", Version: "v5"}, Current: "v4", Kind: domain.DepMajor},
		},
		CheckedAt:    created,
		SnoozedUntil: created.Add(time.Hour),
		State:        domain.DepsFailed,
		Error:        "boom",
		RunID:        7,
		RunUpdates: []domain.DepUpdate{
			{DepRef: domain.DepRef{Ecosystem: domain.EcosystemGoMod, Name: "golang.org/x/mod", Version: "v0.2.0"}, Current: "v0.1.0", Kind: domain.DepMinor},
			{DepRef: domain.DepRef{Ecosystem: domain.EcosystemActions, Directory: ".github/workflows/ci.yml", Name: "actions/checkout", Version: "v5"}, Current: "v4", Kind: domain.DepMajor, New: true},
		},
		MR:         42,
		MRVersions: []domain.DepRef{{Ecosystem: domain.EcosystemGoMod, Name: "a/b", Version: "v1.0.1"}},
	}
}

func TestDepsRoundTripAndAbsent(t *testing.T) {
	ctx := t.Context()
	d := openStore(t, t.TempDir()).Deps()
	n := name(t, "proj")
	got, err := d.Get(ctx, n)
	require.NoError(t, err)
	require.Equal(t, domain.RepoDeps{Repo: n}, got)

	want := fullDeps(n)
	_, err = d.Mutate(ctx, n, func(r *domain.RepoDeps) (bool, error) { *r = want; return true, nil })
	require.NoError(t, err)
	got, err = d.Get(ctx, n)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestDepsListOrder(t *testing.T) {
	ctx := t.Context()
	d := openStore(t, t.TempDir()).Deps()
	for _, s := range []string{"zeta", "alpha", "mid"} {
		_, err := d.Mutate(ctx, name(t, s), func(r *domain.RepoDeps) (bool, error) { r.MR = 1; return true, nil })
		require.NoError(t, err)
	}
	list, err := d.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 3)
	require.Equal(t, "alpha", list[0].Repo.String())
	require.Equal(t, "mid", list[1].Repo.String())
	require.Equal(t, "zeta", list[2].Repo.String())
}

func TestDepsMutate(t *testing.T) {
	ctx := t.Context()
	d := openStore(t, t.TempDir()).Deps()
	n := name(t, "proj")

	got, err := d.Mutate(ctx, n, func(r *domain.RepoDeps) (bool, error) {
		require.Equal(t, domain.RepoDeps{Repo: n}, *r)
		return false, nil
	})
	require.NoError(t, err)
	require.Equal(t, domain.RepoDeps{Repo: n}, got)
	list, err := d.List(ctx)
	require.NoError(t, err)
	require.Empty(t, list, "changed=false must not create")

	got, err = d.Mutate(ctx, n, func(r *domain.RepoDeps) (bool, error) { r.MR = 7; return true, nil })
	require.NoError(t, err)
	require.EqualValues(t, 7, got.MR)
	got, err = d.Mutate(ctx, n, func(r *domain.RepoDeps) (bool, error) { r.MR = 8; return false, nil })
	require.NoError(t, err)
	require.EqualValues(t, 7, got.MR)
	got, err = d.Get(ctx, n)
	require.NoError(t, err)
	require.EqualValues(t, 7, got.MR)

	boom := errors.New("boom")
	_, err = d.Mutate(ctx, n, func(*domain.RepoDeps) (bool, error) { return true, boom })
	require.ErrorIs(t, err, boom)
}

func TestDepsMutateInterleavedRetries(t *testing.T) {
	ctx := t.Context()
	d := openStore(t, t.TempDir()).Deps()
	n := name(t, "proj")
	calls := 0
	got, err := d.Mutate(ctx, n, func(r *domain.RepoDeps) (bool, error) {
		calls++
		if calls == 1 { // a concurrent create commits after our read of the absent record
			_, err := d.Mutate(ctx, n, func(c *domain.RepoDeps) (bool, error) { c.MR = 5; return true, nil })
			require.NoError(t, err)
		}
		r.RunID = 11
		return true, nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.EqualValues(t, 5, got.MR, "retry saw the winner's write")
	require.EqualValues(t, 11, got.RunID)
}

func TestDepsMutateConcurrent(t *testing.T) {
	ctx := t.Context()
	d := openStore(t, t.TempDir()).Deps()
	n := name(t, "proj")
	var wg sync.WaitGroup
	const workers = 4
	for range workers {
		wg.Go(func() {
			_, err := d.Mutate(ctx, n, func(r *domain.RepoDeps) (bool, error) { r.MR++; return true, nil })
			if err != nil {
				require.ErrorIs(t, err, domain.ErrConflict)
			}
		})
	}
	wg.Wait()
	got, err := d.Get(ctx, n)
	require.NoError(t, err)
	require.Positive(t, got.MR)
}

func TestDepsIgnore(t *testing.T) {
	ctx := t.Context()
	d := openStore(t, t.TempDir()).Deps()
	n := name(t, "proj")
	other := name(t, "other")
	root := domain.DepRef{Ecosystem: domain.EcosystemGoMod, Name: "example.com/a/b", Version: "v1.0.0"}
	slashDir := domain.DepRef{Ecosystem: domain.EcosystemActions, Directory: ".github/workflows/ci.yml", Name: "actions/checkout", Version: "v5"}

	ok, err := d.IsIgnored(ctx, n, root)
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, d.Ignore(ctx, n, []domain.DepRef{root, slashDir}))
	require.NoError(t, d.Ignore(ctx, n, []domain.DepRef{root}), "idempotent")
	require.NoError(t, d.Ignore(ctx, n, nil))

	for _, r := range []domain.DepRef{root, slashDir} {
		ok, err = d.IsIgnored(ctx, n, r)
		require.NoError(t, err)
		require.True(t, ok)
	}
	// Components that would collide with a naive "/"-joined key.
	for _, r := range []domain.DepRef{
		{Ecosystem: root.Ecosystem, Directory: "example.com", Name: "a/b", Version: root.Version},
		{Ecosystem: root.Ecosystem, Name: "example.com/a/b", Version: "v1.0.1"},
		{Ecosystem: root.Ecosystem, Name: "example.com/a", Version: "b@v1.0.0"},
		{Ecosystem: root.Ecosystem, Directory: "-", Name: root.Name, Version: root.Version},
		{Ecosystem: root.Ecosystem, Directory: "%", Name: root.Name, Version: root.Version},
		{Ecosystem: slashDir.Ecosystem, Directory: ".github", Name: "workflows/ci.yml/actions/checkout", Version: "v5"},
	} {
		ok, err = d.IsIgnored(ctx, n, r)
		require.NoError(t, err)
		require.False(t, ok, "%+v", r)
	}
	ok, err = d.IsIgnored(ctx, other, root)
	require.NoError(t, err)
	require.False(t, ok, "ignores are per repository")
}

func TestDepsFirstSeen(t *testing.T) {
	ctx := t.Context()
	d := openStore(t, t.TempDir()).Deps()
	t1, t2 := created, created.Add(time.Hour)
	got, err := d.FirstSeen(ctx, domain.EcosystemActions, "actions/checkout", "v5", "abc", t1)
	require.NoError(t, err)
	require.True(t, t1.Equal(got))
	got, err = d.FirstSeen(ctx, domain.EcosystemActions, "actions/checkout", "v5", "abc", t2)
	require.NoError(t, err)
	require.True(t, t1.Equal(got), "first time kept")
	got, err = d.FirstSeen(ctx, domain.EcosystemActions, "actions/checkout", "v5", "def", t2)
	require.NoError(t, err)
	require.True(t, t2.Equal(got), "another sha is another record")
}

func TestDepsFirstSeenRace(t *testing.T) {
	ctx := t.Context()
	d := openStore(t, t.TempDir()).Deps()
	var wg sync.WaitGroup
	res := make([]time.Time, 6)
	for i := range res {
		wg.Go(func() {
			got, err := d.FirstSeen(ctx, domain.EcosystemGoMod, "m", "v1.0.0", "", created.Add(time.Duration(i)*time.Minute))
			require.NoError(t, err)
			res[i] = got
		})
	}
	wg.Wait()
	for _, r := range res {
		require.True(t, res[0].Equal(r))
	}
}

func TestDepsDeletedWithRepository(t *testing.T) {
	ctx := t.Context()
	s := openStore(t, t.TempDir())
	d := s.Deps()
	n := name(t, "proj")
	keep := name(t, "keep")
	ref := domain.DepRef{Ecosystem: domain.EcosystemGoMod, Name: "a/b", Version: "v1.0.0"}
	for _, r := range []domain.RepoName{n, keep} {
		require.NoError(t, s.Create(ctx, domain.Repo{Name: r, Visibility: domain.VisibilityPrivate, CreatedAt: created}))
		_, err := d.Mutate(ctx, r, func(x *domain.RepoDeps) (bool, error) { x.MR = 3; return true, nil })
		require.NoError(t, err)
		require.NoError(t, d.Ignore(ctx, r, []domain.DepRef{ref}))
	}
	seen, err := d.FirstSeen(ctx, domain.EcosystemGoMod, "a/b", "v1.0.0", "", created)
	require.NoError(t, err)

	require.NoError(t, s.Delete(ctx, n))

	got, err := d.Get(ctx, n)
	require.NoError(t, err)
	require.Equal(t, domain.RepoDeps{Repo: n}, got)
	ok, err := d.IsIgnored(ctx, n, ref)
	require.NoError(t, err)
	require.False(t, ok)
	got, err = d.Get(ctx, keep)
	require.NoError(t, err)
	require.EqualValues(t, 3, got.MR)
	ok, err = d.IsIgnored(ctx, keep, ref)
	require.NoError(t, err)
	require.True(t, ok)
	again, err := d.FirstSeen(ctx, domain.EcosystemGoMod, "a/b", "v1.0.0", "", created.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, seen.Equal(again), "dseen survives repository deletion")
}
