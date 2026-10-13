// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// ciRepo opens a store holding repository "proj".
func ciRepo(t *testing.T) (*kvstore.Store, *kvstore.CI, domain.RepoName) {
	t.Helper()
	s := openStore(t, t.TempDir())
	n := name(t, "proj")
	require.NoError(t, s.Create(context.Background(), domain.Repo{Name: n, Visibility: domain.VisibilityPrivate, CreatedAt: created}))
	return s, s.CI(), n
}

func draftOf(repo domain.RepoName, workflow string, jobs int) func(id, number int64) ports.CIRunDraft {
	return func(id, number int64) ports.CIRunDraft {
		d := ports.CIRunDraft{
			Run: domain.CIRun{
				Repo: repo, ID: id, Number: number, Attempt: 1, WorkflowPath: workflow, WorkflowName: "ci", Event: "push",
				Ref: "refs/heads/main", Sha: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: domain.CIQueued, CreatedAt: created,
			},
			Source: domain.CIRunSource{WorkflowYAML: "on: push", GithubJSON: "{}"},
		}
		for i := range jobs {
			j := domain.CIJob{
				ID: domain.NewJobID(id, i, 0), RunID: id, Attempt: 1, RunAttempt: 1, Repo: repo, Key: "build", Name: "build",
				Phase: domain.CIPhaseReady, ReadyAt: created.Add(time.Duration(i) * time.Second), CreatedAt: created,
				Labels: []string{"self-hosted"}, Index: i, TimeoutMinutes: 360, Planned: true,
			}
			d.Jobs = append(d.Jobs, j)
			d.Checks = append(d.Checks, domain.CICheck{
				ID: j.ID, Repo: repo, Sha: d.Run.Sha, Name: j.Name, Status: domain.CIQueued, RunID: id, JobID: j.ID, Attempt: 1,
				WorkflowName: "ci", CreatedAt: created,
			})
		}
		return d
	}
}

func TestCICreateRunTakesIncreasingIdsAndPerWorkflowNumbers(t *testing.T) {
	ctx := context.Background()
	_, ci, n := ciRepo(t)

	a, err := ci.CreateRun(ctx, n, ".github/workflows/ci.yml", draftOf(n, ".github/workflows/ci.yml", 1))
	require.NoError(t, err)
	b, err := ci.CreateRun(ctx, n, ".github/workflows/ci.yml", draftOf(n, ".github/workflows/ci.yml", 1))
	require.NoError(t, err)
	c, err := ci.CreateRun(ctx, n, ".github/workflows/lint.yml", draftOf(n, ".github/workflows/lint.yml", 0))
	require.NoError(t, err)

	assert.Equal(t, []int64{1, 2, 3}, []int64{a.Run.ID, b.Run.ID, c.Run.ID})
	assert.Equal(t, []int64{1, 2, 1}, []int64{a.Run.Number, b.Run.Number, c.Run.Number})
	assert.NotZero(t, a.Run.Version)
}

func TestCIConcurrentCreateRunNeverSharesAnId(t *testing.T) {
	ctx := context.Background()
	_, ci, n := ciRepo(t)
	var mu sync.Mutex
	ids := map[int64]bool{}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := ci.CreateRun(ctx, n, "w.yml", draftOf(n, "w.yml", 1))
			if assert.NoError(t, err) {
				mu.Lock()
				defer mu.Unlock()
				assert.False(t, ids[d.Run.ID], "id %d handed out twice", d.Run.ID)
				ids[d.Run.ID] = true
			}
		}()
	}
	wg.Wait()
	assert.Len(t, ids, 12)
}

func TestCICreateRunFailsOnAMissingRepository(t *testing.T) {
	_, ci, _ := ciRepo(t)
	ghost := name(t, "ghost")
	_, err := ci.CreateRun(context.Background(), ghost, "w.yml", draftOf(ghost, "w.yml", 1))
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestCIJobIndexesFollowThePhase(t *testing.T) {
	ctx := context.Background()
	_, ci, n := ciRepo(t)
	d, err := ci.CreateRun(ctx, n, "w.yml", draftOf(n, "w.yml", 2))
	require.NoError(t, err)

	ready, err := ci.ListReadyJobs(ctx, 10)
	require.NoError(t, err)
	require.Len(t, ready, 2)
	assert.Equal(t, d.Jobs[0].ID, ready[0].ID, "oldest first")

	open, err := ci.ListOpenRuns(ctx)
	require.NoError(t, err)
	assert.Len(t, open, 1)

	// Assign the first job: it leaves the queue, enters the active set and its
	// token becomes findable.
	j := ready[0]
	hash := domain.HashTokenSecret("ermitejob_x")
	j.Phase, j.TokenHash, j.AssignedAt = domain.CIPhaseAssigned, hash, created.Add(time.Minute)
	j.TokenValidUntil = created.Add(48 * time.Hour)
	_, err = ci.Apply(ctx, ports.CIWrite{Jobs: []domain.CIJob{j}})
	require.NoError(t, err)

	ready, _ = ci.ListReadyJobs(ctx, 10)
	assert.Len(t, ready, 1)
	active, err := ci.ListActiveJobs(ctx)
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, j.ID, active[0].ID)
	found, err := ci.FindJobByToken(ctx, hash, created.Add(2*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, j.ID, found.ID)
	_, err = ci.FindJobByToken(ctx, hash, created.Add(72*time.Hour))
	assert.ErrorIs(t, err, domain.ErrNotFound, "an expired token is not found")

	// A new token replaces the old one.
	j2, err := ci.GetJob(ctx, j.RunID, j.RunAttempt, j.ID)
	require.NoError(t, err)
	j2.TokenHash = domain.HashTokenSecret("ermitejob_y")
	_, err = ci.Apply(ctx, ports.CIWrite{Jobs: []domain.CIJob{j2}})
	require.NoError(t, err)
	_, err = ci.FindJobByToken(ctx, hash, created.Add(2*time.Minute))
	assert.ErrorIs(t, err, domain.ErrNotFound, "the replaced token is gone")

	// End it.
	j3, _ := ci.GetJob(ctx, j.RunID, j.RunAttempt, j.ID)
	j3.Phase, j3.Conclusion = domain.CIPhaseEnded, domain.CISuccess
	_, err = ci.Apply(ctx, ports.CIWrite{Jobs: []domain.CIJob{j3}})
	require.NoError(t, err)
	active, _ = ci.ListActiveJobs(ctx)
	assert.Empty(t, active)
}

func TestCIApplyChecksVersions(t *testing.T) {
	ctx := context.Background()
	_, ci, n := ciRepo(t)
	d, err := ci.CreateRun(ctx, n, "w.yml", draftOf(n, "w.yml", 1))
	require.NoError(t, err)

	a, err := ci.GetRun(ctx, n, d.Run.ID)
	require.NoError(t, err)
	b := a
	a.Status = domain.CIInProgress
	_, err = ci.Apply(ctx, ports.CIWrite{Runs: []domain.CIRun{a}})
	require.NoError(t, err)
	b.Status = domain.CICompleted
	_, err = ci.Apply(ctx, ports.CIWrite{Runs: []domain.CIRun{b}})
	assert.ErrorIs(t, err, domain.ErrConflict, "a write on a stale copy loses")
}

func TestCIChecksAreOrderedAndLimited(t *testing.T) {
	ctx := context.Background()
	_, ci, n := ciRepo(t)
	d, err := ci.CreateRun(ctx, n, "w.yml", draftOf(n, "w.yml", 3))
	require.NoError(t, err)

	checks, more, err := ci.ListChecks(ctx, n, d.Run.Sha, 10)
	require.NoError(t, err)
	assert.Len(t, checks, 3)
	assert.False(t, more)
	checks, more, err = ci.ListChecks(ctx, n, d.Run.Sha, 2)
	require.NoError(t, err)
	assert.Len(t, checks, 2)
	assert.True(t, more)
	none, _, err := ci.ListChecks(ctx, n, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 10)
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestCIListRunsNewestFirstAndBounded(t *testing.T) {
	ctx := context.Background()
	s, ci, n := ciRepo(t)
	other := name(t, "other")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: other, Visibility: domain.VisibilityPublic, CreatedAt: created}))
	_, err := ci.CreateRun(ctx, other, "w.yml", draftOf(other, "w.yml", 0))
	require.NoError(t, err)
	for range 5 {
		_, err := ci.CreateRun(ctx, n, "w.yml", draftOf(n, "w.yml", 0))
		require.NoError(t, err)
	}
	runs, err := ci.ListRuns(ctx, n, 0, 3)
	require.NoError(t, err)
	require.Len(t, runs, 3)
	assert.Equal(t, []int64{6, 5, 4}, []int64{runs[0].ID, runs[1].ID, runs[2].ID})
	runs, err = ci.ListRuns(ctx, n, 4, 10)
	require.NoError(t, err)
	assert.Equal(t, []int64{3, 2}, []int64{runs[0].ID, runs[1].ID})
	ids, err := ci.RunIDs(ctx, n)
	require.NoError(t, err)
	assert.Len(t, ids, 5)
}

func TestCIRunnersCreateReplaceFindRemove(t *testing.T) {
	ctx := context.Background()
	_, ci, _ := ciRepo(t)
	h1, h2 := domain.HashTokenSecret("ermiterun_1"), domain.HashTokenSecret("ermiterun_2")
	r1 := domain.CIRunner{ID: "r1", Name: "Alpha", Labels: []string{"linux"}, Isolation: domain.CIIsolationVM, TokenHash: h1, RegisteredAt: created}
	_, err := ci.CreateRunner(ctx, r1, false)
	require.NoError(t, err)

	r2 := domain.CIRunner{ID: "r2", Name: "alpha", TokenHash: h2, RegisteredAt: created}
	_, err = ci.CreateRunner(ctx, r2, false)
	assert.ErrorIs(t, err, domain.ErrAlreadyExists, "names are case-insensitive")

	removed, err := ci.CreateRunner(ctx, r2, true)
	require.NoError(t, err)
	require.Len(t, removed, 1)
	assert.Equal(t, "r1", removed[0].ID)
	_, err = ci.FindRunnerByToken(ctx, h1)
	assert.ErrorIs(t, err, domain.ErrNotFound, "the replaced runner's token stops working")
	got, err := ci.FindRunnerByToken(ctx, h2)
	require.NoError(t, err)
	assert.Equal(t, "r2", got.ID)

	list, err := ci.ListRunners(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	_, err = ci.RemoveRunner(ctx, "r2")
	require.NoError(t, err)
	_, err = ci.RemoveRunner(ctx, "r2")
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = ci.FindRunnerByToken(ctx, h2)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	// The name is free again.
	_, err = ci.CreateRunner(ctx, domain.CIRunner{ID: "r3", Name: "alpha", TokenHash: domain.HashTokenSecret("ermiterun_3")}, false)
	assert.NoError(t, err)
}

func TestCISweepRemovesOldCompletedRunsOnly(t *testing.T) {
	ctx := context.Background()
	_, ci, n := ciRepo(t)
	oldRun, err := ci.CreateRun(ctx, n, "w.yml", draftOf(n, "w.yml", 1))
	require.NoError(t, err)
	newRun, err := ci.CreateRun(ctx, n, "w.yml", draftOf(n, "w.yml", 1))
	require.NoError(t, err)
	openRun, err := ci.CreateRun(ctx, n, "w.yml", draftOf(n, "w.yml", 1))
	require.NoError(t, err)

	for _, tc := range []struct {
		d  ports.CIRunDraft
		at time.Time
	}{{oldRun, created}, {newRun, created.Add(100 * 24 * time.Hour)}} {
		run, _ := ci.GetRun(ctx, n, tc.d.Run.ID)
		run.Status, run.Conclusion, run.CompletedAt = domain.CICompleted, domain.CISuccess, tc.at
		job, _ := ci.GetJob(ctx, run.ID, 1, tc.d.Jobs[0].ID)
		job.Phase, job.Conclusion = domain.CIPhaseEnded, domain.CISuccess
		_, err := ci.Apply(ctx, ports.CIWrite{Runs: []domain.CIRun{run}, Jobs: []domain.CIJob{job}})
		require.NoError(t, err)
	}

	ids, err := ci.Sweep(ctx, created.Add(90*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, []int64{oldRun.Run.ID}, ids)

	_, err = ci.GetRun(ctx, n, oldRun.Run.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = ci.GetRunSource(ctx, oldRun.Run.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	jobs, _ := ci.ListJobs(ctx, oldRun.Run.ID, 1)
	assert.Empty(t, jobs)
	_, err = ci.GetRun(ctx, n, newRun.Run.ID)
	assert.NoError(t, err)
	_, err = ci.GetRun(ctx, n, openRun.Run.ID)
	assert.NoError(t, err, "an open run is never swept")

	// Numbers are never reused.
	next, err := ci.CreateRun(ctx, n, "w.yml", draftOf(n, "w.yml", 0))
	require.NoError(t, err)
	assert.Equal(t, int64(4), next.Run.Number)
}

func TestCIRepositoryDeletionRemovesEverythingAndRestartsNumbers(t *testing.T) {
	ctx := context.Background()
	s, ci, n := ciRepo(t)
	d, err := ci.CreateRun(ctx, n, "w.yml", draftOf(n, "w.yml", 2))
	require.NoError(t, err)
	_, err = ci.CreateRunner(ctx, domain.CIRunner{ID: "r1", Name: "a", TokenHash: domain.HashTokenSecret("x")}, false)
	require.NoError(t, err)

	require.NoError(t, s.Delete(ctx, n))

	_, err = ci.GetRun(ctx, n, d.Run.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = ci.GetRunSource(ctx, d.Run.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	jobs, _ := ci.ListJobs(ctx, d.Run.ID, 1)
	assert.Empty(t, jobs)
	checks, _, _ := ci.ListChecks(ctx, n, d.Run.Sha, 10)
	assert.Empty(t, checks)
	ready, _ := ci.ListReadyJobs(ctx, 10)
	assert.Empty(t, ready)
	open, _ := ci.ListOpenRuns(ctx)
	assert.Empty(t, open)
	runners, _ := ci.ListRunners(ctx)
	assert.Len(t, runners, 1, "runners do not belong to a repository")

	// A repository created again under the name starts its run numbers at 1,
	// while ids are never reused.
	require.NoError(t, s.Create(ctx, domain.Repo{Name: n, Visibility: domain.VisibilityPrivate, CreatedAt: created}))
	again, err := ci.CreateRun(ctx, n, "w.yml", draftOf(n, "w.yml", 0))
	require.NoError(t, err)
	assert.Equal(t, int64(1), again.Run.Number)
	assert.Equal(t, int64(2), again.Run.ID)
}

func TestCIStateSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := kvstore.Open(ctx, dir)
	require.NoError(t, err)
	n := name(t, "proj")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: n, Visibility: domain.VisibilityPrivate, CreatedAt: created}))
	ci := s.CI()
	d, err := ci.CreateRun(ctx, n, "w.yml", draftOf(n, "w.yml", 1))
	require.NoError(t, err)
	j := d.Jobs[0]
	hash := domain.HashTokenSecret("ermitejob_z")
	j.Phase, j.TokenHash, j.RunnerID, j.RunnerName = domain.CIPhaseRunning, hash, "r1", "alpha"
	j.Steps = []domain.CIStep{{Number: 1, Name: "Set up job", State: domain.CIStepCompleted, Conclusion: domain.CISuccess, HasLines: true, LogStart: 0}}
	j.Outputs = map[string]string{"a": "b"}
	_, err = ci.Apply(ctx, ports.CIWrite{Jobs: []domain.CIJob{j}})
	require.NoError(t, err)
	require.NoError(t, s.Close(ctx))

	s2, err := kvstore.Open(ctx, dir)
	require.NoError(t, err)
	defer func() { _ = s2.Close(ctx) }()
	ci2 := s2.CI()
	got, err := ci2.FindJobByToken(ctx, hash, time.Time{})
	require.NoError(t, err)
	assert.Equal(t, domain.CIPhaseRunning, got.Phase)
	assert.Equal(t, "alpha", got.RunnerName)
	assert.Equal(t, map[string]string{"a": "b"}, got.Outputs)
	require.Len(t, got.Steps, 1)
	assert.True(t, got.Steps[0].HasLines)
	active, _ := ci2.ListActiveJobs(ctx)
	assert.Len(t, active, 1)
}

func TestCICancelReasonsRoundTrip(t *testing.T) {
	ctx := context.Background()
	_, ci, n := ciRepo(t)
	d, err := ci.CreateRun(ctx, n, "w.yml", draftOf(n, "w.yml", 1))
	require.NoError(t, err)
	for _, reason := range []domain.CICancelReason{domain.CICancelUser, domain.CICancelFailFast, domain.CICancelTimeout, domain.CICancelNone} {
		j, err := ci.GetJob(ctx, d.Run.ID, 1, d.Jobs[0].ID)
		require.NoError(t, err)
		j.CancelReason, j.CancelMessage, j.CancelRequestedAt = reason, "why", created
		_, err = ci.Apply(ctx, ports.CIWrite{Jobs: []domain.CIJob{j}})
		require.NoError(t, err)
		got, err := ci.GetJob(ctx, d.Run.ID, 1, d.Jobs[0].ID)
		require.NoError(t, err)
		assert.Equal(t, reason, got.CancelReason)
	}
}

// Run.Name round-trips, and a run stored without one (before the field existed)
// reads as its workflow name.
func TestCIRunNameRoundTripsAndDefaultsToTheWorkflowName(t *testing.T) {
	ctx := context.Background()
	_, ci, n := ciRepo(t)

	old, err := ci.CreateRun(ctx, n, ".github/workflows/ci.yml", draftOf(n, ".github/workflows/ci.yml", 0))
	require.NoError(t, err)
	got, err := ci.GetRun(ctx, n, old.Run.ID)
	require.NoError(t, err)
	assert.Equal(t, "ci", got.WorkflowName)
	assert.Equal(t, "ci", got.Name, "no stored name: the workflow name")

	named, err := ci.CreateRun(ctx, n, ".github/workflows/ci.yml", func(id, number int64) ports.CIRunDraft {
		d := draftOf(n, ".github/workflows/ci.yml", 0)(id, number)
		d.Run.Name = "run push on branch by someone"
		return d
	})
	require.NoError(t, err)
	got, err = ci.GetRun(ctx, n, named.Run.ID)
	require.NoError(t, err)
	assert.Equal(t, "run push on branch by someone", got.Name)
	assert.Equal(t, "ci", got.WorkflowName, "the workflow name is unchanged")
}
