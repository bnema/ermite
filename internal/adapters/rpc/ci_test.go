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
