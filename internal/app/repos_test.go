// SPDX-License-Identifier: AGPL-3.0-or-later

package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

var now = time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)

type fixture struct {
	meta  *mocks.RepoMetaStore
	git   *mocks.GitRepos
	clock *mocks.Clock
	bus   *mocks.EventBus
	repos *app.Repos
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		meta:  mocks.NewRepoMetaStore(t),
		git:   mocks.NewGitRepos(t),
		clock: mocks.NewClock(t),
		bus:   mocks.NewEventBus(t),
	}
	f.clock.EXPECT().Now().Return(now).Maybe()
	f.repos = app.NewRepos(f.meta, f.git, f.bus, f.clock)
	return f
}

func name(t *testing.T, s string) domain.RepoName {
	t.Helper()
	n, err := domain.ParseRepoName(s)
	require.NoError(t, err)
	return n
}

func TestCreate(t *testing.T) {
	ctx := context.Background()
	errBoom := errors.New("boom")

	t.Run("success is private by default", func(t *testing.T) {
		f := newFixture(t)
		n := name(t, "tools")
		want := domain.Repo{Name: n, Description: "d", Visibility: domain.VisibilityPrivate, CreatedAt: now}
		f.git.EXPECT().Stage(ctx).Return(ports.StagedID("s1"), nil)
		f.meta.EXPECT().Create(ctx, want).Return(nil)
		f.git.EXPECT().Publish(ctx, ports.StagedID("s1"), n).Return(nil)
		f.bus.EXPECT().Publish(ctx, domain.RepoCreated{Repo: n, Visibility: domain.VisibilityPrivate, At: now}).Return().Once()

		got, err := f.repos.Create(ctx, domain.Root(), app.CreateParams{Name: n, Description: "d"})
		require.NoError(t, err)
		assert.Equal(t, want, got)
	})

	t.Run("anonymous is refused before anything runs", func(t *testing.T) {
		f := newFixture(t)
		_, err := f.repos.Create(ctx, domain.Anonymous(), app.CreateParams{Name: name(t, "tools")})
		assert.ErrorIs(t, err, domain.ErrUnauthenticated)
	})

	t.Run("token without repo:write or outside its list is forbidden", func(t *testing.T) {
		for _, p := range []domain.Principal{
			tokenPrincipal(domain.ScopeRepoRead),
			tokenPrincipal(domain.ScopeRepoWrite, "other"),
		} {
			f := newFixture(t)
			_, err := f.repos.Create(ctx, p, app.CreateParams{Name: name(t, "tools")})
			assert.ErrorIs(t, err, domain.ErrForbidden)
		}
	})

	t.Run("token with repo:write creates", func(t *testing.T) {
		f := newFixture(t)
		n := name(t, "tools")
		f.git.EXPECT().Stage(ctx).Return(ports.StagedID("s1"), nil)
		f.meta.EXPECT().Create(ctx, mock.Anything).Return(nil)
		f.git.EXPECT().Publish(ctx, ports.StagedID("s1"), n).Return(nil)
		f.bus.EXPECT().Publish(ctx, mock.Anything).Return().Once()
		_, err := f.repos.Create(ctx, tokenPrincipal(domain.ScopeRepoWrite, "tools"), app.CreateParams{Name: n})
		require.NoError(t, err)
	})

	t.Run("zero name is invalid", func(t *testing.T) {
		f := newFixture(t)
		_, err := f.repos.Create(ctx, domain.Root(), app.CreateParams{})
		assert.ErrorIs(t, err, domain.ErrInvalidName)
	})

	t.Run("unknown visibility is invalid", func(t *testing.T) {
		f := newFixture(t)
		_, err := f.repos.Create(ctx, domain.Root(), app.CreateParams{Name: name(t, "tools"), Visibility: "secret"})
		assert.ErrorIs(t, err, domain.ErrInvalidVisibility)
	})

	t.Run("stage failure stores nothing", func(t *testing.T) {
		f := newFixture(t)
		f.git.EXPECT().Stage(ctx).Return("", errBoom)
		_, err := f.repos.Create(ctx, domain.Root(), app.CreateParams{Name: name(t, "tools")})
		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("existing name discards the staged repository", func(t *testing.T) {
		f := newFixture(t)
		f.git.EXPECT().Stage(ctx).Return(ports.StagedID("s1"), nil)
		f.meta.EXPECT().Create(ctx, mock.Anything).Return(domain.ErrAlreadyExists)
		f.git.EXPECT().Discard(mock.Anything, ports.StagedID("s1")).Return(nil)
		_, err := f.repos.Create(ctx, domain.Root(), app.CreateParams{Name: name(t, "tools")})
		assert.ErrorIs(t, err, domain.ErrAlreadyExists)
	})

	t.Run("publish conflict removes metadata and staging", func(t *testing.T) {
		f := newFixture(t)
		n := name(t, "tools")
		f.git.EXPECT().Stage(ctx).Return(ports.StagedID("s1"), nil)
		f.meta.EXPECT().Create(ctx, mock.Anything).Return(nil)
		f.git.EXPECT().Publish(ctx, ports.StagedID("s1"), n).Return(domain.ErrAlreadyExists)
		f.meta.EXPECT().Delete(mock.Anything, n).Return(nil)
		f.git.EXPECT().Discard(mock.Anything, ports.StagedID("s1")).Return(nil)
		_, err := f.repos.Create(ctx, domain.Root(), app.CreateParams{Name: n})
		assert.ErrorIs(t, err, domain.ErrAlreadyExists)
	})

	t.Run("failed publish removes metadata and staging", func(t *testing.T) {
		f := newFixture(t)
		n := name(t, "tools")
		f.git.EXPECT().Stage(ctx).Return(ports.StagedID("s1"), nil)
		f.meta.EXPECT().Create(ctx, mock.Anything).Return(nil)
		f.git.EXPECT().Publish(ctx, ports.StagedID("s1"), n).Return(errBoom)
		f.meta.EXPECT().Delete(mock.Anything, n).Return(nil)
		f.git.EXPECT().Discard(mock.Anything, ports.StagedID("s1")).Return(nil)
		_, err := f.repos.Create(ctx, domain.Root(), app.CreateParams{Name: n})
		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("cleanup survives a canceled context", func(t *testing.T) {
		f := newFixture(t)
		n := name(t, "tools")
		cctx, cancel := context.WithCancel(ctx)
		f.git.EXPECT().Stage(cctx).Return(ports.StagedID("s1"), nil)
		f.meta.EXPECT().Create(cctx, mock.Anything).Return(nil)
		f.git.EXPECT().Publish(cctx, ports.StagedID("s1"), n).
			RunAndReturn(func(context.Context, ports.StagedID, domain.RepoName) error {
				cancel()
				return errBoom
			})
		f.meta.EXPECT().Delete(mock.Anything, n).RunAndReturn(func(c context.Context, _ domain.RepoName) error {
			return c.Err()
		})
		f.git.EXPECT().Discard(mock.Anything, ports.StagedID("s1")).RunAndReturn(func(c context.Context, _ ports.StagedID) error {
			return c.Err()
		})
		_, err := f.repos.Create(cctx, domain.Root(), app.CreateParams{Name: n})
		assert.ErrorIs(t, err, errBoom)
	})
}

func tokenPrincipal(s domain.Scope, repos ...string) domain.Principal {
	tok := domain.Token{ID: "t", Scopes: []domain.Scope{s}}
	for _, r := range repos {
		n, err := domain.ParseRepoName(r)
		if err != nil {
			panic(err)
		}
		tok.Repos = append(tok.Repos, n)
	}
	return domain.TokenPrincipal(tok)
}

func TestGetAndListAccess(t *testing.T) {
	ctx := context.Background()
	pub := domain.Repo{Name: name(t, "pub"), Visibility: domain.VisibilityPublic}
	priv := domain.Repo{Name: name(t, "priv"), Visibility: domain.VisibilityPrivate}

	tests := []struct {
		name      string
		principal domain.Principal
		get       domain.Repo
		wantErr   error
	}{
		{"anonymous reads public", domain.Anonymous(), pub, nil},
		{"anonymous cannot see private", domain.Anonymous(), priv, domain.ErrNotFound},
		{"root reads public", domain.Root(), pub, nil},
		{"root reads private", domain.Root(), priv, nil},
		{"token with repo:read reads private", tokenPrincipal(domain.ScopeRepoRead), priv, nil},
		{"token limited to the repo reads private", tokenPrincipal(domain.ScopeRepoRead, "priv"), priv, nil},
		{"token limited to another repo cannot see private", tokenPrincipal(domain.ScopeRepoRead, "other"), priv, domain.ErrNotFound},
		{"token without repo:read cannot see private", tokenPrincipal(domain.ScopeRepoWrite), priv, domain.ErrNotFound},
		{"token without repo:read reads public", tokenPrincipal(domain.ScopeRepoWrite), pub, nil},
		{"token limited to another repo reads public", tokenPrincipal(domain.ScopeRepoRead, "other"), pub, nil},
	}
	for _, tt := range tests {
		t.Run("get/"+tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.meta.EXPECT().Get(ctx, tt.get.Name).Return(tt.get, nil)
			got, err := f.repos.Get(ctx, tt.principal, tt.get.Name)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				assert.Zero(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.get, got)
		})
	}

	t.Run("get missing is not found", func(t *testing.T) {
		f := newFixture(t)
		f.meta.EXPECT().Get(ctx, pub.Name).Return(domain.Repo{}, domain.ErrNotFound)
		_, err := f.repos.Get(ctx, domain.Root(), pub.Name)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("private missing and hidden look the same", func(t *testing.T) {
		f := newFixture(t)
		f.meta.EXPECT().Get(ctx, priv.Name).Return(priv, nil)
		_, errHidden := f.repos.Get(ctx, domain.Anonymous(), priv.Name)
		f.meta.EXPECT().Get(ctx, name(t, "gone")).Return(domain.Repo{}, domain.ErrNotFound)
		_, errMissing := f.repos.Get(ctx, domain.Anonymous(), name(t, "gone"))
		assert.Equal(t, errMissing, errHidden)
	})

	listTests := []struct {
		name      string
		principal domain.Principal
		want      []domain.Repo
	}{
		{"anonymous lists public only", domain.Anonymous(), []domain.Repo{pub}},
		{"root lists all", domain.Root(), []domain.Repo{priv, pub}},
	}
	for _, tt := range listTests {
		t.Run("list/"+tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.meta.EXPECT().List(ctx).Return([]domain.Repo{priv, pub}, nil)
			got, err := f.repos.List(ctx, tt.principal)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("list error", func(t *testing.T) {
		f := newFixture(t)
		boom := errors.New("boom")
		f.meta.EXPECT().List(ctx).Return(nil, boom)
		_, err := f.repos.List(ctx, domain.Root())
		assert.ErrorIs(t, err, boom)
	})
}

func TestReconcile(t *testing.T) {
	ctx := context.Background()

	t.Run("registers orphan dir as private, reports missing dir, skips invalid", func(t *testing.T) {
		f := newFixture(t)
		orphan, kept, ghost := name(t, "orphan"), name(t, "kept"), name(t, "ghost")
		f.git.EXPECT().PurgeStaging(ctx).Return(nil)
		f.git.EXPECT().List(ctx).Return(ports.RepoDirs{
			Names:   []domain.RepoName{orphan, kept},
			Invalid: []string{"Bad Name.git"},
		}, nil)
		f.meta.EXPECT().List(ctx).Return([]domain.Repo{
			{Name: ghost, Visibility: domain.VisibilityPublic},
			{Name: kept, Visibility: domain.VisibilityPublic},
		}, nil)
		f.meta.EXPECT().Create(ctx, domain.Repo{Name: orphan, Visibility: domain.VisibilityPrivate, CreatedAt: now}).Return(nil)

		rep, err := f.repos.Reconcile(ctx)
		require.NoError(t, err)
		assert.Equal(t, []domain.RepoName{orphan}, rep.Registered)
		assert.Equal(t, []domain.RepoName{ghost}, rep.MissingDirs)
		assert.Equal(t, []string{"Bad Name.git"}, rep.InvalidDirs)
	})

	t.Run("purge failure stops", func(t *testing.T) {
		f := newFixture(t)
		boom := errors.New("boom")
		f.git.EXPECT().PurgeStaging(ctx).Return(boom)
		_, err := f.repos.Reconcile(ctx)
		assert.ErrorIs(t, err, boom)
	})

	t.Run("register failure stops", func(t *testing.T) {
		f := newFixture(t)
		boom := errors.New("boom")
		orphan := name(t, "orphan")
		f.git.EXPECT().PurgeStaging(ctx).Return(nil)
		f.git.EXPECT().List(ctx).Return(ports.RepoDirs{Names: []domain.RepoName{orphan}}, nil)
		f.meta.EXPECT().List(ctx).Return(nil, nil)
		f.meta.EXPECT().Create(ctx, mock.Anything).Return(boom)
		_, err := f.repos.Reconcile(ctx)
		assert.ErrorIs(t, err, boom)
	})
}
