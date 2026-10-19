// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/eventbus"
	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
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
