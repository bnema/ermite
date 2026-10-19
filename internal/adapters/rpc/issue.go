// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"fmt"
	"strconv"

	"connectrpc.com/connect"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// issueService implements ermiteconnect.IssueServiceHandler.
type issueService struct {
	issues     ports.IssueService
	moderation ports.IssueModerationService
}

func issueStateToPB(s domain.IssueState) pb.IssueState {
	switch s {
	case domain.IssueStateOpen:
		return pb.IssueState_ISSUE_STATE_OPEN
	case domain.IssueStateClosed:
		return pb.IssueState_ISSUE_STATE_CLOSED
	case domain.IssueStatePending:
		return pb.IssueState_ISSUE_STATE_PENDING
	}
	return pb.IssueState_ISSUE_STATE_UNSPECIFIED
}

// issueStateFromPB maps the state filter of a listing; unspecified is the
// empty state, which means every state. Pending is not listable.
func issueStateFromPB(s pb.IssueState) (domain.IssueState, error) {
	switch s {
	case pb.IssueState_ISSUE_STATE_UNSPECIFIED:
		return "", nil
	case pb.IssueState_ISSUE_STATE_OPEN:
		return domain.IssueStateOpen, nil
	case pb.IssueState_ISSUE_STATE_CLOSED:
		return domain.IssueStateClosed, nil
	}
	return "", fmt.Errorf("%w: issue state %v", domain.ErrInvalid, s)
}

func issuesModeToPB(m domain.IssuesMode) pb.IssuesMode {
	switch m.OrDefault() {
	case domain.IssuesOff:
		return pb.IssuesMode_ISSUES_MODE_OFF
	case domain.IssuesOwner:
		return pb.IssuesMode_ISSUES_MODE_OWNER
	case domain.IssuesPublic:
		return pb.IssuesMode_ISSUES_MODE_PUBLIC
	}
	return pb.IssuesMode_ISSUES_MODE_UNSPECIFIED
}

func issuesModeFromPB(m pb.IssuesMode) (domain.IssuesMode, error) {
	switch m {
	case pb.IssuesMode_ISSUES_MODE_OFF:
		return domain.IssuesOff, nil
	case pb.IssuesMode_ISSUES_MODE_OWNER:
		return domain.IssuesOwner, nil
	case pb.IssuesMode_ISSUES_MODE_PUBLIC:
		return domain.IssuesPublic, nil
	}
	return "", fmt.Errorf("%w: issues mode %v", domain.ErrInvalid, m)
}

func issueToPB(i domain.Issue) *pb.Issue {
	return &pb.Issue{
		Repo:         i.Repo.String(),
		Number:       i.Number,
		Title:        i.Title.String(),
		Body:         i.Body.String(),
		State:        issueStateToPB(i.State),
		Author:       i.Author,
		CreatedAt:    timestamp(i.CreatedAt),
		UpdatedAt:    timestamp(i.UpdatedAt),
		CommentCount: int32(i.CommentSeq), //nolint:gosec // bounded by usecase.MaxCommentsPerIssue
	}
}

func issueCommentToPB(c domain.IssueComment) *pb.IssueComment {
	return &pb.IssueComment{
		Id:        strconv.FormatInt(c.ID, 10),
		Author:    c.Author,
		Text:      c.Text.String(),
		CreatedAt: timestamp(c.CreatedAt),
	}
}

func (s *issueService) CreateIssue(ctx context.Context, req *connect.Request[pb.CreateIssueRequest]) (*connect.Response[pb.CreateIssueResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	issue, err := s.issues.Create(ctx, caller(ctx), name, ports.CreateIssueParams{Title: req.Msg.GetTitle(), Body: req.Msg.GetBody()})
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.CreateIssueResponse{Issue: issueToPB(issue)}), nil
}

func (s *issueService) ListIssues(ctx context.Context, req *connect.Request[pb.ListIssuesRequest]) (*connect.Response[pb.ListIssuesResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	state, err := issueStateFromPB(req.Msg.GetState())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	page, err := s.issues.List(ctx, caller(ctx), name, ports.ListIssuesParams{
		State: state, PageSize: int(req.Msg.GetPageSize()), PageToken: req.Msg.GetPageToken(),
	})
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := &pb.ListIssuesResponse{NextPageToken: page.NextPageToken, Issues: make([]*pb.Issue, 0, len(page.Issues))}
	for _, i := range page.Issues {
		out.Issues = append(out.Issues, issueToPB(i))
	}
	return connect.NewResponse(out), nil
}

func (s *issueService) GetIssue(ctx context.Context, req *connect.Request[pb.GetIssueRequest]) (*connect.Response[pb.GetIssueResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	d, err := s.issues.Get(ctx, caller(ctx), name, req.Msg.GetNumber())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := &pb.GetIssueResponse{Issue: issueToPB(d.Issue), Comments: make([]*pb.IssueComment, 0, len(d.Comments))}
	for _, c := range d.Comments {
		out.Comments = append(out.Comments, issueCommentToPB(c))
	}
	return connect.NewResponse(out), nil
}

func (s *issueService) AddIssueComment(ctx context.Context, req *connect.Request[pb.AddIssueCommentRequest]) (*connect.Response[pb.AddIssueCommentResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	c, err := s.issues.AddComment(ctx, caller(ctx), name, req.Msg.GetNumber(), req.Msg.GetText())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.AddIssueCommentResponse{Comment: issueCommentToPB(c)}), nil
}

func (s *issueService) CloseIssue(ctx context.Context, req *connect.Request[pb.CloseIssueRequest]) (*connect.Response[pb.CloseIssueResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	issue, err := s.issues.Close(ctx, caller(ctx), name, req.Msg.GetNumber())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.CloseIssueResponse{Issue: issueToPB(issue)}), nil
}

func (s *issueService) ReopenIssue(ctx context.Context, req *connect.Request[pb.ReopenIssueRequest]) (*connect.Response[pb.ReopenIssueResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	issue, err := s.issues.Reopen(ctx, caller(ctx), name, req.Msg.GetNumber())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.ReopenIssueResponse{Issue: issueToPB(issue)}), nil
}

func pendingIssueToPB(p domain.PendingIssue) *pb.PendingIssue {
	return &pb.PendingIssue{
		Id: p.ID.String(), Repo: p.Repo.String(), Title: p.Title.String(), Body: p.Body.String(),
		Author: p.Author, CreatedAt: timestamp(p.CreatedAt),
	}
}

// pendingRef parses the repository and the id of a moderation request. A
// malformed id is reported as not found, like an unknown one.
func pendingRef(repo, id string) (domain.RepoName, domain.PendingIssueID, error) {
	name, err := domain.ParseRepoName(repo)
	if err != nil {
		return domain.RepoName{}, "", err
	}
	pid, err := domain.ParsePendingIssueID(id)
	if err != nil {
		return domain.RepoName{}, "", domain.ErrNotFound
	}
	return name, pid, nil
}

func (s *issueService) ListPendingIssues(ctx context.Context, req *connect.Request[pb.ListPendingIssuesRequest]) (*connect.Response[pb.ListPendingIssuesResponse], error) {
	var name domain.RepoName
	if req.Msg.GetRepo() != "" {
		var err error
		if name, err = domain.ParseRepoName(req.Msg.GetRepo()); err != nil {
			return nil, connectError(ctx, err)
		}
	}
	page, err := s.moderation.ListPending(ctx, caller(ctx), name, ports.ListPendingParams{
		PageSize: int(req.Msg.GetPageSize()), PageToken: req.Msg.GetPageToken(),
	})
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := &pb.ListPendingIssuesResponse{NextPageToken: page.NextPageToken, Issues: make([]*pb.PendingIssue, 0, len(page.Issues))}
	for _, p := range page.Issues {
		out.Issues = append(out.Issues, pendingIssueToPB(p))
	}
	return connect.NewResponse(out), nil
}

func (s *issueService) ApproveIssue(ctx context.Context, req *connect.Request[pb.ApproveIssueRequest]) (*connect.Response[pb.ApproveIssueResponse], error) {
	name, id, err := pendingRef(req.Msg.GetRepo(), req.Msg.GetId())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	issue, err := s.moderation.Approve(ctx, caller(ctx), name, id)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.ApproveIssueResponse{Issue: issueToPB(issue)}), nil
}

func (s *issueService) RejectIssue(ctx context.Context, req *connect.Request[pb.RejectIssueRequest]) (*connect.Response[pb.RejectIssueResponse], error) {
	name, id, err := pendingRef(req.Msg.GetRepo(), req.Msg.GetId())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	if err := s.moderation.Reject(ctx, caller(ctx), name, id, req.Msg.GetBan()); err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.RejectIssueResponse{}), nil
}
