// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

type issueUnit struct {
	meta   *mocks.RepoMetaStore
	store  *mocks.IssueStore
	mrs    *mocks.MergeRequestStore
	bus    *mocks.EventBus
	issues *usecase.Issues
	repo   domain.Repo
}

func newIssueUnit(t *testing.T, vis domain.Visibility, mode domain.IssuesMode) *issueUnit {
	t.Helper()
	settings := domain.DefaultRepoSettings()
	settings.Issues = mode
	u := &issueUnit{
		meta:  mocks.NewRepoMetaStore(t),
		store: mocks.NewIssueStore(t),
		mrs:   mocks.NewMergeRequestStore(t),
		bus:   mocks.NewEventBus(t),
		repo:  domain.Repo{Name: name(t, "proj"), Visibility: vis, Settings: settings},
	}
	clock := mocks.NewClock(t)
	clock.EXPECT().Now().Return(now).Maybe()
	u.meta.EXPECT().Get(mock.Anything, u.repo.Name).Return(u.repo, nil).Maybe()
	u.issues = usecase.NewIssues(usecase.IssuesConfig{Meta: u.meta, Store: u.store, MergeRequests: u.mrs, Bus: u.bus, Clock: clock})
	return u
}

func issuePrincipal(scopes ...domain.Scope) domain.Principal {
	return domain.TokenPrincipal(domain.Token{ID: "t", Label: "agent-1", Scopes: scopes, BranchPrefixes: []string{"agent/"}})
}

func TestIssuesOffHidesEverything(t *testing.T) {
	t.Parallel()
	for _, mode := range []domain.IssuesMode{domain.IssuesOff, ""} {
		u := newIssueUnit(t, domain.VisibilityPublic, mode)
		ctx, n := context.Background(), u.repo.Name
		_, err := u.issues.Create(ctx, domain.Root(), n, ports.CreateIssueParams{Title: "t"})
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = u.issues.List(ctx, domain.Root(), n, ports.ListIssuesParams{})
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = u.issues.Get(ctx, domain.Owner(), n, 1)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = u.issues.AddComment(ctx, domain.Root(), n, 1, "x")
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = u.issues.Close(ctx, domain.Root(), n, 1)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = u.issues.Reopen(ctx, domain.Root(), n, 1)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = u.issues.Kind(ctx, domain.Root(), n, 1)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	}
}

func TestIssueAccessRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("private repository hides reads", func(t *testing.T) {
		t.Parallel()
		u := newIssueUnit(t, domain.VisibilityPrivate, domain.IssuesOwner)
		_, err := u.issues.List(ctx, domain.Anonymous(), u.repo.Name, ports.ListIssuesParams{})
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = u.issues.Get(ctx, issuePrincipal(domain.ScopeIssueWrite), u.repo.Name, 1)
		assert.ErrorIs(t, err, domain.ErrNotFound, "issue:write does not read")
	})
	t.Run("writes", func(t *testing.T) {
		t.Parallel()
		u := newIssueUnit(t, domain.VisibilityPublic, domain.IssuesOwner)
		p := ports.CreateIssueParams{Title: "t"}
		_, err := u.issues.Create(ctx, domain.Anonymous(), u.repo.Name, p)
		assert.ErrorIs(t, err, domain.ErrUnauthenticated)
		_, err = u.issues.Create(ctx, issuePrincipal(domain.ScopeRepoRead), u.repo.Name, p)
		assert.ErrorIs(t, err, domain.ErrForbidden)
	})
	t.Run("private write without the scope", func(t *testing.T) {
		t.Parallel()
		u := newIssueUnit(t, domain.VisibilityPrivate, domain.IssuesOwner)
		_, err := u.issues.Create(ctx, issuePrincipal(domain.ScopeRepoWrite), u.repo.Name, ports.CreateIssueParams{Title: "t"})
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
}

func TestIssueCreateAndTransitions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := issuePrincipal(domain.ScopeIssueWrite) // branch-limited: issues write no ref

	t.Run("create", func(t *testing.T) {
		t.Parallel()
		u := newIssueUnit(t, domain.VisibilityPrivate, domain.IssuesOwner)
		u.store.EXPECT().Create(mock.Anything, mock.MatchedBy(func(i domain.Issue) bool {
			return i.Repo == u.repo.Name && i.Title.String() == "Crash" && i.Body.String() == "why" &&
				i.State == domain.IssueStateOpen && i.Author == "agent-1" && i.CreatedAt.Equal(now)
		})).RunAndReturn(func(_ context.Context, i domain.Issue) (domain.Issue, error) { i.Number = 7; return i, nil })
		u.bus.EXPECT().Publish(mock.Anything, mock.MatchedBy(func(ev domain.Event) bool {
			o, ok := ev.(domain.IssueOpened)
			return ok && o.Issue.Number == 7 && o.Visibility == domain.VisibilityPrivate
		})).Return()
		got, err := u.issues.Create(ctx, p, u.repo.Name, ports.CreateIssueParams{Title: " Crash ", Body: "why"})
		require.NoError(t, err)
		assert.EqualValues(t, 7, got.Number)
	})

	t.Run("invalid text stores nothing", func(t *testing.T) {
		t.Parallel()
		u := newIssueUnit(t, domain.VisibilityPrivate, domain.IssuesOwner)
		_, err := u.issues.Create(ctx, p, u.repo.Name, ports.CreateIssueParams{Title: ""})
		var fe *domain.InvalidFieldsError
		require.ErrorAs(t, err, &fe)
	})

	open := domain.Issue{Repo: name(t, "proj"), Number: 3, State: domain.IssueStateOpen, Version: 1}

	t.Run("close with every update conflicting is an error and publishes nothing", func(t *testing.T) {
		t.Parallel()
		u := newIssueUnit(t, domain.VisibilityPublic, domain.IssuesOwner)
		u.store.EXPECT().Get(mock.Anything, u.repo.Name, int64(3)).Return(open, nil)
		u.store.EXPECT().Update(mock.Anything, mock.Anything).Return(domain.ErrConflict)
		_, err := u.issues.Close(ctx, p, u.repo.Name, 3)
		assert.ErrorIs(t, err, domain.ErrConflict)
	})

	t.Run("close retries a conflict then publishes", func(t *testing.T) {
		t.Parallel()
		u := newIssueUnit(t, domain.VisibilityPublic, domain.IssuesOwner)
		u.store.EXPECT().Get(mock.Anything, u.repo.Name, int64(3)).Return(open, nil).Twice()
		u.store.EXPECT().Update(mock.Anything, mock.Anything).Return(domain.ErrConflict).Once()
		u.store.EXPECT().Update(mock.Anything, mock.MatchedBy(func(i domain.Issue) bool { return i.State == domain.IssueStateClosed })).Return(nil).Once()
		u.bus.EXPECT().Publish(mock.Anything, mock.MatchedBy(func(ev domain.Event) bool {
			c, ok := ev.(domain.IssueClosed)
			return ok && c.Issue.State == domain.IssueStateClosed
		})).Return()
		got, err := u.issues.Close(ctx, p, u.repo.Name, 3)
		require.NoError(t, err)
		assert.Equal(t, domain.IssueStateClosed, got.State)
	})

	t.Run("close of a closed issue", func(t *testing.T) {
		t.Parallel()
		u := newIssueUnit(t, domain.VisibilityPublic, domain.IssuesOwner)
		closed := open
		closed.State = domain.IssueStateClosed
		u.store.EXPECT().Get(mock.Anything, u.repo.Name, int64(3)).Return(closed, nil)
		_, err := u.issues.Close(ctx, p, u.repo.Name, 3)
		assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
	})

	t.Run("reopen", func(t *testing.T) {
		t.Parallel()
		u := newIssueUnit(t, domain.VisibilityPublic, domain.IssuesOwner)
		closed := open
		closed.State = domain.IssueStateClosed
		u.store.EXPECT().Get(mock.Anything, u.repo.Name, int64(3)).Return(closed, nil)
		u.store.EXPECT().Update(mock.Anything, mock.Anything).Return(nil)
		u.bus.EXPECT().Publish(mock.Anything, mock.MatchedBy(func(ev domain.Event) bool { _, ok := ev.(domain.IssueReopened); return ok })).Return()
		got, err := u.issues.Reopen(ctx, p, u.repo.Name, 3)
		require.NoError(t, err)
		assert.Equal(t, domain.IssueStateOpen, got.State)
	})

	t.Run("reopen of an open issue", func(t *testing.T) {
		t.Parallel()
		u := newIssueUnit(t, domain.VisibilityPublic, domain.IssuesOwner)
		u.store.EXPECT().Get(mock.Anything, u.repo.Name, int64(3)).Return(open, nil)
		_, err := u.issues.Reopen(ctx, p, u.repo.Name, 3)
		assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
	})
}

func TestIssueAddComment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := issuePrincipal(domain.ScopeIssueWrite)
	issue := domain.Issue{Repo: name(t, "proj"), Number: 3, State: domain.IssueStateClosed, CommentSeq: 4, Version: 1}

	t.Run("every save conflicting is an error and publishes nothing", func(t *testing.T) {
		t.Parallel()
		u := newIssueUnit(t, domain.VisibilityPublic, domain.IssuesOwner)
		u.store.EXPECT().Get(mock.Anything, u.repo.Name, int64(3)).Return(issue, nil)
		u.store.EXPECT().SaveComment(mock.Anything, mock.Anything, mock.Anything).Return(domain.ErrConflict)
		_, err := u.issues.AddComment(ctx, p, u.repo.Name, 3, "hi") // the bus mock fails on any Publish
		assert.ErrorIs(t, err, domain.ErrConflict)
	})

	t.Run("on a closed issue, with a retry", func(t *testing.T) {
		t.Parallel()
		u := newIssueUnit(t, domain.VisibilityPublic, domain.IssuesOwner)
		fresh := issue
		fresh.CommentSeq = 5
		u.store.EXPECT().Get(mock.Anything, u.repo.Name, int64(3)).Return(issue, nil).Once()
		u.store.EXPECT().Get(mock.Anything, u.repo.Name, int64(3)).Return(fresh, nil).Once()
		u.store.EXPECT().SaveComment(mock.Anything, mock.Anything, mock.Anything).Return(domain.ErrConflict).Once()
		u.store.EXPECT().SaveComment(mock.Anything, mock.MatchedBy(func(i domain.Issue) bool { return i.CommentSeq == 6 }),
			mock.MatchedBy(func(c domain.IssueComment) bool { return c.ID == 6 && c.Author == "agent-1" && c.Text.String() == "hi" })).Return(nil).Once()
		u.bus.EXPECT().Publish(mock.Anything, mock.MatchedBy(func(ev domain.Event) bool {
			c, ok := ev.(domain.IssueCommented)
			return ok && c.Number == 3 && c.Comment.ID == 6
		})).Return()
		c, err := u.issues.AddComment(ctx, p, u.repo.Name, 3, "hi")
		require.NoError(t, err)
		assert.EqualValues(t, 6, c.ID)
	})

	t.Run("blank", func(t *testing.T) {
		t.Parallel()
		u := newIssueUnit(t, domain.VisibilityPublic, domain.IssuesOwner)
		u.store.EXPECT().Get(mock.Anything, u.repo.Name, int64(3)).Return(issue, nil)
		_, err := u.issues.AddComment(ctx, p, u.repo.Name, 3, " \n ")
		var fe *domain.InvalidFieldsError
		require.ErrorAs(t, err, &fe)
	})

	t.Run("full", func(t *testing.T) {
		t.Parallel()
		u := newIssueUnit(t, domain.VisibilityPublic, domain.IssuesOwner)
		full := issue
		full.CommentSeq = usecase.MaxCommentsPerIssue
		u.store.EXPECT().Get(mock.Anything, u.repo.Name, int64(3)).Return(full, nil)
		_, err := u.issues.AddComment(ctx, p, u.repo.Name, 3, "x")
		assert.ErrorIs(t, err, domain.ErrResourceExhausted)
	})
}

func TestIssueKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("issue", func(t *testing.T) {
		t.Parallel()
		u := newIssueUnit(t, domain.VisibilityPublic, domain.IssuesOwner)
		u.store.EXPECT().Get(mock.Anything, u.repo.Name, int64(1)).Return(domain.Issue{Number: 1}, nil)
		k, err := u.issues.Kind(ctx, domain.Anonymous(), u.repo.Name, 1)
		require.NoError(t, err)
		assert.Equal(t, domain.NumberIssue, k)
	})
	t.Run("merge request", func(t *testing.T) {
		t.Parallel()
		u := newIssueUnit(t, domain.VisibilityPublic, domain.IssuesOwner)
		u.store.EXPECT().Get(mock.Anything, u.repo.Name, int64(1)).Return(domain.Issue{}, domain.ErrNotFound)
		u.mrs.EXPECT().Get(mock.Anything, u.repo.Name, int64(1)).Return(domain.MergeRequest{Number: 1}, nil)
		k, err := u.issues.Kind(ctx, domain.Anonymous(), u.repo.Name, 1)
		require.NoError(t, err)
		assert.Equal(t, domain.NumberMergeRequest, k)
	})
	t.Run("none", func(t *testing.T) {
		t.Parallel()
		u := newIssueUnit(t, domain.VisibilityPublic, domain.IssuesOwner)
		u.store.EXPECT().Get(mock.Anything, u.repo.Name, int64(1)).Return(domain.Issue{}, domain.ErrNotFound)
		u.mrs.EXPECT().Get(mock.Anything, u.repo.Name, int64(1)).Return(domain.MergeRequest{}, domain.ErrNotFound)
		k, err := u.issues.Kind(ctx, domain.Anonymous(), u.repo.Name, 1)
		require.NoError(t, err)
		assert.Equal(t, domain.NumberNone, k)
	})
	t.Run("private repository", func(t *testing.T) {
		t.Parallel()
		u := newIssueUnit(t, domain.VisibilityPrivate, domain.IssuesOwner)
		_, err := u.issues.Kind(ctx, domain.Anonymous(), u.repo.Name, 1)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
}
