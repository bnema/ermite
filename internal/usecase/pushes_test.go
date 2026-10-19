// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

type pushFixture struct {
	meta   *mocks.RepoMetaStore
	git    *mocks.GitRepos
	bus    *mocks.EventBus
	clock  *mocks.Clock
	tokens *mocks.TokenStore
	cirepo *mocks.CIRepoTokenStore
	sched  *mocks.MaintenanceScheduler
	pushes *usecase.Pushes
}

func newPushFixture(t *testing.T) *pushFixture {
	t.Helper()
	f := &pushFixture{
		meta:   mocks.NewRepoMetaStore(t),
		git:    mocks.NewGitRepos(t),
		bus:    mocks.NewEventBus(t),
		clock:  mocks.NewClock(t),
		tokens: mocks.NewTokenStore(t),
		cirepo: mocks.NewCIRepoTokenStore(t),
		sched:  mocks.NewMaintenanceScheduler(t),
	}
	f.clock.EXPECT().Now().Return(now).Maybe()
	repos := usecase.NewRepos(f.meta, f.git, f.bus, f.clock)
	auth := usecase.NewAuth(domain.HashTokenSecret(rootSecret), f.tokens, f.clock).WithRepoTokens(f.cirepo)
	f.pushes = usecase.NewPushes(f.meta, repos, f.git, f.bus, f.clock, f.sched, auth)
	return f
}

const (
	oidA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	oidB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestPushesOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := name(t, "tools")
	priv := domain.Repo{Name: n, Visibility: domain.VisibilityPrivate}
	pub := domain.Repo{Name: n, Visibility: domain.VisibilityPublic}

	tests := []struct {
		name   string
		p      domain.Principal
		repo   *domain.Repo // nil: missing
		create bool
		want   error
	}{
		{"anonymous", domain.Anonymous(), &pub, false, domain.ErrUnauthenticated},
		{"root, existing", domain.Root(), &priv, false, nil},
		{"writer, existing", tokenPrincipal(domain.ScopeRepoWrite), &priv, false, nil},
		{"writer limited to the repo", tokenPrincipal(domain.ScopeRepoWrite, "tools"), &priv, false, nil},
		{"writer limited to another repo, private", tokenPrincipal(domain.ScopeRepoWrite, "other"), &priv, false, domain.ErrNotFound},
		{"writer limited to another repo, public", tokenPrincipal(domain.ScopeRepoWrite, "other"), &pub, false, domain.ErrForbidden},
		{"reader of a private repo", tokenPrincipal(domain.ScopeRepoRead), &priv, false, domain.ErrForbidden},
		{"reader of a public repo", tokenPrincipal(domain.ScopeRepoRead), &pub, false, domain.ErrForbidden},
		{"token with no relevant scope, private", tokenPrincipal(domain.ScopeMRWrite), &priv, false, domain.ErrNotFound},
		{"missing, no create", domain.Root(), nil, false, domain.ErrNotFound},
		{"missing, reader may not create", tokenPrincipal(domain.ScopeRepoRead), nil, true, domain.ErrNotFound},
		{"missing, writer limited elsewhere", tokenPrincipal(domain.ScopeRepoWrite, "other"), nil, true, domain.ErrNotFound},
		{"existing, writer limited to branches", branchLimited(domain.ScopeRepoWrite), &priv, false, nil},
		{"missing, writer limited to branches may not create", branchLimited(domain.ScopeRepoWrite), nil, true, domain.ErrNotFound},
		{"missing, writer limited to branches and to the repo may not create", branchLimited(domain.ScopeRepoWrite, "tools"), nil, true, domain.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newPushFixture(t)
			if !tt.p.IsAnonymous() {
				if tt.repo != nil {
					f.meta.EXPECT().Get(ctx, n).Return(*tt.repo, nil)
				} else {
					f.meta.EXPECT().Get(ctx, n).Return(domain.Repo{}, domain.ErrNotFound)
				}
			}
			// The bus mock expects no Publish: nothing is created here.
			err := f.pushes.Open(ctx, tt.p, n, tt.create)
			if tt.want != nil {
				assert.ErrorIs(t, err, tt.want)
				return
			}
			require.NoError(t, err)
		})
	}

	t.Run("creates a private repository and publishes RepoCreated", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t)
		f.meta.EXPECT().Get(ctx, n).Return(domain.Repo{}, domain.ErrNotFound)
		f.git.EXPECT().Stage(ctx).Return(ports.StagedID("s1"), nil)
		want := domain.Repo{Name: n, Visibility: domain.VisibilityPrivate, CreatedAt: now, Settings: domain.DefaultRepoSettings()}
		f.meta.EXPECT().Create(ctx, want).Return(nil)
		f.git.EXPECT().Publish(ctx, ports.StagedID("s1"), n).Return(nil)
		f.bus.EXPECT().Publish(ctx, domain.RepoCreated{Repo: n, Visibility: domain.VisibilityPrivate, At: now}).Return()

		require.NoError(t, f.pushes.Open(ctx, tokenPrincipal(domain.ScopeRepoWrite, "tools"), n, true))
	})

	t.Run("a concurrent first push wins: this one pushes to its repository", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t)
		f.meta.EXPECT().Get(ctx, n).Return(domain.Repo{}, domain.ErrNotFound).Once()
		f.git.EXPECT().Stage(ctx).Return(ports.StagedID("s1"), nil)
		f.meta.EXPECT().Create(ctx, mock.Anything).Return(domain.ErrAlreadyExists)
		f.git.EXPECT().Discard(mock.Anything, ports.StagedID("s1")).Return(nil)
		f.meta.EXPECT().Get(ctx, n).Return(priv, nil).Once()

		// The bus mock expects no Publish: the repository was not created here.
		require.NoError(t, f.pushes.Open(ctx, domain.Root(), n, true))
	})

	t.Run("store failure", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t)
		boom := errors.New("boom")
		f.meta.EXPECT().Get(ctx, n).Return(domain.Repo{}, boom)
		err := f.pushes.Open(ctx, domain.Root(), n, true)
		assert.ErrorIs(t, err, boom)
	})

	t.Run("create failure is returned and nothing is published", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t)
		boom := errors.New("boom")
		f.meta.EXPECT().Get(ctx, n).Return(domain.Repo{}, domain.ErrNotFound)
		f.git.EXPECT().Stage(ctx).Return(ports.StagedID(""), boom)
		err := f.pushes.Open(ctx, domain.Root(), n, true)
		assert.ErrorIs(t, err, boom)
	})
}

func TestPushesCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := name(t, "tools")
	ups := []domain.RefUpdate{{Old: domain.ZeroOID, New: oidA, Ref: "refs/heads/main"}}
	writer := domain.TokenPrincipal(domain.Token{ID: "w1", Scopes: scopes(domain.ScopeRepoWrite), Repos: names(t, "tools")})
	reader := domain.TokenPrincipal(domain.Token{ID: "r1", Scopes: scopes(domain.ScopeRepoRead)})

	t.Run("root is always valid and needs no lookup", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t) // the token store mock expects no call
		require.NoError(t, f.check(ctx, domain.Root(), n, ups))
	})

	t.Run("a writer whose token still exists", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t)
		f.tokens.EXPECT().Get(ctx, "w1").Return(domain.Token{ID: "w1", Scopes: scopes(domain.ScopeRepoWrite)}, nil)
		require.NoError(t, f.check(ctx, writer, n, ups))
	})

	t.Run("a token revoked during the push is rejected", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t)
		f.tokens.EXPECT().Get(ctx, "w1").Return(domain.Token{}, domain.ErrNotFound)
		assert.ErrorIs(t, f.check(ctx, writer, n, ups), domain.ErrRejected)
	})

	t.Run("a token that expired during the push is rejected", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t)
		f.tokens.EXPECT().Get(ctx, "w1").Return(domain.Token{ID: "w1", Scopes: scopes(domain.ScopeRepoWrite), ExpiresAt: now.Add(-time.Second)}, nil)
		assert.ErrorIs(t, f.check(ctx, writer, n, ups), domain.ErrRejected)
	})

	t.Run("a store failure fails closed", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t)
		boom := errors.New("boom")
		f.tokens.EXPECT().Get(ctx, "w1").Return(domain.Token{}, boom)
		err := f.check(ctx, writer, n, ups)
		require.ErrorIs(t, err, boom)
		assert.NotErrorIs(t, err, domain.ErrRejected, "an internal failure, not a refusal")
	})

	t.Run("a token without write access is rejected", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t)
		f.tokens.EXPECT().Get(ctx, "r1").Return(domain.Token{ID: "r1", Scopes: scopes(domain.ScopeRepoRead)}, nil)
		assert.ErrorIs(t, f.check(ctx, reader, n, ups), domain.ErrRejected)
	})

	t.Run("anonymous is rejected", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t)
		assert.ErrorIs(t, f.check(ctx, domain.Anonymous(), n, ups), domain.ErrRejected)
	})
}

// AC5: a token limited to branch prefixes pushes only under them.
func TestPushesCheckBranchPrefixes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := name(t, "tools")
	limited := domain.TokenPrincipal(domain.Token{
		ID: "b1", Scopes: scopes(domain.ScopeRepoWrite), BranchPrefixes: []string{"agent/claude/", "bot/"},
	})
	up := func(old, new, ref string) domain.RefUpdate { return domain.RefUpdate{Old: old, New: new, Ref: ref} }

	tests := []struct {
		name    string
		updates []domain.RefUpdate
		refused string // the ref named by the error; "" = accepted
	}{
		{"create under the prefix", []domain.RefUpdate{up(domain.ZeroOID, oidA, "refs/heads/agent/claude/x")}, ""},
		{"force-push under the prefix", []domain.RefUpdate{up(oidA, oidB, "refs/heads/agent/claude/x")}, ""},
		{"delete under the prefix", []domain.RefUpdate{up(oidA, domain.ZeroOID, "refs/heads/agent/claude/x")}, ""},
		{"second prefix", []domain.RefUpdate{up(domain.ZeroOID, oidA, "refs/heads/bot/y")}, ""},
		{"several under the prefixes", []domain.RefUpdate{
			up(domain.ZeroOID, oidA, "refs/heads/agent/claude/a"), up(domain.ZeroOID, oidA, "refs/heads/bot/b"),
		}, ""},
		{"main", []domain.RefUpdate{up(oidA, oidB, "refs/heads/main")}, "refs/heads/main"},
		{"delete main", []domain.RefUpdate{up(oidA, domain.ZeroOID, "refs/heads/main")}, "refs/heads/main"},
		{"the prefix itself without a name", []domain.RefUpdate{up(domain.ZeroOID, oidA, "refs/heads/agent/claude")}, "refs/heads/agent/claude"},
		{"a sibling that shares the text", []domain.RefUpdate{up(domain.ZeroOID, oidA, "refs/heads/agent/claudex/y")}, "refs/heads/agent/claudex/y"},
		{"a tag", []domain.RefUpdate{up(domain.ZeroOID, oidA, "refs/tags/v1")}, "refs/tags/v1"},
		{"a tag named like the prefix", []domain.RefUpdate{up(domain.ZeroOID, oidA, "refs/tags/agent/claude/v1")}, "refs/tags/agent/claude/v1"},
		{"a notes ref", []domain.RefUpdate{up(domain.ZeroOID, oidA, "refs/notes/commits")}, "refs/notes/commits"},
		{"a ref outside refs/heads", []domain.RefUpdate{up(domain.ZeroOID, oidA, "refs/agent/claude/x")}, "refs/agent/claude/x"},
		{"a path trick", []domain.RefUpdate{up(domain.ZeroOID, oidA, "refs/heads/agent/claude/../main")}, "refs/heads/agent/claude/../main"},
		{"mixed push: one allowed, one forbidden", []domain.RefUpdate{
			up(domain.ZeroOID, oidA, "refs/heads/agent/claude/ok"), up(oidA, oidB, "refs/heads/main"),
		}, "refs/heads/main"},
		{"mixed push, forbidden first", []domain.RefUpdate{
			up(oidA, oidB, "refs/heads/main"), up(domain.ZeroOID, oidA, "refs/heads/agent/claude/ok"),
		}, "refs/heads/main"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newPushFixture(t)
			f.tokens.EXPECT().Get(ctx, "b1").Return(domain.Token{ID: "b1", Scopes: scopes(domain.ScopeRepoWrite)}, nil)
			err := f.check(ctx, limited, n, tt.updates)
			if tt.refused == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, domain.ErrRejected)
			assert.Contains(t, err.Error(), tt.refused+" is outside the branches this token may push (agent/claude/, bot/)")
		})
	}

	t.Run("a token without limit pushes anywhere", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t)
		f.tokens.EXPECT().Get(ctx, "w1").Return(domain.Token{ID: "w1", Scopes: scopes(domain.ScopeRepoWrite)}, nil)
		free := domain.TokenPrincipal(domain.Token{ID: "w1", Scopes: scopes(domain.ScopeRepoWrite)})
		require.NoError(t, f.check(ctx, free, n, []domain.RefUpdate{
			up(domain.ZeroOID, oidA, "refs/tags/v1"), up(oidA, oidB, "refs/heads/main"),
		}))
	})

	t.Run("root pushes anywhere", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t)
		require.NoError(t, f.check(ctx, domain.Root(), n, []domain.RefUpdate{up(domain.ZeroOID, oidA, "refs/tags/v1")}))
	})

	t.Run("a limited token without repo:write is rejected for the repository first", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t)
		f.tokens.EXPECT().Get(ctx, "b2").Return(domain.Token{ID: "b2", Scopes: scopes(domain.ScopeRepoRead)}, nil)
		p := domain.TokenPrincipal(domain.Token{ID: "b2", Scopes: scopes(domain.ScopeRepoRead), BranchPrefixes: []string{"a/"}})
		err := f.check(ctx, p, n, []domain.RefUpdate{up(domain.ZeroOID, oidA, "refs/heads/a/x")})
		require.ErrorIs(t, err, domain.ErrRejected)
		assert.Contains(t, err.Error(), "no write access")
	})
}

func TestPushesRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := name(t, "tools")
	p := tokenPrincipal(domain.ScopeRepoWrite)
	ups := []domain.RefUpdate{
		{Old: domain.ZeroOID, New: oidA, Ref: "refs/heads/dev"},
		{Old: domain.ZeroOID, New: oidB, Ref: "refs/heads/main"},
		{Old: domain.ZeroOID, New: oidB, Ref: "refs/tags/v1"},
	}

	t.Run("sets pushed_at, publishes the exact updates, schedules maintenance", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t)
		f.sched.EXPECT().Schedule(ctx, n).Return().Once()
		repo := domain.Repo{Name: n, Visibility: domain.VisibilityPrivate, CreatedAt: now.Add(-time.Hour)}
		expectRepoMutation(t, f.meta, repo, domain.Repo{Name: n, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt, PushedAt: now, ActiveAt: now}).Once()
		f.git.EXPECT().DefaultBranch(ctx, n).Return("main", true, nil)
		f.bus.EXPECT().Publish(ctx, domain.PushEvent{Repo: n, Updates: ups, At: now, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt}).Return()

		require.NoError(t, f.pushes.Record(ctx, p, n, ups))
	})

	t.Run("tells the reader that the repository changed", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t)
		f.sched.EXPECT().Schedule(ctx, n).Return().Once()
		fresh := mocks.NewRepoFreshener(t)
		fresh.EXPECT().Refresh(ctx, n).Return().Once()
		f.pushes.WithFreshener(fresh)
		expectPushMutation(t, f.meta, domain.Repo{Name: n}, nil)
		f.git.EXPECT().DefaultBranch(ctx, n).Return("main", true, nil)
		f.bus.EXPECT().Publish(ctx, mock.Anything).Return()
		require.NoError(t, f.pushes.Record(ctx, p, n, ups))
	})

	t.Run("pushed_at preserves a later push", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t)
		f.sched.EXPECT().Schedule(ctx, n).Return().Once()
		later := domain.Repo{Name: n, PushedAt: now.Add(time.Second), ActiveAt: now.Add(time.Second)}
		expectPushMutation(t, f.meta, later, nil) // a later push recorded meanwhile
		f.git.EXPECT().DefaultBranch(ctx, n).Return("main", true, nil)
		f.bus.EXPECT().Publish(ctx, mock.Anything).Return()
		require.NoError(t, f.pushes.Record(ctx, p, n, ups))
	})

	t.Run("a deleted repository is not an error", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t)
		f.sched.EXPECT().Schedule(ctx, n).Return().Once()
		expectPushMutation(t, f.meta, domain.Repo{}, domain.ErrNotFound)
		f.git.EXPECT().DefaultBranch(ctx, n).Return("main", true, nil)
		f.bus.EXPECT().Publish(ctx, mock.Anything).Return()
		require.NoError(t, f.pushes.Record(ctx, p, n, ups))
	})

	t.Run("a repository deleted before the default branch step is not an error", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t)
		f.sched.EXPECT().Schedule(ctx, n).Return().Once()
		expectPushMutation(t, f.meta, domain.Repo{}, domain.ErrNotFound)
		f.git.EXPECT().DefaultBranch(ctx, n).Return("", false, domain.ErrNotFound)
		f.bus.EXPECT().Publish(ctx, mock.Anything).Return()
		require.NoError(t, f.pushes.Record(ctx, p, n, ups))
	})

	t.Run("a repository deleted before HEAD is set is not an error", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t)
		f.sched.EXPECT().Schedule(ctx, n).Return().Once()
		expectPushMutation(t, f.meta, domain.Repo{}, domain.ErrNotFound)
		f.git.EXPECT().DefaultBranch(ctx, n).Return("trunk", false, nil)
		f.git.EXPECT().SetDefaultBranch(ctx, n, "main").Return(domain.ErrNotFound)
		f.bus.EXPECT().Publish(ctx, mock.Anything).Return()
		require.NoError(t, f.pushes.Record(ctx, p, n, ups))
	})

	t.Run("every step runs when one fails", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t)
		f.sched.EXPECT().Schedule(ctx, n).Return().Once()
		boom := errors.New("boom")
		expectPushMutation(t, f.meta, domain.Repo{}, boom)
		f.git.EXPECT().DefaultBranch(ctx, n).Return("", false, boom)
		f.bus.EXPECT().Publish(ctx, mock.Anything).Return()
		err := f.pushes.Record(ctx, p, n, ups)
		assert.ErrorIs(t, err, boom)
	})
}

func TestPushesDefaultBranchRule(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
			f := newPushFixture(t)
			f.sched.EXPECT().Schedule(ctx, n).Return().Once()
			expectPushMutation(t, f.meta, domain.Repo{Name: n, PushedAt: now, ActiveAt: now}, nil)
			f.git.EXPECT().DefaultBranch(ctx, n).Return(tt.head, tt.exists, nil)
			if tt.want != "" {
				f.git.EXPECT().SetDefaultBranch(ctx, n, tt.want).Return(nil)
			}
			f.bus.EXPECT().Publish(ctx, mock.Anything).Return()
			require.NoError(t, f.pushes.Record(ctx, p, n, tt.updates))
		})
	}

	t.Run("set failure is reported", func(t *testing.T) {
		t.Parallel()
		f := newPushFixture(t)
		f.sched.EXPECT().Schedule(ctx, n).Return().Once()
		boom := errors.New("boom")
		expectPushMutation(t, f.meta, domain.Repo{Name: n, PushedAt: now, ActiveAt: now}, nil)
		f.git.EXPECT().DefaultBranch(ctx, n).Return("main", false, nil)
		f.git.EXPECT().SetDefaultBranch(ctx, n, "dev").Return(boom)
		f.bus.EXPECT().Publish(ctx, mock.Anything).Return()
		assert.ErrorIs(t, f.pushes.Record(ctx, p, n, []domain.RefUpdate{up("refs/heads/dev")}), boom)
	})
}

// mockAny matches any argument of a mocked call.
var mockAny = mock.Anything

// check is Pushes.Check for a push with nothing in quarantine.
func (f *pushFixture) check(ctx context.Context, p domain.Principal, n domain.RepoName, ups []domain.RefUpdate) error {
	return f.pushes.Check(ctx, p, n, ups, ports.PushObjects{})
}

type fakeGuard struct {
	path string
	err  error
	got  []domain.RefUpdate
	objs ports.PushObjects
}

func (g *fakeGuard) ChangedWorkflow(_ context.Context, _ domain.RepoName, ups []domain.RefUpdate, o ports.PushObjects) (string, error) {
	g.got, g.objs = ups, o
	return g.path, g.err
}

// A push by the token of a job is judged on the workflows it changes, and by it
// alone: every other principal is not asked.
func TestPushesCheckRefusesWorkflowChangesOfJobTokens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := name(t, "tools")
	ups := []domain.RefUpdate{{Old: oidA, New: oidB, Ref: "refs/heads/main"}, {Old: domain.ZeroOID, New: oidB, Ref: "refs/tags/v1"}}
	objs := ports.PushObjects{Dir: "/d/repos/tools.git/objects/incoming-1"}
	exp := now.Add(time.Hour)
	job := domain.RepoTokenPrincipal(domain.CIRepoToken{ID: "j1", Repo: n, ExpiresAt: exp, Permissions: map[string]string{"contents": "write"}})
	readOnlyJob := domain.RepoTokenPrincipal(domain.CIRepoToken{ID: "j2", Repo: n, ExpiresAt: exp})

	build := func(t *testing.T, g *fakeGuard) *pushFixture {
		f := newPushFixture(t)
		if g != nil {
			f.pushes.WithWorkflowGuard(g)
		}
		f.cirepo.EXPECT().GetRepoToken(ctx, mock.Anything).Return(domain.CIRepoToken{ID: "j1", ExpiresAt: exp}, nil).Maybe()
		return f
	}

	t.Run("no workflow touched", func(t *testing.T) {
		t.Parallel()
		g := &fakeGuard{}
		f := build(t, g)
		require.NoError(t, f.pushes.Check(ctx, job, n, ups, objs))
		assert.Equal(t, ups, g.got, "every ref is judged")
		assert.Equal(t, objs, g.objs, "from the objects of the push in quarantine")
	})
	t.Run("a workflow touched refuses the whole push with GitHub's text", func(t *testing.T) {
		t.Parallel()
		f := build(t, &fakeGuard{path: ".github/workflows/ci.yml"})
		err := f.pushes.Check(ctx, job, n, ups, objs)
		require.ErrorIs(t, err, domain.ErrRejected)
		assert.Contains(t, err.Error(), "refusing to allow a GitHub App to create or update workflow `.github/workflows/ci.yml` without `workflows` permission")
	})
	t.Run("a failure to tell refuses it, not as a policy", func(t *testing.T) {
		t.Parallel()
		f := build(t, &fakeGuard{err: errors.New("git broke")})
		err := f.pushes.Check(ctx, job, n, ups, objs)
		require.Error(t, err)
		assert.NotErrorIs(t, err, domain.ErrRejected)
	})
	t.Run("a push beyond the guard's bounds is refused as a policy, with the reason", func(t *testing.T) {
		t.Parallel()
		f := build(t, &fakeGuard{err: fmt.Errorf("%w: the push brings more than 10000 commits", domain.ErrInvalid)})
		err := f.pushes.Check(ctx, job, n, ups, objs)
		require.ErrorIs(t, err, domain.ErrRejected)
		assert.Contains(t, err.Error(), "more than 10000 commits")
	})
	t.Run("without a guard a job token cannot be judged: refused", func(t *testing.T) {
		t.Parallel()
		f := build(t, nil)
		assert.ErrorIs(t, f.pushes.Check(ctx, job, n, ups, objs), domain.ErrRejected)
	})
	t.Run("a job token without contents: write cannot push at all", func(t *testing.T) {
		t.Parallel()
		f := build(t, &fakeGuard{})
		assert.ErrorIs(t, f.pushes.Check(ctx, readOnlyJob, n, ups, objs), domain.ErrRejected)
	})
	t.Run("the owner is not asked", func(t *testing.T) {
		t.Parallel()
		g := &fakeGuard{path: ".github/workflows/ci.yml"}
		f := build(t, g)
		require.NoError(t, f.pushes.Check(ctx, domain.Root(), n, ups, objs))
		assert.Nil(t, g.got)
	})
	t.Run("a job token never creates a repository on push", func(t *testing.T) {
		t.Parallel()
		f := build(t, &fakeGuard{})
		f.meta.EXPECT().Get(ctx, n).Return(domain.Repo{}, domain.ErrNotFound)
		assert.ErrorIs(t, f.pushes.Open(ctx, job, n, true), domain.ErrNotFound)
	})
}
