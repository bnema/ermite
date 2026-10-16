// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"bnema.dev/ermite/internal/domain"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/usecase"
)

// concurrencyHarness is a CI harness with fast deadlines and one runner.
func concurrencyHarness(t *testing.T) (*ciHarness, *fakeRunner) {
	t.Helper()
	c := newCIHarness(t, func(cfg *usecase.CIConfig) {
		cfg.CancelGrace, cfg.Tick, cfg.PollTimeout = 300*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond
	})
	return c, c.registerRunner("alpha", "ubuntu-latest")
}

// workflowAt is a one-job workflow on main with extra top-level and job lines.
func workflowAt(top, job string) string {
	return "name: ci\non:\n  push:\n    branches: [main]\n" + top +
		"jobs:\n  build:\n    runs-on: ubuntu-latest\n" + job +
		"    steps:\n      - run: echo hello\n"
}

// pushN pushes a new commit with the workflow and returns the new run.
func (c *ciHarness) pushN(n int, wf string) *pb.Run {
	c.t.Helper()
	before := len(c.runs())
	// A comment makes each push a new commit even when the workflow repeats.
	c.push("main", map[string]string{".github/workflows/ci.yml": fmt.Sprintf("# push %d\n%s", n, wf)}, "")
	c.waitFor("the run exists", func() bool { return len(c.runs()) == before+1 })
	return c.runs()[0] // newest first
}

func (c *ciHarness) jobsOf(run *pb.Run) []*pb.RunJob { return c.getRun(run.GetId()).GetJobs() }

func (c *ciHarness) waitJobs(what string, run *pb.Run, cond func([]*pb.RunJob) bool) {
	c.t.Helper()
	c.waitFor(what, func() bool { return cond(c.jobsOf(run)) })
}

func allStatus(s pb.CiStatus) func([]*pb.RunJob) bool {
	return func(js []*pb.RunJob) bool {
		for _, j := range js {
			if j.GetStatus() != s {
				return false
			}
		}
		return len(js) > 0
	}
}

func (c *ciHarness) conclusionOf(run *pb.Run) pb.CiConclusion {
	return c.getRun(run.GetId()).GetRun().GetConclusion()
}

func (c *ciHarness) cancelAnnotation(run *pb.Run, group string) bool {
	for _, j := range c.jobsOf(run) {
		for _, a := range c.annotations(run.GetId(), j.GetId()).GetAnnotations() {
			if a.GetAnnotation().GetMessage() == "Canceling since a higher priority waiting request for '"+group+"' exists" {
				return true
			}
		}
	}
	return false
}

const wfGroup = "concurrency:\n  group: Deploy\n"

// AC4: a workflow-level group lets one run go and one wait; a newer waiting run
// replaces the older one, which ends cancelled with GitHub's annotation; the
// group is handed over when the running one completes.
func TestCIWorkflowConcurrencyPendingReplacement(t *testing.T) {
	c, runner := concurrencyHarness(t)
	r1 := c.pushN(1, workflowAt(wfGroup, ""))
	job1 := runner.await()
	require.Equal(t, r1.GetId(), job1.GetRunId())

	r2 := c.pushN(2, workflowAt(wfGroup, ""))
	c.waitJobs("the second run is pending", r2, allStatus(pb.CiStatus_CI_STATUS_PENDING))
	assert.Equal(t, "Deploy", c.jobsOf(r2)[0].GetConcurrencyGroup())
	assert.Equal(t, pb.CiStatus_CI_STATUS_QUEUED, c.getRun(r2.GetId()).GetRun().GetStatus(), "a run is queued, only its jobs are pending")
	// The pending job is not assignable.
	j, err := runner.fetch("")
	require.NoError(t, err)
	assert.Nil(t, j)

	r3 := c.pushN(3, workflowAt(wfGroup, ""))
	c.waitFor("the older pending run is cancelled", func() bool {
		return c.getRun(r2.GetId()).GetRun().GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED
	})
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_CANCELLED, c.conclusionOf(r2))
	assert.True(t, c.cancelAnnotation(r2, "Deploy"))
	c.waitJobs("the third run is pending", r3, allStatus(pb.CiStatus_CI_STATUS_PENDING))
	assert.NotEqual(t, pb.CiStatus_CI_STATUS_COMPLETED, c.getRun(r1.GetId()).GetRun().GetStatus(), "the running run is left alone")

	runner.runJob(job1, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	job3 := runner.await()
	assert.Equal(t, r3.GetId(), job3.GetRunId(), "the group goes to the waiting run")
	runner.runJob(job3, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	c.waitFor("the third run completes", func() bool {
		return c.getRun(r3.GetId()).GetRun().GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED
	})

	// The group is free: a new run takes it at once.
	r4 := c.pushN(4, workflowAt(wfGroup, ""))
	job4 := runner.await()
	assert.Equal(t, r4.GetId(), job4.GetRunId())
}

// AC4: cancel-in-progress cancels the running run, whose runner is told through
// the cancel mechanism of a user cancel; an expression decides it, and groups
// are compared without regard to case.
func TestCIWorkflowConcurrencyCancelInProgress(t *testing.T) {
	for _, tt := range []struct{ name, cancel string }{
		{"true", "true"},
		{"expression", "${{ github.ref == 'refs/heads/main' }}"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, runner := concurrencyHarness(t)
			r1 := c.pushN(1, workflowAt("concurrency:\n  group: Release\n  cancel-in-progress: "+tt.cancel+"\n", ""))
			job1 := runner.await()
			watch, err := runner.jobAPI(job1).runner.WatchJob(c.ctx, connect.NewRequest(&pb.WatchJobRequest{JobId: job1.GetJobId()}))
			require.NoError(t, err)

			r2 := c.pushN(2, workflowAt("concurrency:\n  group: RELEASE\n  cancel-in-progress: "+tt.cancel+"\n", ""))
			var cancel *pb.CancelRequest
			for watch.Receive() {
				if m := watch.Msg().GetCancel(); m != nil {
					cancel = m
					break
				}
			}
			require.NotNil(t, cancel, "the running job is asked to stop")
			assert.Equal(t, pb.CancelReason_CANCEL_REASON_USER, cancel.GetReason())
			assert.Contains(t, cancel.GetMessage(), "higher priority waiting request for 'RELEASE'")

			_, err = runner.jobAPI(job1).runner.CompleteJob(c.ctx, connect.NewRequest(&pb.CompleteJobRequest{
				JobId: job1.GetJobId(), Conclusion: pb.CiConclusion_CI_CONCLUSION_CANCELLED,
			}))
			require.NoError(t, err)
			c.waitFor("the first run is cancelled", func() bool { return c.conclusionOf(r1) == pb.CiConclusion_CI_CONCLUSION_CANCELLED })
			job2 := runner.await()
			assert.Equal(t, r2.GetId(), job2.GetRunId(), "the waiting run takes the group")
		})
	}
	t.Run("false expression only queues", func(t *testing.T) {
		c, runner := concurrencyHarness(t)
		top := "concurrency:\n  group: G\n  cancel-in-progress: ${{ github.ref == 'refs/heads/other' }}\n"
		c.pushN(1, workflowAt(top, ""))
		job1 := runner.await()
		r2 := c.pushN(2, workflowAt(top, ""))
		c.waitJobs("pending", r2, allStatus(pb.CiStatus_CI_STATUS_PENDING))
		runner.heartbeat(job1)
		time.Sleep(200 * time.Millisecond)
		assert.NotEqual(t, pb.CiConclusion_CI_CONCLUSION_CANCELLED, c.conclusionOf(c.runs()[1]))
	})
}

const matrixJob = `    strategy:
      matrix:
        n: [1, 2]
    concurrency:
      group: shared-${{ 'X' }}
`

// AC4: the legs of a matrix share a job-level group: one runs, one waits, and
// a leg is freed when the other ends.
func TestCIJobConcurrencyMatrixLegsShareAGroup(t *testing.T) {
	c, runner := concurrencyHarness(t)
	r := c.pushN(1, workflowAt("", matrixJob))
	job1 := runner.await()
	c.waitJobs("one leg is pending", r, func(js []*pb.RunJob) bool {
		pending, other := 0, 0
		for _, j := range js {
			switch j.GetStatus() {
			case pb.CiStatus_CI_STATUS_PENDING:
				pending++
			default:
				other++
			}
		}
		return len(js) == 2 && pending == 1 && other == 1
	})
	j, err := runner.fetch("")
	require.NoError(t, err)
	assert.Nil(t, j, "the pending leg is not assignable")
	runner.runJob(job1, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	job2 := runner.await()
	assert.NotEqual(t, job1.GetJobId(), job2.GetJobId())
	runner.runJob(job2, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	c.waitFor("the run completes", func() bool { return c.getRun(r.GetId()).GetRun().GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, c.conclusionOf(r))
}

// AC4: a job-level group is shared between runs, case-insensitively; the third
// waiting job replaces the second, which ends cancelled with the annotation.
func TestCIJobConcurrencyAcrossRuns(t *testing.T) {
	c, runner := concurrencyHarness(t)
	group := func(g string) string { return "    concurrency: " + g + "\n" }
	r1 := c.pushN(1, workflowAt("", group("Build-Lock")))
	job1 := runner.await()
	r2 := c.pushN(2, workflowAt("", group("build-lock")))
	c.waitJobs("pending", r2, allStatus(pb.CiStatus_CI_STATUS_PENDING))
	assert.Equal(t, "build-lock", c.jobsOf(r2)[0].GetConcurrencyGroup())
	r3 := c.pushN(3, workflowAt("", group("BUILD-LOCK")))
	c.waitFor("the second job is cancelled", func() bool { return c.conclusionOf(r2) == pb.CiConclusion_CI_CONCLUSION_CANCELLED })
	assert.True(t, c.cancelAnnotation(r2, "BUILD-LOCK"), "the group is named as the newer request wrote it")
	c.waitJobs("pending", r3, allStatus(pb.CiStatus_CI_STATUS_PENDING))
	_ = r1
	runner.runJob(job1, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	job3 := runner.await()
	assert.Equal(t, r3.GetId(), job3.GetRunId())
}

// AC4: the group is freed when its run is cancelled, and when a re-run joins
// the group again it waits behind the run that took it meanwhile.
func TestCIConcurrencyFreedByCancelAndRerunRejoins(t *testing.T) {
	c, runner := concurrencyHarness(t)
	r1 := c.pushN(1, workflowAt(wfGroup, ""))
	job1 := runner.await()
	_, err := c.root.run.CancelRun(c.ctx, connect.NewRequest(&pb.CancelRunRequest{Repo: "proj", RunId: r1.GetId()}))
	require.NoError(t, err)
	runner.heartbeat(job1)
	c.waitFor("the cancelled run completes", func() bool { return c.getRun(r1.GetId()).GetRun().GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })

	r2 := c.pushN(2, workflowAt(wfGroup, ""))
	job2 := runner.await()
	assert.Equal(t, r2.GetId(), job2.GetRunId(), "a cancelled run freed the group")

	_, err = c.root.run.RerunRun(c.ctx, connect.NewRequest(&pb.RerunRunRequest{Repo: "proj", RunId: r1.GetId()}))
	require.NoError(t, err)
	c.waitJobs("the re-run waits for the group", r1, allStatus(pb.CiStatus_CI_STATUS_PENDING))
	runner.runJob(job2, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	job1b := runner.await()
	assert.Equal(t, r1.GetId(), job1b.GetRunId(), "the re-run takes the group once it is free")
}

// AC4: a job lost to a missing heartbeat frees its group.
func TestCIConcurrencyFreedByLostJob(t *testing.T) {
	c := newCIHarness(t, func(cfg *usecase.CIConfig) {
		cfg.HeartbeatDeadline, cfg.Tick, cfg.PollTimeout = 300*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond
	})
	runner := c.registerRunner("alpha", "ubuntu-latest")
	r1 := c.pushN(1, workflowAt(wfGroup, ""))
	job1 := runner.await()
	runner.heartbeat(job1)
	r2 := c.pushN(2, workflowAt(wfGroup, ""))
	c.waitFor("the lost job fails", func() bool { return c.conclusionOf(r1) == pb.CiConclusion_CI_CONCLUSION_FAILURE })
	job2 := runner.await()
	assert.Equal(t, r2.GetId(), job2.GetRunId())
}

// A group that evaluates to nothing fails the job as GitHub does, and so does
// queue: max with cancel-in-progress, literal or evaluated to true.
func TestCIConcurrencyErrors(t *testing.T) {
	c, runner := concurrencyHarness(t)
	_ = runner
	r1 := c.pushN(1, workflowAt("", "    concurrency:\n      group: ${{ vars.NOPE }}\n"))
	c.waitJobs("failed", r1, allStatus(pb.CiStatus_CI_STATUS_COMPLETED))
	assert.Contains(t, c.jobsOf(r1)[0].GetError(), "concurrency group name cannot be empty")

	// The literal pair is a workflow validation error (GitHub's docs): the run
	// fails with no job, like an invalid workflow file, at job and workflow level.
	r3 := c.pushN(3, workflowAt("", "    concurrency:\n      group: g\n      queue: max\n      cancel-in-progress: true\n"))
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_FAILURE, r3.GetConclusion())
	assert.Empty(t, c.jobsOf(r3))
	assert.Equal(t, ".github/workflows/ci.yml (Line: 12, Col: 27): 'queue: max' cannot be combined with 'cancel-in-progress: true'.", c.getRun(r3.GetId()).GetRun().GetError())
	r4 := c.pushN(4, workflowAt("concurrency: {group: g, queue: max, cancel-in-progress: true}\n", ""))
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_FAILURE, r4.GetConclusion())
	assert.Contains(t, c.getRun(r4.GetId()).GetRun().GetError(), "'queue: max' cannot be combined with 'cancel-in-progress: true'.")

	r2 := c.pushN(2, workflowAt("", "    concurrency:\n      group: g\n      queue: max\n      cancel-in-progress: ${{ github.ref == 'refs/heads/main' }}\n"))
	c.waitJobs("failed", r2, allStatus(pb.CiStatus_CI_STATUS_COMPLETED))
	assert.Equal(t, "'queue: max' cannot be combined with 'cancel-in-progress: true'.", strings.TrimSpace(c.jobsOf(r2)[0].GetError()))
}

// holdGroup writes a group behind the use case's back, as a crash would leave
// it: the entries are named and marked whatever the state of their runs.
func (c *ciHarness) holdGroup(group string, running *domain.CIConcurrencyEntry, pending ...domain.CIConcurrencyEntry) {
	c.t.Helper()
	require.NoError(c.t, c.forge.store.CI().UpdateConcurrency(c.ctx, c.h("proj"), group, func(st *domain.CIConcurrencyGroupState) error {
		st.Record.Running, st.Record.Pending = running, pending
		st.Marks = map[string]domain.CIConcurrencyMark{}
		if running != nil {
			st.Marks[running.Key()] = domain.CIConcurrencyMark{Entry: *running, Group: group, State: domain.CIConcurrencyHolding}
		}
		for _, p := range pending {
			st.Marks[p.Key()] = domain.CIConcurrencyMark{Entry: p, Group: group, State: domain.CIConcurrencyPending}
		}
		return nil
	}))
}

func (c *ciHarness) groupState(group string) (st domain.CIConcurrencyGroupState) {
	c.t.Helper()
	require.NoError(c.t, c.forge.store.CI().UpdateConcurrency(c.ctx, c.h("proj"), group, func(s *domain.CIConcurrencyGroupState) error {
		st = *s
		return nil
	}))
	return st
}

func (c *ciHarness) groupRecord(group string) domain.CIConcurrencyRecord {
	return c.groupState(group).Record
}

// markState is the mark of an entry in a group, "" when it has none.
func (c *ciHarness) markState(group string, e domain.CIConcurrencyEntry) domain.CIConcurrencyState {
	return c.groupState(group).Marks[e.Key()].State
}

// A crash between the write of an ended job and the release of its group leaves
// the record holding it: the next run to ask finds the entry dead and takes the
// group.
func TestCIConcurrencyHealsAfterACrash(t *testing.T) {
	c, runner := concurrencyHarness(t)
	r1 := c.pushN(1, workflowAt(wfGroup, ""))
	runner.runJob(runner.await(), pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	c.waitFor("the first run completes", func() bool { return c.getRun(r1.GetId()).GetRun().GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })
	c.waitFor("the group is released", func() bool { return c.groupRecord("Deploy").IsEmpty() })
	c.holdGroup("Deploy", &domain.CIConcurrencyEntry{RunID: r1.GetId(), Attempt: 1}) // the release that did not happen

	r2 := c.pushN(2, workflowAt(wfGroup, ""))
	job2 := runner.await()
	assert.Equal(t, r2.GetId(), job2.GetRunId(), "the dead entry does not hold the group")
	rec := c.groupRecord("Deploy")
	require.NotNil(t, rec.Running)
	assert.Equal(t, r2.GetId(), rec.Running.RunID)
}

// A run that is deleted (the retention sweep, not its repository) frees the
// group it held and the one it waited in.
func TestCIConcurrencyHealsAfterRunDeletion(t *testing.T) {
	c, runner := concurrencyHarness(t)
	r1 := c.pushN(1, workflowAt(wfGroup, ""))
	runner.runJob(runner.await(), pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	c.waitFor("completes", func() bool { return c.getRun(r1.GetId()).GetRun().GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })
	c.waitFor("released", func() bool { return c.groupRecord("Deploy").IsEmpty() })
	ghost := &domain.CIConcurrencyEntry{RunID: r1.GetId(), Attempt: 1}
	c.holdGroup("Deploy", ghost)
	ids, err := c.forge.store.CI().Sweep(c.ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Contains(t, ids, r1.GetId(), "the run is deleted")

	r2 := c.pushN(2, workflowAt(wfGroup, ""))
	job2 := runner.await()
	assert.Equal(t, r2.GetId(), job2.GetRunId(), "a deleted run does not hold the group")
}

// flakyStore fails the writes that match, as many times as fails says. The
// failure is returned before anything is written.
type flakyStore struct {
	ports.CIStore
	match func(ports.CIWrite) bool
	fails atomic.Int32
	hits  atomic.Int32
}

func (f *flakyStore) Apply(ctx context.Context, w ports.CIWrite) (uint64, error) {
	if f.match != nil && f.match(w) && f.fails.Add(-1) >= 0 {
		f.hits.Add(1)
		return 0, domain.ErrConflict
	}
	return f.CIStore.Apply(ctx, w)
}

// flakyConcurrencyHarness is a harness whose store fails the writes fs.match
// selects. retries is CIConfig.ConcurrencyRetries: 0 keeps the default, a
// negative number leaves the recovery of a lost delivery to the reconciliation.
func flakyConcurrencyHarness(t *testing.T, retries int) (*ciHarness, *fakeRunner, *flakyStore) {
	t.Helper()
	fs := &flakyStore{}
	c := newCIHarness(t, func(cfg *usecase.CIConfig) {
		cfg.CancelGrace, cfg.Tick, cfg.PollTimeout = 300*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond
		cfg.ConcurrencyRetries = retries
		fs.CIStore, cfg.Store = cfg.Store, fs
	})
	return c, c.registerRunner("alpha", "ubuntu-latest"), fs
}

// endsCancelled matches the write that ends a job of the run as cancelled.
func endsCancelled(runID int64) func(ports.CIWrite) bool {
	return func(w ports.CIWrite) bool {
		for _, j := range w.Jobs {
			if j.RunID == runID && j.Ended() && j.Conclusion == domain.CICancelled {
				return true
			}
		}
		return false
	}
}

// A replaced pending run whose cancel write fails once is cancelled by the
// retry.
func TestCIConcurrencyCancelSurvivesAFailedDelivery(t *testing.T) {
	c, runner, fs := flakyConcurrencyHarness(t, 0)
	c.pushN(1, workflowAt(wfGroup, ""))
	job1 := runner.await()
	r2 := c.pushN(2, workflowAt(wfGroup, ""))
	c.waitJobs("pending", r2, allStatus(pb.CiStatus_CI_STATUS_PENDING))

	fs.match = endsCancelled(r2.GetId())
	fs.fails.Store(1)
	r3 := c.pushN(3, workflowAt(wfGroup, ""))
	c.waitFor("the first delivery failed", func() bool { return fs.hits.Load() == 1 })
	rec := c.groupRecord("Deploy")
	require.Len(t, rec.Pending, 1)
	assert.Equal(t, r3.GetId(), rec.Pending[0].RunID)
	c.waitFor("the replaced run is cancelled by the retry", func() bool { return c.conclusionOf(r2) == pb.CiConclusion_CI_CONCLUSION_CANCELLED })
	assert.True(t, c.cancelAnnotation(r2, "Deploy"))
	runner.runJob(job1, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	assert.Equal(t, r3.GetId(), runner.await().GetRunId())
}

// Nothing is bounded: more replacements than any list could hold, every cancel
// write failing, and after the store works again the reconciliation discovers
// each victim through its "replaced" mark in the group, and cancels it.
func TestCIConcurrencyReconcileCancelsEveryReplacedVictim(t *testing.T) {
	const replaced = 35
	c, runner, fs := flakyConcurrencyHarness(t, -1)
	c.pushN(1, workflowAt(wfGroup, ""))
	runner.await()

	// Each new run replaces the one that waited before it; no cancel lands.
	fs.match = func(w ports.CIWrite) bool {
		for _, j := range w.Jobs {
			if j.Ended() && j.Conclusion == domain.CICancelled {
				return true
			}
		}
		return false
	}
	fs.fails.Store(1 << 20)
	var victims []*pb.Run
	prev := c.pushN(2, workflowAt(wfGroup, ""))
	c.waitJobs("pending", prev, allStatus(pb.CiStatus_CI_STATUS_PENDING))
	for i := range replaced {
		next := c.pushN(3+i, workflowAt(wfGroup, ""))
		c.waitFor("the newer run takes the waiting place", func() bool {
			rec := c.groupRecord("Deploy")
			return len(rec.Pending) == 1 && rec.Pending[0].RunID == next.GetId()
		})
		c.waitJobs("the newer run is pending", next, allStatus(pb.CiStatus_CI_STATUS_PENDING))
		victims = append(victims, prev)
		prev = next
	}
	for _, v := range victims {
		assert.NotEqual(t, pb.CiConclusion_CI_CONCLUSION_CANCELLED, c.conclusionOf(v), "no cancel landed yet")
	}
	// The test has no in-process retry (ConcurrencyRetries is negative): only
	// the reconciliation can recover the victims.
	fs.fails.Store(0)
	c.forge.CI.ReconcileConcurrency(c.ctx)
	for _, v := range victims {
		c.waitFor("a replaced run is cancelled", func() bool { return c.conclusionOf(v) == pb.CiConclusion_CI_CONCLUSION_CANCELLED })
		assert.True(t, c.cancelAnnotation(v, "Deploy"))
	}
	c.waitJobs("the last one still waits", prev, allStatus(pb.CiStatus_CI_STATUS_PENDING))
	assert.NotEqual(t, pb.CiConclusion_CI_CONCLUSION_CANCELLED, c.conclusionOf(prev))
}

// A running entry that cancel-in-progress replaced stays named with Canceling
// set; when its cancel is lost, the reconciliation asks again.
func TestCIConcurrencyReconcileAsksARunningVictimAgain(t *testing.T) {
	c, runner, fs := flakyConcurrencyHarness(t, -1)
	top := "concurrency:\n  group: Deploy\n  cancel-in-progress: true\n"
	r1 := c.pushN(1, workflowAt(top, ""))
	job1 := runner.await()
	watch, err := runner.jobAPI(job1).runner.WatchJob(c.ctx, connect.NewRequest(&pb.WatchJobRequest{JobId: job1.GetJobId()}))
	require.NoError(t, err)

	// The cancel of the running run cannot be written.
	fs.match = func(w ports.CIWrite) bool {
		for _, r := range w.Runs {
			if r.ID == r1.GetId() && r.CancelRequested {
				return true
			}
		}
		return false
	}
	fs.fails.Store(1 << 20)
	c.pushN(2, workflowAt(top, ""))
	c.waitFor("the cancel fails", func() bool { return fs.hits.Load() >= 1 })
	rec := c.groupRecord("Deploy")
	require.NotNil(t, rec.Running)
	assert.True(t, rec.Running.Canceling, "the victim stays named, marked")
	assert.Equal(t, r1.GetId(), rec.Running.RunID)

	fs.fails.Store(0)
	c.forge.CI.ReconcileConcurrency(c.ctx)
	var cancel *pb.CancelRequest
	for watch.Receive() {
		if m := watch.Msg().GetCancel(); m != nil {
			cancel = m
			break
		}
	}
	require.NotNil(t, cancel, "the runner is told after the reconciliation")
	// The runner stops: its watch ends with the job.
	_, err = runner.jobAPI(job1).runner.CompleteJob(c.ctx, connect.NewRequest(&pb.CompleteJobRequest{
		JobId: job1.GetJobId(), Conclusion: pb.CiConclusion_CI_CONCLUSION_CANCELLED,
	}))
	require.NoError(t, err)
}

// Crash between the write of the group and the write of the entry: the group
// names a pending run whose own record does not say it waits yet. It is not
// stale and not cancelled; the reconciliation plans it, which marks it
// pending, and the group goes on to it when the holder ends.
func TestCIConcurrencyCrashBetweenGroupAndEntry(t *testing.T) {
	c, runner := concurrencyHarness(t)
	c.pushN(1, workflowAt(wfGroup, ""))
	job1 := runner.await()

	// Stop the planning of the second run from marking it, as a crash would:
	// name it in the group by hand, behind the first.
	store := c.forge.store.CI()
	r2 := c.pushN(2, workflowAt(wfGroup, ""))
	c.waitJobs("pending", r2, allStatus(pb.CiStatus_CI_STATUS_PENDING))
	run, err := store.GetRun(c.ctx, c.h("proj"), r2.GetId())
	require.NoError(t, err)
	run.Concurrency.State = domain.CIConcurrencyIdle
	jobs, err := store.ListJobs(c.ctx, r2.GetId(), 1)
	require.NoError(t, err)
	for i := range jobs {
		jobs[i].Concurrency.State = domain.CIConcurrencyIdle
	}
	_, err = store.Apply(c.ctx, ports.CIWrite{Runs: []domain.CIRun{run}, Jobs: jobs})
	require.NoError(t, err)
	rec := c.groupRecord("Deploy")
	require.Len(t, rec.Pending, 1)
	require.Equal(t, r2.GetId(), rec.Pending[0].RunID, "the group names the run")
	assert.Equal(t, domain.CIConcurrencyPending, c.markState("Deploy", rec.Pending[0]), "and marks it, in the same batch")

	c.forge.CI.ReconcileConcurrency(c.ctx)
	c.waitFor("the entry is marked pending again", func() bool {
		r, err := store.GetRun(c.ctx, c.h("proj"), r2.GetId())
		return err == nil && r.Concurrency.State == domain.CIConcurrencyPending
	})
	time.Sleep(200 * time.Millisecond)
	assert.NotEqual(t, pb.CiConclusion_CI_CONCLUSION_CANCELLED, c.conclusionOf(r2), "a named entry is not a replaced one")
	rec = c.groupRecord("Deploy")
	require.Len(t, rec.Pending, 1)
	assert.Equal(t, r2.GetId(), rec.Pending[0].RunID)

	runner.runJob(job1, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	assert.Equal(t, r2.GetId(), runner.await().GetRunId())
}

// holdsGroup matches the write that tells a run it holds its group.
func holdsGroup(runID int64) func(ports.CIWrite) bool {
	return func(w ports.CIWrite) bool {
		for _, r := range w.Runs {
			if r.ID == runID && r.Concurrency.State == domain.CIConcurrencyHolding {
				return true
			}
		}
		return false
	}
}

// HIGH-2: a promotion whose planning fails once is tried again.
func TestCIConcurrencyPromotionIsRetried(t *testing.T) {
	c, runner, fs := flakyConcurrencyHarness(t, 0)
	c.pushN(1, workflowAt(wfGroup, ""))
	job1 := runner.await()
	r2 := c.pushN(2, workflowAt(wfGroup, ""))
	c.waitJobs("pending", r2, allStatus(pb.CiStatus_CI_STATUS_PENDING))

	fs.match = holdsGroup(r2.GetId())
	fs.fails.Store(1)
	runner.runJob(job1, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	c.waitFor("the first planning failed", func() bool { return fs.hits.Load() == 1 })
	job2 := runner.await()
	assert.Equal(t, r2.GetId(), job2.GetRunId(), "the retry planned the promoted run")
}

// HIGH-2: a holder that was never told it holds the group is planned by the
// reconciliation, whatever happened to the retries.
func TestCIConcurrencyReconcilePlansAStaleHolder(t *testing.T) {
	c, runner, fs := flakyConcurrencyHarness(t, -1)
	c.pushN(1, workflowAt(wfGroup, ""))
	job1 := runner.await()
	r2 := c.pushN(2, workflowAt(wfGroup, ""))
	c.waitJobs("pending", r2, allStatus(pb.CiStatus_CI_STATUS_PENDING))

	fs.match = holdsGroup(r2.GetId())
	fs.fails.Store(1000)
	runner.runJob(job1, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	c.waitFor("the planning fails", func() bool { return fs.hits.Load() >= 1 })
	rec := c.groupRecord("Deploy")
	require.NotNil(t, rec.Running)
	assert.Equal(t, r2.GetId(), rec.Running.RunID, "the group names the promoted run")
	c.waitJobs("which still shows pending", r2, allStatus(pb.CiStatus_CI_STATUS_PENDING))

	fs.fails.Store(0)
	c.forge.CI.ReconcileConcurrency(c.ctx)
	job2 := runner.await()
	assert.Equal(t, r2.GetId(), job2.GetRunId())
}

const siblingJobs = `    steps:
      - run: echo grouped
  sibling:
    runs-on: ubuntu-latest
    steps:
      - run: echo sibling
`

// jobWorkflow is a workflow of a grouped job with the given concurrency lines
// and an unrelated sibling.
func jobWorkflow(concurrency string) string {
	return "name: ci\non:\n  push:\n    branches: [main]\njobs:\n  grouped:\n    runs-on: ubuntu-latest\n" +
		concurrency + siblingJobs
}

// awaitKey fetches jobs until the one of the run with the key comes.
func (r *fakeRunner) awaitKey(runID int64, key string) *pb.Job {
	r.c.t.Helper()
	for range 20 {
		if j := r.await(); j.GetRunId() == runID && j.GetJobKey() == key {
			return j
		}
	}
	r.c.t.Fatalf("no job %s of run %d", key, runID)
	return nil
}

// T-2: a job-level cancel-in-progress (true, or an expression) cancels the
// running job of the group only: its sibling goes on, and the replacement
// becomes assignable.
func TestCIJobConcurrencyCancelInProgress(t *testing.T) {
	for _, tt := range []struct{ name, cancel string }{
		{"true", "true"},
		{"expression", "${{ github.ref == 'refs/heads/main' }}"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, runner := concurrencyHarness(t)
			wf := jobWorkflow("    concurrency:\n      group: jobgroup\n      cancel-in-progress: " + tt.cancel + "\n")
			r1 := c.pushN(1, wf)
			grouped1, sibling1 := runner.awaitKey(r1.GetId(), "grouped"), runner.awaitKey(r1.GetId(), "sibling")
			watch, err := runner.jobAPI(grouped1).runner.WatchJob(c.ctx, connect.NewRequest(&pb.WatchJobRequest{JobId: grouped1.GetJobId()}))
			require.NoError(t, err)

			r2 := c.pushN(2, wf)
			var cancel *pb.CancelRequest
			for watch.Receive() {
				if m := watch.Msg().GetCancel(); m != nil {
					cancel = m
					break
				}
			}
			require.NotNil(t, cancel, "the grouped job is asked to stop")
			assert.Contains(t, cancel.GetMessage(), "higher priority waiting request for 'jobgroup'")

			// The sibling of the first run was not touched.
			runner.heartbeat(sibling1)
			for _, j := range c.jobsOf(r1) {
				if j.GetKey() == "sibling" {
					assert.NotEqual(t, pb.CiStatus_CI_STATUS_COMPLETED, j.GetStatus())
					assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_UNSPECIFIED, j.GetConclusion())
				}
			}
			_, err = runner.jobAPI(grouped1).runner.CompleteJob(c.ctx, connect.NewRequest(&pb.CompleteJobRequest{
				JobId: grouped1.GetJobId(), Conclusion: pb.CiConclusion_CI_CONCLUSION_CANCELLED,
			}))
			require.NoError(t, err)
			runner.runJob(sibling1, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)

			grouped2 := runner.awaitKey(r2.GetId(), "grouped")
			assert.Equal(t, r2.GetId(), grouped2.GetRunId(), "the replacement is assignable")
			c.waitFor("the first run is over", func() bool { return c.getRun(r1.GetId()).GetRun().GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })
			for _, j := range c.jobsOf(r1) {
				want := pb.CiConclusion_CI_CONCLUSION_CANCELLED
				if j.GetKey() == "sibling" {
					want = pb.CiConclusion_CI_CONCLUSION_SUCCESS
				}
				assert.Equal(t, want, j.GetConclusion(), j.GetKey())
			}
		})
	}
}

// T-3: the server restarts: the groups are repaired at startup. A group held by
// an ended run is freed and the run behind it is planned; a group whose only
// run is over is removed.
func TestCIConcurrencyReconcileAtRestart(t *testing.T) {
	c, runner := concurrencyHarness(t)
	r1 := c.pushN(1, workflowAt(wfGroup, ""))
	runner.await()
	r2 := c.pushN(2, workflowAt(wfGroup, ""))
	c.waitJobs("pending", r2, allStatus(pb.CiStatus_CI_STATUS_PENDING))

	// The first run ends in the store without the release (the crash), and a
	// second group is held by a run that is over.
	store := c.forge.store.CI()
	jobs, err := store.ListJobs(c.ctx, r1.GetId(), 1)
	require.NoError(t, err)
	run, err := store.GetRun(c.ctx, c.h("proj"), r1.GetId())
	require.NoError(t, err)
	jobs[0].Phase, jobs[0].Conclusion = domain.CIPhaseEnded, domain.CISuccess
	run.Status, run.Conclusion, run.CompletedAt = domain.CICompleted, domain.CISuccess, time.Now()
	_, err = store.Apply(c.ctx, ports.CIWrite{Jobs: jobs, Runs: []domain.CIRun{run}})
	require.NoError(t, err)
	c.holdGroup("Stale", &domain.CIConcurrencyEntry{RunID: r1.GetId(), Attempt: 1})
	rec := c.groupRecord("Deploy")
	require.NotNil(t, rec.Running)
	require.Equal(t, r1.GetId(), rec.Running.RunID, "the record still holds the ended run")

	// Restart on the same data.
	cfg, hookExe := c.forge.cfg, binary(t)
	c.srv.Close()
	c.forge.Close(c.ctx)
	f2, err := newForge(c.ctx, cfg, options{hookExe: hookExe, ci: func(cc *usecase.CIConfig) {
		cc.CancelGrace, cc.Tick, cc.PollTimeout = 300*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond
	}})
	require.NoError(t, err)
	t.Cleanup(func() { f2.Close(context.WithoutCancel(c.ctx)) })
	srv := httptest.NewServer(f2.server.Handler())
	t.Cleanup(srv.Close)

	var got *pb.Job
	c.waitFor("the waiting run is planned by the startup repair", func() bool {
		res, err := newClients(srv.URL+"/-/api", runner.secret).runner.FetchJob(c.ctx, connect.NewRequest(&pb.FetchJobRequest{}))
		if err == nil && res.Msg.GetJob() != nil {
			got = res.Msg.GetJob()
		}
		return got != nil
	})
	assert.Equal(t, r2.GetId(), got.GetRunId())
	groups, err := f2.store.CI().ListConcurrency(c.ctx)
	require.NoError(t, err)
	require.Len(t, groups, 1, "the stale group is gone")
	assert.Equal(t, "DEPLOY", groups[0].Group)
	require.NotNil(t, groups[0].State.Record.Running)
	assert.Equal(t, r2.GetId(), groups[0].State.Record.Running.RunID)
	assert.Empty(t, groups[0].State.Record.Pending)
}

// Both writes of a run's own record fail when it queues (its "pending" state),
// and the cancel of the run fails for good: the group alone knows B. A newer run
// replaces B; B's mark is "replaced", written in the batch that unnamed it. After
// the store works again the reconciliation finds B from that mark and cancels it.
func TestCIConcurrencyReplacedRunWithLostWritesIsCancelled(t *testing.T) {
	c, runner, fs := flakyConcurrencyHarness(t, -1)
	r1 := c.pushN(1, workflowAt(wfGroup, ""))
	runner.await()

	// B queues, but no run after the first gets the pending state in its own
	// record, and no cancel lands.
	fs.match = func(w ports.CIWrite) bool {
		for _, r := range w.Runs {
			if r.ID > r1.GetId() && r.Concurrency.State == domain.CIConcurrencyPending {
				return true
			}
		}
		for _, j := range w.Jobs {
			if j.RunID > r1.GetId() && j.Ended() && j.Conclusion == domain.CICancelled {
				return true
			}
		}
		return false
	}
	fs.fails.Store(1 << 20)
	b := c.pushN(2, workflowAt(wfGroup, ""))
	c.waitFor("B is named by the group", func() bool {
		rec := c.groupRecord("Deploy")
		return len(rec.Pending) == 1 && rec.Pending[0].RunID == b.GetId()
	})
	bEntry := domain.CIConcurrencyEntry{RunID: b.GetId(), Attempt: 1}
	assert.Equal(t, domain.CIConcurrencyPending, c.markState("Deploy", bEntry), "the group batch marked it")

	// C replaces B: B is unnamed and marked replaced in one batch.
	c.pushN(3, workflowAt(wfGroup, ""))
	c.waitFor("B is marked replaced", func() bool { return c.markState("Deploy", bEntry) == domain.CIConcurrencyReplaced })
	rec := c.groupRecord("Deploy")
	require.Len(t, rec.Pending, 1)
	assert.NotEqual(t, b.GetId(), rec.Pending[0].RunID, "B is no longer named")
	assert.NotEqual(t, pb.CiConclusion_CI_CONCLUSION_CANCELLED, c.conclusionOf(b))

	fs.fails.Store(0)
	c.forge.CI.ReconcileConcurrency(c.ctx)
	c.waitFor("B ends cancelled", func() bool { return c.conclusionOf(b) == pb.CiConclusion_CI_CONCLUSION_CANCELLED })
	assert.True(t, c.cancelAnnotation(b, "Deploy"))
	c.waitFor("its mark goes with it", func() bool { return c.markState("Deploy", bEntry) == "" })
}

// As above, but only B's own writes fail: C's record knows it is pending, so
// the reconciliation does not plan C again, and no other decision on the group
// prunes B's mark. The cancel that ends B frees the group that decided it,
// although B's own record names no group.
func TestCIConcurrencyCancelOfARunUnawareOfItsGroupDropsItsMark(t *testing.T) {
	c, runner, fs := flakyConcurrencyHarness(t, -1)
	r1 := c.pushN(1, workflowAt(wfGroup, ""))
	runner.await()

	bID := r1.GetId() + 1 // run ids are taken in sequence
	fs.match = func(w ports.CIWrite) bool {
		for _, r := range w.Runs {
			if r.ID == bID && r.Concurrency.State == domain.CIConcurrencyPending {
				return true
			}
		}
		for _, j := range w.Jobs {
			if j.RunID == bID && j.Ended() && j.Conclusion == domain.CICancelled {
				return true
			}
		}
		return false
	}
	fs.fails.Store(1 << 20)
	b := c.pushN(2, workflowAt(wfGroup, ""))
	require.Equal(t, bID, b.GetId())
	bEntry := domain.CIConcurrencyEntry{RunID: bID, Attempt: 1}
	c.waitFor("B is named by the group", func() bool { return c.markState("Deploy", bEntry) == domain.CIConcurrencyPending })

	r3 := c.pushN(3, workflowAt(wfGroup, ""))
	c.waitFor("B is marked replaced", func() bool { return c.markState("Deploy", bEntry) == domain.CIConcurrencyReplaced })
	c.waitJobs("C knows it waits", r3, func(js []*pb.RunJob) bool {
		return len(js) == 1 && js[0].GetStatus() == pb.CiStatus_CI_STATUS_PENDING
	})
	run, err := c.forge.store.CI().GetRun(c.ctx, c.h("proj"), bID)
	require.NoError(t, err)
	require.Empty(t, run.Concurrency.Group, "B's own record names no group")

	fs.fails.Store(0)
	c.forge.CI.ReconcileConcurrency(c.ctx)
	c.waitFor("B ends cancelled", func() bool { return c.conclusionOf(b) == pb.CiConclusion_CI_CONCLUSION_CANCELLED })
	c.waitFor("its mark goes with it", func() bool { return c.markState("Deploy", bEntry) == "" })
	rec := c.groupRecord("Deploy")
	require.Len(t, rec.Pending, 1)
	assert.Equal(t, r3.GetId(), rec.Pending[0].RunID, "C still waits")
}

// A grouped job B is replaced by C, the cancel never lands, C is promoted when
// the running job ends, and an unrelated sibling of B completes and advances B:
// B's grouped job must not take the free place, it ends cancelled and never runs.
func TestCIJobConcurrencyReplacedJobNeverAcquiresAgain(t *testing.T) {
	c, runner, fs := flakyConcurrencyHarness(t, -1)
	wf := jobWorkflow("    concurrency: jobgroup\n")
	r1 := c.pushN(1, wf)
	grouped1, sibling1 := runner.awaitKey(r1.GetId(), "grouped"), runner.awaitKey(r1.GetId(), "sibling")

	rB := c.pushN(2, wf)
	siblingB := runner.awaitKey(rB.GetId(), "sibling") // B's unrelated job is running
	c.waitJobs("B's grouped job waits", rB, func(js []*pb.RunJob) bool {
		for _, j := range js {
			if j.GetKey() == "grouped" {
				return j.GetStatus() == pb.CiStatus_CI_STATUS_PENDING
			}
		}
		return false
	})

	fs.match = endsCancelled(rB.GetId())
	fs.fails.Store(1 << 20)
	rC := c.pushN(3, wf)
	bEntry := domain.CIConcurrencyEntry{RunID: rB.GetId(), Attempt: 1, JobID: domain.NewJobID(rB.GetId(), 0, 0)}
	c.waitFor("B's grouped job is replaced", func() bool { return c.markState("jobgroup", bEntry) == domain.CIConcurrencyReplaced })

	// The running job ends: C's grouped job is promoted.
	runner.runJob(grouped1, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	runner.runJob(sibling1, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	groupedC := runner.awaitKey(rC.GetId(), "grouped")
	rec := c.groupRecord("jobgroup")
	require.NotNil(t, rec.Running)
	assert.Equal(t, rC.GetId(), rec.Running.RunID)

	// B's sibling completes: B is advanced, its grouped job asks for the group.
	runner.runJob(siblingB, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	c.waitFor("B was advanced", func() bool {
		for _, j := range c.jobsOf(rB) {
			if j.GetKey() == "sibling" {
				return j.GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED
			}
		}
		return false
	})
	time.Sleep(300 * time.Millisecond)
	rec = c.groupRecord("jobgroup")
	require.NotNil(t, rec.Running)
	assert.Equal(t, rC.GetId(), rec.Running.RunID, "C still holds the group")
	if len(rec.Pending) > 0 {
		assert.NotEqual(t, rB.GetId(), rec.Pending[0].RunID, "B did not take the waiting place back")
	}
	// Nothing of B's grouped job is given to a runner.
	for range 5 {
		j, err := runner.fetch("")
		require.NoError(t, err)
		if j != nil {
			assert.False(t, j.GetRunId() == rB.GetId() && j.GetJobKey() == "grouped", "B's grouped job must not run")
		}
	}

	// Once the store works, the reconciliation cancels it.
	fs.fails.Store(0)
	c.forge.CI.ReconcileConcurrency(c.ctx)
	c.waitFor("B's grouped job ends cancelled", func() bool {
		for _, j := range c.jobsOf(rB) {
			if j.GetKey() == "grouped" {
				return j.GetConclusion() == pb.CiConclusion_CI_CONCLUSION_CANCELLED
			}
		}
		return false
	})
	runner.runJob(groupedC, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
}

// The same, with an empty group: B's grouped job is replaced and cancelled
// before its replacement ends too; a replaced job never runs even when the
// group is free.
func TestCIJobConcurrencyReplacedJobDoesNotTakeAnEmptyGroup(t *testing.T) {
	c, runner, fs := flakyConcurrencyHarness(t, -1)
	wf := jobWorkflow("    concurrency: jobgroup\n")
	r1 := c.pushN(1, wf)
	grouped1, sibling1 := runner.awaitKey(r1.GetId(), "grouped"), runner.awaitKey(r1.GetId(), "sibling")
	rB := c.pushN(2, wf)
	siblingB := runner.awaitKey(rB.GetId(), "sibling")
	c.waitJobs("pending", rB, func(js []*pb.RunJob) bool {
		for _, j := range js {
			if j.GetKey() == "grouped" {
				return j.GetStatus() == pb.CiStatus_CI_STATUS_PENDING
			}
		}
		return false
	})
	fs.match = endsCancelled(rB.GetId())
	fs.fails.Store(1 << 20)
	rC := c.pushN(3, wf)
	bEntry := domain.CIConcurrencyEntry{RunID: rB.GetId(), Attempt: 1, JobID: domain.NewJobID(rB.GetId(), 0, 0)}
	c.waitFor("replaced", func() bool { return c.markState("jobgroup", bEntry) == domain.CIConcurrencyReplaced })

	// Both the running job and the promoted one finish: the group is empty.
	runner.runJob(grouped1, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	runner.runJob(sibling1, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	runner.runJob(runner.awaitKey(rC.GetId(), "grouped"), pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	c.waitFor("the group is free", func() bool { return c.groupRecord("jobgroup").Running == nil })

	runner.runJob(siblingB, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil) // advances B
	time.Sleep(300 * time.Millisecond)
	assert.Nil(t, c.groupRecord("jobgroup").Running, "B's replaced job did not take the empty group")
	for range 5 {
		j, err := runner.fetch("")
		require.NoError(t, err)
		if j != nil {
			assert.False(t, j.GetRunId() == rB.GetId() && j.GetJobKey() == "grouped", "B's grouped job must not run")
		}
	}
	fs.fails.Store(0)
	c.forge.CI.ReconcileConcurrency(c.ctx)
	c.waitFor("it ends cancelled", func() bool {
		for _, j := range c.jobsOf(rB) {
			if j.GetKey() == "grouped" {
				return j.GetConclusion() == pb.CiConclusion_CI_CONCLUSION_CANCELLED
			}
		}
		return false
	})
}

// Crash between the group batch and the write of a JOB's own state: the group
// names (and marks) the grouped job as waiting, the job's record does not say so
// yet. It is not replaced and not cancelled; reconciliation tells it, and it runs
// when its turn comes.
func TestCIJobConcurrencyCrashBetweenGroupAndJob(t *testing.T) {
	c, runner := concurrencyHarness(t)
	group := "    concurrency: jobgroup\n"
	c.pushN(1, workflowAt("", group))
	job1 := runner.await()
	r2 := c.pushN(2, workflowAt("", group))
	c.waitJobs("pending", r2, allStatus(pb.CiStatus_CI_STATUS_PENDING))

	store := c.forge.store.CI()
	jobs, err := store.ListJobs(c.ctx, r2.GetId(), 1)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	entry := domain.CIConcurrencyEntry{RunID: r2.GetId(), Attempt: 1, JobID: jobs[0].ID}
	jobs[0].Concurrency.State = domain.CIConcurrencyIdle // the write that was lost
	_, err = store.Apply(c.ctx, ports.CIWrite{Jobs: jobs})
	require.NoError(t, err)
	rec := c.groupRecord("jobgroup")
	require.Len(t, rec.Pending, 1)
	assert.Equal(t, entry.JobID, rec.Pending[0].JobID, "the group names the job")
	assert.Equal(t, domain.CIConcurrencyPending, c.markState("jobgroup", entry), "and its mark says so")

	c.forge.CI.ReconcileConcurrency(c.ctx)
	c.waitFor("the job is told it waits", func() bool {
		js, err := store.ListJobs(c.ctx, r2.GetId(), 1)
		return err == nil && js[0].Concurrency.State == domain.CIConcurrencyPending
	})
	time.Sleep(200 * time.Millisecond)
	assert.NotEqual(t, pb.CiConclusion_CI_CONCLUSION_CANCELLED, c.conclusionOf(r2), "a named job is not a replaced one")
	assert.Equal(t, domain.CIConcurrencyPending, c.markState("jobgroup", entry))

	runner.runJob(job1, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	assert.Equal(t, r2.GetId(), runner.await().GetRunId(), "it runs when the group is free")
}

// plannedWrite matches a write of a run after the first that stores planned jobs.
func plannedWrite(after int64) func(ports.CIWrite) bool {
	return func(w ports.CIWrite) bool {
		for _, j := range w.Jobs {
			if j.RunID > after && j.Planned {
				return true
			}
		}
		return false
	}
}

const matrixGroupWorkflow = `name: b
on:
  push:
    branches: [other]
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        n: [1, 2, 3]
    concurrency: shared
    steps:
      - run: echo ${{ matrix.n }}
`

const holderWorkflow = `name: a
on:
  push:
    branches: [main]
jobs:
  hold:
    runs-on: ubuntu-latest
    concurrency: shared
    steps:
      - run: echo hold
`

// Run B plans three legs of a matrix that share a group held by A. The legs
// replace one another (leg 0 by leg 1 by leg 2: only one waits), the group
// batches commit, and the write of B's planned jobs fails, leaving the
// placeholder of the job in the store. After the store recovers, the
// reconciliation must plan B before it cancels anything: only legs 0 and 1
// end cancelled, and leg 2, the legitimate waiting one, runs when A ends.
func TestCIJobConcurrencyRecoveryPlansTheMatrixBeforeCancellingALeg(t *testing.T) {
	c, runner, fs := flakyConcurrencyHarness(t, -1)
	a := c.push("main", map[string]string{".github/workflows/a.yml": holderWorkflow}, "")
	_ = a
	holder := runner.await()
	require.Equal(t, "hold", holder.GetJobKey())
	aRun := holder.GetRunId()

	fs.match = plannedWrite(aRun)
	fs.fails.Store(1 << 20)
	c.push("other", map[string]string{".github/workflows/b.yml": matrixGroupWorkflow}, "")
	c.waitFor("B's run exists", func() bool {
		for _, r := range c.runs() {
			if r.GetId() > aRun {
				return true
			}
		}
		return false
	})
	var bRun *pb.Run
	for _, r := range c.runs() {
		if r.GetId() > aRun {
			bRun = r
		}
	}
	require.NotNil(t, bRun)
	g := domain.ConcurrencyKey("shared")
	c.waitFor("the group batches committed: two legs are marked replaced", func() bool {
		replaced := 0
		for _, m := range c.groupState(g).Marks {
			if m.Entry.RunID == bRun.GetId() && m.State == domain.CIConcurrencyReplaced {
				replaced++
			}
		}
		return replaced == 2
	})
	assert.Equal(t, 1, len(c.jobsOf(bRun)), "B's job is still the placeholder: its planned legs were never stored")

	fs.fails.Store(0)
	c.forge.CI.ReconcileConcurrency(c.ctx)
	c.waitFor("B has its three legs", func() bool { return len(c.jobsOf(bRun)) == 3 })
	c.waitFor("legs 0 and 1 end cancelled", func() bool {
		n := 0
		for _, j := range c.jobsOf(bRun) {
			if j.GetConclusion() == pb.CiConclusion_CI_CONCLUSION_CANCELLED {
				n++
			}
		}
		return n == 2
	})
	byCombo := map[string]*pb.RunJob{}
	for _, j := range c.jobsOf(bRun) {
		byCombo[j.GetName()] = j
	}
	var waiting []*pb.RunJob
	for _, j := range c.jobsOf(bRun) {
		if j.GetConclusion() != pb.CiConclusion_CI_CONCLUSION_CANCELLED {
			waiting = append(waiting, j)
		}
	}
	require.Len(t, waiting, 1, "exactly one leg is not cancelled")
	assert.Equal(t, pb.CiStatus_CI_STATUS_PENDING, waiting[0].GetStatus(), "the final leg waits for A")
	assert.Contains(t, waiting[0].GetName(), "3", "and it is the last leg")

	runner.runJob(holder, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	last := runner.await()
	assert.Equal(t, bRun.GetId(), last.GetRunId())
	assert.Equal(t, waiting[0].GetId(), last.GetJobId(), "the final leg runs when A ends")
	runner.runJob(last, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
}

// Stale marks (of runs that are gone) are pruned a bounded number per
// transaction: a decision on a group with a long backlog succeeds at once, and
// the reconciliation takes the others, a bounded batch at a time.
func TestCIConcurrencyPrunesStaleMarksInBoundedBatches(t *testing.T) {
	const stale, bound = 10, 3
	c := newCIHarness(t, func(cfg *usecase.CIConfig) {
		cfg.CancelGrace, cfg.Tick, cfg.PollTimeout = 300*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond
		cfg.ConcurrencyPruneBatch = bound
	})
	runner := c.registerRunner("alpha", "ubuntu-latest")
	require.NoError(t, c.forge.store.CI().UpdateConcurrency(c.ctx, c.h("proj"), "Deploy", func(st *domain.CIConcurrencyGroupState) error {
		st.Marks = map[string]domain.CIConcurrencyMark{}
		for i := range stale {
			e := domain.CIConcurrencyEntry{RunID: int64(9000 + i), Attempt: 1, JobID: "9000-0-0"}
			st.Marks[e.Key()] = domain.CIConcurrencyMark{Entry: e, Group: "Deploy", State: domain.CIConcurrencyPending}
		}
		return nil
	}))
	require.Len(t, c.groupState("Deploy").Marks, stale)

	r := c.pushN(1, workflowAt(wfGroup, ""))
	job := runner.await()
	assert.Equal(t, r.GetId(), job.GetRunId(), "the decision succeeded with the backlog in place")
	st := c.groupState("Deploy")
	require.NotNil(t, st.Record.Running)
	assert.Equal(t, r.GetId(), st.Record.Running.RunID)
	assert.Len(t, st.Marks, stale-bound+1, "one transaction pruned a bounded number of the stale marks, and marked the run")

	c.forge.CI.ReconcileConcurrency(c.ctx)
	st = c.groupState("Deploy")
	assert.Len(t, st.Marks, 1, "the reconciliation prunes the rest, a batch at a time")
	assert.Contains(t, st.Marks, domain.CIConcurrencyEntry{RunID: r.GetId(), Attempt: 1}.Key())
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
}

func (c *ciHarness) hasAnnotation(run *pb.Run, msg string) bool {
	for _, j := range c.jobsOf(run) {
		for _, a := range c.annotations(run.GetId(), j.GetId()).GetAnnotations() {
			if a.GetAnnotation().GetMessage() == msg {
				return true
			}
		}
	}
	return false
}

// pendingRuns is the waiting line of a group, as run ids.
func (c *ciHarness) pendingRuns(group string) []int64 {
	var ids []int64
	for _, p := range c.groupRecord(group).Pending {
		ids = append(ids, p.RunID)
	}
	return ids
}

func (c *ciHarness) completed(run *pb.Run) bool {
	return c.getRun(run.GetId()).GetRun().GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED
}

// queue: max at the workflow and at the job level: three runs of one group
// run one after the other in the order they started waiting, none cancelled.
func TestCIConcurrencyQueueMaxRunsInOrder(t *testing.T) {
	for _, tt := range []struct{ name, top, job string }{
		{"workflow", "concurrency:\n  group: Deploy\n  queue: max\n", ""},
		{"job", "", "    concurrency:\n      group: Deploy\n      queue: max\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, runner := concurrencyHarness(t)
			wf := workflowAt(tt.top, tt.job)
			r1 := c.pushN(1, wf)
			job1 := runner.await()
			require.Equal(t, r1.GetId(), job1.GetRunId())
			r2 := c.pushN(2, wf)
			c.waitJobs("the second run is pending", r2, allStatus(pb.CiStatus_CI_STATUS_PENDING))
			r3 := c.pushN(3, wf)
			c.waitJobs("the third run is pending", r3, allStatus(pb.CiStatus_CI_STATUS_PENDING))
			assert.Equal(t, []int64{r2.GetId(), r3.GetId()}, c.pendingRuns("Deploy"), "both wait, in order")
			rec := c.groupRecord("Deploy")
			assert.True(t, rec.Running.Max && rec.Pending[0].Max && rec.Pending[1].Max, "each entry records its queue mode")

			runner.runJob(job1, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
			job2 := runner.await()
			assert.Equal(t, r2.GetId(), job2.GetRunId(), "the head of the line goes first")
			assert.Equal(t, []int64{r3.GetId()}, c.pendingRuns("Deploy"))
			runner.runJob(job2, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
			job3 := runner.await()
			assert.Equal(t, r3.GetId(), job3.GetRunId())
			runner.runJob(job3, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
			for _, r := range []*pb.Run{r1, r2, r3} {
				c.waitFor("the run completes", func() bool { return c.completed(r) })
				assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_SUCCESS, c.conclusionOf(r))
			}
			c.waitFor("the group is free", func() bool { return c.groupRecord("Deploy").IsEmpty() })
		})
	}
}

// A queue: max entry that finds the line full is cancelled with ermite's
// annotation; the waiting ones stay. The cap is lowered to 2 here (it is
// domain.CIMaxConcurrencyQueue, 100, by default; see the domain tests).
func TestCIConcurrencyQueueMaxFullLineCancelsTheNewcomer(t *testing.T) {
	c := newCIHarness(t, func(cfg *usecase.CIConfig) {
		cfg.CancelGrace, cfg.Tick, cfg.PollTimeout = 300*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond
		cfg.ConcurrencyQueueMax = 2
	})
	runner := c.registerRunner("alpha", "ubuntu-latest")
	wf := workflowAt("concurrency:\n  group: Deploy\n  queue: max\n", "")
	r1 := c.pushN(1, wf)
	job1 := runner.await()
	r2 := c.pushN(2, wf)
	c.waitJobs("pending", r2, allStatus(pb.CiStatus_CI_STATUS_PENDING))
	r3 := c.pushN(3, wf)
	c.waitJobs("pending", r3, allStatus(pb.CiStatus_CI_STATUS_PENDING))

	r4 := c.pushN(4, wf)
	c.waitFor("the newcomer is cancelled", func() bool { return c.completed(r4) })
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_CANCELLED, c.conclusionOf(r4))
	assert.True(t, c.hasAnnotation(r4, domain.ConcurrencyQueueFullMessage("Deploy")))
	assert.False(t, c.cancelAnnotation(r4, "Deploy"))
	assert.Equal(t, []int64{r2.GetId(), r3.GetId()}, c.pendingRuns("Deploy"), "the line is kept")
	assert.False(t, c.groupRecord("Deploy").Names(domain.CIConcurrencyEntry{RunID: r4.GetId(), Attempt: 1}), "the newcomer was never named")
	assert.False(t, c.completed(r1))
	assert.False(t, c.completed(r2))
	assert.False(t, c.completed(r3))

	runner.runJob(job1, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	job2 := runner.await()
	assert.Equal(t, r2.GetId(), job2.GetRunId())
	runner.runJob(job2, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	job3 := runner.await()
	assert.Equal(t, r3.GetId(), job3.GetRunId())
	runner.runJob(job3, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	c.waitFor("the last run completes", func() bool { return c.completed(r3) })
}

// ermite's rule for a mixed group: a queue: single entry that starts waiting
// cancels every waiting entry, queue: max ones included, and is then the only
// one; the running entry is left alone (no cancel-in-progress).
func TestCIConcurrencyQueueSingleCancelsTheMaxLine(t *testing.T) {
	c, runner := concurrencyHarness(t)
	maxWF := workflowAt("concurrency:\n  group: Deploy\n  queue: max\n", "")
	r1 := c.pushN(1, maxWF)
	job1 := runner.await()
	r2 := c.pushN(2, maxWF)
	c.waitJobs("pending", r2, allStatus(pb.CiStatus_CI_STATUS_PENDING))
	r3 := c.pushN(3, maxWF)
	c.waitJobs("pending", r3, allStatus(pb.CiStatus_CI_STATUS_PENDING))

	r4 := c.pushN(4, workflowAt(wfGroup, ""))
	c.waitJobs("the single run is pending", r4, allStatus(pb.CiStatus_CI_STATUS_PENDING))
	for _, r := range []*pb.Run{r2, r3} {
		c.waitFor("the max run is cancelled", func() bool { return c.completed(r) })
		assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_CANCELLED, c.conclusionOf(r))
		assert.True(t, c.cancelAnnotation(r, "Deploy"))
	}
	assert.Equal(t, []int64{r4.GetId()}, c.pendingRuns("Deploy"))
	assert.False(t, c.completed(r1), "the running run is left alone")

	runner.runJob(job1, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	job4 := runner.await()
	assert.Equal(t, r4.GetId(), job4.GetRunId())
	runner.runJob(job4, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	c.waitFor("the single run completes", func() bool { return c.completed(r4) })
}
