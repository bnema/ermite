// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"fmt"
	"strconv"

	"connectrpc.com/connect"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/domain"
)

// mergeRequestService implements ermiteconnect.MergeRequestServiceHandler.
type mergeRequestService struct{ mrs *app.MergeRequests }

func mergeRequestStateToPB(s domain.MRState) pb.MergeRequestState {
	switch s {
	case domain.MRStateOpen:
		return pb.MergeRequestState_MERGE_REQUEST_STATE_OPEN
	case domain.MRStateMerged:
		return pb.MergeRequestState_MERGE_REQUEST_STATE_MERGED
	case domain.MRStateClosed:
		return pb.MergeRequestState_MERGE_REQUEST_STATE_CLOSED
	}
	return pb.MergeRequestState_MERGE_REQUEST_STATE_UNSPECIFIED
}

// mergeRequestStateFromPB maps the state filter of a listing; unspecified is
// the empty state, which means every state.
func mergeRequestStateFromPB(s pb.MergeRequestState) (domain.MRState, error) {
	switch s {
	case pb.MergeRequestState_MERGE_REQUEST_STATE_UNSPECIFIED:
		return "", nil
	case pb.MergeRequestState_MERGE_REQUEST_STATE_OPEN:
		return domain.MRStateOpen, nil
	case pb.MergeRequestState_MERGE_REQUEST_STATE_MERGED:
		return domain.MRStateMerged, nil
	case pb.MergeRequestState_MERGE_REQUEST_STATE_CLOSED:
		return domain.MRStateClosed, nil
	}
	return "", fmt.Errorf("%w: merge request state %v", domain.ErrInvalid, s)
}

func diffSideToPB(s domain.DiffSide) pb.DiffSide {
	switch s {
	case domain.SideOld:
		return pb.DiffSide_DIFF_SIDE_OLD
	case domain.SideNew:
		return pb.DiffSide_DIFF_SIDE_NEW
	}
	return pb.DiffSide_DIFF_SIDE_UNSPECIFIED
}

func diffSideFromPB(s pb.DiffSide) (domain.DiffSide, error) {
	switch s {
	case pb.DiffSide_DIFF_SIDE_UNSPECIFIED:
		return domain.SideNone, nil
	case pb.DiffSide_DIFF_SIDE_OLD:
		return domain.SideOld, nil
	case pb.DiffSide_DIFF_SIDE_NEW:
		return domain.SideNew, nil
	}
	return "", fmt.Errorf("%w: diff side %v", domain.ErrInvalid, s)
}

func mergeRequestToPB(mr domain.MergeRequest) *pb.MergeRequest {
	return &pb.MergeRequest{
		Repo:           mr.Repo.String(),
		Number:         mr.Number,
		Title:          mr.Title.String(),
		Description:    mr.Description.String(),
		SourceBranch:   mr.Source,
		TargetBranch:   mr.Target,
		State:          mergeRequestStateToPB(mr.State),
		Author:         mr.Author,
		CreatedAt:      timestamp(mr.CreatedAt),
		UpdatedAt:      timestamp(mr.UpdatedAt),
		HeadId:         mr.HeadID.String(),
		MergedCommitId: mr.MergedCommitID.String(),
		MergeMode:      mergeModeToPB(mr.MergeMode),
	}
}

func commentToPB(c domain.Comment) *pb.Comment {
	return &pb.Comment{
		Id:        strconv.FormatInt(c.ID, 10),
		Author:    c.Author,
		Path:      c.Path,
		Line:      int32(c.Line), //nolint:gosec // a line of a diff, validated against it
		Side:      diffSideToPB(c.Side),
		CommitId:  c.CommitID.String(),
		Text:      c.Text.String(),
		Resolved:  c.Resolved,
		CreatedAt: timestamp(c.CreatedAt),
	}
}

// commentID parses the opaque id of a comment. One that this server never
// issued is 0, which names no comment.
func commentID(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 || strconv.FormatInt(n, 10) != s {
		return 0
	}
	return n
}

// headID parses a commit id of a request; the empty string is the zero id.
//
//hexcheck:ignore suspicious-business-logic-in-adapter parses a request field into the domain id and names the field in the error; no rule is decided here
func headID(s string) (domain.ObjectID, error) {
	if s == "" {
		return domain.ObjectID{}, nil
	}
	var v domain.Violations
	id, err := domain.ParseObjectID(s)
	if err != nil {
		v.Add("head_id", "is not a full commit id")
	}
	return id, v.Err()
}

func (s *mergeRequestService) CreateMergeRequest(ctx context.Context, req *connect.Request[pb.CreateMergeRequestRequest]) (*connect.Response[pb.CreateMergeRequestResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	mr, err := s.mrs.Create(ctx, caller(ctx), name, app.CreateMergeRequestParams{
		Title: req.Msg.GetTitle(), Description: req.Msg.GetDescription(),
		Source: req.Msg.GetSourceBranch(), Target: req.Msg.GetTargetBranch(),
	})
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.CreateMergeRequestResponse{MergeRequest: mergeRequestToPB(mr)}), nil
}

func (s *mergeRequestService) ListMergeRequests(ctx context.Context, req *connect.Request[pb.ListMergeRequestsRequest]) (*connect.Response[pb.ListMergeRequestsResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	state, err := mergeRequestStateFromPB(req.Msg.GetState())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	page, err := s.mrs.List(ctx, caller(ctx), name, app.ListMergeRequestsParams{
		State: state, PageSize: int(req.Msg.GetPageSize()), PageToken: req.Msg.GetPageToken(),
	})
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := &pb.ListMergeRequestsResponse{NextPageToken: page.NextPageToken, MergeRequests: make([]*pb.MergeRequest, 0, len(page.MergeRequests))}
	for _, mr := range page.MergeRequests {
		out.MergeRequests = append(out.MergeRequests, mergeRequestToPB(mr))
	}
	return connect.NewResponse(out), nil
}

func (s *mergeRequestService) GetMergeRequest(ctx context.Context, req *connect.Request[pb.GetMergeRequestRequest]) (*connect.Response[pb.GetMergeRequestResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	d, err := s.mrs.Get(ctx, caller(ctx), name, req.Msg.GetNumber(), req.Msg.GetSinceLastRead())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := &pb.GetMergeRequestResponse{
		MergeRequest:     mergeRequestToPB(d.MergeRequest),
		CommitsTruncated: d.CommitsTruncated,
		DiffBaseId:       d.DiffBaseID.String(),
		Stats:            diffStatsToPB(d.Stats),
		Diff:             diffToPB(d.Diff),
		LastReadId:       d.LastReadID.String(),
		LastReadLost:     d.LastReadLost,
	}
	for _, c := range d.Commits {
		out.Commits = append(out.Commits, commitToPB(c))
	}
	for _, c := range d.Comments {
		out.Comments = append(out.Comments, commentToPB(c))
	}
	return connect.NewResponse(out), nil
}

func (s *mergeRequestService) AddComment(ctx context.Context, req *connect.Request[pb.AddCommentRequest]) (*connect.Response[pb.AddCommentResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	side, err := diffSideFromPB(req.Msg.GetSide())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	c, err := s.mrs.AddComment(ctx, caller(ctx), name, req.Msg.GetNumber(), app.AddCommentParams{
		Path: req.Msg.GetPath(), Line: int(req.Msg.GetLine()), Side: side, Text: req.Msg.GetText(),
	})
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.AddCommentResponse{Comment: commentToPB(c)}), nil
}

func (s *mergeRequestService) ResolveComment(ctx context.Context, req *connect.Request[pb.ResolveCommentRequest]) (*connect.Response[pb.ResolveCommentResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	// An id this server never issued is 0: the use case answers not found, after
	// the repository and the scope were checked, as for any unknown comment.
	c, err := s.mrs.ResolveComment(ctx, caller(ctx), name, req.Msg.GetNumber(), commentID(req.Msg.GetCommentId()), req.Msg.GetResolved())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.ResolveCommentResponse{Comment: commentToPB(c)}), nil
}

func (s *mergeRequestService) CloseMergeRequest(ctx context.Context, req *connect.Request[pb.CloseMergeRequestRequest]) (*connect.Response[pb.CloseMergeRequestResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	mr, err := s.mrs.Close(ctx, caller(ctx), name, req.Msg.GetNumber())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.CloseMergeRequestResponse{MergeRequest: mergeRequestToPB(mr)}), nil
}

func (s *mergeRequestService) MergeMergeRequest(ctx context.Context, req *connect.Request[pb.MergeMergeRequestRequest]) (*connect.Response[pb.MergeMergeRequestResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	mode, err := mergeModeFromPB(req.Msg.GetMode())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	head, err := headID(req.Msg.GetHeadId())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	res, err := s.mrs.Merge(ctx, caller(ctx), name, req.Msg.GetNumber(), app.MergeParams{
		Mode: mode, HeadID: head, DeleteSource: req.Msg.GetDeleteSourceBranch(),
	})
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.MergeMergeRequestResponse{
		MergeRequest: mergeRequestToPB(res.MergeRequest), SourceBranchDeleted: res.SourceDeleted,
	}), nil
}

func (s *mergeRequestService) MarkRead(ctx context.Context, req *connect.Request[pb.MarkReadRequest]) (*connect.Response[pb.MarkReadResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	head, err := headID(req.Msg.GetHeadId())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	if _, err := s.mrs.MarkRead(ctx, caller(ctx), name, req.Msg.GetNumber(), head); err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.MarkReadResponse{}), nil
}
