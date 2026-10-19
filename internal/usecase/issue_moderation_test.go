// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

type moderationUnit struct {
	meta    *mocks.RepoMetaStore
	pending *mocks.PendingIssueStore
	bans    *mocks.IssueBanStore
	bus     *mocks.EventBus
	mod     *usecase.IssueModeration
	repo    domain.Repo
	id      domain.PendingIssueID
}

func newModerationUnit(t *testing.T, mode domain.IssuesMode) *moderationUnit {
	t.Helper()
	id, err := domain.NewPendingIssueID()
	require.NoError(t, err)
	u := &moderationUnit{
		meta:    mocks.NewRepoMetaStore(t),
		pending: mocks.NewPendingIssueStore(t),
		bans:    mocks.NewIssueBanStore(t),
		bus:     mocks.NewEventBus(t),
		repo:    domain.Repo{Name: name(t, "proj"), Visibility: domain.VisibilityPublic, CreatedAt: now, Settings: withIssues(mode)},
		id:      id,
	}
	clock := mocks.NewClock(t)
	clock.EXPECT().Now().Return(now).Maybe()
	u.meta.EXPECT().Get(mock.Anything, u.repo.Name).Return(u.repo, nil).Maybe()
	u.mod = usecase.NewIssueModeration(usecase.IssueModerationConfig{Meta: u.meta, Pending: u.pending, Bans: u.bans, Bus: u.bus, Clock: clock})
	return u
}

func TestIssueModerationIsOwnerOnly(t *testing.T) {
	t.Parallel()
	u := newModerationUnit(t, domain.IssuesPublic) // no store is ever called
	ctx := context.Background()
	notOwners := map[string]domain.Principal{
		"anonymous":    domain.Anonymous(),
		"issue writer": issuePrincipal(domain.ScopeIssueWrite),
		"one scope short": allScopesToken(func(tk *domain.Token) {
			tk.Scopes = tk.Scopes[1:]
		}),
		"repo limited":   allScopesToken(func(tk *domain.Token) { tk.Repos = []domain.RepoName{u.repo.Name} }),
		"branch limited": allScopesToken(func(tk *domain.Token) { tk.BranchPrefixes = []string{"agent/"} }),
	}
	for who, p := range notOwners {
		_, err := u.mod.ListPending(ctx, p, domain.RepoName{}, ports.ListPendingParams{})
		assert.ErrorIs(t, err, domain.ErrNotFound, who)
		_, err = u.mod.Approve(ctx, p, u.repo.Name, u.id)
		assert.ErrorIs(t, err, domain.ErrNotFound, who)
		assert.ErrorIs(t, u.mod.Reject(ctx, p, u.repo.Name, u.id, true), domain.ErrNotFound, who)
	}
}

func TestIssueModerationListAppliesPageLimits(t *testing.T) {
	t.Parallel()
	u := newModerationUnit(t, domain.IssuesPublic)
	u.pending.EXPECT().List(mock.Anything, domain.RepoName{}, ports.ListPendingParams{PageSize: usecase.DefaultPageSize, PageToken: "t"}).
		Return(ports.PendingIssuePage{NextPageToken: "n"}, nil).Once()
	got, err := u.mod.ListPending(context.Background(), domain.Owner(), domain.RepoName{}, ports.ListPendingParams{PageToken: "t"})
	require.NoError(t, err)
	assert.Equal(t, "n", got.NextPageToken)
	_, err = u.mod.ListPending(context.Background(), domain.Owner(), domain.RepoName{}, ports.ListPendingParams{PageSize: -1})
	assert.ErrorIs(t, err, domain.ErrInvalidPageSize)
}

func TestIssueModerationApprove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("publishes IssueOpened after the commit", func(t *testing.T) {
		u := newModerationUnit(t, domain.IssuesPublic)
		issue := domain.Issue{Repo: u.repo.Name, Number: 7, State: domain.IssueStateOpen}
		u.pending.EXPECT().Approve(mock.Anything, u.repo.Name, u.id).Return(issue, nil).Once()
		u.bus.EXPECT().Publish(mock.Anything, domain.IssueOpened{Issue: issue, At: now, Visibility: u.repo.Visibility, CreatedAt: u.repo.CreatedAt}).Once()
		got, err := u.mod.Approve(ctx, domain.Owner(), u.repo.Name, u.id)
		require.NoError(t, err)
		assert.Equal(t, int64(7), got.Number)
	})
	t.Run("issues off hides the repository", func(t *testing.T) {
		for _, mode := range []domain.IssuesMode{domain.IssuesOff, ""} {
			u := newModerationUnit(t, mode)
			_, err := u.mod.Approve(ctx, domain.Root(), u.repo.Name, u.id)
			assert.ErrorIs(t, err, domain.ErrNotFound)
		}
	})
	t.Run("no event when the store refuses", func(t *testing.T) {
		u := newModerationUnit(t, domain.IssuesOwner)
		u.pending.EXPECT().Approve(mock.Anything, u.repo.Name, u.id).Return(domain.Issue{}, domain.ErrNotFound).Once()
		_, err := u.mod.Approve(ctx, domain.Root(), u.repo.Name, u.id)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
}

func TestIssueModerationReject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hash := bytes.Repeat([]byte{3}, domain.IPHashLen)
	t.Run("with a ban", func(t *testing.T) {
		u := newModerationUnit(t, domain.IssuesPublic)
		u.pending.EXPECT().Get(mock.Anything, u.repo.Name, u.id).Return(domain.PendingIssue{ID: u.id, Repo: u.repo.Name, IPHash: hash}, nil).Once()
		u.bans.EXPECT().Ban(mock.Anything, hash, now.Add(domain.BanDuration)).Return(nil).Once()
		u.pending.EXPECT().Delete(mock.Anything, u.repo.Name, u.id).Return(nil).Once()
		u.bus.EXPECT().Publish(mock.Anything, domain.IssuePendingRemoved{Repo: u.repo.Name, ID: u.id, At: now}).Once()
		require.NoError(t, u.mod.Reject(ctx, domain.Owner(), u.repo.Name, u.id, true))
	})
	t.Run("without a ban", func(t *testing.T) {
		u := newModerationUnit(t, domain.IssuesPublic)
		u.pending.EXPECT().Get(mock.Anything, u.repo.Name, u.id).Return(domain.PendingIssue{ID: u.id, IPHash: hash}, nil).Once()
		u.pending.EXPECT().Delete(mock.Anything, u.repo.Name, u.id).Return(nil).Once()
		u.bus.EXPECT().Publish(mock.Anything, domain.IssuePendingRemoved{Repo: u.repo.Name, ID: u.id, At: now}).Once()
		require.NoError(t, u.mod.Reject(ctx, domain.Owner(), u.repo.Name, u.id, false))
	})
	t.Run("unknown id", func(t *testing.T) {
		u := newModerationUnit(t, domain.IssuesPublic)
		u.pending.EXPECT().Get(mock.Anything, u.repo.Name, u.id).Return(domain.PendingIssue{}, domain.ErrNotFound).Once()
		assert.ErrorIs(t, u.mod.Reject(ctx, domain.Owner(), u.repo.Name, u.id, true), domain.ErrNotFound)
	})
	t.Run("a failed ban keeps the pending issue", func(t *testing.T) {
		u := newModerationUnit(t, domain.IssuesPublic)
		u.pending.EXPECT().Get(mock.Anything, u.repo.Name, u.id).Return(domain.PendingIssue{ID: u.id, IPHash: hash}, nil).Once()
		u.bans.EXPECT().Ban(mock.Anything, hash, mock.Anything).Return(assert.AnError).Once()
		assert.ErrorIs(t, u.mod.Reject(ctx, domain.Owner(), u.repo.Name, u.id, true), assert.AnError)
	})
}

func TestIssueKeysSubkeys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	master := bytes.Repeat([]byte{0xAB}, 32)
	store := mocks.NewIssueKeyStore(t)
	store.EXPECT().Key(mock.Anything).Return(master, nil)
	k1, err := usecase.NewIssueKeys(ctx, store)
	require.NoError(t, err)
	k2, err := usecase.NewIssueKeys(ctx, store)
	require.NoError(t, err)

	assert.Equal(t, k1.PoWKey(), k2.PoWKey(), "stable")
	assert.Equal(t, k1.FormTokenKey(), k2.FormTokenKey())
	assert.Equal(t, k1.IPHash("1.2.3.4"), k2.IPHash("1.2.3.4"))

	assert.Len(t, k1.PoWKey(), 32)
	assert.NotEqual(t, k1.PoWKey(), k1.FormTokenKey(), "one key per purpose")
	assert.NotEqual(t, master, k1.PoWKey())
	assert.NotEqual(t, master, k1.FormTokenKey())

	h := k1.IPHash("1.2.3.4")
	assert.Len(t, h, domain.IPHashLen)
	assert.NotEqual(t, h, k1.IPHash("1.2.3.5"))
	assert.NotContains(t, string(h), "1.2.3.4")

	other := mocks.NewIssueKeyStore(t)
	other.EXPECT().Key(mock.Anything).Return(bytes.Repeat([]byte{0xCD}, 32), nil)
	k3, err := usecase.NewIssueKeys(ctx, other)
	require.NoError(t, err)
	assert.NotEqual(t, h, k3.IPHash("1.2.3.4"), "another server key, another hash")

	// The returned keys are copies.
	pow := k1.PoWKey()
	pow[0] ^= 0xFF
	assert.NotEqual(t, pow, k1.PoWKey())
}

func TestIssueSubmittedReachesOnlyTheOwnerTopic(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newEventRig(t, 16)
		a := r.repo("aaa", domain.VisibilityPublic)
		r.meta.EXPECT().Get(mock.Anything, a).Unset()
		r.meta.EXPECT().Get(mock.Anything, a).Return(domain.Repo{Name: a, Visibility: domain.VisibilityPublic, CreatedAt: now, Settings: withIssues(domain.IssuesPublic)}, nil).Maybe()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		owner := r.watch(ctx, domain.Owner(), ports.WatchParams{Topics: []string{"owner"}})
		home := r.watch(ctx, domain.Owner(), ports.WatchParams{Topics: []string{"home"}})
		repoTopic := r.watch(ctx, domain.Owner(), ports.WatchParams{Topics: []string{"repo:aaa"}})
		rootRepo := r.watch(ctx, domain.Root(), ports.WatchParams{Topics: []string{"repo:aaa", "home"}})
		anon := r.watch(ctx, domain.Anonymous(), ports.WatchParams{Topics: []string{"repo:aaa", "home"}})
		synctest.Wait()

		r.feed <- domain.IssueSubmitted{Repo: a, ID: "x", At: now}
		// A sentinel every watcher sees, to prove the first one was skipped.
		r.feed <- domain.IssueOpened{Issue: domain.Issue{Repo: a, Number: 1}, At: now, Visibility: domain.VisibilityPublic, CreatedAt: now}

		sub, ok := owner.next(t).Event.(domain.IssueSubmitted)
		require.True(t, ok, "the owner topic gets it")
		assert.Equal(t, a, sub.Repo)
		_, ok = owner.next(t).Event.(domain.IssueOpened)
		require.True(t, ok)
		for name, w := range map[string]*watcher{"home": home, "repo topic": repoTopic, "root repo+home": rootRepo, "anonymous": anon} {
			e, ok := w.next(t).Event.(domain.IssueOpened)
			require.True(t, ok, "%s: the first event is the sentinel, not issue.submitted (got %T)", name, e)
		}
	})
}

func TestIssuePendingRemovedReachesOnlyOwnerTopics(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newEventRig(t, 16)
		a := r.repo("aaa", domain.VisibilityPublic)
		r.meta.EXPECT().Get(mock.Anything, a).Unset()
		r.meta.EXPECT().Get(mock.Anything, a).Return(domain.Repo{Name: a, Visibility: domain.VisibilityPublic, CreatedAt: now, Settings: withIssues(domain.IssuesPublic)}, nil).Maybe()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		owner1 := r.watch(ctx, domain.Owner(), ports.WatchParams{Topics: []string{"owner"}})
		owner2 := r.watch(ctx, domain.Owner(), ports.WatchParams{Topics: []string{"owner"}})
		home := r.watch(ctx, domain.Owner(), ports.WatchParams{Topics: []string{"home"}})
		repoTopic := r.watch(ctx, domain.Owner(), ports.WatchParams{Topics: []string{"repo:aaa"}})
		anon := r.watch(ctx, domain.Anonymous(), ports.WatchParams{Topics: []string{"repo:aaa", "home"}})
		synctest.Wait()

		r.feed <- domain.IssuePendingRemoved{Repo: a, ID: "x", At: now}
		r.feed <- domain.IssueOpened{Issue: domain.Issue{Repo: a, Number: 1}, At: now, Visibility: domain.VisibilityPublic, CreatedAt: now}

		for name, w := range map[string]*watcher{"owner 1": owner1, "owner 2": owner2} {
			_, ok := w.next(t).Event.(domain.IssuePendingRemoved)
			require.True(t, ok, "%s gets the refresh", name)
			_, ok = w.next(t).Event.(domain.IssueOpened)
			require.True(t, ok)
		}
		for name, w := range map[string]*watcher{"home": home, "repo topic": repoTopic, "anonymous": anon} {
			e, ok := w.next(t).Event.(domain.IssueOpened)
			require.True(t, ok, "%s: the first event is the sentinel (got %T)", name, e)
		}
	})
}
