// SPDX-License-Identifier: AGPL-3.0-or-later

package app_test

import (
	"context"
	"errors"
	"sync"
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

type scheduler struct {
	mu    sync.Mutex
	names []domain.RepoName
}

func (s *scheduler) Schedule(n domain.RepoName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names = append(s.names, n)
}

type pushFixture struct {
	meta   *mocks.RepoMetaStore
	git    *mocks.GitRepos
	bus    *mocks.EventBus
	clock  *mocks.Clock
	sched  *scheduler
	pushes *app.Pushes
}

func newPushFixture(t *testing.T) *pushFixture {
	t.Helper()
	f := &pushFixture{
		meta:  mocks.NewRepoMetaStore(t),
		git:   mocks.NewGitRepos(t),
		bus:   mocks.NewEventBus(t),
		clock: mocks.NewClock(t),
		sched: &scheduler{},
	}
	f.clock.EXPECT().Now().Return(now).Maybe()
	repos := app.NewRepos(f.meta, f.git, f.clock)
	f.pushes = app.NewPushes(f.meta, repos, f.git, f.bus, f.clock, f.sched)
	return f
}

const (
	oidA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	oidB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestPushesOpen(t *testing.T) {
	ctx := context.Background()
	n := name(t, "tools")
	priv := domain.Repo{Name: n, Visibility: domain.VisibilityPrivate}
	pub := domain.Repo{Name: n, Visibility: domain.VisibilityPublic}

	tests := []struct {
		name    string
		p       domain.Principal
		repo    *domain.Repo // nil: missing
		create  bool
		want    error
		created bool
	}{
		{"anonymous", domain.Anonymous(), &pub, false, domain.ErrUnauthenticated, false},
		{"root, existing", domain.Root(), &priv, false, nil, false},
		{"writer, existing", tokenPrincipal(domain.ScopeRepoWrite), &priv, false, nil, false},
		{"writer limited to the repo", tokenPrincipal(domain.ScopeRepoWrite, "tools"), &priv, false, nil, false},
		{"writer limited to another repo, private", tokenPrincipal(domain.ScopeRepoWrite, "other"), &priv, false, domain.ErrNotFound, false},
		{"writer limited to another repo, public", tokenPrincipal(domain.ScopeRepoWrite, "other"), &pub, false, domain.ErrForbidden, false},
		{"reader of a private repo", tokenPrincipal(domain.ScopeRepoRead), &priv, false, domain.ErrForbidden, false},
		{"reader of a public repo", tokenPrincipal(domain.ScopeRepoRead), &pub, false, domain.ErrForbidden, false},
		{"token with no relevant scope, private", tokenPrincipal(domain.ScopeMRWrite), &priv, false, domain.ErrNotFound, false},
		{"missing, no create", domain.Root(), nil, false, domain.ErrNotFound, false},
		{"missing, reader may not create", tokenPrincipal(domain.ScopeRepoRead), nil, true, domain.ErrNotFound, false},
		{"missing, writer limited elsewhere", tokenPrincipal(domain.ScopeRepoWrite, "other"), nil, true, domain.ErrNotFound, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPushFixture(t)
			if !tt.p.IsAnonymous() {
				if tt.repo != nil {
					f.meta.EXPECT().Get(ctx, n).Return(*tt.repo, nil)
				} else {
					f.meta.EXPECT().Get(ctx, n).Return(domain.Repo{}, domain.ErrNotFound)
				}
			}
			repo, created, err := f.pushes.Open(ctx, tt.p, n, tt.create)
			if tt.want != nil {
				assert.ErrorIs(t, err, tt.want)
				assert.False(t, created)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.created, created)
			assert.Equal(t, n, repo.Name)
		})
	}

	t.Run("creates a private repository and publishes RepoCreated", func(t *testing.T) {
		f := newPushFixture(t)
		f.meta.EXPECT().Get(ctx, n).Return(domain.Repo{}, domain.ErrNotFound)
		f.git.EXPECT().Stage(ctx).Return(ports.StagedID("s1"), nil)
		want := domain.Repo{Name: n, Visibility: domain.VisibilityPrivate, CreatedAt: now}
		f.meta.EXPECT().Create(ctx, want).Return(nil)
		f.git.EXPECT().Publish(ctx, ports.StagedID("s1"), n).Return(nil)
		f.bus.EXPECT().Publish(ctx, domain.RepoCreated{Repo: n, Visibility: domain.VisibilityPrivate, At: now}).Return()

		repo, created, err := f.pushes.Open(ctx, tokenPrincipal(domain.ScopeRepoWrite, "tools"), n, true)
		require.NoError(t, err)
		assert.True(t, created)
		assert.Equal(t, want, repo)
	})

	t.Run("a concurrent first push wins: this one pushes to its repository", func(t *testing.T) {
		f := newPushFixture(t)
		f.meta.EXPECT().Get(ctx, n).Return(domain.Repo{}, domain.ErrNotFound).Once()
		f.git.EXPECT().Stage(ctx).Return(ports.StagedID("s1"), nil)
		f.meta.EXPECT().Create(ctx, mock.Anything).Return(domain.ErrAlreadyExists)
		f.git.EXPECT().Discard(mock.Anything, ports.StagedID("s1")).Return(nil)
		f.meta.EXPECT().Get(ctx, n).Return(priv, nil).Once()

		repo, created, err := f.pushes.Open(ctx, domain.Root(), n, true)
		require.NoError(t, err)
		assert.False(t, created)
		assert.Equal(t, priv, repo)
	})

	t.Run("store failure", func(t *testing.T) {
		f := newPushFixture(t)
		boom := errors.New("boom")
		f.meta.EXPECT().Get(ctx, n).Return(domain.Repo{}, boom)
		_, _, err := f.pushes.Open(ctx, domain.Root(), n, true)
		assert.ErrorIs(t, err, boom)
	})

	t.Run("create failure is returned and nothing is published", func(t *testing.T) {
		f := newPushFixture(t)
		boom := errors.New("boom")
		f.meta.EXPECT().Get(ctx, n).Return(domain.Repo{}, domain.ErrNotFound)
		f.git.EXPECT().Stage(ctx).Return(ports.StagedID(""), boom)
		_, _, err := f.pushes.Open(ctx, domain.Root(), n, true)
		assert.ErrorIs(t, err, boom)
	})
}

func TestPushesCheck(t *testing.T) {
	ctx := context.Background()
	n := name(t, "tools")
	f := newPushFixture(t)
	ups := []domain.RefUpdate{{Old: domain.ZeroOID, New: oidA, Ref: "refs/heads/main"}}

	require.NoError(t, f.pushes.Check(ctx, domain.Root(), n, ups))
	require.NoError(t, f.pushes.Check(ctx, tokenPrincipal(domain.ScopeRepoWrite, "tools"), n, ups))
	assert.ErrorIs(t, f.pushes.Check(ctx, tokenPrincipal(domain.ScopeRepoRead), n, ups), domain.ErrRejected)
	assert.ErrorIs(t, f.pushes.Check(ctx, domain.Anonymous(), n, ups), domain.ErrRejected)
}

func TestPushesRecord(t *testing.T) {
	ctx := context.Background()
	n := name(t, "tools")
	p := tokenPrincipal(domain.ScopeRepoWrite)
	ups := []domain.RefUpdate{
		{Old: domain.ZeroOID, New: oidA, Ref: "refs/heads/dev"},
		{Old: domain.ZeroOID, New: oidB, Ref: "refs/heads/main"},
		{Old: domain.ZeroOID, New: oidB, Ref: "refs/tags/v1"},
	}

	t.Run("sets pushed_at, publishes the exact updates, schedules maintenance", func(t *testing.T) {
		f := newPushFixture(t)
		repo := domain.Repo{Name: n, Visibility: domain.VisibilityPrivate, CreatedAt: now.Add(-time.Hour)}
		f.meta.EXPECT().Get(ctx, n).Return(repo, nil)
		f.meta.EXPECT().Update(ctx, domain.Repo{Name: n, Visibility: domain.VisibilityPrivate, CreatedAt: repo.CreatedAt, PushedAt: now}).Return(nil)
		f.git.EXPECT().DefaultBranch(ctx, n).Return("main", true, nil)
		f.bus.EXPECT().Publish(ctx, domain.PushEvent{Repo: n, Updates: ups, At: now}).Return()

		require.NoError(t, f.pushes.Record(ctx, p, n, ups))
		assert.Equal(t, []domain.RepoName{n}, f.sched.names)
	})

	t.Run("pushed_at retries after a conflict and never goes backwards", func(t *testing.T) {
		f := newPushFixture(t)
		repo := domain.Repo{Name: n}
		f.meta.EXPECT().Get(ctx, n).Return(repo, nil).Once()
		f.meta.EXPECT().Update(ctx, mock.Anything).Return(domain.ErrConflict).Once()
		later := domain.Repo{Name: n, PushedAt: now.Add(time.Second)}
		f.meta.EXPECT().Get(ctx, n).Return(later, nil).Once() // a later push recorded meanwhile
		f.git.EXPECT().DefaultBranch(ctx, n).Return("main", true, nil)
		f.bus.EXPECT().Publish(ctx, mock.Anything).Return()
		require.NoError(t, f.pushes.Record(ctx, p, n, ups))
	})

	t.Run("a deleted repository is not an error", func(t *testing.T) {
		f := newPushFixture(t)
		f.meta.EXPECT().Get(ctx, n).Return(domain.Repo{}, domain.ErrNotFound)
		f.git.EXPECT().DefaultBranch(ctx, n).Return("main", true, nil)
		f.bus.EXPECT().Publish(ctx, mock.Anything).Return()
		require.NoError(t, f.pushes.Record(ctx, p, n, ups))
	})

	t.Run("every step runs when one fails", func(t *testing.T) {
		f := newPushFixture(t)
		boom := errors.New("boom")
		f.meta.EXPECT().Get(ctx, n).Return(domain.Repo{}, boom)
		f.git.EXPECT().DefaultBranch(ctx, n).Return("", false, boom)
		f.bus.EXPECT().Publish(ctx, mock.Anything).Return()
		err := f.pushes.Record(ctx, p, n, ups)
		assert.ErrorIs(t, err, boom)
		assert.Equal(t, []domain.RepoName{n}, f.sched.names, "maintenance still scheduled")
	})

	t.Run("nil scheduler is allowed", func(t *testing.T) {
		f := newPushFixture(t)
		repos := app.NewRepos(f.meta, f.git, f.clock)
		pushes := app.NewPushes(f.meta, repos, f.git, f.bus, f.clock, nil)
		f.meta.EXPECT().Get(ctx, n).Return(domain.Repo{Name: n}, nil)
		f.meta.EXPECT().Update(ctx, mock.Anything).Return(nil)
		f.git.EXPECT().DefaultBranch(ctx, n).Return("main", true, nil)
		f.bus.EXPECT().Publish(ctx, mock.Anything).Return()
		require.NoError(t, pushes.Record(ctx, p, n, ups))
	})
}

func TestPushesDefaultBranchRule(t *testing.T) {
	ctx := context.Background()
	n := name(t, "tools")
	p := domain.Root()
	up := func(ref string) domain.RefUpdate { return domain.RefUpdate{Old: domain.ZeroOID, New: oidA, Ref: ref} }
	del := func(ref string) domain.RefUpdate { return domain.RefUpdate{Old: oidA, New: domain.ZeroOID, Ref: ref} }

	tests := []struct {
		name    string
		head    string
		exists  bool
		updates []domain.RefUpdate
		want    string // "" = HEAD untouched
	}{
		{"HEAD already valid", "main", true, []domain.RefUpdate{up("refs/heads/dev")}, ""},
		{"main preferred", "trunk", false, []domain.RefUpdate{up("refs/heads/dev"), up("refs/heads/main")}, "main"},
		{"first pushed branch", "main", false, []domain.RefUpdate{up("refs/tags/v1"), up("refs/heads/dev"), up("refs/heads/feature")}, "dev"},
		{"HEAD names another missing branch", "trunk", false, []domain.RefUpdate{up("refs/heads/dev")}, "dev"},
		{"only tags pushed", "main", false, []domain.RefUpdate{up("refs/tags/v1")}, ""},
		{"deleted branches never become HEAD", "main", false, []domain.RefUpdate{del("refs/heads/old"), up("refs/heads/dev")}, "dev"},
		{"only deletions", "main", false, []domain.RefUpdate{del("refs/heads/old")}, ""},
		{"HEAD already points to the only candidate", "dev", false, []domain.RefUpdate{up("refs/heads/dev")}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPushFixture(t)
			f.meta.EXPECT().Get(ctx, n).Return(domain.Repo{Name: n, PushedAt: now}, nil)
			f.git.EXPECT().DefaultBranch(ctx, n).Return(tt.head, tt.exists, nil)
			if tt.want != "" {
				f.git.EXPECT().SetDefaultBranch(ctx, n, tt.want).Return(nil)
			}
			f.bus.EXPECT().Publish(ctx, mock.Anything).Return()
			require.NoError(t, f.pushes.Record(ctx, p, n, tt.updates))
		})
	}

	t.Run("set failure is reported", func(t *testing.T) {
		f := newPushFixture(t)
		boom := errors.New("boom")
		f.meta.EXPECT().Get(ctx, n).Return(domain.Repo{Name: n, PushedAt: now}, nil)
		f.git.EXPECT().DefaultBranch(ctx, n).Return("main", false, nil)
		f.git.EXPECT().SetDefaultBranch(ctx, n, "dev").Return(boom)
		f.bus.EXPECT().Publish(ctx, mock.Anything).Return()
		assert.ErrorIs(t, f.pushes.Record(ctx, p, n, []domain.RefUpdate{up("refs/heads/dev")}), boom)
	})
}

// mockAny matches any argument of a mocked call.
var mockAny = mock.Anything
