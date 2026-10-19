// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/eventbus"
	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

// Issues over the real store: shared numbers with merge requests, the life of
// an issue and its events.
func TestIssuesOverTheStore(t *testing.T) {
	ctx := context.Background()
	store, err := kvstore.Open(ctx, filepath.Join(t.TempDir(), "kv"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })
	bus := eventbus.NewWithBuffer(64)
	events := bus.Subscribe(ctx)
	n := name(t, "proj")
	settings := domain.DefaultRepoSettings()
	settings.Issues = domain.IssuesOwner
	require.NoError(t, store.Create(ctx, domain.Repo{Name: n, Visibility: domain.VisibilityPublic, CreatedAt: now, Settings: settings}))

	issues := usecase.NewIssues(usecase.IssuesConfig{Meta: store, Store: store.Issues(), MergeRequests: store.MergeRequests(), Bus: bus, Clock: fixedClock{}})
	agent := domain.TokenPrincipal(domain.Token{ID: "t", Label: "bot", Scopes: []domain.Scope{domain.ScopeIssueWrite}, BranchPrefixes: []string{"x/"}})

	mrTitle, err := domain.ParseMRTitle("Change")
	require.NoError(t, err)
	mr, err := store.MergeRequests().Create(ctx, domain.MergeRequest{
		Repo: n, Title: mrTitle, State: domain.MRStateOpen, Source: "feat", Target: "main", Author: "a", CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	assert.EqualValues(t, 1, mr.Number)

	issue, err := issues.Create(ctx, agent, n, ports.CreateIssueParams{Title: "Crash", Body: "details"})
	require.NoError(t, err)
	assert.EqualValues(t, 2, issue.Number, "the number follows the merge request")
	assert.Equal(t, "bot", issue.Author)
	repo, err := store.Get(ctx, n)
	require.NoError(t, err)
	assert.True(t, repo.ActiveAt.Equal(now))
	assert.True(t, repo.PushedAt.IsZero())

	kind, err := issues.Kind(ctx, domain.Anonymous(), n, 1)
	require.NoError(t, err)
	assert.Equal(t, domain.NumberMergeRequest, kind)
	kind, err = issues.Kind(ctx, domain.Anonymous(), n, 2)
	require.NoError(t, err)
	assert.Equal(t, domain.NumberIssue, kind)
	kind, err = issues.Kind(ctx, domain.Anonymous(), n, 3)
	require.NoError(t, err)
	assert.Equal(t, domain.NumberNone, kind)

	_, err = issues.AddComment(ctx, agent, n, 2, "me too")
	require.NoError(t, err)
	_, err = issues.Close(ctx, agent, n, 2)
	require.NoError(t, err)
	_, err = issues.AddComment(ctx, agent, n, 2, "after close")
	require.NoError(t, err)
	_, err = issues.Close(ctx, agent, n, 2)
	assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
	_, err = issues.Reopen(ctx, agent, n, 2)
	require.NoError(t, err)

	d, err := issues.Get(ctx, domain.Anonymous(), n, 2)
	require.NoError(t, err)
	assert.Equal(t, domain.IssueStateOpen, d.Issue.State)
	require.Len(t, d.Comments, 2)
	assert.Equal(t, "me too", d.Comments[0].Text.String())
	assert.EqualValues(t, 2, d.Issue.CommentSeq)

	page, err := issues.List(ctx, domain.Anonymous(), n, ports.ListIssuesParams{State: domain.IssueStateClosed})
	require.NoError(t, err)
	assert.Empty(t, page.Issues)

	var kinds []string
	for len(events) > 0 {
		kinds = append(kinds, (<-events).EventKind())
	}
	assert.Equal(t, []string{"issue.opened", "issue.commented", "issue.closed", "issue.commented", "issue.reopened"}, kinds)
}

// Moderated submissions are private work until approval makes them public activity.
func TestModeratedIssueActivityOverStore(t *testing.T) {
	ctx := t.Context()
	store, err := kvstore.Open(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close(ctx)) })
	n := name(t, "proj")
	initial := now.Add(-time.Hour)
	require.NoError(t, store.Create(ctx, domain.Repo{Name: n, Visibility: domain.VisibilityPublic, CreatedAt: initial, ActiveAt: initial, Settings: withIssues(domain.IssuesPublic)}))
	clock := mocks.NewClock(t)
	at := now
	clock.EXPECT().Now().RunAndReturn(func() time.Time { return at }).Maybe()
	keys, err := usecase.NewIssueKeys(ctx, store.IssueKeys())
	require.NoError(t, err)
	limiter := mocks.NewSubmissionLimiter(t)
	limiter.EXPECT().Recent(mock.Anything, mock.Anything).Return(0, 0).Maybe()
	limiter.EXPECT().Reserve(mock.Anything, mock.Anything).Return(true, time.Duration(0), func() {}).Maybe()
	bus := mocks.NewEventBus(t)
	var kinds []string
	bus.EXPECT().Publish(mock.Anything, mock.Anything).Run(func(_ context.Context, ev domain.Event) {
		repo, err := store.Get(ctx, n)
		require.NoError(t, err)
		if ev.EventKind() == "issue.opened" {
			require.Equal(t, at, repo.ActiveAt)
		} else {
			require.Equal(t, initial, repo.ActiveAt)
		}
		kinds = append(kinds, ev.EventKind())
	}).Maybe()
	submissions := usecase.NewIssueSubmissions(usecase.IssueSubmissionsConfig{Meta: store, Pending: store.PendingIssues(), Bans: store.IssueBans(), Limiter: limiter, Keys: keys, Bus: bus, Clock: clock, BaseDifficulty: testBits, MaxDifficulty: testBits})
	moderation := usecase.NewIssueModeration(usecase.IssueModerationConfig{Meta: store, Pending: store.PendingIssues(), Bans: store.IssueBans(), Bus: bus, Clock: clock})
	submit := func() domain.PendingIssueID {
		ch, err := submissions.Challenge(ctx, n, "client")
		require.NoError(t, err)
		sub := ports.IssueSubmission{Token: ch.Token, FormToken: ch.FormToken, Solution: solve(t, ch), Title: "Bug", Body: "Details"}
		at = at.Add(usecase.MinFillTime)
		require.NoError(t, submissions.Submit(ctx, n, "client", sub))
		repo, err := store.Get(ctx, n)
		require.NoError(t, err)
		require.Equal(t, initial, repo.ActiveAt)
		page, err := store.PendingIssues().List(ctx, n, ports.ListPendingParams{PageSize: 10})
		require.NoError(t, err)
		require.Len(t, page.Issues, 1)
		return page.Issues[0].ID
	}
	id := submit()
	require.NoError(t, moderation.Reject(ctx, domain.Owner(), n, id, false))
	repo, err := store.Get(ctx, n)
	require.NoError(t, err)
	require.Equal(t, initial, repo.ActiveAt)
	id = submit()
	_, err = moderation.Approve(ctx, domain.Owner(), n, id)
	require.NoError(t, err)
	repo, err = store.Get(ctx, n)
	require.NoError(t, err)
	require.Equal(t, at, repo.ActiveAt)
	require.True(t, repo.PushedAt.IsZero())
	require.Equal(t, []string{"issue.submitted", "issue.pending_removed", "issue.submitted", "issue.opened"}, kinds)
}
