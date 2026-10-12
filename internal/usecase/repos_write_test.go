// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/usecase"
)

func ptr[T any](v T) *T { return &v }

func TestUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := name(t, "tools")
	priv := domain.Repo{Name: n, Visibility: domain.VisibilityPrivate, Description: "old"}
	admin := tokenPrincipal(domain.ScopeRepoAdmin, "tools")

	t.Run("description and visibility are stored and RepoUpdated is published", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.meta.EXPECT().Get(ctx, n).Return(priv, nil).Twice()
		want := domain.Repo{Name: n, Visibility: domain.VisibilityPublic, Description: "new", Settings: domain.DefaultRepoSettings()}
		f.meta.EXPECT().Update(ctx, want).Return(nil)
		f.bus.EXPECT().Publish(ctx, domain.RepoUpdated{
			Repo: n, At: now, Visibility: domain.VisibilityPublic, Previous: domain.VisibilityPrivate,
		}).Return().Once()

		got, err := f.repos.Update(ctx, admin, n, ports.UpdateRepoParams{
			Description: ptr("new"), Visibility: ptr(domain.VisibilityPublic),
		})
		require.NoError(t, err)
		assert.Equal(t, want, got)
	})

	t.Run("a lost race on the metadata is retried", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.meta.EXPECT().Get(ctx, n).Return(priv, nil)
		f.meta.EXPECT().Update(ctx, mock.Anything).Return(domain.ErrConflict).Once()
		f.meta.EXPECT().Update(ctx, mock.Anything).Return(nil).Once()
		f.bus.EXPECT().Publish(ctx, mock.Anything).Return().Once()
		_, err := f.repos.Update(ctx, admin, n, ports.UpdateRepoParams{Description: ptr("x")})
		require.NoError(t, err)
	})

	t.Run("the default branch is checked, then HEAD moves", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.meta.EXPECT().Get(ctx, n).Return(priv, nil)
		f.git.EXPECT().HasBranch(ctx, n, "dev").Return(true, nil)
		f.git.EXPECT().SetDefaultBranch(ctx, n, "dev").Return(nil)
		f.bus.EXPECT().Publish(ctx, mock.Anything).Return().Once()
		got, err := f.repos.Update(ctx, admin, n, ports.UpdateRepoParams{DefaultBranch: ptr("dev")})
		require.NoError(t, err)
		assert.Equal(t, priv, got)
	})

	t.Run("metadata is written before HEAD moves, and the event follows both", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		var order []string
		f.meta.EXPECT().Get(ctx, n).Return(priv, nil)
		f.git.EXPECT().HasBranch(ctx, n, "dev").Return(true, nil)
		f.meta.EXPECT().Update(ctx, mock.Anything).RunAndReturn(func(context.Context, domain.Repo) error {
			order = append(order, "meta")
			return nil
		})
		f.git.EXPECT().SetDefaultBranch(ctx, n, "dev").RunAndReturn(func(context.Context, domain.RepoName, string) error {
			order = append(order, "head")
			return nil
		})
		f.bus.EXPECT().Publish(ctx, mock.Anything).Run(func(context.Context, domain.Event) { order = append(order, "event") }).Return().Once()
		_, err := f.repos.Update(ctx, admin, n, ports.UpdateRepoParams{Description: ptr("x"), DefaultBranch: ptr("dev")})
		require.NoError(t, err)
		assert.Equal(t, []string{"meta", "head", "event"}, order)
	})

	t.Run("a HEAD that cannot move after the metadata was stored still publishes, then fails", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		boom := errors.New("boom")
		pub := domain.Repo{Name: n, Visibility: domain.VisibilityPublic, Description: "old"}
		f.meta.EXPECT().Get(ctx, n).Return(pub, nil)
		f.git.EXPECT().HasBranch(ctx, n, "dev").Return(true, nil)
		f.meta.EXPECT().Update(ctx, mock.Anything).Return(nil)
		f.git.EXPECT().SetDefaultBranch(ctx, n, "dev").Return(boom)
		f.bus.EXPECT().Publish(ctx, domain.RepoUpdated{
			Repo: n, At: now, Visibility: domain.VisibilityPrivate, Previous: domain.VisibilityPublic,
		}).Return().Once()
		_, err := f.repos.Update(ctx, admin, n, ports.UpdateRepoParams{Visibility: ptr(domain.VisibilityPrivate), DefaultBranch: ptr("dev")})
		assert.ErrorIs(t, err, boom)
	})

	t.Run("a HEAD that cannot move with no metadata change publishes nothing", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		boom := errors.New("boom")
		f.meta.EXPECT().Get(ctx, n).Return(priv, nil)
		f.git.EXPECT().HasBranch(ctx, n, "dev").Return(true, nil)
		f.git.EXPECT().SetDefaultBranch(ctx, n, "dev").Return(boom)
		_, err := f.repos.Update(ctx, admin, n, ports.UpdateRepoParams{DefaultBranch: ptr("dev")})
		assert.ErrorIs(t, err, boom)
	})

	t.Run("a missing branch is a failed precondition and HEAD stays", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.meta.EXPECT().Get(ctx, n).Return(priv, nil)
		f.git.EXPECT().HasBranch(ctx, n, "nope").Return(false, nil)
		_, err := f.repos.Update(ctx, admin, n, ports.UpdateRepoParams{Description: ptr("x"), DefaultBranch: ptr("nope")})
		assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
	})

	t.Run("invalid input changes nothing", func(t *testing.T) {
		for name, params := range map[string]ports.UpdateRepoParams{
			"empty":             {},
			"empty branch":      {DefaultBranch: ptr("")},
			"multi-line":        {Description: ptr("a\nb")},
			"too long":          {Description: ptr(strings.Repeat("x", usecase.MaxDescriptionLen+1))},
			"bad utf-8":         {Description: ptr("\xff")},
			"unknown visiblity": {Visibility: ptr(domain.Visibility("secret"))},
		} {
			f := newFixture(t)
			f.meta.EXPECT().Get(ctx, n).Return(priv, nil)
			_, err := f.repos.Update(ctx, admin, n, params)
			require.Error(t, err, name)
			assert.True(t, errors.Is(err, domain.ErrInvalid) || errors.Is(err, domain.ErrInvalidVisibility), "%s: %v", name, err)
		}
	})

	access := []struct {
		name string
		p    domain.Principal
		repo domain.Repo
		want error
	}{
		{"anonymous on a private repository sees nothing", domain.Anonymous(), priv, domain.ErrNotFound},
		{"anonymous on a public repository must log in", domain.Anonymous(), domain.Repo{Name: n, Visibility: domain.VisibilityPublic}, domain.ErrUnauthenticated},
		{"reader of a private repository is forbidden", tokenPrincipal(domain.ScopeRepoRead), priv, domain.ErrForbidden},
		{"writer of a private repository is hidden", tokenPrincipal(domain.ScopeRepoWrite), priv, domain.ErrNotFound},
		{"admin of another repository is hidden", tokenPrincipal(domain.ScopeRepoAdmin, "other"), priv, domain.ErrNotFound},
		{"writer of a public repository is forbidden", tokenPrincipal(domain.ScopeRepoWrite), domain.Repo{Name: n, Visibility: domain.VisibilityPublic}, domain.ErrForbidden},
	}
	for _, tt := range access {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.meta.EXPECT().Get(ctx, n).Return(tt.repo, nil)
			_, err := f.repos.Update(ctx, tt.p, n, ports.UpdateRepoParams{Description: ptr("x")})
			assert.ErrorIs(t, err, tt.want)
			err = f.repos.Delete(ctx, tt.p, n)
			assert.ErrorIs(t, err, tt.want)
		})
	}

	t.Run("a token limited to branches may not delete a repository", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t) // no Trash, no Delete
		f.meta.EXPECT().Get(ctx, n).Return(priv, nil)
		err := f.repos.Delete(ctx, branchLimited(domain.ScopeRepoAdmin), n)
		assert.ErrorIs(t, err, domain.ErrForbidden)
	})

	t.Run("a missing repository is not found", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.meta.EXPECT().Get(ctx, n).Return(domain.Repo{}, domain.ErrNotFound)
		_, err := f.repos.Update(ctx, domain.Root(), n, ports.UpdateRepoParams{Description: ptr("x")})
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
}

func TestDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := name(t, "tools")
	priv := domain.Repo{Name: n, Visibility: domain.VisibilityPrivate}

	t.Run("trash, forget the metadata, publish RepoDeleted with the visibility", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.meta.EXPECT().Get(ctx, n).Return(priv, nil)
		f.git.EXPECT().Trash(ctx, n).Return(nil)
		f.meta.EXPECT().Delete(ctx, n).Return(nil)
		f.bus.EXPECT().Publish(ctx, domain.RepoDeleted{Repo: n, Visibility: domain.VisibilityPrivate, At: now}).Return().Once()
		require.NoError(t, f.repos.Delete(ctx, tokenPrincipal(domain.ScopeRepoAdmin, "tools"), n))
	})

	t.Run("a directory already gone still loses its metadata", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.meta.EXPECT().Get(ctx, n).Return(priv, nil)
		f.git.EXPECT().Trash(ctx, n).Return(domain.ErrNotFound)
		f.meta.EXPECT().Delete(ctx, n).Return(nil)
		f.bus.EXPECT().Publish(ctx, mock.Anything).Return().Once()
		require.NoError(t, f.repos.Delete(ctx, domain.Root(), n))
	})

	t.Run("a failed move keeps the metadata", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		boom := errors.New("boom")
		f.meta.EXPECT().Get(ctx, n).Return(priv, nil)
		f.git.EXPECT().Trash(ctx, n).Return(boom)
		assert.ErrorIs(t, f.repos.Delete(ctx, domain.Root(), n), boom)
	})
}

func TestView(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := name(t, "tools")
	repo := domain.Repo{Name: n}

	f := newFixture(t)
	f.git.EXPECT().HeadBranch(ctx, n).Return("main", nil).Once()
	v, err := f.repos.View(ctx, repo)
	require.NoError(t, err)
	assert.Equal(t, "main", v.DefaultBranch)

	f.git.EXPECT().HeadBranch(ctx, n).Return("", domain.ErrNotFound).Once()
	v, err = f.repos.View(ctx, repo)
	require.NoError(t, err, "a missing directory has no default branch")
	assert.Empty(t, v.DefaultBranch)
}

func TestListPageSizes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var all []domain.Repo
	for i := range usecase.MaxRepoPageSize + 10 {
		all = append(all, domain.Repo{Name: name(t, fmt.Sprintf("r%04d", i)), Visibility: domain.VisibilityPublic})
	}
	for _, tt := range []struct {
		name string
		size int
		want int
		more bool
	}{
		{"default", 0, usecase.DefaultPageSize, true},
		{"within the maximum", 500, 500, true},
		{"the maximum", usecase.MaxRepoPageSize, usecase.MaxRepoPageSize, true},
		{"lowered to the maximum", usecase.MaxRepoPageSize * 10, usecase.MaxRepoPageSize, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.meta.EXPECT().List(ctx).Return(all, nil)
			page, err := f.repos.ListPage(ctx, domain.Anonymous(), tt.size, "")
			require.NoError(t, err)
			assert.Len(t, page.Repos, tt.want)
			assert.Equal(t, tt.more, page.NextPageToken != "")
		})
	}
}

func TestListPage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var all []domain.Repo
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		all = append(all, domain.Repo{Name: name(t, s), Visibility: domain.VisibilityPublic})
	}
	priv := domain.Repo{Name: name(t, "bb"), Visibility: domain.VisibilityPrivate}
	all = append(all[:2], append([]domain.Repo{priv}, all[2:]...)...)

	f := newFixture(t)
	f.meta.EXPECT().List(ctx).Return(all, nil)
	var got []string
	token := ""
	for pages := 0; ; pages++ {
		require.Less(t, pages, 10)
		page, err := f.repos.ListPage(ctx, domain.Anonymous(), 2, token)
		require.NoError(t, err)
		for _, r := range page.Repos {
			got = append(got, r.Name.String())
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	assert.Equal(t, []string{"a", "b", "c", "d", "e"}, got, "private repositories never show, pages do not overlap")

	_, err := f.repos.ListPage(ctx, domain.Root(), 0, "garbage")
	assert.ErrorIs(t, err, domain.ErrInvalidPageToken)
	_, err = f.repos.ListPage(ctx, domain.Root(), -1, "")
	assert.ErrorIs(t, err, domain.ErrInvalidPageSize)
}
