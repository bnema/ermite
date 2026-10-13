// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

const otherJobSecret = "ermitejob_ZYXWVUTSRQPONMLKJIHGFEDCBAzyxwvutsrqponmlkj"

func warn(step int32, msg string) domain.CIAnnotation {
	return domain.CIAnnotation{Level: domain.AnnotationWarning, Step: step, Message: msg}
}

// UpdateJob replaces the annotations of the steps it names, in the batch of the
// job, also for a completed step and a step that is not in the call; a retry
// writes nothing.
func TestUpdateJobReplacesAnnotationsPerStep(t *testing.T) {
	t.Parallel()
	u := newCIUnit(t, nil)
	j := ciJob(t, domain.CIPhaseRunning, jobSecret)
	j.Steps = []domain.CIStep{{Number: 1, State: domain.CIStepCompleted, Conclusion: domain.CISuccess, StartedAt: now, CompletedAt: now}}
	u.store.EXPECT().FindJobByToken(mock.Anything, domain.HashTokenSecret(jobSecret), mock.Anything).Return(j, nil)
	u.store.EXPECT().GetJob(mock.Anything, j.RunID, j.RunAttempt, j.ID).Return(j, nil)
	u.store.EXPECT().JobAnnotations(mock.Anything, j.RunID, j.Attempt, j.ID).Return([]domain.CIAnnotation{warn(1, "old"), warn(5, "kept")}, nil).Once()
	var written ports.CIWrite
	u.store.EXPECT().Apply(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, w ports.CIWrite) (uint64, error) {
		written = w
		return 9, nil
	}).Once()
	u.meta.EXPECT().Get(mock.Anything, j.Repo).Return(domain.Repo{Name: j.Repo}, nil)
	u.logs.EXPECT().Count(mock.Anything, mock.Anything).Return(0, nil)
	u.bus.EXPECT().Publish(mock.Anything, mock.Anything).Return()

	// Step 1 is completed: its annotations are replaced, the step is not touched.
	completed := ports.StepUpdate{Number: 1, State: domain.CIStepCompleted, Conclusion: domain.CISuccess, StartedAt: now, CompletedAt: now}
	require.NoError(t, u.ci.UpdateJob(context.Background(), jobSecret, jobIDOne, []ports.StepUpdate{completed},
		[]domain.CIAnnotation{warn(1, "new"), warn(1, "new too"), warn(9, "a step not in the call")}))

	require.Len(t, written.Annotations, 2, "step 5 is not named: it is kept")
	assert.EqualValues(t, 1, written.Annotations[0].Step)
	assert.Equal(t, []string{"new", "new too"}, []string{written.Annotations[0].Items[0].Message, written.Annotations[0].Items[1].Message})
	assert.EqualValues(t, 9, written.Annotations[1].Step)
	assert.Equal(t, ports.CILogRef{RunID: j.RunID, JobID: j.ID, Attempt: j.Attempt}, written.Annotations[0].Exec, "keyed by the attempt the job executes in")
	assert.Equal(t, j.Repo, written.Annotations[0].Repo)
	require.Len(t, written.Jobs, 1, "annotations travel with the job record")
}

func TestUpdateJobRetryWritesNothing(t *testing.T) {
	t.Parallel()
	u := newCIUnit(t, nil)
	j := ciJob(t, domain.CIPhaseRunning, jobSecret)
	u.store.EXPECT().FindJobByToken(mock.Anything, mock.Anything, mock.Anything).Return(j, nil)
	u.store.EXPECT().GetJob(mock.Anything, j.RunID, j.RunAttempt, j.ID).Return(j, nil)
	u.store.EXPECT().JobAnnotations(mock.Anything, j.RunID, j.Attempt, j.ID).Return([]domain.CIAnnotation{warn(2, "same")}, nil)
	// No Apply is expected: the mock fails the test on one.
	require.NoError(t, u.ci.UpdateJob(context.Background(), jobSecret, jobIDOne, nil, []domain.CIAnnotation{warn(2, "same")}))
}

// A call that breaks a limit is refused before anything is read or written.
func TestUpdateJobAnnotationLimitsStopBeforeTheStore(t *testing.T) {
	t.Parallel()
	u := newCIUnit(t, nil)
	j := ciJob(t, domain.CIPhaseRunning, jobSecret)
	u.store.EXPECT().FindJobByToken(mock.Anything, mock.Anything, mock.Anything).Return(j, nil)
	eleven := make([]domain.CIAnnotation, 11)
	for i := range eleven {
		eleven[i] = warn(3, "m")
	}
	for name, anns := range map[string][]domain.CIAnnotation{
		"eleven":     eleven,
		"too long":   {warn(1, strings.Repeat("x", 4097))},
		"no message": {warn(1, "")},
	} {
		err := u.ci.UpdateJob(context.Background(), jobSecret, jobIDOne, nil, anns)
		assert.ErrorIs(t, err, domain.ErrInvalid, name)
	}
}

func TestResultsAreRefusedWithAnotherJobsToken(t *testing.T) {
	t.Parallel()
	u := newCIUnit(t, nil)
	other := ciJob(t, domain.CIPhaseRunning, otherJobSecret)
	other.ID = "7-1-0"
	u.store.EXPECT().FindJobByToken(mock.Anything, domain.HashTokenSecret(otherJobSecret), mock.Anything).Return(other, nil)
	ctx := context.Background()

	err := u.ci.UpdateJob(ctx, otherJobSecret, jobIDOne, nil, []domain.CIAnnotation{warn(1, "x")})
	assert.ErrorIs(t, err, domain.ErrForbidden)
	err = u.ci.UploadSummary(ctx, otherJobSecret, jobIDOne, 1, "x")
	assert.ErrorIs(t, err, domain.ErrForbidden)
	_, err = u.ci.CompleteJob(ctx, otherJobSecret, ports.CompleteJobParams{JobID: jobIDOne, Conclusion: domain.CISuccess, Annotations: []domain.CIAnnotation{warn(1, "x")}})
	assert.ErrorIs(t, err, domain.ErrForbidden)

	// Not a job token at all.
	for _, secret := range []string{"", "ermiterun_" + strings.Repeat("a", 43), "ermite_" + strings.Repeat("a", 43)} {
		assert.ErrorIs(t, u.ci.UploadSummary(ctx, secret, jobIDOne, 1, "x"), domain.ErrUnauthenticated, secret)
	}
}

func TestEndedJobsRejectAnnotationsAndSummaries(t *testing.T) {
	t.Parallel()
	u := newCIUnit(t, nil)
	j := ciJob(t, domain.CIPhaseEnded, jobSecret)
	j.Conclusion, j.Error = domain.CICancelled, "stopped"
	u.store.EXPECT().FindJobByToken(mock.Anything, mock.Anything, mock.Anything).Return(j, nil)
	ctx := context.Background()

	for name, err := range map[string]error{
		"UpdateJob":     u.ci.UpdateJob(ctx, jobSecret, jobIDOne, nil, []domain.CIAnnotation{warn(1, "x")}),
		"UploadSummary": u.ci.UploadSummary(ctx, jobSecret, jobIDOne, 1, "x"),
	} {
		ended, ok := domain.IsJobEnded(err)
		require.True(t, ok, name)
		assert.Equal(t, domain.CICancelled, ended.Conclusion, name)
		assert.Equal(t, "stopped", ended.Message, name)
	}

	// CompleteJob on an ended job returns the stored result and stores nothing,
	// whatever the annotations: the mocks expect no read and no write.
	res, err := u.ci.CompleteJob(ctx, jobSecret, ports.CompleteJobParams{
		JobID: jobIDOne, Conclusion: domain.CISuccess, Annotations: []domain.CIAnnotation{warn(1, "ignored")},
	})
	require.NoError(t, err)
	assert.False(t, res.Applied)
	assert.Equal(t, domain.CICancelled, res.Conclusion)
}

func TestUploadSummaryRules(t *testing.T) {
	t.Parallel()
	u := newCIUnit(t, nil)
	j := ciJob(t, domain.CIPhaseRunning, jobSecret)
	u.store.EXPECT().FindJobByToken(mock.Anything, mock.Anything, mock.Anything).Return(j, nil)
	u.store.EXPECT().GetJob(mock.Anything, j.RunID, j.RunAttempt, j.ID).Return(j, nil)
	ctx := context.Background()
	ref := ports.CILogRef{RunID: j.RunID, JobID: j.ID, Attempt: j.Attempt}

	for name, tc := range map[string]struct {
		step int32
		md   string
	}{
		"step 0":           {0, "x"},
		"step 1001":        {1001, "x"},
		"1 MiB and a byte": {1, strings.Repeat("x", 1<<20+1)},
		"invalid UTF-8":    {1, "a\xffb"},
		"negative":         {-1, "x"},
	} {
		assert.ErrorIs(t, u.ci.UploadSummary(ctx, jobSecret, jobIDOne, tc.step, tc.md), domain.ErrInvalid, name)
	}

	max := strings.Repeat("x", 1<<20)
	u.sums.EXPECT().Put(mock.Anything, ref, int32(1), max).Return(nil).Once()
	u.sums.EXPECT().Put(mock.Anything, ref, int32(1000), "").Return(nil).Once()
	require.NoError(t, u.ci.UploadSummary(ctx, jobSecret, jobIDOne, 1, max), "exactly 1 MiB")
	require.NoError(t, u.ci.UploadSummary(ctx, jobSecret, jobIDOne, 1000, ""), "empty removes")
}

// The summaries are read through the same access as GetJob, per attempt of the
// job, at most 20.
func TestGetStepSummariesAndListAnnotations(t *testing.T) {
	t.Parallel()
	u := newCIUnit(t, nil)
	repo := name(t, "proj")
	j := ciJob(t, domain.CIPhaseEnded, jobSecret)
	j.Attempt, j.RunAttempt = 1, 2 // kept from attempt 1 by a re-run
	u.meta.EXPECT().Get(mock.Anything, repo).Return(domain.Repo{Name: repo, Visibility: domain.VisibilityPublic}, nil)
	u.store.EXPECT().GetRun(mock.Anything, repo, int64(7)).Return(domain.CIRun{Repo: repo, ID: 7, Attempt: 2}, nil)
	u.store.EXPECT().GetJob(mock.Anything, int64(7), int32(2), jobIDOne).Return(j, nil)
	u.store.EXPECT().ListJobs(mock.Anything, int64(7), int32(2)).Return([]domain.CIJob{j}, nil)
	u.store.EXPECT().JobAnnotations(mock.Anything, int64(7), int32(1), jobIDOne).Return([]domain.CIAnnotation{warn(1, "from attempt 1")}, nil)
	u.sums.EXPECT().List(mock.Anything, ports.CILogRef{RunID: 7, JobID: jobIDOne, Attempt: 1}, domain.CIMaxListedSummaries).
		Return([]domain.CIStepSummary{{Step: 3, Markdown: "s"}}, true, nil)
	ctx := context.Background()

	anns, err := u.ci.ListAnnotations(ctx, domain.Anonymous(), repo, 7, 0, "")
	require.NoError(t, err)
	require.Len(t, anns.Annotations, 1)
	assert.Equal(t, jobIDOne, anns.Annotations[0].JobID)
	assert.False(t, anns.Truncated)

	sums, err := u.ci.GetStepSummaries(ctx, domain.Anonymous(), repo, jobIDOne, 0)
	require.NoError(t, err)
	assert.True(t, sums.Truncated)
	assert.Equal(t, []domain.CIStepSummary{{Step: 3, Markdown: "s"}}, sums.Summaries)

	_, err = u.ci.ListAnnotations(ctx, domain.Anonymous(), repo, 7, 3, "")
	assert.ErrorIs(t, err, domain.ErrNotFound, "an attempt that does not exist")
	_, err = u.ci.ListAnnotations(ctx, domain.Anonymous(), repo, 7, 0, "7-9-0")
	assert.ErrorIs(t, err, domain.ErrNotFound, "a job that is not of the run")
}

func TestListAnnotationsIsBoundedAndTruncated(t *testing.T) {
	t.Parallel()
	u := newCIUnit(t, nil)
	repo := name(t, "proj")
	j := ciJob(t, domain.CIPhaseEnded, jobSecret)
	u.meta.EXPECT().Get(mock.Anything, repo).Return(domain.Repo{Name: repo, Visibility: domain.VisibilityPublic}, nil)
	u.store.EXPECT().GetRun(mock.Anything, repo, int64(7)).Return(domain.CIRun{Repo: repo, ID: 7, Attempt: 1}, nil)
	u.store.EXPECT().ListJobs(mock.Anything, int64(7), int32(1)).Return([]domain.CIJob{j}, nil)
	many := make([]domain.CIAnnotation, 1001)
	for i := range many {
		many[i] = warn(int32(i%1000)+1, "m")
	}
	u.store.EXPECT().JobAnnotations(mock.Anything, int64(7), int32(1), jobIDOne).Return(many, nil)
	got, err := u.ci.ListAnnotations(context.Background(), domain.Anonymous(), repo, 7, 0, "")
	require.NoError(t, err)
	assert.Len(t, got.Annotations, 1000)
	assert.True(t, got.Truncated)
}

// A private repository is not readable by an anonymous caller: not found, as
// for GetRun.
func TestResultsReadsFollowTheAccessOfTheRepository(t *testing.T) {
	t.Parallel()
	u := newCIUnit(t, nil)
	repo := name(t, "proj")
	u.meta.EXPECT().Get(mock.Anything, repo).Return(domain.Repo{Name: repo, Visibility: domain.VisibilityPrivate}, nil)
	_, err := u.ci.ListAnnotations(context.Background(), domain.Anonymous(), repo, 7, 0, "")
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = u.ci.GetStepSummaries(context.Background(), domain.Anonymous(), repo, jobIDOne, 0)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}
