// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

func repoToken(id, secret string, run jobRefT, repo domain.RepoName, archive bool) domain.CIRepoToken {
	return domain.CIRepoToken{
		ID: id, Hash: domain.HashTokenSecret(secret), Repo: repo, RunID: run.RunID, RunAttempt: 1, JobID: run.JobID,
		Archive: archive, ExpiresAt: created.Add(time.Hour), CreatedAt: created,
	}
}

func TestCIRepoTokensLifecycle(t *testing.T) {
	ctx := context.Background()
	s, ci, n := ciRepo(t)
	d, err := ci.CreateRun(ctx, n, "w.yml", draftOf(n, "w.yml", 2))
	require.NoError(t, err)
	a, b := d.Jobs[0], d.Jobs[1]
	refA := jobRefT{RunID: d.Run.ID, JobID: a.ID}
	refB := jobRefT{RunID: d.Run.ID, JobID: b.ID}

	own := repoToken("t1", "ermitegh_own", refA, n, false)
	arch := repoToken("t2", "ermitegh_arch", refA, n, true)
	other := repoToken("t3", "ermitegh_other", refB, n, false)
	_, err = ci.Apply(ctx, ports.CIWrite{RepoTokens: []domain.CIRepoToken{own, arch, other}})
	require.NoError(t, err)

	got, err := ci.FindRepoTokenByHash(ctx, own.Hash)
	require.NoError(t, err)
	assert.Equal(t, own.ID, got.ID)
	assert.Equal(t, n, got.Repo)
	assert.False(t, got.Archive)
	assert.True(t, got.ExpiresAt.Equal(own.ExpiresAt))
	byID, err := ci.GetRepoToken(ctx, "t2")
	require.NoError(t, err)
	assert.True(t, byID.Archive)
	_, err = ci.FindRepoTokenByHash(ctx, domain.HashTokenSecret("ermitegh_nope"))
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = ci.GetRepoToken(ctx, "../token/x")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	// They live apart from the stored tokens: ListTokens-style reads see none.
	stored, err := s.Tokens().List(ctx)
	require.NoError(t, err)
	assert.Empty(t, stored)
	_, err = s.Tokens().FindByHash(ctx, own.Hash)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = s.Tokens().Get(ctx, "t1")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	// A token that exists is not created twice.
	_, err = ci.Apply(ctx, ports.CIWrite{RepoTokens: []domain.CIRepoToken{own}})
	assert.Error(t, err)

	// Revoking a job deletes its tokens, and only its tokens.
	_, err = ci.Apply(ctx, ports.CIWrite{RevokeJobs: []ports.CIJobRef{{RunID: d.Run.ID, RunAttempt: 1, JobID: a.ID}}})
	require.NoError(t, err)
	for _, tok := range []domain.CIRepoToken{own, arch} {
		_, err = ci.FindRepoTokenByHash(ctx, tok.Hash)
		assert.ErrorIs(t, err, domain.ErrNotFound, tok.ID)
		_, err = ci.GetRepoToken(ctx, tok.ID)
		assert.ErrorIs(t, err, domain.ErrNotFound, tok.ID)
	}
	_, err = ci.FindRepoTokenByHash(ctx, other.Hash)
	assert.NoError(t, err, "the other job keeps its token")

	// Revoking again, or a job without tokens, changes nothing.
	_, err = ci.Apply(ctx, ports.CIWrite{RevokeJobs: []ports.CIJobRef{{RunID: d.Run.ID, RunAttempt: 1, JobID: a.ID}, {RunID: 99, RunAttempt: 1, JobID: "99-0-0"}}})
	require.NoError(t, err)

	// A token can be revoked and created again in one batch (a new assignment).
	again := repoToken("t4", "ermitegh_again", refB, n, false)
	_, err = ci.Apply(ctx, ports.CIWrite{RevokeJobs: []ports.CIJobRef{{RunID: d.Run.ID, RunAttempt: 1, JobID: b.ID}}, RepoTokens: []domain.CIRepoToken{again}})
	require.NoError(t, err)
	_, err = ci.FindRepoTokenByHash(ctx, other.Hash)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = ci.FindRepoTokenByHash(ctx, again.Hash)
	assert.NoError(t, err)

	// A token of a repository that no longer exists cannot be created.
	require.NoError(t, s.Delete(ctx, n))
	_, err = ci.FindRepoTokenByHash(ctx, again.Hash)
	assert.ErrorIs(t, err, domain.ErrNotFound, "deleting the repository deletes the tokens of its runs")
	_, err = ci.Apply(ctx, ports.CIWrite{RepoTokens: []domain.CIRepoToken{repoToken("t5", "ermitegh_late", refA, n, false)}})
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestCIActionResolutionsAreKeptPerAttemptAndSwept(t *testing.T) {
	ctx := context.Background()
	_, ci, n := ciRepo(t)
	d, err := ci.CreateRun(ctx, n, "w.yml", draftOf(n, "w.yml", 1))
	require.NoError(t, err)
	r := domain.CIActionResolution{
		RunID: d.Run.ID, RunAttempt: 1, Key: "actions/checkout@v4", NameWithOwner: "actions/checkout",
		ResolvedSHA: "1111111111111111111111111111111111111111", Source: domain.CIActionGitHub,
	}
	_, err = ci.Apply(ctx, ports.CIWrite{Resolutions: []domain.CIActionResolution{r}})
	require.NoError(t, err)

	got, err := ci.GetActionResolution(ctx, d.Run.ID, 1, "actions/checkout@v4")
	require.NoError(t, err)
	assert.Equal(t, r, got)
	_, err = ci.GetActionResolution(ctx, d.Run.ID, 2, "actions/checkout@v4")
	assert.ErrorIs(t, err, domain.ErrNotFound, "another attempt resolves again")
	_, err = ci.GetActionResolution(ctx, d.Run.ID, 1, "actions/checkout@v5")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	// With a token of the run, both go when the run is swept.
	tok := repoToken("t1", "ermitegh_x", jobRefT{RunID: d.Run.ID, JobID: d.Jobs[0].ID}, n, false)
	_, err = ci.Apply(ctx, ports.CIWrite{RepoTokens: []domain.CIRepoToken{tok}})
	require.NoError(t, err)
	run, _ := ci.GetRun(ctx, n, d.Run.ID)
	run.Status, run.Conclusion, run.CompletedAt = domain.CICompleted, domain.CISuccess, created
	job, _ := ci.GetJob(ctx, run.ID, 1, d.Jobs[0].ID)
	job.Phase, job.Conclusion = domain.CIPhaseEnded, domain.CISuccess
	_, err = ci.Apply(ctx, ports.CIWrite{Runs: []domain.CIRun{run}, Jobs: []domain.CIJob{job}})
	require.NoError(t, err)
	ids, err := ci.Sweep(ctx, created.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, []int64{d.Run.ID}, ids)
	_, err = ci.GetActionResolution(ctx, d.Run.ID, 1, "actions/checkout@v4")
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = ci.FindRepoTokenByHash(ctx, tok.Hash)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

// jobRefT names a job of the first attempt of a run in these tests.
type jobRefT struct {
	RunID int64
	JobID string
}
