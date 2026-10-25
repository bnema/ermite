// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"errors"
	"strings"
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

// jobsRig is a Bernard over a mocked deps store and a mocked DepsRunner, with no
// ecosystem and no git: it only settles records.
func jobsRig(t *testing.T) (*bernardRig, *mocks.DepsRunner) {
	t.Helper()
	r := newBernardRig(t, "0 6 * * *")
	runs := mocks.NewDepsRunner(t)
	b, err := usecase.NewBernard(usecase.BernardConfig{
		Store: r.store, Meta: r.meta, Reader: r.reader, Registry: func() ports.DepsRegistry { return r.reg },
		Bus: r.bus, Clock: r.clock, CheckAt: "0 6 * * *", Runs: runs, RunnerLabel: "ubuntu-latest", Tick: time.Hour,
	})
	require.NoError(t, err)
	r.b = b
	return r, runs
}

const jobsRunID = 31

func applying(r *bernardRig) {
	r.rec.State, r.rec.RunID = domain.DepsApplying, jobsRunID
	r.rec.RunUpdates = []domain.DepUpdate{{DepRef: oneRef[0], Current: "v1.0.0", Kind: domain.DepMinor}}
}

var oneRef = []domain.DepRef{{Ecosystem: domain.EcosystemGoMod, Name: "x/y", Version: "v1.2.3"}}

func endedJob(r *bernardRig, conclusion domain.CIConclusion, msg string) (domain.CIRun, domain.CIJob) {
	run := domain.CIRun{Repo: r.repo, ID: jobsRunID, Sha: strings.Repeat("a", 40), Event: domain.CIEventDependencies, Status: domain.CICompleted, Conclusion: conclusion}
	return run, domain.CIJob{
		ID: domain.NewJobID(jobsRunID, 0, 0), RunID: jobsRunID, Repo: r.repo, Phase: domain.CIPhaseEnded, Conclusion: conclusion, Error: msg,
		Deps: &domain.CIDepsJob{Updates: oneRef},
	}
}

func TestBernardDepsJobEndedFailsWithAOneLineReason(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name       string
		conclusion domain.CIConclusion
		jobError   string
		report     *ports.DepsReport
		want       string
	}{
		{"report error", domain.CIFailure, "", &ports.DepsReport{Error: "go get: no such version\nstack\nmore"}, "go get: no such version"},
		{"report error wins over a success", domain.CISuccess, "", &ports.DepsReport{Error: "tidy failed"}, "tidy failed"},
		{"cancelled", domain.CICancelled, "Canceled by owner", nil, "the run ended: cancelled: Canceled by owner"},
		{"timed out", domain.CITimedOut, "The job exceeded its timeout of 10 minutes", nil, "the run ended: timed_out: The job exceeded its timeout of 10 minutes"},
		{"lost runner", domain.CIFailure, "The runner alpha lost contact\nsecond line", nil, "the run ended: failure: The runner alpha lost contact"},
		{"failure with no message", domain.CIFailure, "", &ports.DepsReport{}, "the run ended: failure"},
		{"success without a report", domain.CISuccess, "", nil, "the runner sent no report"},
		{"unappliable report", domain.CISuccess, "", &ports.DepsReport{}, "failed precondition: no Go module ecosystem"},
		{"long reason", domain.CIFailure, "", &ports.DepsReport{Error: strings.Repeat("é", 4000)}, strings.Repeat("é", 1024)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r, _ := jobsRig(t)
			applying(r)
			run, job := endedJob(r, c.conclusion, c.jobError)
			r.b.DepsJobEnded(context.Background(), run, job, c.report)

			assert.Equal(t, domain.DepsFailed, r.rec.State)
			assert.Equal(t, c.want, r.rec.Error)
			assert.NotContains(t, r.rec.Error, "\n")
			assert.EqualValues(t, jobsRunID, r.rec.RunID, "the record keeps the link to the run")
			assert.Empty(t, r.rec.RunUpdates)
			assert.Equal(t, 1, r.changedCount(), "the Inbox is told")
		})
	}
}

func TestBernardDepsJobEndedFromASupersededRunWritesNothing(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		move func(*domain.RepoDeps)
	}{
		{"idle after the merge request was closed", func(d *domain.RepoDeps) { d.State = domain.DepsIdle }},
		{"another run", func(d *domain.RepoDeps) { d.RunID = jobsRunID + 1 }},
		{"failed already", func(d *domain.RepoDeps) { d.State, d.Error = domain.DepsFailed, "first" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r, _ := jobsRig(t)
			applying(r)
			c.move(&r.rec)
			before := r.rec
			for _, conclusion := range []domain.CIConclusion{domain.CICancelled, domain.CIFailure, domain.CISuccess} {
				run, job := endedJob(r, conclusion, "x")
				r.b.DepsJobEnded(context.Background(), run, job, &ports.DepsReport{Error: "late"})
			}
			assert.Equal(t, before, r.rec)
			assert.Zero(t, r.changedCount())
		})
	}
}

func TestBernardDepsJobEndedSurvivesAStoreFailure(t *testing.T) {
	t.Parallel()
	r, _ := jobsRig(t)
	r.store.ExpectedCalls = nil
	r.store.EXPECT().Get(mock.Anything, r.repo).Return(domain.RepoDeps{}, errors.New("disk")).Once()
	run, job := endedJob(r, domain.CIFailure, "")
	r.b.DepsJobEnded(context.Background(), run, job, nil) // logged, no panic, no write
}

// Start settles what a previous process left applying: the run is the truth.
func TestBernardStartReconcilesApplyingRecords(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		run    domain.CIRun
		getErr error
		want   string // "" = untouched
	}{
		{"open run is left alone", domain.CIRun{ID: jobsRunID, Status: domain.CIInProgress}, nil, ""},
		{"queued run is left alone", domain.CIRun{ID: jobsRunID, Status: domain.CIQueued}, nil, ""},
		{"completed with success", domain.CIRun{ID: jobsRunID, Status: domain.CICompleted, Conclusion: domain.CISuccess}, nil, "the server restarted before the result was applied"},
		{"completed with failure", domain.CIRun{ID: jobsRunID, Status: domain.CICompleted, Conclusion: domain.CIFailure}, nil, "the run ended: failure"},
		{"completed cancelled", domain.CIRun{ID: jobsRunID, Status: domain.CICompleted, Conclusion: domain.CICancelled}, nil, "the run ended: cancelled"},
		{"run swept", domain.CIRun{}, domain.ErrNotFound, "the run no longer exists"},
		{"read error leaves it for the next start", domain.CIRun{}, errors.New("disk"), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r, runs := jobsRig(t)
			applying(r)
			before := r.rec
			runs.EXPECT().GetDepsRun(mock.Anything, r.repo, int64(jobsRunID)).Return(c.run, c.getErr).Once()
			r.store.EXPECT().List(mock.Anything).Return([]domain.RepoDeps{r.rec, {Repo: name(t, "idle")}}, nil)
			r.bus.EXPECT().Subscribe(mock.Anything).Return(make(<-chan domain.Event)).Maybe()
			r.b.Start()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r.b.Close(ctx)
			if c.want == "" {
				assert.Equal(t, before, r.rec)
				return
			}
			assert.Equal(t, domain.DepsFailed, r.rec.State)
			assert.Equal(t, c.want, r.rec.Error)
			assert.EqualValues(t, jobsRunID, r.rec.RunID)
		})
	}
}

func TestBernardStartFailsAnApplyingRecordWithoutARun(t *testing.T) {
	t.Parallel()
	r, _ := jobsRig(t)
	r.rec.State = domain.DepsApplying // a record of no run: GetDepsRun is not called
	r.store.EXPECT().List(mock.Anything).Return([]domain.RepoDeps{r.rec}, nil)
	r.bus.EXPECT().Subscribe(mock.Anything).Return(make(<-chan domain.Event)).Maybe()
	r.b.Start()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.b.Close(ctx)
	assert.Equal(t, domain.DepsFailed, r.rec.State)
	assert.Equal(t, "the run no longer exists", r.rec.Error)
}

// TestBernardScheduledCheckHasADeadline: each repository of a scheduled scan
// is checked under the 60 s deadline of a manual check.
func TestBernardScheduledCheckHasADeadline(t *testing.T) {
	r, _ := jobsRig(t)
	r.meta.EXPECT().List(mock.Anything).Return([]domain.Repo{{Name: r.repo}}, nil)
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil)
	var left time.Duration
	r.reader.EXPECT().ResolveRevision(mock.Anything, r.repo, domain.Revision{}).RunAndReturn(
		func(ctx context.Context, _ domain.RepoName, _ domain.Revision) (domain.ResolvedRevision, error) {
			dl, ok := ctx.Deadline()
			require.True(t, ok, "no deadline")
			left = time.Until(dl)
			return domain.ResolvedRevision{}, domain.ErrNotFound
		})
	r.b.ScheduleNow(context.Background())
	assert.Positive(t, left)
	assert.LessOrEqual(t, left, 60*time.Second)
}
