// SPDX-License-Identifier: AGPL-3.0-or-later
package kvstore_test

import (
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestIssueLinksReplace(t *testing.T) {
	ctx := context.Background()
	s, _, repo := repoWithMRs(t)
	links := s.IssueLinks()
	added, removed, err := links.ReplaceLinks(ctx, repo, 9, []int64{3, 2, 3}, []int64{2}, ports.ReferenceEvent{})
	require.NoError(t, err)
	assert.Equal(t, []domain.IssueLink{{Issue: 2, MergeRequest: 9, Closes: true}, {Issue: 3, MergeRequest: 9}}, added)
	assert.Empty(t, removed)
	got, err := links.LinksOfMergeRequest(ctx, repo, 9)
	require.NoError(t, err)
	assert.Equal(t, added, got)
	added, removed, err = links.ReplaceLinks(ctx, repo, 9, []int64{2, 3}, []int64{2}, ports.ReferenceEvent{})
	require.NoError(t, err)
	assert.Empty(t, added)
	assert.Empty(t, removed)
	added, removed, err = links.ReplaceLinks(ctx, repo, 9, []int64{3, 4}, []int64{3}, ports.ReferenceEvent{})
	require.NoError(t, err)
	assert.Equal(t, []domain.IssueLink{{Issue: 3, MergeRequest: 9, Closes: true}, {Issue: 4, MergeRequest: 9}}, added)
	assert.Equal(t, []domain.IssueLink{{Issue: 2, MergeRequest: 9, Closes: true}}, removed)
	got, err = links.LinksOfIssue(ctx, repo, 2, 100)
	require.NoError(t, err)
	assert.Empty(t, got)
	added, removed, err = links.ReplaceLinks(ctx, repo, 9, []int64{3}, nil, ports.ReferenceEvent{})
	require.NoError(t, err)
	assert.Empty(t, added)
	assert.Equal(t, []domain.IssueLink{{Issue: 3, MergeRequest: 9, Closes: true}, {Issue: 4, MergeRequest: 9}}, removed)
	for _, mr := range []int64{12, 1, 5} {
		_, _, err = links.ReplaceLinks(ctx, repo, mr, []int64{3}, nil, ports.ReferenceEvent{})
		require.NoError(t, err)
	}
	got, err = links.LinksOfIssue(ctx, repo, 3, 2)
	require.NoError(t, err)
	assert.Equal(t, []domain.IssueLink{{Issue: 3, MergeRequest: 1}, {Issue: 3, MergeRequest: 5}}, got)
	require.NoError(t, s.Delete(ctx, repo))
	assert.Zero(t, countKeys(t, s, "isref/"+repo.String()+"/"))
	assert.Zero(t, countKeys(t, s, "mrref/"+repo.String()+"/"))
	_, _, err = links.ReplaceLinks(ctx, repo, 9, []int64{2}, nil, ports.ReferenceEvent{})
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestIssueClosedByRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _, repo := repoWithMRs(t)
	issue := newIssue(t, repo, "closed by merge")
	issue.State = domain.IssueStateClosed
	issue.ClosedByMergeRequest = 17
	stored, err := s.Issues().Create(ctx, issue)
	require.NoError(t, err)
	assert.Equal(t, int64(17), stored.ClosedByMergeRequest)
	stored.ClosedByMergeRequest = 0
	require.NoError(t, s.Issues().Update(ctx, stored))
	stored, err = s.Issues().Get(ctx, repo, stored.Number)
	require.NoError(t, err)
	assert.Zero(t, stored.ClosedByMergeRequest)
}
