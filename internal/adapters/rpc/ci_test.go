// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

// RunService.GetJob: the job with its steps and the range of their lines, the
// states and conclusions of the contract, and the codes of the refusals.
func TestRunServiceGetJob(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	job := ports.JobView{CIJob: domain.CIJob{
		ID: "7-0-0", RunID: 7, Attempt: 2, RunAttempt: 2, Repo: mustRepoName(t, "demo"), Key: "build", Name: "build (linux)",
		Phase: domain.CIPhaseEnded, Conclusion: domain.CIFailure, Error: "boom", Labels: []string{"self-hosted"}, Needs: []string{"lint"},
		RunnerName: "alpha", Isolation: domain.CIIsolationVM, CreatedAt: at, StartedAt: at, CompletedAt: at.Add(time.Minute), Version: 9,
		Steps: []domain.CIStep{
			{Number: 1, Name: "Set up job", State: domain.CIStepCompleted, Conclusion: domain.CISuccess, StartedAt: at, CompletedAt: at, HasLines: true, LogStart: 0},
			{Number: 2, Name: "Run make", State: domain.CIStepCompleted, Conclusion: domain.CIFailure, StartedAt: at, CompletedAt: at, HasLines: true, LogStart: 3},
		},
	}, Lines: 8}

	runs := mocks.NewRunService(t)
	runs.EXPECT().GetJob(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), "7-0-0", int32(2)).Return(job, nil).Once()
	resp, err := (&runService{runs: runs}).GetJob(ownerCtx(), connect.NewRequest(&pb.GetJobRequest{Repo: "demo", JobId: "7-0-0", Attempt: 2}))
	require.NoError(t, err)
	got := resp.Msg.GetJob()
	assert.Equal(t, "7-0-0", got.GetId())
	assert.EqualValues(t, 2, got.GetAttempt())
	assert.Equal(t, "build (linux)", got.GetName())
	assert.Equal(t, pb.CiStatus_CI_STATUS_COMPLETED, got.GetStatus())
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_FAILURE, got.GetConclusion())
	assert.Equal(t, "boom", got.GetError())
	assert.Equal(t, pb.RunnerIsolation_RUNNER_ISOLATION_VM, got.GetIsolation())
	assert.Equal(t, []string{"lint"}, got.GetNeeds())
	assert.EqualValues(t, 9, got.GetVersion())
	require.Len(t, got.GetSteps(), 2)
	assert.Equal(t, []int64{0, 3}, []int64{got.GetSteps()[0].GetLogStart(), got.GetSteps()[0].GetLogEnd()}, "a step ends where the next begins")
	assert.Equal(t, []int64{3, 8}, []int64{got.GetSteps()[1].GetLogStart(), got.GetSteps()[1].GetLogEnd()}, "the last ends at the end of the log")
	assert.Equal(t, pb.StepState_STEP_STATE_COMPLETED, got.GetSteps()[1].GetState())

	for name, tc := range map[string]struct {
		repo string
		err  error
		code connect.Code
	}{
		"invalid repo":  {"../x", nil, connect.CodeInvalidArgument},
		"unknown job":   {"demo", domain.ErrNotFound, connect.CodeNotFound},
		"not readable":  {"demo", domain.ErrNotFound, connect.CodeNotFound},
		"store failure": {"demo", assert.AnError, connect.CodeInternal},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runs := mocks.NewRunService(t)
			if tc.err != nil {
				runs.EXPECT().GetJob(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.JobView{}, tc.err).Once()
			}
			_, err := (&runService{runs: runs}).GetJob(ownerCtx(), connect.NewRequest(&pb.GetJobRequest{Repo: tc.repo, JobId: "7-0-0"}))
			requireCode(t, tc.code, err)
		})
	}
}

// The end of a job reaches the runner as failed_precondition with the JobEnded
// detail; a bad credential counts against the client address.
func TestRunnerServiceErrors(t *testing.T) {
	t.Parallel()
	ended := &domain.JobEndedError{Conclusion: domain.CICancelled, Message: "stopped"}
	err := ciError(ownerCtx(), ended)
	requireCode(t, connect.CodeFailedPrecondition, err)
	var ce *connect.Error
	require.ErrorAs(t, err, &ce)
	require.Len(t, ce.Details(), 1)
	msg, derr := ce.Details()[0].Value()
	require.NoError(t, derr)
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_CANCELLED, msg.(*pb.JobEnded).GetConclusion())
	assert.Equal(t, "stopped", msg.(*pb.JobEnded).GetError())

	requireCode(t, connect.CodeOutOfRange, ciError(ownerCtx(), &ports.ErrCILogGap{Count: 4}))

	limiter := mocks.NewFailureLimiter(t)
	limiter.EXPECT().Fail(mock.Anything, "client-key").Return().Once()
	s := &runnerService{runners: mocks.NewRunnerService(t), limiter: limiter}
	ctx := t.Context()
	ctx = context.WithValue(ctx, addrKey{}, "client-key")
	requireCode(t, connect.CodeUnauthenticated, s.failed(ctx, domain.ErrUnauthenticated))
	requireCode(t, connect.CodePermissionDenied, s.failed(ctx, domain.ErrForbidden)) // not a bad credential: no Fail
}

// The annotations of a request are mapped to the domain; a level that is not
// one of the three is invalid_argument before the use case sees the call.
func TestRunnerServiceAnnotationsAndSummary(t *testing.T) {
	t.Parallel()
	anns := []*pb.Annotation{
		{Level: pb.AnnotationLevel_ANNOTATION_LEVEL_FAILURE, Title: "t", Message: "m", File: "f.go", StartLine: 1, EndLine: 2, StartColumn: 3, EndColumn: 4, StepNumber: 5},
		{Level: pb.AnnotationLevel_ANNOTATION_LEVEL_NOTICE, Message: "n", StepNumber: 1},
	}
	want := []domain.CIAnnotation{
		{Level: domain.AnnotationFailure, Title: "t", Message: "m", File: "f.go", StartLine: 1, EndLine: 2, StartColumn: 3, EndColumn: 4, Step: 5},
		{Level: domain.AnnotationNotice, Message: "n", Step: 1},
	}
	ctx := context.WithValue(ownerCtx(), secretKey{}, "ermitejob_x")

	runners := mocks.NewRunnerService(t)
	runners.EXPECT().UpdateJob(mock.Anything, "ermitejob_x", "7-0-0", mock.Anything, want).Return(nil).Once()
	runners.EXPECT().CompleteJob(mock.Anything, "ermitejob_x", mock.MatchedBy(func(p ports.CompleteJobParams) bool {
		return p.JobID == "7-0-0" && len(p.Annotations) == 2 && p.Annotations[0] == want[0]
	})).Return(ports.CompleteJobResult{Conclusion: domain.CISuccess, Applied: true}, nil).Once()
	runners.EXPECT().UploadSummary(mock.Anything, "ermitejob_x", "7-0-0", int32(3), "# md").Return(nil).Once()
	runners.EXPECT().UploadSummary(mock.Anything, "ermitejob_x", "7-0-0", int32(3), "").Return(&domain.JobEndedError{Conclusion: domain.CIFailure}).Once()
	s := &runnerService{runners: runners}

	_, err := s.UpdateJob(ctx, connect.NewRequest(&pb.UpdateJobRequest{JobId: "7-0-0", Annotations: anns}))
	require.NoError(t, err)
	_, err = s.CompleteJob(ctx, connect.NewRequest(&pb.CompleteJobRequest{JobId: "7-0-0", Conclusion: pb.CiConclusion_CI_CONCLUSION_SUCCESS, Annotations: anns}))
	require.NoError(t, err)
	_, err = s.UploadSummary(ctx, connect.NewRequest(&pb.UploadSummaryRequest{JobId: "7-0-0", StepNumber: 3, Markdown: "# md"}))
	require.NoError(t, err)
	_, err = s.UploadSummary(ctx, connect.NewRequest(&pb.UploadSummaryRequest{JobId: "7-0-0", StepNumber: 3}))
	requireCode(t, connect.CodeFailedPrecondition, err)

	_, err = s.UpdateJob(ctx, connect.NewRequest(&pb.UpdateJobRequest{JobId: "7-0-0", Annotations: []*pb.Annotation{{Message: "no level", StepNumber: 1}}}))
	requireCode(t, connect.CodeInvalidArgument, err)
	_, err = s.UpdateJob(ctx, connect.NewRequest(&pb.UpdateJobRequest{JobId: "7-0-0", Annotations: []*pb.Annotation{{Level: 99, Message: "x", StepNumber: 1}}}))
	requireCode(t, connect.CodeInvalidArgument, err)
	_, err = s.CompleteJob(ctx, connect.NewRequest(&pb.CompleteJobRequest{JobId: "7-0-0", Conclusion: pb.CiConclusion_CI_CONCLUSION_SUCCESS, Annotations: []*pb.Annotation{{Message: "x"}}}))
	requireCode(t, connect.CodeInvalidArgument, err)
}

// StepUpdate.outcome reaches the use case, and Step.outcome is the conclusion
// for a completed step stored without one, and unset before the end.
func TestStepOutcomeMapping(t *testing.T) {
	t.Parallel()
	ups, err := stepUpdatesFromPB([]*pb.StepUpdate{{Number: 1, State: pb.StepState_STEP_STATE_COMPLETED, Conclusion: pb.CiConclusion_CI_CONCLUSION_SUCCESS, Outcome: pb.CiConclusion_CI_CONCLUSION_FAILURE}})
	require.NoError(t, err)
	assert.Equal(t, domain.CIFailure, ups[0].Outcome)
	_, err = stepUpdatesFromPB([]*pb.StepUpdate{{Number: 1, Outcome: 99}})
	assert.Error(t, err)

	steps := stepsToPB([]domain.CIStep{
		{Number: 1, State: domain.CIStepCompleted, Conclusion: domain.CISuccess, Outcome: domain.CIFailure},
		{Number: 2, State: domain.CIStepCompleted, Conclusion: domain.CIFailure},
		{Number: 3, State: domain.CIStepInProgress},
		{Number: 4, State: domain.CIStepCompleted, Conclusion: domain.CISkipped, Outcome: domain.CISkipped},
	}, 0)
	got := make([]pb.CiConclusion, len(steps))
	for i, s := range steps {
		got[i] = s.GetOutcome()
	}
	assert.Equal(t, []pb.CiConclusion{
		pb.CiConclusion_CI_CONCLUSION_FAILURE, pb.CiConclusion_CI_CONCLUSION_FAILURE,
		pb.CiConclusion_CI_CONCLUSION_UNSPECIFIED, pb.CiConclusion_CI_CONCLUSION_SKIPPED,
	}, got)
}

func TestRunServiceAnnotationsAndSummaries(t *testing.T) {
	t.Parallel()
	repo := mustRepoName(t, "demo")
	runs := mocks.NewRunService(t)
	runs.EXPECT().ListAnnotations(mock.Anything, domain.Owner(), repo, int64(7), int32(2), "7-0-0").Return(ports.AnnotationList{
		Annotations: []ports.JobAnnotation{{JobID: "7-0-0", Annotation: domain.CIAnnotation{Level: domain.AnnotationWarning, Message: "m", File: "a.go", StartLine: 4, Step: 3}}},
		Truncated:   true,
	}, nil).Once()
	runs.EXPECT().GetStepSummaries(mock.Anything, domain.Owner(), repo, "7-0-0", int32(0), int64(0)).Return(ports.SummaryList{
		Summaries: []domain.CIStepSummary{{Step: 2, Markdown: "# s"}}, Truncated: true,
	}, nil).Once()
	runs.EXPECT().ListAnnotations(mock.Anything, mock.Anything, repo, int64(8), int32(0), "").Return(ports.AnnotationList{}, domain.ErrNotFound).Once()
	s := &runService{runs: runs}

	res, err := s.ListAnnotations(ownerCtx(), connect.NewRequest(&pb.ListAnnotationsRequest{Repo: "demo", RunId: 7, Attempt: 2, JobId: "7-0-0"}))
	require.NoError(t, err)
	require.Len(t, res.Msg.GetAnnotations(), 1)
	a := res.Msg.GetAnnotations()[0]
	assert.Equal(t, "7-0-0", a.GetJobId())
	assert.Equal(t, pb.AnnotationLevel_ANNOTATION_LEVEL_WARNING, a.GetAnnotation().GetLevel())
	assert.EqualValues(t, 3, a.GetAnnotation().GetStepNumber())
	assert.EqualValues(t, 4, a.GetAnnotation().GetStartLine())
	assert.True(t, res.Msg.GetTruncated())

	sums, err := s.GetStepSummaries(ownerCtx(), connect.NewRequest(&pb.GetStepSummariesRequest{Repo: "demo", JobId: "7-0-0"}))
	require.NoError(t, err)
	require.Len(t, sums.Msg.GetSummaries(), 1)
	assert.EqualValues(t, 2, sums.Msg.GetSummaries()[0].GetStepNumber())
	assert.Equal(t, "# s", sums.Msg.GetSummaries()[0].GetMarkdown())
	assert.True(t, sums.Msg.GetTruncated())

	_, err = s.ListAnnotations(ownerCtx(), connect.NewRequest(&pb.ListAnnotationsRequest{Repo: "demo", RunId: 8}))
	requireCode(t, connect.CodeNotFound, err)
	_, err = s.ListAnnotations(ownerCtx(), connect.NewRequest(&pb.ListAnnotationsRequest{Repo: "../x", RunId: 8}))
	requireCode(t, connect.CodeInvalidArgument, err)
	_, err = s.GetStepSummaries(ownerCtx(), connect.NewRequest(&pb.GetStepSummariesRequest{Repo: "../x"}))
	requireCode(t, connect.CodeInvalidArgument, err)
}

// UploadSummary is a job call: its bearer secret is the job token, not a stored
// token, and a summary of exactly 1 MiB passes the size limit of the runner side.
func TestUploadSummaryIsAJobCallWithRoomForOneMiB(t *testing.T) {
	t.Parallel()
	assert.True(t, runnerSecretProcedures[ermiteconnect.RunnerServiceUploadSummaryProcedure])
	assert.Greater(t, RunnerMaxRequestBytes, 1<<20+len("7-0-0")+16, "1 MiB plus the envelope")
	assert.Equal(t, MaxRequestBytes, 1<<20, "the other services keep the 1 MiB limit")
	assert.Contains(t, Features, "ci.annotations")
	assert.Contains(t, Features, "ci.summaries")
}

// deps.runs tells clients that a dependency job is a job of a run like any
// other: its calls take the job token and WatchJob carries the cancel request.
func TestDepsRunsFeatureIsListed(t *testing.T) {
	t.Parallel()
	assert.Contains(t, Features, "deps.runs")
}
