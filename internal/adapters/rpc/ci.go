// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// secretKey carries the bearer secret of a runner or job call to the handler:
// those secrets are not stored tokens, the use case checks them.
type secretKey struct{}

func secretFrom(ctx context.Context) string {
	s, _ := ctx.Value(secretKey{}).(string)
	return s
}

// runnerSecretProcedures are the calls whose bearer secret is a runner token
// or a job token: the interceptor does not authenticate them as stored tokens.
var runnerSecretProcedures = map[string]bool{
	ermiteconnect.RunnerServiceFetchJobProcedure:    true,
	ermiteconnect.RunnerServiceUpdateJobProcedure:   true,
	ermiteconnect.RunnerServiceAppendLogProcedure:   true,
	ermiteconnect.RunnerServiceHeartbeatProcedure:   true,
	ermiteconnect.RunnerServiceWatchJobProcedure:    true,
	ermiteconnect.RunnerServiceCompleteJobProcedure: true,
	// ResolveActions and UploadSummary are job calls too.
	ermiteconnect.RunnerServiceResolveActionsProcedure: true,
	ermiteconnect.RunnerServiceUploadSummaryProcedure:  true,
}

// ciError maps an error of the CI use cases. A call on a job that ended gets
// failed_precondition with a JobEnded detail, as runner.proto says.
func ciError(ctx context.Context, err error) error {
	if ended, ok := domain.IsJobEnded(err); ok {
		ce := connect.NewError(connect.CodeFailedPrecondition, errors.New("the job has ended"))
		if d, derr := connect.NewErrorDetail(jobEndedToPB(ended)); derr == nil {
			ce.AddDetail(d)
		}
		return ce
	}
	if errors.Is(err, domain.ErrOutOfRange) {
		return connect.NewError(connect.CodeOutOfRange, err)
	}
	return connectError(ctx, err)
}

// runnerService implements ermiteconnect.RunnerServiceHandler.
type runnerService struct {
	runners ports.RunnerService
	limiter ports.FailureLimiter
}

// failed counts a call that was refused for its credential against the client
// address, like every other authentication failure, and maps the error.
func (s *runnerService) failed(ctx context.Context, err error) error {
	if errors.Is(err, domain.ErrUnauthenticated) && s.limiter != nil {
		if addr := addrFromContext(ctx); addr != "" {
			s.limiter.Fail(ctx, addr)
		}
	}
	return ciError(ctx, err)
}

func (s *runnerService) RegisterRunner(ctx context.Context, req *connect.Request[pb.RegisterRunnerRequest]) (*connect.Response[pb.RegisterRunnerResponse], error) {
	iso, err := isolationFromPB(req.Msg.GetIsolation())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	out, err := s.runners.Register(ctx, caller(ctx), ports.RegisterRunnerParams{
		Name: req.Msg.GetName(), Labels: req.Msg.GetLabels(), Version: req.Msg.GetVersion(), Isolation: iso,
		Arch: req.Msg.GetArch(), OS: req.Msg.GetOs(), Replace: req.Msg.GetReplace(),
	})
	if err != nil {
		return nil, ciError(ctx, err)
	}
	return connect.NewResponse(&pb.RegisterRunnerResponse{RunnerId: out.Runner.ID, RunnerToken: out.Token}), nil
}

func (s *runnerService) FetchJob(ctx context.Context, req *connect.Request[pb.FetchJobRequest]) (*connect.Response[pb.FetchJobResponse], error) {
	res, err := s.runners.FetchJob(ctx, secretFrom(ctx), ports.FetchJobParams{Labels: req.Msg.GetLabels(), PollID: req.Msg.GetPollId()})
	if err != nil {
		return nil, s.failed(ctx, err)
	}
	out := &pb.FetchJobResponse{}
	if res.Job != nil {
		out.Job = runnerJobToPB(res.Job)
	} else if res.RetryAfter > 0 {
		out.RetryAfter = durationpb.New(res.RetryAfter)
	}
	return connect.NewResponse(out), nil
}

func (s *runnerService) UpdateJob(ctx context.Context, req *connect.Request[pb.UpdateJobRequest]) (*connect.Response[pb.UpdateJobResponse], error) {
	steps, err := stepUpdatesFromPB(req.Msg.GetSteps())
	if err != nil {
		return nil, s.failed(ctx, err)
	}
	anns, err := annotationsFromPB(req.Msg.GetAnnotations())
	if err != nil {
		return nil, s.failed(ctx, err)
	}
	if err := s.runners.UpdateJob(ctx, secretFrom(ctx), req.Msg.GetJobId(), steps, anns); err != nil {
		return nil, s.failed(ctx, err)
	}
	return connect.NewResponse(&pb.UpdateJobResponse{}), nil
}

func (s *runnerService) AppendLog(ctx context.Context, req *connect.Request[pb.AppendLogRequest]) (*connect.Response[pb.AppendLogResponse], error) {
	lines := make([]domain.CILogLine, 0, len(req.Msg.GetLines()))
	for _, l := range req.Msg.GetLines() {
		lines = append(lines, domain.CILogLine{Time: goTime(l.GetTime()), Text: l.GetText(), Continued: l.GetContinued()})
	}
	next, err := s.runners.AppendLog(ctx, secretFrom(ctx), req.Msg.GetJobId(), req.Msg.GetStep(), req.Msg.GetOffset(), lines)
	if err != nil {
		var gap *ports.ErrCILogGap
		if errors.As(err, &gap) {
			return nil, connect.NewError(connect.CodeOutOfRange, err)
		}
		return nil, s.failed(ctx, err)
	}
	return connect.NewResponse(&pb.AppendLogResponse{NextOffset: next}), nil
}

func (s *runnerService) Heartbeat(ctx context.Context, req *connect.Request[pb.HeartbeatRequest]) (*connect.Response[pb.HeartbeatResponse], error) {
	if err := s.runners.Heartbeat(ctx, secretFrom(ctx), req.Msg.GetJobId()); err != nil {
		return nil, s.failed(ctx, err)
	}
	return connect.NewResponse(&pb.HeartbeatResponse{}), nil
}

func (s *runnerService) WatchJob(ctx context.Context, req *connect.Request[pb.WatchJobRequest], stream *connect.ServerStream[pb.WatchJobResponse]) error {
	// A client's call returns when the response begins: send the headers now,
	// not with the first message, which may be 30 seconds away. Connect
	// flushes them for a nil message; a failure shows on the first real send.
	ready := func() { _ = stream.Send(nil) }
	err := s.runners.WatchJob(ctx, secretFrom(ctx), req.Msg.GetJobId(), ready, func(sig ports.JobSignal) error {
		switch {
		case sig.Cancel != nil:
			return stream.Send(&pb.WatchJobResponse{Msg: &pb.WatchJobResponse_Cancel{Cancel: &pb.CancelRequest{
				Reason: cancelReasonToPB(sig.Cancel.Reason), Message: sig.Cancel.Message, RequestedAt: timestamp(sig.Cancel.RequestedAt),
			}}})
		case sig.Ended != nil:
			return stream.Send(&pb.WatchJobResponse{Msg: &pb.WatchJobResponse_Ended{Ended: jobEndedToPB(sig.Ended)}})
		}
		return stream.Send(&pb.WatchJobResponse{Msg: &pb.WatchJobResponse_Keepalive{Keepalive: &pb.Keepalive{}}})
	})
	return s.failed(ctx, err)
}

func (s *runnerService) CompleteJob(ctx context.Context, req *connect.Request[pb.CompleteJobRequest]) (*connect.Response[pb.CompleteJobResponse], error) {
	concl, err := conclusionFromPB(req.Msg.GetConclusion())
	if err != nil {
		return nil, s.failed(ctx, err)
	}
	steps, err := stepUpdatesFromPB(req.Msg.GetSteps())
	if err != nil {
		return nil, s.failed(ctx, err)
	}
	anns, err := annotationsFromPB(req.Msg.GetAnnotations())
	if err != nil {
		return nil, s.failed(ctx, err)
	}
	res, err := s.runners.CompleteJob(ctx, secretFrom(ctx), ports.CompleteJobParams{
		JobID: req.Msg.GetJobId(), Conclusion: concl, Outputs: req.Msg.GetOutputs(),
		RunnerShutdown: req.Msg.GetRunnerShutdown(), Steps: steps, Annotations: anns,
	})
	if err != nil {
		return nil, s.failed(ctx, err)
	}
	return connect.NewResponse(&pb.CompleteJobResponse{Conclusion: conclusionToPB(res.Conclusion), Error: res.Error, Applied: res.Applied}), nil
}

//
//hexcheck:ignore suspicious-business-logic-in-adapter maps the references of the request to strings and the answers back to the contract; the rules are the use case's
func (s *runnerService) ResolveActions(ctx context.Context, req *connect.Request[pb.ResolveActionsRequest]) (*connect.Response[pb.ResolveActionsResponse], error) {
	uses := make([]string, 0, len(req.Msg.GetRefs()))
	for _, r := range req.Msg.GetRefs() {
		uses = append(uses, r.GetUses())
	}
	res, err := s.runners.ResolveActions(ctx, secretFrom(ctx), req.Msg.GetJobId(), uses)
	if err != nil {
		return nil, s.failed(ctx, err)
	}
	out := &pb.ResolveActionsResponse{Actions: make([]*pb.ResolvedAction, 0, len(res))}
	for _, a := range res {
		out.Actions = append(out.Actions, resolvedActionToPB(a))
	}
	return connect.NewResponse(out), nil
}

// UploadSummary stores the summary of a step.
func (s *runnerService) UploadSummary(ctx context.Context, req *connect.Request[pb.UploadSummaryRequest]) (*connect.Response[pb.UploadSummaryResponse], error) {
	if err := s.runners.UploadSummary(ctx, secretFrom(ctx), req.Msg.GetJobId(), req.Msg.GetStepNumber(), req.Msg.GetMarkdown()); err != nil {
		return nil, s.failed(ctx, err)
	}
	return connect.NewResponse(&pb.UploadSummaryResponse{}), nil
}

// runService implements ermiteconnect.RunServiceHandler.
type runService struct {
	ermiteconnect.UnimplementedRunServiceHandler
	runs ports.RunService
}

func repoOf(s string) (domain.RepoName, error) { return domain.ParseRepoName(s) }

func (s *runService) ListRuns(ctx context.Context, req *connect.Request[pb.ListRunsRequest]) (*connect.Response[pb.ListRunsResponse], error) {
	name, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	if _, err := statusFromPB(req.Msg.GetStatus()); err != nil {
		return nil, ciError(ctx, err)
	}
	page, err := s.runs.ListRuns(ctx, caller(ctx), name, ports.ListRunsParams{
		PageSize: int(req.Msg.GetPageSize()), PageToken: req.Msg.GetPageToken(),
		Workflow: req.Msg.GetWorkflow(), Branch: req.Msg.GetBranch(), Status: statusOrZero(req.Msg.GetStatus()),
	})
	if err != nil {
		return nil, ciError(ctx, err)
	}
	out := &pb.ListRunsResponse{NextPageToken: page.NextPageToken, Runs: make([]*pb.Run, 0, len(page.Runs))}
	for _, r := range page.Runs {
		out.Runs = append(out.Runs, runToPB(r))
	}
	return connect.NewResponse(out), nil
}

func statusOrZero(s pb.CiStatus) domain.CIStatus {
	d, _ := statusFromPB(s)
	return d
}

func (s *runService) GetRun(ctx context.Context, req *connect.Request[pb.GetRunRequest]) (*connect.Response[pb.GetRunResponse], error) {
	name, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	d, err := s.runs.GetRun(ctx, caller(ctx), name, req.Msg.GetRunId(), req.Msg.GetAttempt())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	out := &pb.GetRunResponse{Run: runToPB(d.Run), LatestAttempt: d.LatestAttempt}
	for _, j := range d.Jobs {
		out.Jobs = append(out.Jobs, jobViewToPB(j))
	}
	return connect.NewResponse(out), nil
}

func (s *runService) GetJob(ctx context.Context, req *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error) {
	name, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	j, err := s.runs.GetJob(ctx, caller(ctx), name, req.Msg.GetJobId(), req.Msg.GetAttempt())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	return connect.NewResponse(&pb.GetJobResponse{Job: jobViewToPB(j)}), nil
}

func (s *runService) GetJobLog(ctx context.Context, req *connect.Request[pb.GetJobLogRequest], stream *connect.ServerStream[pb.GetJobLogResponse]) error {
	name, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return ciError(ctx, err)
	}
	err = s.runs.GetJobLog(ctx, caller(ctx), name, ports.GetJobLogParams{
		JobID: req.Msg.GetJobId(), Attempt: req.Msg.GetAttempt(), Offset: req.Msg.GetOffset(), Follow: req.Msg.GetFollow(),
		Client: clientFromContext(ctx),
		Ready:  func() { _ = stream.Send(nil) },
	}, func(b ports.LogBatch) error {
		out := &pb.GetJobLogResponse{Offset: b.Offset, Lines: make([]*pb.JobLogLine, 0, len(b.Lines))}
		for _, l := range b.Lines {
			out.Lines = append(out.Lines, logLineToPB(l))
		}
		return stream.Send(out)
	})
	return ciError(ctx, err)
}

func (s *runService) CancelRun(ctx context.Context, req *connect.Request[pb.CancelRunRequest]) (*connect.Response[pb.CancelRunResponse], error) {
	name, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	run, err := s.runs.CancelRun(ctx, caller(ctx), name, req.Msg.GetRunId())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	return connect.NewResponse(&pb.CancelRunResponse{Run: runToPB(run)}), nil
}

func (s *runService) RerunRun(ctx context.Context, req *connect.Request[pb.RerunRunRequest]) (*connect.Response[pb.RerunRunResponse], error) {
	name, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	run, err := s.runs.RerunRun(ctx, caller(ctx), name, req.Msg.GetRunId(), req.Msg.GetFailedOnly())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	return connect.NewResponse(&pb.RerunRunResponse{Run: runToPB(run)}), nil
}

func (s *runService) DispatchWorkflow(ctx context.Context, req *connect.Request[pb.DispatchWorkflowRequest]) (*connect.Response[pb.DispatchWorkflowResponse], error) {
	name, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	run, err := s.runs.DispatchWorkflow(ctx, caller(ctx), name, ports.DispatchParams{
		WorkflowPath: req.Msg.GetWorkflowPath(), Ref: req.Msg.GetRef(), Inputs: req.Msg.GetInputs(),
	})
	if err != nil {
		return nil, ciError(ctx, err)
	}
	return connect.NewResponse(&pb.DispatchWorkflowResponse{Run: runToPB(run)}), nil
}

func (s *runService) ListChecks(ctx context.Context, req *connect.Request[pb.ListChecksRequest]) (*connect.Response[pb.ListChecksResponse], error) {
	name, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	res, err := s.runs.ListChecks(ctx, caller(ctx), name, req.Msg.GetSha())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	out := &pb.ListChecksResponse{Truncated: res.Truncated}
	for _, c := range res.Checks {
		out.Checks = append(out.Checks, checkToPB(c.CICheck))
	}
	return connect.NewResponse(out), nil
}

func (s *runService) ListRunners(ctx context.Context, req *connect.Request[pb.ListRunnersRequest]) (*connect.Response[pb.ListRunnersResponse], error) {
	page, err := s.runs.ListRunners(ctx, caller(ctx), int(req.Msg.GetPageSize()), req.Msg.GetPageToken())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	out := &pb.ListRunnersResponse{NextPageToken: page.NextPageToken, Runners: make([]*pb.Runner, 0, len(page.Runners))}
	for _, r := range page.Runners {
		out.Runners = append(out.Runners, runnerToPB(r))
	}
	return connect.NewResponse(out), nil
}

func (s *runService) RemoveRunner(ctx context.Context, req *connect.Request[pb.RemoveRunnerRequest]) (*connect.Response[pb.RemoveRunnerResponse], error) {
	if err := s.runs.RemoveRunner(ctx, caller(ctx), req.Msg.GetRunnerId()); err != nil {
		return nil, ciError(ctx, err)
	}
	return connect.NewResponse(&pb.RemoveRunnerResponse{}), nil
}

// ListAnnotations lists the annotations of a run attempt.
func (s *runService) ListAnnotations(ctx context.Context, req *connect.Request[pb.ListAnnotationsRequest]) (*connect.Response[pb.ListAnnotationsResponse], error) {
	name, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	res, err := s.runs.ListAnnotations(ctx, caller(ctx), name, req.Msg.GetRunId(), req.Msg.GetAttempt(), req.Msg.GetJobId())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	out := &pb.ListAnnotationsResponse{Truncated: res.Truncated, Annotations: make([]*pb.JobAnnotation, 0, len(res.Annotations))}
	for _, a := range res.Annotations {
		out.Annotations = append(out.Annotations, &pb.JobAnnotation{JobId: a.JobID, Annotation: annotationToPB(a.Annotation)})
	}
	return connect.NewResponse(out), nil
}

// GetStepSummaries returns the summaries of a job.
func (s *runService) GetStepSummaries(ctx context.Context, req *connect.Request[pb.GetStepSummariesRequest]) (*connect.Response[pb.GetStepSummariesResponse], error) {
	name, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	res, err := s.runs.GetStepSummaries(ctx, caller(ctx), name, req.Msg.GetJobId(), req.Msg.GetAttempt(), 0)
	if err != nil {
		return nil, ciError(ctx, err)
	}
	out := &pb.GetStepSummariesResponse{Truncated: res.Truncated, Summaries: make([]*pb.StepSummary, 0, len(res.Summaries))}
	for _, m := range res.Summaries {
		out.Summaries = append(out.Summaries, &pb.StepSummary{StepNumber: m.Step, Markdown: m.Markdown})
	}
	return connect.NewResponse(out), nil
}
