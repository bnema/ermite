// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestRunnerDepsFetchConversion(t *testing.T) {
	t.Parallel()
	for _, kinds := range [][]string{nil, {"workflow", "deps"}} {
		runners := mocks.NewRunnerService(t)
		runners.EXPECT().FetchJob(mock.Anything, mock.Anything, ports.FetchJobParams{Kinds: kinds, Labels: []string{"ubuntu-latest"}, PollID: "poll", Version: "test"}).Return(ports.FetchJobResult{Job: &ports.RunnerJob{
			Job: domain.CIJob{ID: "12-0-0", RunID: 12, RunAttempt: 1, TimeoutMinutes: 10, Labels: []string{"ubuntu-latest"}}, Repo: mustRepoName(t, "demo"), Sha: "sha", Ref: "refs/heads/main", Token: "job-secret", GithubToken: "repo-secret",
			Deps: &domain.CIDepsJob{GoVersion: "1.27.1", NodeVersion: "22", Details: []domain.CIDepsUpdate{{Ref: domain.DepRef{Ecosystem: domain.EcosystemGoMod, Directory: "sub", Name: "example.org/mod", Version: "v1.2.3"}, NewName: "example.org/mod/v2", Range: "^2.0.0", Manager: "pnpm", ManagerVersion: "9.15.0", Root: "web"}}},
		}}, nil).Once()
		res, err := (&runnerService{runners: runners}).FetchJob(context.Background(), &pb.FetchJobRequest{Kinds: kinds, Labels: []string{"ubuntu-latest"}, PollId: "poll", Version: "test"})
		require.NoError(t, err)
		j := res.GetJob()
		assert.Equal(t, "12-0-0", j.GetJobId())
		assert.EqualValues(t, 12, j.GetRunId())
		assert.EqualValues(t, 1, j.GetAttempt())
		assert.EqualValues(t, 10, j.GetTimeoutMinutes())
		assert.Equal(t, []string{"ubuntu-latest"}, j.GetLabels())
		assert.Equal(t, "refs/heads/main", j.GetRef())
		assert.Empty(t, j.GetWorkflowPath())
		assert.Empty(t, j.GetWorkflowYaml())
		assert.Equal(t, "demo", j.GetRepo())
		assert.Equal(t, "sha", j.GetSha())
		assert.Equal(t, "job-secret", j.GetJobToken())
		assert.Equal(t, "repo-secret", j.GetGithubToken())
		assert.False(t, j.GetTrusted())
		assert.Empty(t, j.GetSecrets())
		assert.Empty(t, j.GetVars())
		assert.Empty(t, j.GetIdTokenUrl())
		require.NotNil(t, j.GetDeps())
		assert.Equal(t, "1.27.1", j.GetDeps().GetGoVersion())
		assert.Equal(t, "22", j.GetDeps().GetNodeVersion())
		require.Len(t, j.GetDeps().GetUpdates(), 1)
		assert.Equal(t, &pb.DepsJobUpdate{Ecosystem: "gomod", Directory: "sub", Name: "example.org/mod", Version: "v1.2.3", NewName: "example.org/mod/v2", Range: "^2.0.0", Manager: "pnpm", ManagerVersion: "9.15.0", Root: "web"}, j.GetDeps().GetUpdates()[0])
	}
	assert.Nil(t, runnerJobToPB(&ports.RunnerJob{}).GetDeps())
}

func TestRunnerDepsCompleteConversion(t *testing.T) {
	t.Parallel()
	runners := mocks.NewRunnerService(t)
	report := &pb.DepsReport{Error: "runner error", Files: []*pb.DepsFile{{Path: "go.mod", Content: []byte("first")}, {Path: "go.mod", Content: []byte("second")}}}
	runners.EXPECT().CompleteJob(mock.Anything, mock.Anything, mock.MatchedBy(func(p ports.CompleteJobParams) bool {
		return p.JobID == "12-0-0" && p.Conclusion == domain.CIFailure && assert.Equal(t, &ports.DepsReport{Error: "runner error", Files: []ports.DepsFile{{Path: "go.mod", Content: []byte("first")}, {Path: "go.mod", Content: []byte("second")}}}, p.DepsReport)
	})).Return(ports.CompleteJobResult{Conclusion: domain.CIFailure, Applied: true}, nil).Once()
	res, err := (&runnerService{runners: runners}).CompleteJob(context.Background(), &pb.CompleteJobRequest{JobId: "12-0-0", Conclusion: pb.CiConclusion_CI_CONCLUSION_FAILURE, DepsReport: report})
	require.NoError(t, err)
	assert.True(t, res.GetApplied())
	assert.Nil(t, depsReportFromPB(nil))
	assert.NotNil(t, depsReportFromPB(&pb.DepsReport{}))
}

// CompleteJob alone accepts a deps report beyond the 1 MiB runner limit.
func TestCompleteJobAcceptsLargeDepsReport(t *testing.T) {
	t.Parallel()
	runners := mocks.NewRunnerService(t)
	runners.EXPECT().CompleteJob(mock.Anything, mock.Anything, mock.MatchedBy(func(p ports.CompleteJobParams) bool {
		return p.DepsReport != nil && len(p.DepsReport.Files) == 1 && len(p.DepsReport.Files[0].Content) == 3<<20
	})).Return(ports.CompleteJobResult{Conclusion: domain.CISuccess, Applied: true}, nil).Once()
	srv := httptest.NewServer(New(Config{Runner: runners, Sessions: mocks.NewSessionService(t), SigningKey: mocks.NewSigningKeyService(t)}).Handler())
	t.Cleanup(srv.Close)
	c := ermiteconnect.NewRunnerServiceClient(connect.NewClient(connecthttp.NewTransport(srv.Client(), srv.URL, connecthttp.WithReadMaxBytes(0))))
	req := &pb.CompleteJobRequest{JobId: "12-0-0", Conclusion: pb.CiConclusion_CI_CONCLUSION_SUCCESS, DepsReport: &pb.DepsReport{Files: []*pb.DepsFile{{Path: "go.sum", Content: make([]byte, 3<<20)}}}}
	ctx, info := connect.NewClientContext(context.Background())
	info.RequestHeader().Set("Authorization", "Bearer ermitejob_x")
	res, err := c.CompleteJob(ctx, req)
	require.NoError(t, err)
	assert.True(t, res.GetApplied())

	big := &pb.FetchJobRequest{PollId: strings.Repeat("a", RunnerMaxRequestBytes+1)}
	info.RequestHeader().Set("Authorization", "Bearer ermiterunner_x")
	_, err = c.FetchJob(ctx, big)
	requireCode(t, connect.CodeResourceExhausted, err)
}
