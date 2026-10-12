// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

func TestMergeRequestServiceCreate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		repo     string
		usecase  error
		wantCode connect.Code
		wantCall bool
	}{
		{name: "ok", repo: "demo", wantCall: true},
		{name: "invalid repo", repo: "../x", wantCode: connect.CodeInvalidArgument},
		{name: "not found", repo: "demo", usecase: domain.ErrNotFound, wantCode: connect.CodeNotFound, wantCall: true},
		{name: "forbidden", repo: "demo", usecase: domain.ErrForbidden, wantCode: connect.CodePermissionDenied, wantCall: true},
		{name: "already open", repo: "demo", usecase: domain.ErrAlreadyExists, wantCode: connect.CodeAlreadyExists, wantCall: true},
		{name: "invalid fields", repo: "demo", usecase: domain.ErrInvalid, wantCode: connect.CodeInvalidArgument, wantCall: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mrs := mocks.NewMergeRequestService(t)
			if tt.wantCall {
				mrs.EXPECT().Create(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), ports.CreateMergeRequestParams{
					Title: "t", Description: "d", Source: "feat", Target: "main",
				}).Return(domain.MergeRequest{Number: 7, Source: "feat", Target: "main", State: domain.MRStateOpen}, tt.usecase).Once()
			}
			resp, err := (&mergeRequestService{mrs}).CreateMergeRequest(ownerCtx(), connect.NewRequest(&pb.CreateMergeRequestRequest{
				Repo: tt.repo, Title: "t", Description: "d", SourceBranch: "feat", TargetBranch: "main",
			}))
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				return
			}
			require.NoError(t, err)
			assert.EqualValues(t, 7, resp.Msg.GetMergeRequest().GetNumber())
			assert.Equal(t, pb.MergeRequestState_MERGE_REQUEST_STATE_OPEN, resp.Msg.GetMergeRequest().GetState())
		})
	}
}

func TestMergeRequestServiceListRejectsUnknownState(t *testing.T) {
	t.Parallel()
	_, err := (&mergeRequestService{mocks.NewMergeRequestService(t)}).ListMergeRequests(ownerCtx(),
		connect.NewRequest(&pb.ListMergeRequestsRequest{Repo: "demo", State: pb.MergeRequestState(99)}))
	requireCode(t, connect.CodeInvalidArgument, err)
}

func TestMergeRequestServiceListFiltersByState(t *testing.T) {
	t.Parallel()
	mrs := mocks.NewMergeRequestService(t)
	mrs.EXPECT().List(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), ports.ListMergeRequestsParams{
		State: domain.MRStateMerged, PageSize: 3, PageToken: "tok",
	}).Return(ports.MergeRequestPage{MergeRequests: []domain.MergeRequest{{Number: 1}}, NextPageToken: "n"}, nil).Once()
	resp, err := (&mergeRequestService{mrs}).ListMergeRequests(ownerCtx(), connect.NewRequest(&pb.ListMergeRequestsRequest{
		Repo: "demo", State: pb.MergeRequestState_MERGE_REQUEST_STATE_MERGED, PageSize: 3, PageToken: "tok",
	}))
	require.NoError(t, err)
	assert.Len(t, resp.Msg.GetMergeRequests(), 1)
	assert.Equal(t, "n", resp.Msg.GetNextPageToken())
}

func TestMergeRequestServiceAddComment(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		side     pb.DiffSide
		usecase  error
		wantCode connect.Code
		wantCall bool
	}{
		{name: "ok", side: pb.DiffSide_DIFF_SIDE_NEW, wantCall: true},
		{name: "unknown side", side: pb.DiffSide(99), wantCode: connect.CodeInvalidArgument},
		{name: "merge request not open", side: pb.DiffSide_DIFF_SIDE_NEW, usecase: domain.ErrPreconditionFailed, wantCode: connect.CodeFailedPrecondition, wantCall: true},
		{name: "concurrent update", side: pb.DiffSide_DIFF_SIDE_NEW, usecase: domain.ErrConflict, wantCode: connect.CodeAborted, wantCall: true},
		{name: "not found", side: pb.DiffSide_DIFF_SIDE_NEW, usecase: domain.ErrNotFound, wantCode: connect.CodeNotFound, wantCall: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mrs := mocks.NewMergeRequestService(t)
			if tt.wantCall {
				mrs.EXPECT().AddComment(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), int64(2), mock.Anything).
					Return(domain.Comment{ID: 5}, tt.usecase).Once()
			}
			resp, err := (&mergeRequestService{mrs}).AddComment(ownerCtx(), connect.NewRequest(&pb.AddCommentRequest{
				Repo: "demo", Number: 2, Path: "f.go", Line: 3, Side: tt.side, Text: "hi",
			}))
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "5", resp.Msg.GetComment().GetId())
		})
	}
}

func TestMergeRequestServiceResolveCommentMapsUnissuedIDsToZero(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"", "abc", "-1", "0", "007", "99999999999999999999"} {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			mrs := mocks.NewMergeRequestService(t)
			mrs.EXPECT().ResolveComment(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), int64(1), int64(0), true).
				Return(domain.Comment{}, domain.ErrNotFound).Once()
			_, err := (&mergeRequestService{mrs}).ResolveComment(ownerCtx(), connect.NewRequest(&pb.ResolveCommentRequest{
				Repo: "demo", Number: 1, CommentId: id, Resolved: true,
			}))
			requireCode(t, connect.CodeNotFound, err)
		})
	}
}

func TestMergeRequestServiceMerge(t *testing.T) {
	t.Parallel()
	const head = "0123456789abcdef0123456789abcdef01234567"
	tests := []struct {
		name     string
		req      *pb.MergeMergeRequestRequest
		usecase  error
		wantCode connect.Code
		wantCall bool
	}{
		{name: "ok", req: &pb.MergeMergeRequestRequest{Repo: "demo", Number: 1, HeadId: head, DeleteSourceBranch: true}, wantCall: true},
		{name: "head is not a full id", req: &pb.MergeMergeRequestRequest{Repo: "demo", Number: 1, HeadId: "abc"}, wantCode: connect.CodeInvalidArgument},
		{name: "invalid repo", req: &pb.MergeMergeRequestRequest{Repo: "../x", Number: 1}, wantCode: connect.CodeInvalidArgument},
		{name: "head moved", req: &pb.MergeMergeRequestRequest{Repo: "demo", Number: 1, HeadId: head}, usecase: domain.ErrConflict, wantCode: connect.CodeAborted, wantCall: true},
		{name: "checks failing", req: &pb.MergeMergeRequestRequest{Repo: "demo", Number: 1}, usecase: domain.ErrPreconditionFailed, wantCode: connect.CodeFailedPrecondition, wantCall: true},
		{name: "forbidden", req: &pb.MergeMergeRequestRequest{Repo: "demo", Number: 1}, usecase: domain.ErrForbidden, wantCode: connect.CodePermissionDenied, wantCall: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mrs := mocks.NewMergeRequestService(t)
			if tt.wantCall {
				mrs.EXPECT().Merge(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), int64(1), mock.MatchedBy(func(p ports.MergeParams) bool {
					return p.DeleteSource == tt.req.GetDeleteSourceBranch() && (tt.req.GetHeadId() == "") == p.HeadID.IsZero()
				})).Return(ports.MergeResult{MergeRequest: domain.MergeRequest{Number: 1}, SourceDeleted: true}, tt.usecase).Once()
			}
			resp, err := (&mergeRequestService{mrs}).MergeMergeRequest(ownerCtx(), connect.NewRequest(tt.req))
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				return
			}
			require.NoError(t, err)
			assert.True(t, resp.Msg.GetSourceBranchDeleted())
		})
	}
}

func TestMergeRequestServiceCloseAndMarkRead(t *testing.T) {
	t.Parallel()
	t.Run("close not found", func(t *testing.T) {
		t.Parallel()
		mrs := mocks.NewMergeRequestService(t)
		mrs.EXPECT().Close(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), int64(9)).Return(domain.MergeRequest{}, domain.ErrNotFound).Once()
		_, err := (&mergeRequestService{mrs}).CloseMergeRequest(ownerCtx(), connect.NewRequest(&pb.CloseMergeRequestRequest{Repo: "demo", Number: 9}))
		requireCode(t, connect.CodeNotFound, err)
	})
	t.Run("close ok", func(t *testing.T) {
		t.Parallel()
		mrs := mocks.NewMergeRequestService(t)
		mrs.EXPECT().Close(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), int64(9)).Return(domain.MergeRequest{Number: 9, State: domain.MRStateClosed}, nil).Once()
		resp, err := (&mergeRequestService{mrs}).CloseMergeRequest(ownerCtx(), connect.NewRequest(&pb.CloseMergeRequestRequest{Repo: "demo", Number: 9}))
		require.NoError(t, err)
		assert.Equal(t, pb.MergeRequestState_MERGE_REQUEST_STATE_CLOSED, resp.Msg.GetMergeRequest().GetState())
	})
	t.Run("mark read rejects a short head", func(t *testing.T) {
		t.Parallel()
		_, err := (&mergeRequestService{mocks.NewMergeRequestService(t)}).MarkRead(ownerCtx(),
			connect.NewRequest(&pb.MarkReadRequest{Repo: "demo", Number: 1, HeadId: "abc"}))
		requireCode(t, connect.CodeInvalidArgument, err)
	})
	t.Run("mark read unauthenticated", func(t *testing.T) {
		t.Parallel()
		mrs := mocks.NewMergeRequestService(t)
		mrs.EXPECT().MarkRead(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), int64(1), domain.ObjectID{}).Return(domain.ObjectID{}, domain.ErrUnauthenticated).Once()
		_, err := (&mergeRequestService{mrs}).MarkRead(ownerCtx(), connect.NewRequest(&pb.MarkReadRequest{Repo: "demo", Number: 1}))
		requireCode(t, connect.CodeUnauthenticated, err)
	})
}

func TestMergeRequestServiceGet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		usecase  error
		wantCode connect.Code
	}{
		{name: "ok"},
		{name: "not found", usecase: domain.ErrNotFound, wantCode: connect.CodeNotFound},
		{name: "unauthenticated", usecase: domain.ErrUnauthenticated, wantCode: connect.CodeUnauthenticated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mrs := mocks.NewMergeRequestService(t)
			mrs.EXPECT().Get(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), int64(4), true).
				Return(ports.MergeRequestDetail{MergeRequest: domain.MergeRequest{Number: 4}, CommitsTruncated: true}, tt.usecase).Once()
			resp, err := (&mergeRequestService{mrs}).GetMergeRequest(ownerCtx(), connect.NewRequest(&pb.GetMergeRequestRequest{Repo: "demo", Number: 4, SinceLastRead: true}))
			if tt.wantCode != 0 {
				requireCode(t, tt.wantCode, err)
				return
			}
			require.NoError(t, err)
			assert.EqualValues(t, 4, resp.Msg.GetMergeRequest().GetNumber())
			assert.True(t, resp.Msg.GetCommitsTruncated())
		})
	}
}
