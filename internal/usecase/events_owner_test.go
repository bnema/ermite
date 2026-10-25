// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/usecase"
)

func TestWatchOwnerTopicIsUnknownToEveryoneElse(t *testing.T) {
	t.Parallel()
	r := newEventRig(t, 0)
	noop := func(ports.WatchEvent) error { return nil }
	unknown := r.events.Watch(context.Background(), domain.Anonymous(), ports.WatchParams{Topics: []string{"nope"}}, noop)
	require.ErrorIs(t, unknown, domain.ErrInvalid)

	partial := tokenWith("t1", now.Add(0), "pub") // limited to a repository
	for _, p := range []domain.Principal{domain.Anonymous(), partial, issuePrincipal(domain.ScopeIssueWrite)} {
		err := r.events.Watch(context.Background(), p, ports.WatchParams{Topics: []string{"owner"}}, noop)
		require.ErrorIs(t, err, domain.ErrInvalid)
		assert.Equal(t, strings.ReplaceAll(unknown.Error(), "nope", "owner"), err.Error(), "same answer as an unknown topic")
	}
}

func TestWatchOwnerTopicSpansRepositoriesAndHonoursIssuesOff(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newEventRig(t, 16)
		a, b := name(t, "aaa"), name(t, "bbb")
		var mu sync.Mutex
		bMode := domain.IssuesOwner
		r.meta.EXPECT().Get(mock.Anything, a).Return(domain.Repo{Name: a, Visibility: domain.VisibilityPrivate, CreatedAt: now, Settings: withIssues(domain.IssuesOwner)}, nil)
		r.meta.EXPECT().Get(mock.Anything, b).RunAndReturn(func(context.Context, domain.RepoName) (domain.Repo, error) {
			mu.Lock()
			defer mu.Unlock()
			return domain.Repo{Name: b, Visibility: domain.VisibilityPrivate, CreatedAt: now, Settings: withIssues(bMode)}, nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w := r.watch(ctx, domain.Owner(), ports.WatchParams{Topics: []string{"owner"}})
		synctest.Wait()

		r.feed <- domain.IssueOpened{Issue: domain.Issue{Repo: a, Number: 1}, At: now, Visibility: domain.VisibilityPrivate, CreatedAt: now}
		_, ok := w.next(t).Event.(domain.IssueOpened)
		require.True(t, ok)
		r.feed <- domain.MergeRequestOpened{MergeRequest: domain.MergeRequest{Repo: b, Number: 2}, At: now, Visibility: domain.VisibilityPrivate, CreatedAt: now}
		_, ok = w.next(t).Event.(domain.MergeRequestOpened)
		require.True(t, ok, "a second repository")

		// Pushes are not inbox news.
		r.feed <- r.pushEvent(a, sentinelOID, "refs/heads/x")
		r.feed <- domain.IssueOpened{Issue: domain.Issue{Repo: b, Number: 3}, At: now, Visibility: domain.VisibilityPrivate, CreatedAt: now}
		_, ok = w.next(t).Event.(domain.IssueOpened)
		require.True(t, ok, "the push was skipped")

		mu.Lock()
		bMode = domain.IssuesOff
		mu.Unlock()
		r.feed <- domain.IssueOpened{Issue: domain.Issue{Repo: b, Number: 4}, At: now, Visibility: domain.VisibilityPrivate, CreatedAt: now}
		r.feed <- domain.MergeRequestClosed{MergeRequest: domain.MergeRequest{Repo: b, Number: 2}, At: now, Visibility: domain.VisibilityPrivate, CreatedAt: now}
		e := w.next(t)
		_, ok = e.Event.(domain.MergeRequestClosed)
		require.True(t, ok, "the issue event of the issues-off repository is not delivered, got %T", e.Event)
	})
}

// The journal signal is the owner's: the owner topic receives it; the home
// and repository topics (the web and the API streams) never do, whoever
// watches them. The push that follows proves those streams had their turn.
func TestJournalUpdatedReachesOnlyTheOwnerTopic(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newEventRig(t, 16)
		pub := r.repo("pub", domain.VisibilityPublic)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		owner := r.watch(ctx, domain.Owner(), ports.WatchParams{Topics: []string{"owner"}})
		others := map[string]*watcher{
			"home, anonymous": r.watch(ctx, domain.Anonymous(), ports.WatchParams{Topics: []string{"home"}}),
			"repo, anonymous": r.watch(ctx, domain.Anonymous(), ports.WatchParams{Topics: []string{"repo:pub"}}),
			"home, root":      r.watch(ctx, domain.Root(), ports.WatchParams{Topics: []string{"home"}}),
			"repo, owner":     r.watch(ctx, domain.Owner(), ports.WatchParams{Topics: []string{"repo:pub"}}),
		}
		synctest.Wait()

		signal := domain.JournalUpdated{Repo: pub, At: now}
		r.feed <- signal
		r.push(pub, 0)
		assert.Equal(t, signal, owner.next(t).Event)
		for who, w := range others {
			_, ok := w.next(t).Event.(domain.PushEvent)
			assert.True(t, ok, "%s: the push comes first, the signal never", who)
		}
	})
}

func withIssues(m domain.IssuesMode) domain.RepoSettings {
	st := domain.DefaultRepoSettings()
	st.Issues = m
	return st
}

func allScopesToken(mutate func(*domain.Token)) domain.Principal {
	tok := domain.Token{ID: "tok", Scopes: domain.AllScopes()}
	if mutate != nil {
		mutate(&tok)
	}
	return domain.TokenPrincipal(tok)
}

func TestWatchOwnerTopicAccessMatrix(t *testing.T) {
	t.Parallel()
	missingOne := domain.AllScopes()[1:]
	cases := []struct {
		name    string
		p       domain.Principal
		allowed bool
	}{
		{"root", domain.Root(), true},
		{"owner session", domain.Owner(), true},
		{"unrestricted all-scope token", allScopesToken(nil), true},
		{"token missing one scope", allScopesToken(func(tk *domain.Token) { tk.Scopes = missingOne }), false},
		{"repo-limited all-scope token", allScopesToken(func(tk *domain.Token) { tk.Repos = []domain.RepoName{name(t, "aaa")} }), false},
		{"branch-limited all-scope token", allScopesToken(func(tk *domain.Token) { tk.BranchPrefixes = []string{"agent/"} }), false},
		{"anonymous", domain.Anonymous(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				r := newEventRig(t, 16)
				a, b := name(t, "aaa"), name(t, "bbb")
				for _, n := range []domain.RepoName{a, b} {
					r.meta.EXPECT().Get(mock.Anything, n).Return(domain.Repo{Name: n, Visibility: domain.VisibilityPrivate, CreatedAt: now, Settings: withIssues(domain.IssuesOwner)}, nil).Maybe()
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				noop := func(ports.WatchEvent) error { return nil }
				if !tc.allowed {
					err := r.events.Watch(ctx, tc.p, ports.WatchParams{Topics: []string{"owner"}}, noop)
					require.ErrorIs(t, err, domain.ErrInvalid)
					assert.EqualError(t, err, domain.ErrInvalid.Error()+`: unknown topic "owner" (want "home" or "repo:<name>")`)
					return
				}
				w := r.watch(ctx, tc.p, ports.WatchParams{Topics: []string{"owner"}})
				synctest.Wait()
				require.Equal(t, 1, r.events.Watchers())
				r.feed <- domain.IssueOpened{Issue: domain.Issue{Repo: a, Number: 1}, At: now, Visibility: domain.VisibilityPrivate, CreatedAt: now}
				r.feed <- domain.MergeRequestOpened{MergeRequest: domain.MergeRequest{Repo: b, Number: 2}, At: now, Visibility: domain.VisibilityPrivate, CreatedAt: now}
				assert.Equal(t, a, w.next(t).Event.(domain.IssueOpened).Issue.Repo)
				assert.Equal(t, b, w.next(t).Event.(domain.MergeRequestOpened).MergeRequest.Repo)
				issueEdit := domain.IssueEdited{Issue: domain.Issue{Repo: a, Number: 1}, ChangedFields: []string{"body"}, At: now, Visibility: domain.VisibilityPrivate, CreatedAt: now}
				mrEdit := domain.MergeRequestEdited{MergeRequest: domain.MergeRequest{Repo: b, Number: 2}, ChangedFields: []string{"title"}, At: now, Visibility: domain.VisibilityPrivate, CreatedAt: now}
				r.feed <- issueEdit
				r.feed <- mrEdit
				assert.Equal(t, issueEdit, w.next(t).Event)
				assert.Equal(t, mrEdit, w.next(t).Event)

			})
		})
	}
}

func TestWatchOwnerTopicFollowsRepoUpdatedAndDeleted(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newEventRig(t, 16)
		a := r.repo("aaa", domain.VisibilityPrivate)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		owner := r.watch(ctx, domain.Owner(), ports.WatchParams{Topics: []string{"owner"}})
		synctest.Wait()
		r.feed <- domain.RepoUpdated{Repo: a, At: now, Visibility: domain.VisibilityPrivate, Previous: domain.VisibilityPrivate, CreatedAt: now}
		r.feed <- domain.RepoDeleted{Repo: a, Visibility: domain.VisibilityPrivate, At: now}
		_, ok := owner.next(t).Event.(domain.RepoUpdated)
		require.True(t, ok)
		_, ok = owner.next(t).Event.(domain.RepoDeleted)
		require.True(t, ok)
	})
}

func TestWatchOwnerTopicEndsWhenTheTokenIsRevoked(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newEventRigConfig(t, usecase.EventsConfig{RevalidateEvery: -1})
		priv := r.repo("priv", domain.VisibilityPrivate)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w := r.watch(ctx, allScopesToken(nil), ports.WatchParams{Topics: []string{"owner"}})
		synctest.Wait()
		require.Equal(t, 1, r.events.Watchers())
		r.feed <- domain.MergeRequestOpened{MergeRequest: domain.MergeRequest{Repo: priv, Number: 1}, At: now, Visibility: domain.VisibilityPrivate, CreatedAt: now}
		w.next(t)

		r.revoked.Store("tok", true)
		r.feed <- domain.MergeRequestOpened{MergeRequest: domain.MergeRequest{Repo: priv, Number: 2}, At: now, Visibility: domain.VisibilityPrivate, CreatedAt: now}
		synctest.Wait()
		select {
		case err := <-w.done:
			assert.ErrorIs(t, err, domain.ErrUnauthenticated)
		default:
			t.Fatal("the owner watch of a revoked token did not end")
		}
		assert.Empty(t, w.got, "nothing was sent after the revocation")
	})
}

func TestWatchOwnerTopicEndsWhenTheTokenExpires(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newEventRig(t, 0)
		priv := r.repo("priv", domain.VisibilityPrivate)
		// The rig's clock stands at now: this token expired at now.
		p := allScopesToken(func(tk *domain.Token) { tk.ExpiresAt = now })
		w := r.watch(context.Background(), p, ports.WatchParams{Topics: []string{"owner"}})
		r.feed <- domain.MergeRequestOpened{MergeRequest: domain.MergeRequest{Repo: priv, Number: 1}, At: now, Visibility: domain.VisibilityPrivate, CreatedAt: now}
		synctest.Wait()
		select {
		case err := <-w.done:
			assert.ErrorIs(t, err, domain.ErrUnauthenticated)
		default:
			t.Fatal("the owner watch of an expired token did not end")
		}
		assert.Empty(t, w.got)
	})
}
