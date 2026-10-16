// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/usecase"
)

// CI-3 D5, AC5: schedule, with an injected clock.

// testClock is a clock the test moves.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) set(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		panic(err)
	}
	c.t = t
}

func scheduleWorkflow(crons ...string) map[string]string {
	on := "on:\n  schedule:\n"
	for _, c := range crons {
		on += "    - cron: '" + c + "'\n"
	}
	return map[string]string{".github/workflows/nightly.yml": "name: nightly\n" + on + "jobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"}
}

func newScheduleHarness(t *testing.T, start string) (*ciHarness, *testClock, *fakeRunner) {
	t.Helper()
	clk := &testClock{}
	clk.set(start)
	c := newCIHarness(t, func(cfg *usecase.CIConfig) {
		cfg.Clock = clk
		cfg.ScheduleTick = time.Hour // the tests call ScheduleNow
	})
	return c, clk, c.registerRunner("alpha", "ubuntu-latest")
}

func createRepoParams(name domain.RepoName) ports.CreateRepoParams {
	return ports.CreateRepoParams{Name: name, Visibility: domain.VisibilityPrivate}
}

func (c *ciHarness) scheduled() []*pb.Run { return scheduleRuns(c.runsOf("nightly")) }

func scheduleRuns(runs []*pb.Run) []*pb.Run {
	var out []*pb.Run
	for _, r := range runs {
		if r.GetEvent() == "schedule" {
			out = append(out, r)
		}
	}
	return out
}

func TestScheduleFiresOncePerMatchingMinuteWithoutCatchUp(t *testing.T) {
	c, clk, runner := newScheduleHarness(t, "2026-03-04 10:00:30")
	c.push("main", scheduleWorkflow("*/5 * * * *"), "")
	ci := c.forge.CI
	tick := func(at string) {
		clk.set(at)
		ci.ScheduleNow(c.ctx)
	}

	tick("2026-03-04 10:00:40")
	assert.Empty(t, c.scheduled(), "the minute ermite started in belongs to the process before it")

	tick("2026-03-04 10:05:10")
	require.Len(t, c.scheduled(), 1)
	run := c.scheduled()[0]
	assert.Equal(t, "schedule", run.GetEvent())
	assert.Equal(t, "refs/heads/main", run.GetRef())
	assert.Equal(t, c.ref("refs/heads/main"), run.GetSha(), "the latest commit of the default branch")
	assert.Equal(t, "root", run.GetActor(), "whoever pushed the tip (the root token here)")
	assert.True(t, run.GetTrusted())

	tick("2026-03-04 10:05:50")
	tick("2026-03-04 10:05:59")
	assert.Len(t, c.scheduled(), 1, "a minute fires once, however many ticks it holds")
	tick("2026-03-04 10:06:00")
	tick("2026-03-04 10:09:00")
	assert.Len(t, c.scheduled(), 1, "minutes that do not match fire nothing")

	// ermite was busy or down for the minutes 10:10 and 10:15: they are not made up.
	tick("2026-03-04 10:20:00")
	assert.Len(t, c.scheduled(), 2, "only the minute the clock reached fires, not the ones skipped")
	assert.Equal(t, time.Date(2026, 3, 4, 10, 20, 0, 0, time.UTC), ci.SchedulePassed())

	job := runner.await()
	gh := c.ghContext(job)
	assert.Equal(t, "schedule", gh["event_name"])
	assert.Equal(t, "refs/heads/main", gh["ref"])
	assert.Equal(t, "*/5 * * * *", gh["event"].(map[string]any)["schedule"], "the payload is the cron entry")
	assert.True(t, job.GetTrusted())
}

// Several cron entries of one workflow that match the same minute start one
// run, with the first matching entry as payload; an entry that matches another
// minute starts its own.
func TestScheduleSeveralMatchingCronsOfOneWorkflowStartOneRun(t *testing.T) {
	c, clk, runner := newScheduleHarness(t, "2026-03-04 10:00:30")
	c.push("main", scheduleWorkflow("*/2 * * * *", "* * * * *", "7 10 * * *"), "")
	ci := c.forge.CI

	clk.set("2026-03-04 10:02:00") // the first and the second entries match
	ci.ScheduleNow(c.ctx)
	require.Len(t, c.scheduled(), 1, "one run for the workflow, not one per entry")
	job := runner.await()
	assert.Equal(t, "*/2 * * * *", c.ghContext(job)["event"].(map[string]any)["schedule"], "the first entry that matches")
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")

	clk.set("2026-03-04 10:03:00") // only the second
	ci.ScheduleNow(c.ctx)
	require.Len(t, c.scheduled(), 2)
	job = runner.await()
	assert.Equal(t, "* * * * *", c.ghContext(job)["event"].(map[string]any)["schedule"])
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
}

func TestScheduleReadsAgainAfterAPushToTheDefaultBranch(t *testing.T) {
	c, clk, _ := newScheduleHarness(t, "2026-03-04 10:00:30")
	c.push("main", scheduleWorkflow("*/5 * * * *"), "")
	ci := c.forge.CI
	tick := func(at string) {
		clk.set(at)
		ci.ScheduleNow(c.ctx)
	}
	tick("2026-03-04 10:05:00")
	require.Len(t, c.scheduled(), 1)
	assert.Equal(t, 1, ci.CachedSchedules(c.name("proj")), "the schedules are cached")

	// 10:07 matches no */5 entry; the new file fires every minute.
	c.push("main", scheduleWorkflow("* * * * *"), "")
	tick("2026-03-04 10:06:00")
	require.Len(t, c.scheduled(), 2, "the pushed workflow is the one scheduled")
	assert.Equal(t, 1, ci.CachedSchedules(c.name("proj")))

	// A push to another branch changes nothing; a push that removes the schedule
	// ends it.
	c.mustGit(c.wt, "checkout", "-q", "-b", "side")
	c.push("side", scheduleWorkflow("0 0 1 1 *"), "")
	tick("2026-03-04 10:07:00")
	require.Len(t, c.scheduled(), 3, "the default branch's workflow still fires every minute")

	c.mustGit(c.wt, "checkout", "-q", "main")
	c.push("main", map[string]string{".github/workflows/nightly.yml": "name: nightly\non: push\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"}, "")
	tick("2026-03-04 10:08:00")
	c.waitFor("the push run of the new file", func() bool { return len(c.runs()) == 4 })
	assert.Len(t, scheduleRuns(c.runs()), 3, "no schedule left on the default branch")
	assert.Equal(t, 0, ci.CachedSchedules(c.name("proj")))
}

func TestScheduleIsRemovedWithTheRepository(t *testing.T) {
	c, clk, _ := newScheduleHarness(t, "2026-03-04 10:00:30")
	c.push("main", scheduleWorkflow("* * * * *"), "")
	ci := c.forge.CI
	clk.set("2026-03-04 10:01:00")
	ci.ScheduleNow(c.ctx)
	require.Len(t, c.scheduled(), 1)
	require.Equal(t, 1, ci.CachedSchedules(c.name("proj")))

	_, err := c.root.repo.DeleteRepo(c.ctx, connect.NewRequest(&pb.DeleteRepoRequest{Repo: "proj"}))
	require.NoError(t, err)
	assert.Equal(t, -1, ci.CachedSchedules(c.name("proj")), "the cached schedules go with the repository")

	for _, at := range []string{"2026-03-04 10:02:00", "2026-03-04 10:03:00"} {
		clk.set(at)
		ci.ScheduleNow(c.ctx)
	}
	assert.Equal(t, -1, ci.CachedSchedules(c.name("proj")), "never scheduled again")

	// The same name created again starts empty: no schedule until it has a workflow.
	_, err = c.forge.Repos.Create(c.ctx, domain.Root(), createRepoParams(c.name("proj")))
	require.NoError(t, err)
	clk.set("2026-03-04 10:04:00")
	ci.ScheduleNow(c.ctx)
	assert.Empty(t, c.scheduled())
}

// More repositories than one tick can handle, and a new minute at every tick:
// each pass is dropped after its first repository, and the cursor makes the next
// one start after it, so that none is starved.
func TestScheduleDoesNotStarveRepositoriesBeyondTheBudget(t *testing.T) {
	clk := &testClock{}
	clk.set("2026-03-04 10:00:30")
	visited := map[domain.RepoName]bool{}
	c := newCIHarness(t, func(cfg *usecase.CIConfig) {
		cfg.Clock = clk
		cfg.ScheduleTick = time.Hour
		cfg.ScheduleBudget = 1
		cfg.ScheduleVisit = func(r domain.RepoName) { visited[r] = true }
	})
	c.registerRunner("alpha", "ubuntu-latest")
	names := []domain.RepoName{c.name("proj")}
	for _, n := range []string{"aa", "zz", "mm", "bb"} {
		names = append(names, c.name(n))
		_, err := c.forge.Repos.Create(c.ctx, domain.Root(), createRepoParams(c.name(n)))
		require.NoError(t, err)
	}
	ci := c.forge.CI
	seen := func() int { return len(visited) }
	minutes := []string{"10:01:00", "10:02:00", "10:03:00", "10:04:00", "10:05:00"}
	for i, m := range minutes {
		clk.set("2026-03-04 " + m)
		ci.ScheduleNow(c.ctx) // one repository, then the next minute drops the pass
		assert.Equal(t, i+1, seen(), "each minute reaches a repository no earlier minute reached")
	}
	assert.Equal(t, len(names), seen(), "every repository was processed")
}

func TestScheduleBoundsTheWorkPerTick(t *testing.T) {
	clk := &testClock{}
	clk.set("2026-03-04 10:00:30")
	c := newCIHarness(t, func(cfg *usecase.CIConfig) {
		cfg.Clock = clk
		cfg.ScheduleTick = time.Hour
		cfg.ScheduleBudget = 1
	})
	c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", scheduleWorkflow("* * * * *"), "")
	for _, name := range []string{"two", "three"} {
		_, err := c.forge.Repos.Create(c.ctx, domain.Root(), createRepoParams(c.name(name)))
		require.NoError(t, err)
	}
	ci := c.forge.CI
	clk.set("2026-03-04 10:01:00")
	ci.ScheduleNow(c.ctx)
	assert.True(t, ci.SchedulePassed().IsZero(), "one repository per tick: the pass is not over")
	ci.ScheduleNow(c.ctx)
	assert.True(t, ci.SchedulePassed().IsZero())
	ci.ScheduleNow(c.ctx)
	assert.Equal(t, time.Date(2026, 3, 4, 10, 1, 0, 0, time.UTC), ci.SchedulePassed(), "three ticks go through three repositories")
	assert.Len(t, c.scheduled(), 1)
}

// CI-4 D6: the timezone of an entry is the wall clock its cron reads.
func TestScheduleTimezone(t *testing.T) {
	c, clk, _ := newScheduleHarness(t, "2026-06-01 06:58:30")
	wf := map[string]string{".github/workflows/nightly.yml": "name: nightly\non:\n  schedule:\n    - cron: '0 9 * * *'\n      timezone: Europe/Paris\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"}
	c.push("main", wf, "")
	tick := func(at string) {
		clk.set(at)
		c.forge.CI.ScheduleNow(c.ctx)
	}
	tick("2026-06-01 06:59:10")
	tick("2026-06-01 09:00:10")
	assert.Empty(t, c.scheduled(), "09:00 UTC is 11:00 in Paris")
	tick("2026-06-02 07:00:10")
	require.Len(t, c.scheduled(), 1, "09:00 in Paris")
}

// A timezone that is no IANA name makes the workflow invalid: its push records
// a failed run with the error, and it is never scheduled.
func TestScheduleInvalidTimezone(t *testing.T) {
	c, clk, _ := newScheduleHarness(t, "2026-06-01 06:58:30")
	wf := map[string]string{".github/workflows/nightly.yml": "name: nightly\non:\n  push:\n  schedule:\n    - cron: '* * * * *'\n      timezone: Mars/Olympus\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"}
	c.push("main", wf, "")
	c.waitFor("the invalid run", func() bool { return len(c.runs()) == 1 })
	run := c.runs()[0]
	assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_FAILURE, run.GetConclusion())
	assert.Equal(t, ".github/workflows/nightly.yml", run.GetName())
	assert.Contains(t, c.getRun(run.GetId()).GetRun().GetError(), `.github/workflows/nightly.yml: invalid schedule timezone "Mars/Olympus"`)
	clk.set("2026-06-01 07:00:10")
	c.forge.CI.ScheduleNow(c.ctx)
	assert.Empty(t, c.scheduled())
}
