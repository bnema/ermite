// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

const (
	depsRunnerSecret = "ermiterun_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN-_0"
	depsJobIDOne     = "deps-0123456789abcdef"
)

type depsCIRig struct {
	*ciUnit
	q *mocks.DepsJobs
}

func newDepsCIRig(t *testing.T, labels ...string) *depsCIRig {
	t.Helper()
	q := mocks.NewDepsJobs(t)
	u := newCIUnit(t, func(c *usecase.CIConfig) {
		c.DepsJobs, c.DepsRunnerLabel, c.PollTimeout = q, "ubuntu-latest", 20*time.Millisecond
	})
	u.store.EXPECT().FindRunnerByToken(mock.Anything, domain.HashTokenSecret(depsRunnerSecret)).
		Return(domain.CIRunner{ID: "r1", Name: "alpha", Labels: labels}, nil)
	u.store.EXPECT().ListReadyJobs(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	return &depsCIRig{ciUnit: u, q: q}
}

func TestFetchJobWithoutDepsKindNeverCallsDepsJobs(t *testing.T) {
	t.Parallel()
	for _, kinds := range [][]string{nil, {"workflow"}} {
		r := newDepsCIRig(t, "ubuntu-latest")
		res, err := r.ci.FetchJob(context.Background(), depsRunnerSecret, ports.FetchJobParams{Kinds: kinds})
		require.NoError(t, err)
		assert.Nil(t, res.Job)
	}
}

func TestFetchJobDepsLabelFilter(t *testing.T) {
	t.Parallel()
	r := newDepsCIRig(t, "self-hosted") // no deps label: Next is not called
	res, err := r.ci.FetchJob(context.Background(), depsRunnerSecret, ports.FetchJobParams{Kinds: []string{"workflow", "deps"}})
	require.NoError(t, err)
	assert.Nil(t, res.Job)
}

func TestFetchJobDepsJobCarriesReadOnlyTokenAndNoSecrets(t *testing.T) {
	t.Parallel()
	r := newDepsCIRig(t, "ubuntu-latest")
	repo := name(t, "proj")
	deadline := now.Add(10 * time.Minute)
	a := ports.DepsAssignment{
		JobID: depsJobIDOne, Repo: repo, Sha: "abc", Deadline: deadline,
		Job: ports.DepsJob{GoVersion: "1.25", Updates: []domain.DepRef{{Ecosystem: domain.EcosystemGoMod, Name: "x/y", Version: "v1.2.3"}}},
	}
	r.q.EXPECT().Next(mock.Anything, "r1").Return(a, true, nil)
	var stored domain.CIRepoToken
	r.store.EXPECT().Apply(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, w ports.CIWrite) (uint64, error) {
		require.Len(t, w.RepoTokens, 1)
		stored = w.RepoTokens[0]
		return 1, nil
	})

	res, err := r.ci.FetchJob(context.Background(), depsRunnerSecret, ports.FetchJobParams{Kinds: []string{"deps"}})
	require.NoError(t, err)
	require.NotNil(t, res.Job)
	j := res.Job
	require.NotNil(t, j.Deps)
	assert.Equal(t, a.Job, *j.Deps)
	assert.Equal(t, depsJobIDOne, j.Job.ID)
	assert.Equal(t, repo, j.Repo)
	assert.Equal(t, "abc", j.Sha)
	assert.False(t, j.Trusted)
	assert.Empty(t, j.Secrets)
	assert.Empty(t, j.Vars)
	assert.Empty(t, j.IDTokenURL)
	assert.Empty(t, j.IDTokenRequestToken)
	assert.Empty(t, j.Token)
	assert.NotEmpty(t, j.GithubToken)
	assert.Equal(t, domain.HashTokenSecret(j.GithubToken), stored.Hash)
	assert.Equal(t, repo, stored.Repo)
	assert.False(t, stored.OIDC)
	assert.Equal(t, deadline.Add(domain.RepoTokenGrace), stored.ExpiresAt)
	for k, lvl := range stored.Permissions {
		assert.Equal(t, domain.PermissionRead, lvl, k)
	}
	assert.Equal(t, domain.PermissionRead, stored.Permissions[domain.PermContents])
	assert.Equal(t, stored.Permissions, j.Permissions)
}

func TestDepsJobCallsAreRoutedByPrefix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lines := []domain.CILogLine{{Time: now, Text: "x"}}
	report := &ports.DepsReport{Error: "boom"}

	r := newDepsCIRig(t)
	r.store.EXPECT().FindRunnerByToken(mock.Anything, mock.Anything).Return(domain.CIRunner{ID: "r1"}, nil).Maybe()
	r.q.EXPECT().Update(mock.Anything, "r1", depsJobIDOne, mock.Anything, mock.Anything).Return(nil)
	r.q.EXPECT().AppendLog(mock.Anything, "r1", depsJobIDOne, int64(0), lines).Return(1, nil)
	r.q.EXPECT().Heartbeat(mock.Anything, "r1", depsJobIDOne).Return(nil)
	r.q.EXPECT().Complete(mock.Anything, "r1", depsJobIDOne, report, domain.CIFailure).Return(nil)

	require.NoError(t, r.ci.UpdateJob(ctx, depsRunnerSecret, depsJobIDOne, nil, nil))
	next, err := r.ci.AppendLog(ctx, depsRunnerSecret, depsJobIDOne, 1, 0, lines)
	require.NoError(t, err)
	assert.EqualValues(t, 1, next)
	require.NoError(t, r.ci.Heartbeat(ctx, depsRunnerSecret, depsJobIDOne))
	res, err := r.ci.CompleteJob(ctx, depsRunnerSecret, ports.CompleteJobParams{JobID: depsJobIDOne, Conclusion: domain.CIFailure, DepsReport: report})
	require.NoError(t, err)
	assert.True(t, res.Applied)
}

func TestDepsJobCallsRefuseABadSecretAndAWrongRunner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newDepsCIRig(t)
	// A job token is not a runner token.
	assert.ErrorIs(t, r.ci.Heartbeat(ctx, jobSecret, depsJobIDOne), domain.ErrUnauthenticated)
	// The queue refuses a runner that does not own the job.
	r.q.EXPECT().Heartbeat(mock.Anything, "r1", depsJobIDOne).Return(domain.ErrNotFound)
	r.store.EXPECT().FindRunnerByToken(mock.Anything, mock.Anything).Return(domain.CIRunner{ID: "r1"}, nil).Maybe()
	assert.ErrorIs(t, r.ci.Heartbeat(ctx, depsRunnerSecret, depsJobIDOne), domain.ErrNotFound)
}

func TestFetchJobRepeatedPollRepeatsDepsAssignment(t *testing.T) {
	t.Parallel()
	r := newDepsCIRig(t, "ubuntu-latest")
	a := ports.DepsAssignment{
		JobID: depsJobIDOne, Repo: name(t, "proj"), Sha: "abc", Deadline: now.Add(10 * time.Minute),
		Job: ports.DepsJob{GoVersion: "1.25"},
	}
	r.q.EXPECT().Next(mock.Anything, "r1").Return(a, true, nil).Once() // a second call would fail the mock
	r.store.EXPECT().Apply(mock.Anything, mock.Anything).Return(uint64(1), nil).Once()
	p := ports.FetchJobParams{Kinds: []string{"deps"}, PollID: "poll-1"}
	first, err := r.ci.FetchJob(context.Background(), depsRunnerSecret, p)
	require.NoError(t, err)
	require.NotNil(t, first.Job)
	again, err := r.ci.FetchJob(context.Background(), depsRunnerSecret, p)
	require.NoError(t, err)
	require.NotNil(t, again.Job)
	assert.Equal(t, first.Job.Job.ID, again.Job.Job.ID)
	assert.Equal(t, first.Job.GithubToken, again.Job.GithubToken)
	assert.Equal(t, first.Job.Deps, again.Job.Deps)

	// The first heartbeat shows the runner has it: the poll is forgotten.
	r.q.EXPECT().Heartbeat(mock.Anything, "r1", depsJobIDOne).Return(nil).Once()
	require.NoError(t, r.ci.Heartbeat(context.Background(), depsRunnerSecret, depsJobIDOne))
	r.q.EXPECT().Next(mock.Anything, "r1").Return(ports.DepsAssignment{}, false, nil)
	none, err := r.ci.FetchJob(context.Background(), depsRunnerSecret, p)
	require.NoError(t, err)
	assert.Nil(t, none.Job)
}

func TestFetchJobDepsOnlyRunnerNeverGetsAWorkflowJob(t *testing.T) {
	t.Parallel()
	r := newDepsCIRig(t, "ubuntu-latest")
	r.q.EXPECT().Next(mock.Anything, "r1").Return(ports.DepsAssignment{}, false, nil)
	// ListReadyJobs is optional in the rig and would hand out a job: it must not be read.
	r.store.ExpectedCalls = nil
	r.store.EXPECT().FindRunnerByToken(mock.Anything, domain.HashTokenSecret(depsRunnerSecret)).
		Return(domain.CIRunner{ID: "r1", Name: "alpha", Labels: []string{"ubuntu-latest"}}, nil)
	res, err := r.ci.FetchJob(context.Background(), depsRunnerSecret, ports.FetchJobParams{Kinds: []string{"deps"}})
	require.NoError(t, err)
	assert.Nil(t, res.Job)
}
