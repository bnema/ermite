// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/cilogs"
	"bnema.dev/ermite/internal/adapters/cisummaries"
	"bnema.dev/ermite/internal/adapters/eventbus"
	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

const depsLabel = "ubuntu-latest"

// endedCall is what the observer was told, with the state of the job in the
// store at that moment.
type endedCall struct {
	run    domain.CIRun
	job    domain.CIJob
	report *ports.DepsReport
	stored domain.CIJob
}

// depsRig is the CI over the real stores and a synctest clock, with a mocked
// observer, a registered runner and one repository.
type depsRig struct {
	t      *testing.T
	ctx    context.Context
	ci     *usecase.CI
	store  *kvstore.Store
	bus    *eventbus.Bus
	logs   *cilogs.Store
	repo   domain.RepoName
	runner string // the runner token
	ended  chan endedCall
	events atomic.Int32 // run events handled (workflow_run)
}

// inBubble runs fn in a synctest bubble with a fresh rig.
func inBubble(t *testing.T, labels []string, tweak func(*usecase.CIConfig), fn func(t *testing.T, r *depsRig)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		fn(t, newDepsRig(t, labels, tweak))
	})
}

func newDepsRig(t *testing.T, labels []string, tweak func(*usecase.CIConfig)) *depsRig {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	store, err := kvstore.Open(ctx, dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close(ctx) })
	r := &depsRig{t: t, ctx: ctx, store: store, bus: eventbus.New(), logs: cilogs.New(t.TempDir()), repo: name(t, "proj"), ended: make(chan endedCall, 16)}
	require.NoError(t, store.Create(ctx, domain.Repo{Name: r.repo, Visibility: domain.VisibilityPrivate, CreatedAt: time.Now()}))

	clock := mocks.NewClock(t)
	clock.EXPECT().Now().RunAndReturn(time.Now).Maybe()
	repos := mocks.NewGitRepos(t)
	repos.EXPECT().DefaultBranch(mock.Anything, mock.Anything).Return("", false, nil).Maybe() // the scheduler looks
	cfg := usecase.CIConfig{
		Store: store.CI(), Logs: r.logs, Summaries: cisummaries.New(t.TempDir()), Meta: store,
		Repos: repos, Reader: mocks.NewRepoReader(t), Git: mocks.NewMergeGit(t), Diff: mocks.NewCIGit(t),
		Bus: r.bus, Clock: clock, Owner: "owner", PublicURL: "https://forge.test",
		PollTimeout: time.Second, Tick: 10 * time.Second,
		RunEventHandled: func(int64, string) { r.events.Add(1) },
	}
	if tweak != nil {
		tweak(&cfg)
	}
	r.ci = usecase.NewCI(ctx, cfg)
	t.Cleanup(func() { r.ci.Close(ctx) })

	obs := mocks.NewDepsRunObserver(t)
	obs.EXPECT().DepsJobEnded(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Run(
		func(c context.Context, run domain.CIRun, job domain.CIJob, report *ports.DepsReport) {
			stored, _ := store.CI().GetJob(c, job.RunID, job.RunAttempt, job.ID)
			r.ended <- endedCall{run: run, job: job, report: report, stored: stored}
		}).Maybe()
	r.ci.SetDepsObserver(obs)

	reg, err := r.ci.Register(ctx, domain.Root(), ports.RegisterRunnerParams{
		Name: "alpha", Labels: labels, Isolation: domain.CIIsolationVM, Arch: "x64", OS: "linux",
	})
	require.NoError(t, err)
	r.runner = reg.Token
	return r
}

// advance moves the virtual clock and lets everything that is due run.
func advance(d time.Duration) {
	<-time.After(d)
	synctest.Wait()
}

func (r *depsRig) params(timeout time.Duration) ports.DepsRunParams {
	return ports.DepsRunParams{
		Repo: r.repo, Ref: "refs/heads/main", Sha: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Title: "build(deps): update 1 dependency",
		Job:   domain.CIDepsJob{GoVersion: "1.26.1", Updates: []domain.DepRef{{Ecosystem: domain.EcosystemGoMod, Name: "golang.org/x/text", Version: "v0.21.0"}}},
		Label: "Ubuntu-Latest", Timeout: timeout,
	}
}

func (r *depsRig) start(timeout time.Duration) domain.CIRun {
	r.t.Helper()
	run, err := r.ci.StartDepsRun(r.ctx, r.params(timeout))
	require.NoError(r.t, err)
	return run
}

// fetch polls as the runner does and returns the job, nil when none.
func (r *depsRig) fetch(kinds ...string) *ports.RunnerJob {
	r.t.Helper()
	res, err := r.ci.FetchJob(r.ctx, r.runner, ports.FetchJobParams{Kinds: kinds})
	require.NoError(r.t, err)
	return res.Job
}

func (r *depsRig) job(run domain.CIRun) domain.CIJob {
	r.t.Helper()
	jobs, err := r.store.CI().ListJobs(r.ctx, run.ID, run.Attempt)
	require.NoError(r.t, err)
	require.Len(r.t, jobs, 1)
	return jobs[0]
}

func (r *depsRig) run(id int64) domain.CIRun {
	r.t.Helper()
	run, err := r.store.CI().GetRun(r.ctx, r.repo, id)
	require.NoError(r.t, err)
	return run
}

func (r *depsRig) nextEnded() endedCall {
	r.t.Helper()
	synctest.Wait()
	select {
	case c := <-r.ended:
		return c
	default:
		require.FailNow(r.t, "the observer was not told")
		return endedCall{}
	}
}

func (r *depsRig) noEnded() {
	r.t.Helper()
	synctest.Wait()
	select {
	case c := <-r.ended:
		require.FailNow(r.t, "the observer was told", "job %s ended %s", c.job.ID, c.job.Conclusion)
	default:
	}
}

func (r *depsRig) tokenGone(secret string) bool {
	_, err := r.store.CI().FindRepoTokenByHash(r.ctx, domain.HashTokenSecret(secret))
	return err != nil
}

// 1: StartDepsRun makes one queued run and one ready job, with no check and no
// workflow_run event.
func TestStartDepsRunCreatesARunAndAReadyJob(t *testing.T) {
	t.Parallel()
	inBubble(t, []string{depsLabel}, nil, func(t *testing.T, r *depsRig) {
		sub := r.bus.Subscribe(r.ctx)
		r.ci.Start()
		before := time.Now().UTC()
		run := r.start(10 * time.Minute)

		got := r.run(run.ID)
		assert.Equal(t, run, got)
		assert.Equal(t, domain.CIRun{
			Repo: r.repo, ID: run.ID, Number: 1, Attempt: 1, WorkflowPath: "bernard/deps", WorkflowName: "Dependencies",
			Name: "build(deps): update 1 dependency", Title: "build(deps): update 1 dependency", Event: "dependencies",
			Ref: "refs/heads/main", Sha: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "bernard",
			Status: domain.CIQueued, Trusted: false, CreatedAt: before, Version: got.Version,
		}, got)
		assert.True(t, got.IsDeps())

		j := r.job(got)
		assert.Equal(t, domain.NewJobID(run.ID, 0, 0), j.ID)
		assert.Equal(t, domain.CIPhaseReady, j.Phase)
		assert.True(t, j.Planned)
		assert.Equal(t, "deps", j.Key)
		assert.Equal(t, "Update dependencies", j.Name)
		assert.Equal(t, []string{"ubuntu-latest"}, j.Labels)
		assert.EqualValues(t, 10, j.TimeoutMinutes)
		assert.Equal(t, 1, j.ComboTotal)
		assert.Equal(t, before, j.ReadyAt)
		require.NotNil(t, j.Deps)
		assert.Equal(t, r.params(0).Job, *j.Deps)

		checks, _, err := r.store.CI().ListChecks(r.ctx, r.repo, run.Sha, 10)
		require.NoError(t, err)
		assert.Empty(t, checks, "a dependency run posts no check")

		// RunUpdated then JobUpdated were published.
		synctest.Wait()
		var kinds []string
		for len(sub) > 0 {
			switch (<-sub).(type) {
			case domain.RunUpdated:
				kinds = append(kinds, "run")
			case domain.JobUpdated:
				kinds = append(kinds, "job")
			}
		}
		assert.Equal(t, []string{"run", "job"}, kinds)
		assert.Zero(t, r.events.Load(), "no workflow_run event")
	})
}

func TestStartDepsRunTimeoutIsRoundedUpToTheMinute(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		timeout time.Duration
		want    int32
	}{{0, 1}, {time.Second, 1}, {time.Minute, 1}, {61 * time.Second, 2}, {10 * time.Minute, 10}} {
		inBubble(t, []string{depsLabel}, nil, func(t *testing.T, r *depsRig) {
			run := r.start(tc.timeout)
			assert.Equal(t, tc.want, r.job(run).TimeoutMinutes, tc.timeout.String())
		})
	}
}

func TestStartDepsRunOnAGoneRepository(t *testing.T) {
	t.Parallel()
	inBubble(t, []string{depsLabel}, nil, func(t *testing.T, r *depsRig) {
		p := r.params(time.Minute)
		p.Repo = name(t, "gone")
		_, err := r.ci.StartDepsRun(r.ctx, p)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
}

// A dependency run waits 24 hours for a runner, like any job.
func TestDepsRunUsesTheNormalQueueTimeout(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 24*time.Hour, domain.CIQueueTimeout)
	inBubble(t, []string{depsLabel}, nil, func(t *testing.T, r *depsRig) {
		r.ci.Start()
		run := r.start(time.Minute)
		advance(domain.CIQueueTimeout - time.Minute)
		r.noEnded()
		assert.Equal(t, domain.CIPhaseReady, r.job(run).Phase)

		advance(2 * time.Minute)
		c := r.nextEnded()
		assert.Nil(t, c.report)
		assert.Equal(t, domain.CIFailure, c.job.Conclusion)
		assert.Equal(t, domain.CIQueueTimeoutMessage, c.job.Error)
		assert.Equal(t, domain.CICompleted, r.run(run.ID).Status)
		assert.Equal(t, domain.CIPhaseEnded, c.stored.Phase, "told after the job was written")
	})
}

// 2: the kinds gate the assignment.
func TestFetchJobKindsGateDepsJobs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		labels  []string
		kinds   []string
		wantJob bool
	}{
		{"no kinds asks for workflow jobs", []string{depsLabel}, nil, false},
		{"workflow only", []string{depsLabel}, []string{"workflow"}, false},
		{"deps only", []string{depsLabel}, []string{"deps"}, true},
		{"workflow and deps", []string{depsLabel}, []string{"workflow", "deps"}, true},
		{"a runner without the label", []string{"other"}, []string{"workflow", "deps"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, tc.labels, nil, func(t *testing.T, r *depsRig) {
				run := r.start(10 * time.Minute)
				j := r.fetch(tc.kinds...)
				if !tc.wantJob {
					assert.Nil(t, j)
					assert.Equal(t, domain.CIPhaseReady, r.job(run).Phase)
					return
				}
				require.NotNil(t, j)
				assert.Equal(t, run.ID, j.Job.RunID)
				assert.Equal(t, domain.CIPhaseAssigned, r.job(run).Phase)
			})
		})
	}
}

func TestFetchJobGivesADepsJobItsReadOnlyContract(t *testing.T) {
	t.Parallel()
	inBubble(t, []string{depsLabel}, func(c *usecase.CIConfig) {
		// Settings and OIDC that would give a workflow job secrets and tokens are
		// not even asked for a dependency job (a mock without expectations).
		c.Settings = mocks.NewCIJobSettings(t)
	}, func(t *testing.T, r *depsRig) {
		run := r.start(10 * time.Minute)
		j := r.fetch("workflow", "deps")
		require.NotNil(t, j)

		assert.Equal(t, domain.NewJobID(run.ID, 0, 0), j.Job.ID)
		assert.Equal(t, run.ID, j.Job.RunID)
		assert.Equal(t, r.repo, j.Repo)
		assert.Equal(t, run.Sha, j.Sha)
		assert.Equal(t, "refs/heads/main", j.Ref)
		assert.True(t, domain.IsJobToken(j.Token))
		assert.NotEmpty(t, j.GithubToken)
		assert.False(t, j.Trusted)
		assert.Empty(t, j.WorkflowPath)
		assert.Empty(t, j.WorkflowYAML)
		assert.Empty(t, j.Secrets)
		assert.Empty(t, j.Vars)
		assert.Empty(t, j.Environment)
		assert.Empty(t, j.IDTokenURL)
		assert.Empty(t, j.IDTokenRequestToken)
		assert.Equal(t, ports.JobCache{}, j.Cache)
		assert.Equal(t, domain.TokenPermissions(nil, false), j.Permissions)
		require.NotNil(t, j.Deps)
		assert.Equal(t, r.params(0).Job, *j.Deps)

		stored := r.job(run)
		assert.Equal(t, domain.CIPhaseAssigned, stored.Phase)
		assert.Equal(t, domain.HashTokenSecret(j.Token), stored.TokenHash)

		tok, err := r.store.CI().FindRepoTokenByHash(r.ctx, domain.HashTokenSecret(j.GithubToken))
		require.NoError(t, err)
		assert.Equal(t, r.repo, tok.Repo)
		assert.False(t, tok.OIDC)
		assert.Equal(t, stored.AssignedAt.Add(10*time.Minute+domain.CITimeoutSlack+domain.RepoTokenGrace), tok.ExpiresAt)
		for k, lvl := range tok.Permissions {
			assert.Equal(t, domain.PermissionRead, lvl, k)
		}
		assert.Equal(t, domain.PermissionRead, tok.Permissions[domain.PermContents])
	})
}

// A retried poll gets the same assignment, as for a workflow job.
func TestFetchJobRepeatedPollRepeatsADepsAssignment(t *testing.T) {
	t.Parallel()
	inBubble(t, []string{depsLabel}, nil, func(t *testing.T, r *depsRig) {
		run := r.start(10 * time.Minute)
		p := ports.FetchJobParams{Kinds: []string{"deps"}, PollID: "poll-1"}
		first, err := r.ci.FetchJob(r.ctx, r.runner, p)
		require.NoError(t, err)
		require.NotNil(t, first.Job)
		again, err := r.ci.FetchJob(r.ctx, r.runner, p)
		require.NoError(t, err)
		require.NotNil(t, again.Job)
		assert.Equal(t, first.Job.Token, again.Job.Token)
		assert.Equal(t, first.Job.GithubToken, again.Job.GithubToken)
		assert.Equal(t, first.Job.Deps, again.Job.Deps)
		assert.Equal(t, run.ID, again.Job.Job.RunID)

		require.NoError(t, r.ci.Heartbeat(r.ctx, first.Job.Token, first.Job.Job.ID))
		none, err := r.ci.FetchJob(r.ctx, r.runner, p)
		require.NoError(t, err)
		assert.Nil(t, none.Job, "acknowledged: nothing to repeat")
	})
}

// A workflow job and a dependency job wait together: each kind gets its own.
func TestFetchJobKeepsKindsApart(t *testing.T) {
	t.Parallel()
	inBubble(t, []string{depsLabel}, nil, func(t *testing.T, r *depsRig) {
		wf, err := r.store.CI().CreateRun(r.ctx, r.repo, ".github/workflows/ci.yml", func(id, number int64) ports.CIRunDraft {
			return ports.CIRunDraft{
				Run: domain.CIRun{
					Repo: r.repo, ID: id, Number: number, Attempt: 1, WorkflowPath: ".github/workflows/ci.yml", WorkflowName: "ci", Name: "ci",
					Event: "push", Ref: "refs/heads/main", Sha: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Status: domain.CIQueued, CreatedAt: time.Now(),
				},
				Source: domain.CIRunSource{Repo: r.repo, WorkflowYAML: "on: push", GithubJSON: "{}"},
				Jobs: []domain.CIJob{{
					ID: domain.NewJobID(id, 0, 0), RunID: id, Attempt: 1, RunAttempt: 1, Repo: r.repo, Key: "build", Name: "build",
					Phase: domain.CIPhaseReady, ReadyAt: time.Now(), CreatedAt: time.Now(), Labels: []string{depsLabel}, TimeoutMinutes: 5, Planned: true, ContextsJSON: "{}",
				}},
			}
		})
		require.NoError(t, err)
		deps := r.start(10 * time.Minute)

		j := r.fetch("deps")
		require.NotNil(t, j)
		assert.Equal(t, deps.ID, j.Job.RunID)
		assert.Nil(t, r.fetch("deps"), "the workflow job is not for a deps-only poll")

		j = r.fetch("workflow")
		require.NotNil(t, j)
		assert.Equal(t, wf.Run.ID, j.Job.RunID)
		assert.Nil(t, j.Deps)
	})
}

// 3: the calls of a dependency job take its job token and use the normal log,
// steps and completion.
func TestDepsJobRunsOnItsJobToken(t *testing.T) {
	t.Parallel()
	inBubble(t, []string{depsLabel}, nil, func(t *testing.T, r *depsRig) {
		run := r.start(10 * time.Minute)
		j := r.fetch("deps")
		require.NotNil(t, j)
		id := j.Job.ID

		// The runner token is no key to a job.
		assert.ErrorIs(t, r.ci.Heartbeat(r.ctx, r.runner, id), domain.ErrUnauthenticated)

		require.NoError(t, r.ci.Heartbeat(r.ctx, j.Token, id))
		running := r.job(run)
		assert.Equal(t, domain.CIPhaseRunning, running.Phase)
		assert.Equal(t, domain.CIInProgress, r.run(run.ID).Status)

		start := time.Now()
		require.NoError(t, r.ci.UpdateJob(r.ctx, j.Token, id, []ports.StepUpdate{{Number: 1, Name: "Update dependencies", State: domain.CIStepInProgress, StartedAt: start}}, nil))
		lines := []domain.CILogLine{{Time: start, Text: "go get golang.org/x/text@v0.21.0"}, {Time: start, Text: "go mod tidy"}}
		next, err := r.ci.AppendLog(r.ctx, j.Token, id, 1, 0, lines)
		require.NoError(t, err)
		assert.EqualValues(t, 2, next)
		// A job token of another job is refused.
		_, err = r.ci.AppendLog(r.ctx, j.Token, "99-0-0", 1, 0, lines)
		assert.ErrorIs(t, err, domain.ErrForbidden)

		r.noEnded()
		report := &ports.DepsReport{Files: []ports.DepsFile{{Path: "go.mod", Content: []byte("module x\n")}}}
		res, err := r.ci.CompleteJob(r.ctx, j.Token, ports.CompleteJobParams{
			JobID: id, Conclusion: domain.CISuccess, DepsReport: report,
			Steps: []ports.StepUpdate{{Number: 1, State: domain.CIStepCompleted, Conclusion: domain.CISuccess, StartedAt: start, CompletedAt: start.Add(time.Second)}},
		})
		require.NoError(t, err)
		assert.True(t, res.Applied)
		assert.Equal(t, domain.CISuccess, res.Conclusion)

		c := r.nextEnded()
		assert.Equal(t, report, c.report)
		assert.Equal(t, run.ID, c.run.ID)
		assert.Equal(t, domain.CICompleted, c.run.Status)
		assert.Equal(t, domain.CISuccess, c.run.Conclusion)
		assert.Equal(t, id, c.job.ID)
		assert.Equal(t, domain.CISuccess, c.job.Conclusion)
		assert.Equal(t, domain.CIPhaseEnded, c.stored.Phase, "told after the write that ended the job")
		require.Len(t, c.job.Steps, 1)
		assert.Equal(t, "Update dependencies", c.job.Steps[0].Name)
		assert.True(t, c.job.Steps[0].HasLines)

		// The log is the normal one.
		count, err := r.logs.Count(r.ctx, ports.CILogRef{RunID: c.job.RunID, JobID: c.job.ID, Attempt: c.job.Attempt})
		require.NoError(t, err)
		assert.EqualValues(t, 2, count)

		assert.True(t, r.tokenGone(j.GithubToken), "the repository token is revoked with the job")
		checks, _, err := r.store.CI().ListChecks(r.ctx, r.repo, run.Sha, 10)
		require.NoError(t, err)
		assert.Empty(t, checks)
		assert.Zero(t, r.events.Load())

		// Once.
		again, err := r.ci.CompleteJob(r.ctx, j.Token, ports.CompleteJobParams{JobID: id, Conclusion: domain.CISuccess, DepsReport: report})
		require.NoError(t, err)
		assert.False(t, again.Applied)
		r.noEnded()
	})
}

func TestDepsJobReportIsPassedOnAFailure(t *testing.T) {
	t.Parallel()
	inBubble(t, []string{depsLabel}, nil, func(t *testing.T, r *depsRig) {
		r.start(10 * time.Minute)
		j := r.fetch("deps")
		require.NoError(t, r.ci.Heartbeat(r.ctx, j.Token, j.Job.ID))
		report := &ports.DepsReport{Error: "go mod tidy failed"}
		_, err := r.ci.CompleteJob(r.ctx, j.Token, ports.CompleteJobParams{JobID: j.Job.ID, Conclusion: domain.CIFailure, DepsReport: report})
		require.NoError(t, err)
		c := r.nextEnded()
		assert.Equal(t, report, c.report)
		assert.Equal(t, domain.CIFailure, c.job.Conclusion)
		assert.Equal(t, domain.CIFailure, c.run.Conclusion)
	})
}

// 4: the server ends a job: the observer hears of it with no report.
func TestDepsJobEndedByTheOwnerBeforeARunnerHasIt(t *testing.T) {
	t.Parallel()
	inBubble(t, []string{depsLabel}, nil, func(t *testing.T, r *depsRig) {
		run := r.start(10 * time.Minute)
		_, err := r.ci.CancelRun(r.ctx, domain.Root(), r.repo, run.ID)
		require.NoError(t, err)

		c := r.nextEnded()
		assert.Nil(t, c.report)
		assert.Equal(t, domain.CICancelled, c.job.Conclusion)
		assert.Equal(t, domain.CICancelled, r.run(run.ID).Conclusion)
		assert.Nil(t, r.fetch("deps"), "a cancelled job is not offered")
		r.noEnded()
	})
}

func TestCancelDepsRunAsksTheRunnerToStop(t *testing.T) {
	t.Parallel()
	inBubble(t, []string{depsLabel}, nil, func(t *testing.T, r *depsRig) {
		run := r.start(10 * time.Minute)
		j := r.fetch("deps")
		require.NoError(t, r.ci.Heartbeat(r.ctx, j.Token, j.Job.ID))

		require.NoError(t, r.ci.CancelDepsRun(r.ctx, r.repo, run.ID))
		require.NoError(t, r.ci.CancelDepsRun(r.ctx, r.repo, run.ID), "a second cancel is a no-op")
		r.noEnded()
		cur := r.job(run)
		assert.Equal(t, domain.CICancelUser, cur.CancelReason)
		assert.True(t, r.run(run.ID).CancelRequested)

		// WatchJob carries the request.
		var got *ports.JobCancel
		ctx, cancel := context.WithCancel(r.ctx)
		done := make(chan error, 1)
		go func() {
			done <- r.ci.WatchJob(ctx, j.Token, j.Job.ID, nil, func(s ports.JobSignal) error {
				if s.Cancel != nil {
					got = s.Cancel
					cancel()
				}
				return nil
			})
		}()
		synctest.Wait()
		require.NoError(t, <-done)
		require.NotNil(t, got)
		assert.Equal(t, domain.CICancelUser, got.Reason)

		_, err := r.ci.CompleteJob(r.ctx, j.Token, ports.CompleteJobParams{JobID: j.Job.ID, Conclusion: domain.CICancelled})
		require.NoError(t, err)
		c := r.nextEnded()
		assert.Nil(t, c.report)
		assert.Equal(t, domain.CICancelled, c.job.Conclusion)
		assert.Equal(t, domain.CICancelled, c.run.Conclusion)
		assert.True(t, r.tokenGone(j.GithubToken))
	})
}

func TestCancelDepsRunIsLimitedToDependencyRuns(t *testing.T) {
	t.Parallel()
	inBubble(t, []string{depsLabel}, nil, func(t *testing.T, r *depsRig) {
		run := r.start(10 * time.Minute)
		assert.ErrorIs(t, r.ci.CancelDepsRun(r.ctx, name(t, "other"), run.ID), domain.ErrNotFound, "another repository")
		assert.ErrorIs(t, r.ci.CancelDepsRun(r.ctx, r.repo, run.ID+100), domain.ErrNotFound)

		require.NoError(t, r.ci.CancelDepsRun(r.ctx, r.repo, run.ID))
		r.nextEnded()
		require.NoError(t, r.ci.CancelDepsRun(r.ctx, r.repo, run.ID), "a completed run is left alone")
		r.noEnded()
	})
}

func TestDepsJobTimesOutThroughTheWatchdog(t *testing.T) {
	t.Parallel()
	inBubble(t, []string{depsLabel}, func(c *usecase.CIConfig) { c.HeartbeatDeadline = 24 * time.Hour }, func(t *testing.T, r *depsRig) {
		r.ci.Start()
		run := r.start(time.Minute)
		j := r.fetch("deps")
		require.NoError(t, r.ci.Heartbeat(r.ctx, j.Token, j.Job.ID))

		// timeout-minutes plus the slack: the cancel request, then the grace.
		advance(time.Minute + domain.CITimeoutSlack - 10*time.Second)
		r.noEnded()
		advance(30 * time.Second)
		r.noEnded()
		assert.Equal(t, domain.CICancelTimeout, r.job(run).CancelReason)

		advance(domain.CICancelGrace + 10*time.Second)
		c := r.nextEnded()
		assert.Nil(t, c.report)
		assert.Equal(t, domain.CITimedOut, c.job.Conclusion)
		assert.Equal(t, domain.CIFailure, r.run(run.ID).Conclusion, "a timeout fails the run")
		assert.True(t, r.tokenGone(j.GithubToken))
	})
}

func TestDepsJobLostRunnerFails(t *testing.T) {
	t.Parallel()
	inBubble(t, []string{depsLabel}, nil, func(t *testing.T, r *depsRig) {
		r.ci.Start()
		run := r.start(60 * time.Minute)
		j := r.fetch("deps")
		require.NoError(t, r.ci.Heartbeat(r.ctx, j.Token, j.Job.ID))

		advance(domain.CIHeartbeatDeadline + 30*time.Second)
		c := r.nextEnded()
		assert.Nil(t, c.report)
		assert.Equal(t, domain.CIFailure, c.job.Conclusion)
		assert.Equal(t, domain.CILostMessage("alpha"), c.job.Error)
		assert.Equal(t, domain.CIFailure, r.run(run.ID).Conclusion)
		assert.True(t, r.tokenGone(j.GithubToken))
		// The runner learns it on its next call.
		err := r.ci.Heartbeat(r.ctx, j.Token, j.Job.ID)
		_, ended := domain.IsJobEnded(err)
		assert.True(t, ended, "%v", err)
	})
}

// A runner that never acknowledges loses the assignment: the job is queued
// again, with no notification, and the repository token is revoked.
func TestDepsJobAssignmentNotAcknowledgedIsQueuedAgain(t *testing.T) {
	t.Parallel()
	inBubble(t, []string{depsLabel}, nil, func(t *testing.T, r *depsRig) {
		r.ci.Start()
		run := r.start(10 * time.Minute)
		j := r.fetch("deps")
		require.NotNil(t, j)

		advance(domain.CIAckTimeout + 15*time.Second)
		r.noEnded()
		cur := r.job(run)
		assert.Equal(t, domain.CIPhaseReady, cur.Phase)
		assert.Equal(t, 1, cur.AssignCancels)
		assert.True(t, r.tokenGone(j.GithubToken))
		assert.NotNil(t, r.fetch("deps"), "offered again")
	})
}

// 5: no re-run, and no planning of the job that was created ready.
func TestRerunDepsRunIsRefused(t *testing.T) {
	t.Parallel()
	inBubble(t, []string{depsLabel}, nil, func(t *testing.T, r *depsRig) {
		run := r.start(10 * time.Minute)
		_, err := r.ci.RerunRun(r.ctx, domain.Root(), r.repo, run.ID, false)
		require.ErrorIs(t, err, domain.ErrPreconditionFailed)
		assert.Contains(t, err.Error(), "Inbox")

		require.NoError(t, r.ci.CancelDepsRun(r.ctx, r.repo, run.ID))
		_, err = r.ci.RerunRun(r.ctx, domain.Root(), r.repo, run.ID, true)
		require.ErrorIs(t, err, domain.ErrPreconditionFailed, "also once completed")
		assert.EqualValues(t, 1, r.run(run.ID).Attempt)
	})
}

func TestOpenDepsRunIsNotPlannedAfterARestart(t *testing.T) {
	t.Parallel()
	inBubble(t, []string{depsLabel}, nil, func(t *testing.T, r *depsRig) {
		run := r.start(10 * time.Minute)
		r.ci.Start() // resume plans the open runs again
		synctest.Wait()

		j := r.job(run)
		assert.Equal(t, domain.CIPhaseReady, j.Phase)
		assert.Empty(t, j.Error)
		assert.Equal(t, domain.CIQueued, r.run(run.ID).Status)
		r.noEnded()
		assert.NotNil(t, r.fetch("deps"))
	})
}

// A dependency job runs no workflow: it has no actions to resolve and no step
// summary to store.
func TestDepsJobRefusesWorkflowCalls(t *testing.T) {
	t.Parallel()
	inBubble(t, []string{depsLabel}, nil, func(t *testing.T, r *depsRig) {
		r.start(10 * time.Minute)
		j := r.fetch("deps")
		require.NotNil(t, j)

		_, err := r.ci.ResolveActions(r.ctx, j.Token, j.Job.ID, []string{"actions/checkout@v4"})
		assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
		err = r.ci.UploadSummary(r.ctx, j.Token, j.Job.ID, 1, "# summary")
		assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
	})
}
