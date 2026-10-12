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
