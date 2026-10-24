// SPDX-License-Identifier: AGPL-3.0-or-later
package rpc

import (
	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestEditRPCs(t *testing.T) {
	empty := ""
	for _, failure := range []error{nil, domain.ErrInvalid, domain.ErrPreconditionFailed, domain.ErrForbidden, domain.ErrNotFound} {
		issues := mocks.NewIssueService(t)
		issues.EXPECT().Edit(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), int64(1), ports.EditIssueParams{Body: &empty}).Return(domain.Issue{Number: 1}, failure).Once()
		res, err := (&issueService{issues: issues}).EditIssue(ownerCtx(), connect.NewRequest(&pb.EditIssueRequest{Repo: "demo", Number: 1, Body: &empty}))
		if failure == nil {
			require.NoError(t, err)
			assert.Equal(t, int64(1), res.Msg.GetIssue().GetNumber())
		} else {
			require.Error(t, err)
			assert.Equal(t, connect.CodeOf(connectError(ownerCtx(), failure)), connect.CodeOf(err))
		}
		mrs := mocks.NewMergeRequestService(t)
		mrs.EXPECT().Edit(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), int64(1), ports.EditMergeRequestParams{Description: &empty}).Return(domain.MergeRequest{Number: 1}, failure).Once()
		mr, err := (&mergeRequestService{mrs: mrs}).EditMergeRequest(ownerCtx(), connect.NewRequest(&pb.EditMergeRequestRequest{Repo: "demo", Number: 1, Description: &empty}))
		if failure == nil {
			require.NoError(t, err)
			assert.Equal(t, int64(1), mr.Msg.GetMergeRequest().GetNumber())
		} else {
			require.Error(t, err)
			assert.Equal(t, connect.CodeOf(connectError(ownerCtx(), failure)), connect.CodeOf(err))
		}
	}
}

func TestEditLiveEventConversion(t *testing.T) {
	repo := mustRepoName(t, "demo")
	issue := watchEventToPB(ports.WatchEvent{Event: domain.IssueEdited{Issue: domain.Issue{Repo: repo, Number: 2}, ChangedFields: []string{"body"}}})
	require.NotNil(t, issue.GetIssueEdited())
	assert.Equal(t, int64(2), issue.GetIssueEdited().GetIssue().GetNumber())
	assert.Equal(t, []string{"body"}, issue.GetIssueEdited().GetChangedFields())
	assert.Nil(t, issue.GetIssueCommented())
	mr := watchEventToPB(ports.WatchEvent{Event: domain.MergeRequestEdited{MergeRequest: domain.MergeRequest{Repo: repo, Number: 3}, ChangedFields: []string{"title"}}})
	require.NotNil(t, mr.GetMergeRequestEdited())
	assert.Equal(t, int64(3), mr.GetMergeRequestEdited().GetMergeRequest().GetNumber())
	assert.Equal(t, []string{"title"}, mr.GetMergeRequestEdited().GetChangedFields())
	assert.Nil(t, mr.GetCommentEdited())
}
