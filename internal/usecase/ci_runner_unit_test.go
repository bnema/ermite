// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bnema/zerowrap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

// ciUnit is the CI use cases over mocks, with a clock the test moves.
type ciUnit struct {
	store *mocks.CIStore
	logs  *mocks.CILogStore
	meta  *mocks.RepoMetaStore
	bus   *mocks.EventBus
	clock *fakeClock
	ci    *usecase.CI
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

func newCIUnit(t *testing.T, tweak func(*usecase.CIConfig)) *ciUnit {
	t.Helper()
	u := &ciUnit{
		store: mocks.NewCIStore(t), logs: mocks.NewCILogStore(t), meta: mocks.NewRepoMetaStore(t),
		bus: mocks.NewEventBus(t), clock: &fakeClock{t: now},
	}
	cfg := usecase.CIConfig{
		Store: u.store, Logs: u.logs, Meta: u.meta, Repos: mocks.NewGitRepos(t), Reader: mocks.NewRepoReader(t),
		Git: mocks.NewMergeGit(t), Diff: mocks.NewCIGit(t), Bus: u.bus, Clock: u.clock, Owner: "owner", PublicURL: "https://forge.test",
	}
	if tweak != nil {
		tweak(&cfg)
	}
	u.ci = usecase.NewCI(context.Background(), cfg)
	t.Cleanup(func() { u.ci.Close(context.Background()) })
	return u
}

const (
	jobSecret = "ermitejob_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN-_0"
	jobIDOne  = "7-0-0"
)

func ciJob(t *testing.T, phase domain.CIJobPhase, secret string) domain.CIJob {
	t.Helper()
	return domain.CIJob{
		ID: jobIDOne, RunID: 7, Attempt: 1, RunAttempt: 1, Repo: name(t, "proj"), Key: "build", Name: "build",
		Phase: phase, TokenHash: domain.HashTokenSecret(secret), RunnerID: "r1", RunnerName: "alpha", Planned: true,
	}
}

// expectToken makes the token lookup find j.
func (u *ciUnit) expectToken(secret string, j domain.CIJob) {
	u.store.EXPECT().FindJobByToken(mock.Anything, domain.HashTokenSecret(secret), mock.Anything).Return(j, nil)
}

// S4: a call whose token was replaced between the lookup and the lock (the
// assignment was cancelled and given again) is unauthenticated, for every
// call that writes the job.
func TestStaleAssignmentIsUnauthenticatedUnderTheLock(t *testing.T) {
	t.Parallel()
	replaced := func(j domain.CIJob) domain.CIJob {
		j.TokenHash = domain.HashTokenSecret("ermitejob_replaced")
		return j
	}
	calls := map[string]func(u *ciUnit) error{
		"UpdateJob": func(u *ciUnit) error {
			return u.ci.UpdateJob(context.Background(), jobSecret, jobIDOne, []ports.StepUpdate{{
				Number: 1, State: domain.CIStepInProgress, StartedAt: now,
			}})
		},
		"AppendLog": func(u *ciUnit) error {
			_, err := u.ci.AppendLog(context.Background(), jobSecret, jobIDOne, 1, 0, []domain.CILogLine{{Time: now, Text: "x"}})
			return err
		},
	}
	for label, call := range calls {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			u := newCIUnit(t, nil)
			j := ciJob(t, domain.CIPhaseRunning, jobSecret)
			u.expectToken(jobSecret, j)
			u.store.EXPECT().GetJob(mock.Anything, j.RunID, j.RunAttempt, j.ID).Return(replaced(j), nil)
			// No write: the stores and the log expect nothing else.
			assert.ErrorIs(t, call(u), domain.ErrUnauthenticated)
		})
	}

	t.Run("CompleteJob", func(t *testing.T) {
		t.Parallel()
		u := newCIUnit(t, nil)
		j := ciJob(t, domain.CIPhaseRunning, jobSecret)
		u.expectToken(jobSecret, j)
		u.store.EXPECT().GetRunSource(mock.Anything, j.RunID).Return(domain.CIRunSource{RunID: j.RunID, Repo: j.Repo}, nil)
		u.store.EXPECT().GetRun(mock.Anything, j.Repo, j.RunID).Return(domain.CIRun{Repo: j.Repo, ID: j.RunID, Attempt: 1}, nil)
		u.store.EXPECT().ListJobs(mock.Anything, j.RunID, int32(1)).Return([]domain.CIJob{replaced(j)}, nil)
		_, err := u.ci.CompleteJob(context.Background(), jobSecret, ports.CompleteJobParams{JobID: j.ID, Conclusion: domain.CISuccess})
		assert.ErrorIs(t, err, domain.ErrUnauthenticated)
	})

	t.Run("an ended job answers with its end, not unauthenticated", func(t *testing.T) {
		t.Parallel()
		u := newCIUnit(t, nil)
		j := ciJob(t, domain.CIPhaseRunning, jobSecret)
		ended := replaced(j)
		ended.Phase, ended.Conclusion = domain.CIPhaseEnded, domain.CICancelled
		u.expectToken(jobSecret, j)
		u.store.EXPECT().GetJob(mock.Anything, j.RunID, j.RunAttempt, j.ID).Return(ended, nil)
		err := u.ci.UpdateJob(context.Background(), jobSecret, jobIDOne, []ports.StepUpdate{{Number: 1, State: domain.CIStepInProgress, StartedAt: now}})
		got, ok := domain.IsJobEnded(err)
		require.True(t, ok, "%v", err)
		assert.Equal(t, domain.CICancelled, got.Conclusion)
	})
}

// S5: the job vanished while the log limit was handled: a plain not found, never
// a typed nil error.
func TestLogLimitOnAVanishedJobIsNotFound(t *testing.T) {
	t.Parallel()
	u := newCIUnit(t, nil)
	j := ciJob(t, domain.CIPhaseRunning, jobSecret)
	u.expectToken(jobSecret, j)
	// UpdateJob-style lookup under lock, then a log at the limit.
	u.store.EXPECT().GetJob(mock.Anything, j.RunID, j.RunAttempt, j.ID).Return(j, nil)
	u.logs.EXPECT().Count(mock.Anything, mock.Anything).Return(0, nil)
	u.logs.EXPECT().Append(mock.Anything, mock.Anything, int64(0), mock.Anything, int64(domain.CIMaxLogBytes)).Return(0, 0, ports.ErrCILogLimit)
	// stopForLogLimit loads the run, and the job is no longer in it.
	u.store.EXPECT().GetRunSource(mock.Anything, j.RunID).Return(domain.CIRunSource{RunID: j.RunID, Repo: j.Repo}, nil)
	u.store.EXPECT().GetRun(mock.Anything, j.Repo, j.RunID).Return(domain.CIRun{Repo: j.Repo, ID: j.RunID, Attempt: 1}, nil)
	u.store.EXPECT().ListJobs(mock.Anything, j.RunID, int32(1)).Return(nil, nil)

	_, err := u.ci.AppendLog(context.Background(), jobSecret, jobIDOne, 1, 0, []domain.CILogLine{{Time: now, Text: "x"}})
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, isEnded := domain.IsJobEnded(err)
	assert.False(t, isEnded)
}

// expectRunJob makes GetJobLog find the job of the repository (a public one).
func (u *ciUnit) expectLogJob(j domain.CIJob) {
	u.meta.EXPECT().Get(mock.Anything, j.Repo).Return(domain.Repo{Name: j.Repo, Visibility: domain.VisibilityPublic}, nil)
	u.store.EXPECT().GetRun(mock.Anything, j.Repo, j.RunID).Return(domain.CIRun{Repo: j.Repo, ID: j.RunID, Attempt: 1}, nil)
	u.store.EXPECT().GetJob(mock.Anything, j.RunID, j.RunAttempt, j.ID).Return(j, nil)
}

// S7: without follow a missing log is not found and an offset past the end is
// out of range; with follow a running job's reader waits for the lines, and a
// job that ended answers as without follow (follow changes nothing then).
func TestGetJobLogFollowAndOutOfRange(t *testing.T) {
	t.Parallel()
	p := domain.Anonymous()
	params := func(follow bool, offset int64) ports.GetJobLogParams {
		return ports.GetJobLogParams{JobID: jobIDOne, Offset: offset, Follow: follow}
	}
	send := func(ports.LogBatch) error { return nil }

	t.Run("no follow: out of range", func(t *testing.T) {
		t.Parallel()
		u := newCIUnit(t, nil)
		j := ciJob(t, domain.CIPhaseRunning, jobSecret)
		u.expectLogJob(j)
		u.logs.EXPECT().Changed(mock.Anything).Return(make(chan struct{}))
		u.logs.EXPECT().Read(mock.Anything, mock.Anything, int64(5), mock.Anything, mock.Anything).Return(nil, domain.ErrOutOfRange)
		err := u.ci.GetJobLog(context.Background(), p, j.Repo, params(false, 5), send)
		assert.ErrorIs(t, err, domain.ErrOutOfRange)
	})
	t.Run("no follow: no log", func(t *testing.T) {
		t.Parallel()
		u := newCIUnit(t, nil)
		j := ciJob(t, domain.CIPhaseReady, jobSecret)
		u.expectLogJob(j)
		u.logs.EXPECT().Changed(mock.Anything).Return(make(chan struct{}))
		u.logs.EXPECT().Read(mock.Anything, mock.Anything, int64(0), mock.Anything, mock.Anything).Return(nil, domain.ErrNotFound)
		assert.ErrorIs(t, u.ci.GetJobLog(context.Background(), p, j.Repo, params(false, 0), send), domain.ErrNotFound)
	})
	t.Run("follow, ended: out of range like without follow", func(t *testing.T) {
		t.Parallel()
		u := newCIUnit(t, nil)
		j := ciJob(t, domain.CIPhaseEnded, jobSecret)
		u.expectLogJob(j)
		u.logs.EXPECT().Changed(mock.Anything).Return(make(chan struct{}))
		u.logs.EXPECT().Read(mock.Anything, mock.Anything, int64(5), mock.Anything, mock.Anything).Return(nil, domain.ErrOutOfRange)
		err := u.ci.GetJobLog(context.Background(), p, j.Repo, params(true, 5), send)
		assert.ErrorIs(t, err, domain.ErrOutOfRange)
	})
	t.Run("follow, running: waits for the line, then sends it", func(t *testing.T) {
		t.Parallel()
		u := newCIUnit(t, nil)
		j := ciJob(t, domain.CIPhaseRunning, jobSecret)
		u.expectLogJob(j)
		wake := make(chan struct{})
		u.logs.EXPECT().Changed(mock.Anything).Return((<-chan struct{})(wake))
		reads := 0
		u.logs.EXPECT().Read(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
			func(_ context.Context, _ ports.CILogRef, from int64, _, _ int) ([]domain.CILogLine, error) {
				reads++
				if from == 0 && reads == 1 {
					return nil, domain.ErrOutOfRange // the runner has not written yet
				}
				if from == 0 {
					return []domain.CILogLine{{Time: now, Text: "late line"}}, nil
				}
				return nil, nil
			})
		ctx, cancel := context.WithCancel(context.Background())
		got := make(chan ports.LogBatch, 1)
		errc := make(chan error, 1)
		go func() {
			errc <- u.ci.GetJobLog(ctx, p, j.Repo, params(true, 0), func(b ports.LogBatch) error {
				got <- b
				cancel() // enough
				return nil
			})
		}()
		select {
		case <-got:
			t.Fatal("a line was sent before it existed")
		case <-time.After(150 * time.Millisecond):
		}
		close(wake) // the append happens
		select {
		case b := <-got:
			require.Len(t, b.Lines, 1)
			assert.Equal(t, "late line", b.Lines[0].Text)
		case <-time.After(5 * time.Second):
			t.Fatal("the reader did not wake up")
		}
		require.NoError(t, <-errc)
	})
}

// S8: a stream that never idles revalidates the token too.
func TestGetJobLogRevalidatesABusyStream(t *testing.T) {
	t.Parallel()
	auth := mocks.NewRevalidator(t)
	u := newCIUnit(t, func(cfg *usecase.CIConfig) { cfg.Auth = auth })
	j := ciJob(t, domain.CIPhaseRunning, jobSecret)
	u.expectLogJob(j)
	u.logs.EXPECT().Changed(mock.Anything).Return(make(chan struct{}))
	// A token principal; the log keeps having lines to send.
	tok := domain.TokenPrincipal(domain.Token{ID: "t1", Scopes: []domain.Scope{domain.ScopeRepoRead}})
	u.logs.EXPECT().Read(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return([]domain.CILogLine{{Time: now, Text: "x"}}, nil)
	revoked := domain.ErrUnauthenticated
	auth.EXPECT().Revalidate(mock.Anything, tok).Return(revoked).Once()

	sent := 0
	start := time.Now()
	err := u.ci.GetJobLog(context.Background(), tok, j.Repo, ports.GetJobLogParams{JobID: jobIDOne, Follow: true}, func(ports.LogBatch) error {
		sent++
		time.Sleep(usecase.DefaultRevalidateEvery / 2)
		if time.Since(start) > 30*time.Second {
			return context.DeadlineExceeded
		}
		return nil
	})
	assert.ErrorIs(t, err, domain.ErrUnauthenticated, "a revoked token stops a busy stream")
	assert.GreaterOrEqual(t, sent, 2)
}

// S9: more than 1,000 checks on a commit is an error for the merge rule, not a
// list cut at 1,000.
func TestCommitChecksRefusesATruncatedList(t *testing.T) {
	t.Parallel()
	u := newCIUnit(t, nil)
	repo := name(t, "proj")
	u.store.EXPECT().ListChecks(mock.Anything, repo, "abc", usecase.MaxCIChecks).Return(make([]domain.CICheck, usecase.MaxCIChecks), true, nil).Once()
	_, err := u.ci.CommitChecks(context.Background(), repo, "abc")
	assert.ErrorIs(t, err, domain.ErrResourceExhausted)

	u.store.EXPECT().ListChecks(mock.Anything, repo, "def", usecase.MaxCIChecks).Return([]domain.CICheck{{Name: "a"}}, false, nil).Once()
	checks, err := u.ci.CommitChecks(context.Background(), repo, "def")
	require.NoError(t, err)
	assert.Len(t, checks, 1)
}

// logCapture is a context whose logger writes into a buffer the test reads.
func logCapture(t *testing.T) (context.Context, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	log := zerowrap.New(zerowrap.Config{Level: "debug", Format: "json", Output: &buf})
	return zerowrap.WithCtx(context.Background(), log), &buf
}

// M5, M6: the sweep removes the runs older than the retention (with the clock
// moved, not waited for), the logs of runs no record has any more, and says so
// when it cannot.
func TestSweep(t *testing.T) {
	t.Parallel()
	retention := 30 * 24 * time.Hour

	t.Run("old runs go with their logs, orphan logs too", func(t *testing.T) {
		t.Parallel()
		u := newCIUnit(t, func(cfg *usecase.CIConfig) { cfg.Retention = retention })
		u.clock.t = now.Add(100 * 24 * time.Hour)
		ctx, out := logCapture(t)
		u.store.EXPECT().Sweep(mock.Anything, u.clock.t.Add(-retention)).Return([]int64{3, 4}, nil)
		u.logs.EXPECT().RemoveRuns(mock.Anything, []int64{3, 4}).Return(nil).Once()
		u.logs.EXPECT().RunIDs(mock.Anything).Return([]int64{5, 6, 7}, nil)
		u.store.EXPECT().GetRunSource(mock.Anything, int64(5)).Return(domain.CIRunSource{}, nil)
		u.store.EXPECT().GetRunSource(mock.Anything, int64(6)).Return(domain.CIRunSource{}, domain.ErrNotFound)
		u.store.EXPECT().GetRunSource(mock.Anything, int64(7)).Return(domain.CIRunSource{}, domain.ErrNotFound)
		u.logs.EXPECT().RemoveRuns(mock.Anything, []int64{6, 7}).Return(nil).Once()
		u.ci.Sweep(ctx)
		assert.NotContains(t, out.String(), `"level":"error"`)
		assert.Contains(t, out.String(), "old runs removed")
	})

	t.Run("a store that fails ends the sweep, logged", func(t *testing.T) {
		t.Parallel()
		u := newCIUnit(t, nil)
		ctx, out := logCapture(t)
		u.store.EXPECT().Sweep(mock.Anything, mock.Anything).Return(nil, errors.New("kv closed"))
		u.ci.Sweep(ctx) // the log store expects no call
		assert.Contains(t, out.String(), "sweep the old runs")
		assert.Contains(t, out.String(), "kv closed")
	})

	t.Run("a log directory that cannot be removed is logged and the orphans are still looked for", func(t *testing.T) {
		t.Parallel()
		u := newCIUnit(t, nil)
		ctx, out := logCapture(t)
		u.store.EXPECT().Sweep(mock.Anything, mock.Anything).Return([]int64{3}, nil)
		u.logs.EXPECT().RemoveRuns(mock.Anything, []int64{3}).Return(errors.New("permission denied")).Once()
		u.logs.EXPECT().RunIDs(mock.Anything).Return([]int64{9}, nil)
		u.store.EXPECT().GetRunSource(mock.Anything, int64(9)).Return(domain.CIRunSource{}, domain.ErrNotFound)
		u.logs.EXPECT().RemoveRuns(mock.Anything, []int64{9}).Return(errors.New("disk on fire")).Once()
		u.ci.Sweep(ctx)
		assert.Contains(t, out.String(), "remove the logs of the old runs")
		assert.Contains(t, out.String(), "permission denied")
		assert.Contains(t, out.String(), "remove the logs of runs that no longer exist")
		assert.Contains(t, out.String(), "disk on fire")
	})

	t.Run("a listing of the log directories that fails is logged", func(t *testing.T) {
		t.Parallel()
		u := newCIUnit(t, nil)
		ctx, out := logCapture(t)
		u.store.EXPECT().Sweep(mock.Anything, mock.Anything).Return(nil, nil)
		u.logs.EXPECT().RunIDs(mock.Anything).Return(nil, errors.New("io error"))
		u.ci.Sweep(ctx)
		assert.Contains(t, out.String(), "list the log directories")
	})

	t.Run("a run with a lookup error keeps its logs", func(t *testing.T) {
		t.Parallel()
		u := newCIUnit(t, nil)
		ctx, out := logCapture(t)
		u.store.EXPECT().Sweep(mock.Anything, mock.Anything).Return(nil, nil)
		u.logs.EXPECT().RunIDs(mock.Anything).Return([]int64{9}, nil)
		u.store.EXPECT().GetRunSource(mock.Anything, int64(9)).Return(domain.CIRunSource{}, errors.New("kv closed"))
		u.ci.Sweep(ctx) // RemoveRuns is not expected: an unknown state removes nothing
		assert.Contains(t, out.String(), "look up a run")
	})
}

// M5: a log that would pass its limit appends nothing; the server adds a last
// line saying so, fails the job and answers failed_precondition with the end.
func TestLogLimitEndsTheJob(t *testing.T) {
	t.Parallel()
	u := newCIUnit(t, nil)
	j := ciJob(t, domain.CIPhaseRunning, jobSecret)
	j.Steps = []domain.CIStep{{Number: 1, State: domain.CIStepInProgress, StartedAt: now, HasLines: true}}
	u.expectToken(jobSecret, j)
	u.store.EXPECT().GetJob(mock.Anything, j.RunID, j.RunAttempt, j.ID).Return(j, nil)
	u.logs.EXPECT().Count(mock.Anything, mock.Anything).Return(10, nil)
	u.logs.EXPECT().Append(mock.Anything, mock.Anything, int64(10), mock.Anything, int64(domain.CIMaxLogBytes)).Return(0, 0, ports.ErrCILogLimit)

	// stopForLogLimit: the run is loaded, the last line is forced in, the job ends.
	u.store.EXPECT().GetRunSource(mock.Anything, j.RunID).Return(domain.CIRunSource{RunID: j.RunID, Repo: j.Repo, WorkflowYAML: "on: push\njobs:\n  build:\n    runs-on: x\n    steps:\n      - run: echo\n"}, nil)
	u.store.EXPECT().GetRun(mock.Anything, j.Repo, j.RunID).Return(domain.CIRun{Repo: j.Repo, ID: j.RunID, Attempt: 1, WorkflowPath: "w.yml"}, nil)
	u.store.EXPECT().ListJobs(mock.Anything, j.RunID, int32(1)).Return([]domain.CIJob{j}, nil)
	var forced []domain.CILogLine
	u.logs.EXPECT().AppendForced(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, _ ports.CILogRef, lines []domain.CILogLine) (int64, error) {
			forced = lines
			return 11, nil
		})
	var written ports.CIWrite
	u.store.EXPECT().Apply(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, w ports.CIWrite) (uint64, error) { written = w; return 5, nil })
	u.meta.EXPECT().Get(mock.Anything, j.Repo).Return(domain.Repo{Name: j.Repo, Visibility: domain.VisibilityPrivate}, nil)
	u.logs.EXPECT().Count(mock.Anything, mock.Anything).Return(11, nil).Maybe()
	u.bus.EXPECT().Publish(mock.Anything, mock.Anything).Return().Maybe()

	_, err := u.ci.AppendLog(context.Background(), jobSecret, jobIDOne, 1, 10, []domain.CILogLine{{Time: now, Text: "one more"}})
	ended, ok := domain.IsJobEnded(err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, domain.CIFailure, ended.Conclusion)
	assert.Contains(t, ended.Message, "512 MiB")

	require.Len(t, forced, 1)
	assert.Contains(t, forced[0].Text, "##[error]")
	assert.Contains(t, forced[0].Text, "512 MiB")
	require.Len(t, written.Jobs, 1)
	assert.Equal(t, domain.CIPhaseEnded, written.Jobs[0].Phase)
	assert.Equal(t, domain.CIFailure, written.Jobs[0].Conclusion)
	assert.Equal(t, domain.CIStepCompleted, written.Jobs[0].Steps[0].State, "an open step is completed with the job")
}
