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

func note(level domain.AnnotationLevel, step int32, msg string) domain.CIAnnotation {
	return domain.CIAnnotation{Level: level, Message: msg, Step: step}
}

func replace(n domain.RepoName, run int64, attempt int32, job string, step int32, items ...domain.CIAnnotation) ports.CIStepAnnotations {
	return ports.CIStepAnnotations{Repo: n, Exec: ports.CILogRef{RunID: run, JobID: job, Attempt: attempt}, Step: step, Items: items}
}

// Annotations are kept per execution of a job and per step: a report replaces
// the annotations of the steps it names, keeps the others, and an empty report
// removes the step's.
func TestCIAnnotationsAreReplacedPerStep(t *testing.T) {
	ctx := context.Background()
	_, ci, n := ciRepo(t)
	d, err := ci.CreateRun(ctx, n, "w.yml", draftOf(n, "w.yml", 2))
	require.NoError(t, err)
	a, b := d.Jobs[0].ID, d.Jobs[1].ID

	full := domain.CIAnnotation{
		Level: domain.AnnotationFailure, Title: "t", Message: "boom", File: "a/b.go",
		StartLine: 3, EndLine: 5, StartColumn: 2, EndColumn: 9, Step: 2,
	}
	_, err = ci.Apply(ctx, ports.CIWrite{Annotations: []ports.CIStepAnnotations{
		replace(n, d.Run.ID, 1, a, 2, full, note(domain.AnnotationNotice, 2, "second")),
		replace(n, d.Run.ID, 1, a, 10, note(domain.AnnotationWarning, 10, "late step")),
		replace(n, d.Run.ID, 1, b, 2, note(domain.AnnotationNotice, 2, "other job")),
	}})
	require.NoError(t, err)

	got, err := ci.JobAnnotations(ctx, d.Run.ID, 1, a)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, full, got[0], "every field round-trips, in the order the step produced them")
	assert.Equal(t, "second", got[1].Message)
	assert.EqualValues(t, 10, got[2].Step, "steps sort numerically, not as text")

	// Replace step 2 only: step 10 and the other job keep theirs.
	_, err = ci.Apply(ctx, ports.CIWrite{Annotations: []ports.CIStepAnnotations{
		replace(n, d.Run.ID, 1, a, 2, note(domain.AnnotationWarning, 2, "only one now")),
	}})
	require.NoError(t, err)
	got, _ = ci.JobAnnotations(ctx, d.Run.ID, 1, a)
	require.Len(t, got, 2)
	assert.Equal(t, "only one now", got[0].Message)
	assert.Equal(t, "late step", got[1].Message)
	other, _ := ci.JobAnnotations(ctx, d.Run.ID, 1, b)
	require.Len(t, other, 1)

	// An empty list removes the step's annotations.
	_, err = ci.Apply(ctx, ports.CIWrite{Annotations: []ports.CIStepAnnotations{replace(n, d.Run.ID, 1, a, 2)}})
	require.NoError(t, err)
	got, _ = ci.JobAnnotations(ctx, d.Run.ID, 1, a)
	require.Len(t, got, 1)
	assert.EqualValues(t, 10, got[0].Step)

	// Another attempt of the job has its own.
	none, err := ci.JobAnnotations(ctx, d.Run.ID, 2, a)
	require.NoError(t, err)
	assert.Empty(t, none)
}

// The annotations are written in the batch of the job: a stale job version
// writes none of them, and a missing repository neither.
func TestCIAnnotationsAreAtomicWithTheJob(t *testing.T) {
	ctx := context.Background()
	_, ci, n := ciRepo(t)
	d, err := ci.CreateRun(ctx, n, "w.yml", draftOf(n, "w.yml", 1))
	require.NoError(t, err)
	job, err := ci.GetJob(ctx, d.Run.ID, 1, d.Jobs[0].ID)
	require.NoError(t, err)

	stale := job
	stale.Version--
	_, err = ci.Apply(ctx, ports.CIWrite{Jobs: []domain.CIJob{stale}, Annotations: []ports.CIStepAnnotations{
		replace(n, d.Run.ID, 1, job.ID, 1, note(domain.AnnotationNotice, 1, "lost")),
	}})
	assert.ErrorIs(t, err, domain.ErrConflict)
	got, _ := ci.JobAnnotations(ctx, d.Run.ID, 1, job.ID)
	assert.Empty(t, got)

	ghost := name(t, "ghost")
	_, err = ci.Apply(ctx, ports.CIWrite{Annotations: []ports.CIStepAnnotations{
		replace(ghost, d.Run.ID, 1, job.ID, 1, note(domain.AnnotationNotice, 1, "no repository")),
	}})
	assert.ErrorIs(t, err, domain.ErrNotFound, "no key is created under a repository that is gone")
}

// Annotations go with their run: the retention sweep, the deletion of the
// repository, and the clearing of an assignment that is taken back.
func TestCIAnnotationsAreDeleted(t *testing.T) {
	ctx := context.Background()
	s, ci, n := ciRepo(t)
	mk := func() ports.CIRunDraft {
		d, err := ci.CreateRun(ctx, n, "w.yml", draftOf(n, "w.yml", 1))
		require.NoError(t, err)
		_, err = ci.Apply(ctx, ports.CIWrite{Annotations: []ports.CIStepAnnotations{
			replace(n, d.Run.ID, 1, d.Jobs[0].ID, 1, note(domain.AnnotationNotice, 1, "x")),
			replace(n, d.Run.ID, 1, d.Jobs[0].ID, 2, note(domain.AnnotationNotice, 2, "y")),
		}})
		require.NoError(t, err)
		return d
	}
	count := func(d ports.CIRunDraft) int {
		got, err := ci.JobAnnotations(ctx, d.Run.ID, 1, d.Jobs[0].ID)
		require.NoError(t, err)
		return len(got)
	}

	// clear
	d := mk()
	_, err := ci.Apply(ctx, ports.CIWrite{ClearAnnotations: []ports.CILogRef{{RunID: d.Run.ID, JobID: d.Jobs[0].ID, Attempt: 1}}})
	require.NoError(t, err)
	assert.Zero(t, count(d))

	// sweep: an old completed run goes with its annotations, a recent one keeps them.
	oldRun, recent := mk(), mk()
	for _, tc := range []struct {
		d  ports.CIRunDraft
		at time.Time
	}{{oldRun, created}, {recent, created.Add(100 * 24 * time.Hour)}} {
		run, _ := ci.GetRun(ctx, n, tc.d.Run.ID)
		run.Status, run.Conclusion, run.CompletedAt = domain.CICompleted, domain.CISuccess, tc.at
		job, _ := ci.GetJob(ctx, run.ID, 1, tc.d.Jobs[0].ID)
		job.Phase, job.Conclusion = domain.CIPhaseEnded, domain.CISuccess
		_, err := ci.Apply(ctx, ports.CIWrite{Runs: []domain.CIRun{run}, Jobs: []domain.CIJob{job}})
		require.NoError(t, err)
	}
	ids, err := ci.Sweep(ctx, created.Add(90*24*time.Hour))
	require.NoError(t, err)
	assert.Contains(t, ids, oldRun.Run.ID)
	assert.Zero(t, count(oldRun))
	assert.Equal(t, 2, count(recent))

	// repository deletion
	require.NoError(t, s.Delete(ctx, n))
	assert.Zero(t, count(recent))
}

func TestCIStepOutcomeRoundTrips(t *testing.T) {
	ctx := context.Background()
	_, ci, n := ciRepo(t)
	d, err := ci.CreateRun(ctx, n, "w.yml", draftOf(n, "w.yml", 1))
	require.NoError(t, err)
	job, _ := ci.GetJob(ctx, d.Run.ID, 1, d.Jobs[0].ID)
	job.Steps = []domain.CIStep{
		{Number: 1, State: domain.CIStepCompleted, Conclusion: domain.CISuccess, Outcome: domain.CIFailure, StartedAt: created, CompletedAt: created},
		{Number: 2, State: domain.CIStepCompleted, Conclusion: domain.CISuccess, StartedAt: created, CompletedAt: created},
	}
	_, err = ci.Apply(ctx, ports.CIWrite{Jobs: []domain.CIJob{job}})
	require.NoError(t, err)
	got, err := ci.GetJob(ctx, d.Run.ID, 1, job.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.CIFailure, got.Steps[0].Outcome, "continue-on-error: failed, then succeeded")
	assert.Equal(t, domain.CINoConclusion, got.Steps[1].Outcome, "stored without one: read as the conclusion")
}
