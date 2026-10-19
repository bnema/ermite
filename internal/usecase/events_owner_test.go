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

func withIssues(m domain.IssuesMode) domain.RepoSettings {
	st := domain.DefaultRepoSettings()
	st.Issues = m
	return st
}
