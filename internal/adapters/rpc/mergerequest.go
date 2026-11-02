// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"fmt"
	"strconv"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// mergeRequestService implements ermiteconnect.MergeRequestServiceHandler.
type mergeRequestService struct {
	mrs ports.MergeRequestService
}

func (s *mergeRequestService) EnableAutoMerge(ctx context.Context, req *pb.EnableAutoMergeRequest) (*pb.EnableAutoMergeResponse, error) {
	name, err := domain.ParseRepoName(req.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	mode, err := mergeModeFromPB(req.GetMode())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	head, err := headID(req.GetHeadId())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	mr, err := s.mrs.EnableAutoMerge(ctx, caller(ctx), name, req.GetNumber(), ports.EnableAutoMergeParams{Mode: mode, HeadID: head})
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.EnableAutoMergeResponse{MergeRequest: mergeRequestToPB(mr)}, nil
}

func (s *mergeRequestService) CancelAutoMerge(ctx context.Context, req *pb.CancelAutoMergeRequest) (*pb.CancelAutoMergeResponse, error) {
	name, err := domain.ParseRepoName(req.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	mr, err := s.mrs.CancelAutoMerge(ctx, caller(ctx), name, req.GetNumber())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.CancelAutoMergeResponse{MergeRequest: mergeRequestToPB(mr)}, nil
}

func (s *mergeRequestService) EditComment(ctx context.Context, req *pb.EditCommentRequest) (*pb.EditCommentResponse, error) {
	name, err := domain.ParseRepoName(req.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	c, err := s.mrs.EditComment(ctx, caller(ctx), name, req.GetNumber(), commentID(req.GetCommentId()), req.GetText())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.EditCommentResponse{Comment: commentToPB(c)}, nil
}

func (s *mergeRequestService) DeleteComment(ctx context.Context, req *pb.DeleteCommentRequest) (*pb.DeleteCommentResponse, error) {
	name, err := domain.ParseRepoName(req.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	c, err := s.mrs.DeleteComment(ctx, caller(ctx), name, req.GetNumber(), commentID(req.GetCommentId()))
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.DeleteCommentResponse{Comment: commentToPB(c)}, nil
}

func verdictToPB(v domain.Verdict) pb.Verdict {
	switch v {
	case domain.VerdictApprove:
		return pb.Verdict_VERDICT_APPROVE
	case domain.VerdictChangesRequested:
		return pb.Verdict_VERDICT_CHANGES_REQUESTED
	}
	return pb.Verdict_VERDICT_UNSPECIFIED
}

func verdictFromPB(v pb.Verdict) (domain.Verdict, error) {
	switch v {
	case pb.Verdict_VERDICT_UNSPECIFIED:
		return domain.VerdictNone, nil
	case pb.Verdict_VERDICT_APPROVE:
		return domain.VerdictApprove, nil
	case pb.Verdict_VERDICT_CHANGES_REQUESTED:
		return domain.VerdictChangesRequested, nil
	}
	return domain.VerdictNone, fmt.Errorf("%w: verdict %v", domain.ErrInvalid, v)
}

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

// mergeRequestToPB maps a request. An open one carries the CHECKING
// mergeability: the server has no result in hand here; Get and List override
// it with the computed one.
func mergeRequestToPB(mr domain.MergeRequest) *pb.MergeRequest {
	out := &pb.MergeRequest{
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
		AutoMerged:     mr.AutoMerged,
	}
	if am := mr.AutoMerge; am != nil {
		out.AutoMerge = &pb.AutoMerge{
			Mode: mergeModeToPB(am.Mode), HeadId: am.HeadID.String(),
			EnabledBy: am.EnablerLabel, EnabledAt: timestamp(am.EnabledAt),
		}
	}
	if mr.IsOpen() {
		out.Mergeability = &pb.Mergeability{State: pb.MergeabilityState_MERGEABILITY_STATE_CHECKING}
	}
	return out
}

// mergeRequestWithMergeabilityToPB is mergeRequestToPB with the mergeability of
// an open request; m is ignored for a request that is not open.
func mergeRequestWithMergeabilityToPB(mr domain.MergeRequest, m domain.Mergeability) *pb.MergeRequest {
	out := mergeRequestToPB(mr)
	if mr.IsOpen() && m.State != "" {
		out.Mergeability = mergeabilityToPB(m)
	}
	return out
}

func mergeabilityStateToPB(s domain.MergeabilityState) pb.MergeabilityState {
	switch s {
	case domain.MergeabilityChecking:
		return pb.MergeabilityState_MERGEABILITY_STATE_CHECKING
	case domain.MergeabilityUpToDate:
		return pb.MergeabilityState_MERGEABILITY_STATE_UP_TO_DATE
	case domain.MergeabilityBehind:
		return pb.MergeabilityState_MERGEABILITY_STATE_BEHIND
	case domain.MergeabilityConflict:
		return pb.MergeabilityState_MERGEABILITY_STATE_CONFLICT
	}
	return pb.MergeabilityState_MERGEABILITY_STATE_UNSPECIFIED
}

func mergeabilityToPB(m domain.Mergeability) *pb.Mergeability {
	return &pb.Mergeability{
		State:                  mergeabilityStateToPB(m.State),
		TargetId:               m.TargetID.String(),
		HeadId:                 m.HeadID.String(),
		Behind:                 int32(m.Behind), //nolint:gosec // capped at domain.MaxBehindCount
		BehindCapped:           m.BehindCapped,
		ConflictPaths:          m.ConflictPaths,
		ConflictPathsTruncated: m.ConflictPathsTruncated,
		CheckedAt:              timestamp(m.CheckedAt),
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
		ReplyTo:   optionalCommentID(c.ReplyTo),
		StartLine: int32(c.StartLine), //nolint:gosec // validated diff line
		Verdict:   verdictToPB(c.Verdict), EditedAt: timestamp(c.EditedAt), Deleted: c.Deleted,
	}
}

func optionalCommentID(id int64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
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

func (s *mergeRequestService) CreateMergeRequest(ctx context.Context, req *pb.CreateMergeRequestRequest) (*pb.CreateMergeRequestResponse, error) {
	name, err := domain.ParseRepoName(req.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	mr, err := s.mrs.Create(ctx, caller(ctx), name, ports.CreateMergeRequestParams{
		Title: req.GetTitle(), Description: req.GetDescription(),
		Source: req.GetSourceBranch(), Target: req.GetTargetBranch(),
	})
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.CreateMergeRequestResponse{MergeRequest: mergeRequestToPB(mr)}, nil
}

func (s *mergeRequestService) ListMergeRequests(ctx context.Context, req *pb.ListMergeRequestsRequest) (*pb.ListMergeRequestsResponse, error) {
	name, err := domain.ParseRepoName(req.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	state, err := mergeRequestStateFromPB(req.GetState())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	page, err := s.mrs.List(ctx, caller(ctx), name, ports.ListMergeRequestsParams{
		State: state, PageSize: int(req.GetPageSize()), PageToken: req.GetPageToken(),
	})
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := &pb.ListMergeRequestsResponse{NextPageToken: page.NextPageToken, MergeRequests: make([]*pb.MergeRequest, 0, len(page.MergeRequests))}
	for _, mr := range page.MergeRequests {
		out.MergeRequests = append(out.MergeRequests, mergeRequestWithMergeabilityToPB(mr, page.Mergeability[mr.Number]))
	}
	return out, nil
}

func (s *mergeRequestService) GetMergeRequest(ctx context.Context, req *pb.GetMergeRequestRequest) (*pb.GetMergeRequestResponse, error) {
	name, err := domain.ParseRepoName(req.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	d, err := s.mrs.Get(ctx, caller(ctx), name, req.GetNumber(), ports.GetMergeRequestOptions{SinceLastRead: req.GetSinceLastRead()})
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := &pb.GetMergeRequestResponse{
		MergeRequest:     mergeRequestWithMergeabilityToPB(d.MergeRequest, d.Mergeability),
		CommitsTruncated: d.CommitsTruncated,
		DiffBaseId:       d.DiffBaseID.String(),
		Stats:            diffStatsToPB(d.Stats),
		Diff:             diffToPB(d.Diff),
		LastReadId:       d.LastReadID.String(),
		LastReadLost:     d.LastReadLost,
	}
	for _, i := range d.Issues {
		out.Issues = append(out.Issues, mergeRequestIssueToPB(i))
	}
	for _, e := range d.IssueEvents {
		out.IssueEvents = append(out.IssueEvents, issueEventToPB(e))
	}
	for _, c := range d.Commits {
		out.Commits = append(out.Commits, commitToPB(c))
	}
	for _, c := range d.Comments {
		out.Comments = append(out.Comments, commentToPB(c))
	}
	for _, v := range d.Verdicts {
		out.Verdicts = append(out.Verdicts, &pb.AuthorVerdict{Author: v.Author, Verdict: verdictToPB(v.Verdict), CommentId: strconv.FormatInt(v.CommentID, 10), CommitId: v.CommitID.String(), CreatedAt: timestamp(v.At), Stale: v.Stale})
	}
	return out, nil
}

func (s *mergeRequestService) AddComment(ctx context.Context, req *pb.AddCommentRequest) (*pb.AddCommentResponse, error) {
	name, err := domain.ParseRepoName(req.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	side, err := diffSideFromPB(req.GetSide())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	verdict, err := verdictFromPB(req.GetVerdict())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	replyTo := commentID(req.GetReplyTo())
	if req.GetReplyTo() != "" && replyTo == 0 {
		var v domain.Violations
		v.Add("reply_to", "is not a note id")
		return nil, connectError(ctx, v.Err())
	}
	c, err := s.mrs.AddComment(ctx, caller(ctx), name, req.GetNumber(), ports.AddCommentParams{
		ReplyTo: replyTo, StartLine: int(req.GetStartLine()), Verdict: verdict,
		Path: req.GetPath(), Line: int(req.GetLine()), Side: side, Text: req.GetText(),
	})
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.AddCommentResponse{Comment: commentToPB(c)}, nil
}

func (s *mergeRequestService) ResolveComment(ctx context.Context, req *pb.ResolveCommentRequest) (*pb.ResolveCommentResponse, error) {
	name, err := domain.ParseRepoName(req.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	// An id this server never issued is 0: the use case answers not found, after
	// the repository and the scope were checked, as for any unknown comment.
	c, err := s.mrs.ResolveComment(ctx, caller(ctx), name, req.GetNumber(), commentID(req.GetCommentId()), req.GetResolved())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.ResolveCommentResponse{Comment: commentToPB(c)}, nil
}

func (s *mergeRequestService) CloseMergeRequest(ctx context.Context, req *pb.CloseMergeRequestRequest) (*pb.CloseMergeRequestResponse, error) {
	name, err := domain.ParseRepoName(req.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	mr, err := s.mrs.Close(ctx, caller(ctx), name, req.GetNumber())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.CloseMergeRequestResponse{MergeRequest: mergeRequestToPB(mr)}, nil
}

func (s *mergeRequestService) MergeMergeRequest(ctx context.Context, req *pb.MergeMergeRequestRequest) (*pb.MergeMergeRequestResponse, error) {
	name, err := domain.ParseRepoName(req.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	mode, err := mergeModeFromPB(req.GetMode())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	head, err := headID(req.GetHeadId())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	res, err := s.mrs.Merge(ctx, caller(ctx), name, req.GetNumber(), ports.MergeParams{
		Mode: mode, HeadID: head, DeleteSource: req.GetDeleteSourceBranch(), IgnoreChecks: req.GetIgnoreChecks(),
	})
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.MergeMergeRequestResponse{
		MergeRequest: mergeRequestToPB(res.MergeRequest), SourceBranchDeleted: res.SourceDeleted,
	}, nil
}

func (s *mergeRequestService) MarkRead(ctx context.Context, req *pb.MarkReadRequest) (*pb.MarkReadResponse, error) {
	name, err := domain.ParseRepoName(req.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	head, err := headID(req.GetHeadId())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	if _, err := s.mrs.MarkRead(ctx, caller(ctx), name, req.GetNumber(), head); err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.MarkReadResponse{}, nil
}

func (s *mergeRequestService) MarkReady(ctx context.Context, req *pb.MarkReadyRequest) (*pb.MarkReadyResponse, error) {
	name, err := domain.ParseRepoName(req.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	mr, err := s.mrs.MarkReady(ctx, caller(ctx), name, req.GetNumber())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.MarkReadyResponse{MergeRequest: mergeRequestToPB(mr)}, nil
}

func (s *mergeRequestService) EditMergeRequest(ctx context.Context, req *pb.EditMergeRequestRequest) (*pb.EditMergeRequestResponse, error) {
	name, err := domain.ParseRepoName(req.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	item, err := s.mrs.Edit(ctx, caller(ctx), name, req.GetNumber(), ports.EditMergeRequestParams{Title: req.Title, Description: req.Description})
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.EditMergeRequestResponse{MergeRequest: mergeRequestToPB(item)}, nil
}
