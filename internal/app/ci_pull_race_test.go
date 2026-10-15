// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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

// pullHook is CIConfig.PullStage for the tests that interleave other changes
// with the workers of the merge requests: armed for one stage, it runs fn (or
// waits to be released) the next time a worker reaches it, once.
type pullHook struct {
	mu      sync.Mutex
	stage   string
	fn      func()
	reached chan struct{}
	release chan struct{}
}

func (h *pullHook) call(stage string) {
	h.mu.Lock()
	if stage != h.stage {
		h.mu.Unlock()
		return
	}
	h.stage = ""
	fn, reached, release := h.fn, h.reached, h.release
	h.mu.Unlock()
	if fn != nil {
		fn()
		return
	}
	close(reached)
	<-release // held until the test releases it; its cleanup always does
}

// block arms the hook: the worker stops at stage and stays there until release
// is called (the cleanup of the test calls it too, so a failed test never leaves
// a worker stuck). reached is closed when the worker has stopped.
func (h *pullHook) block(t *testing.T, stage string) (reached <-chan struct{}, release func()) {
	t.Helper()
	r, rel := make(chan struct{}), make(chan struct{})
	h.mu.Lock()
	h.stage, h.fn, h.reached, h.release = stage, nil, r, rel
	h.mu.Unlock()
	var once sync.Once
	release = func() { once.Do(func() { close(rel) }) }
	t.Cleanup(release)
	return r, release
}

// await waits for the worker to reach a blocked stage, failing the test when it
// does not within the bound.
func await(t *testing.T, reached <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-reached:
	case <-time.After(15 * time.Second):
		t.Fatalf("the worker never reached %s", what)
	}
}

// run arms the hook with a function the worker runs at stage.
func (h *pullHook) run(stage string, fn func()) {
	h.mu.Lock()
	h.stage, h.fn = stage, fn
	h.mu.Unlock()
}

func newHookedHarness(t *testing.T) (*ciHarness, *pullHook) {
	t.Helper()
	h := &pullHook{}
	return newCIHarness(t, func(cfg *usecase.CIConfig) { cfg.PullStage = h.call }), h
}

func (c *ciHarness) parents(sha string) string {
	c.t.Helper()
	return strings.TrimSpace(c.git2("log", "-1", "--format=%P", sha))
}

// S1: an event runs for the commits it was made for, with the trust of whoever
// caused it, however the branch moves before the event is handled.
func TestPullEventKeepsItsCommitsAndTrustWhenALimitedTokenPushesNext(t *testing.T) {
	c, hook := newHookedHarness(t)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	main := strings.TrimSpace(c.push("main", pullWF("", ""), ""))
	c.mustGit(c.wt, "checkout", "-q", "-b", "agent/x")
	x1 := c.push("agent/x", map[string]string{"x1.txt": "1\n"}, "")
	secret, _ := c.branchToken(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite), []string{"agent/"}, "proj")

	reached, release := hook.block(t, "event")
	n := c.openMR(c.root, "agent/x") // the owner opens it: trusted
	await(t, reached, "the opening event")
	// While the owner's event waits, the agent pushes a new commit.
	c.writeFile(c.wt, "x2.txt", []byte("2\n"))
	x2 := strings.TrimSpace(c.mustGit(c.wt, "rev-parse", "HEAD"))
	c.mustGit(c.wt, "push", "-q", c.urlAuth(secret, "/proj.git"), "agent/x")
	release()

	c.waitFor("both runs", func() bool { return len(c.runs()) == 2 })
	runs := c.runs() // newest first: the synchronize, then the opened
	opened, sync := runs[1], runs[0]
	assert.True(t, opened.GetTrusted(), "the owner's event keeps the owner's trust")
	assert.Equal(t, main+" "+x1, c.parents(opened.GetSha()), "and its own commits, not the agent's later one")
	assert.False(t, sync.GetTrusted(), "the agent's push is the agent's")
	assert.Equal(t, main+" "+x2, c.parents(sync.GetSha()))
	assert.Equal(t, "claude", sync.GetActor())
	// The refs are the latest ones.
	c.waitFor("the refs follow the latest push", func() bool {
		return c.ref(domain.PullHeadRef(n)) == x2 && strings.HasSuffix(c.parents(c.ref(domain.PullMergeRef(n))), " "+x2)
	})
	_ = runner
}

// S1: a push made with a job token starts no pull_request run, not even through
// the event of an earlier opening.
func TestPullEventIsNotRunForAJobTokenPush(t *testing.T) {
	c, hook := newHookedHarness(t)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	files := pullWF("", "")
	files[".github/workflows/push.yml"] = "on:\n  push:\n    branches: [feat]\npermissions:\n  contents: write\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo push\n"
	main := strings.TrimSpace(c.push("main", files, ""))
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	f1 := c.push("feat", map[string]string{"f1.txt": "1\n"}, "")
	pushJob := runner.await() // the push workflow's job holds a token with contents: write

	reached, release := hook.block(t, "event")
	c.openMR(c.root, "feat")
	await(t, reached, "the opening event")
	c.writeFile(c.wt, "f2.txt", []byte("2\n"))
	out, err := c.tokenPush(c.wt, pushJob.GetGithubToken(), "HEAD:refs/heads/feat")
	require.NoError(t, err, out)
	release()

	prJob := runner.await()
	runner.runJob(prJob, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
	runner.runJob(pushJob, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
	var pulls []*pb.Run
	for _, r := range c.runs() {
		if r.GetEvent() == "pull_request" {
			pulls = append(pulls, r)
		}
	}
	require.Len(t, pulls, 1, "the job's push started no run")
	assert.Equal(t, main+" "+f1, c.parents(pulls[0].GetSha()))
	time.Sleep(200 * time.Millisecond)
	for _, r := range c.runs() {
		assert.NotEqual(t, "synchronize", r.GetTitle())
	}
	assert.Len(t, c.runs(), 2, "the push run and the opened run")
}

// S4: a request closed while its merge is being computed gets neither the refs
// nor the run of the opening, and the closed run does not use that merge.
func TestPullCloseDuringComputeDiscardsTheOpening(t *testing.T) {
	c, hook := newHookedHarness(t)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", pullWF("    types: [opened, closed]", ""), "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	f1 := c.push("feat", map[string]string{"f.txt": "f\n"}, "")

	hook.run("computed", func() {
		_, err := c.root.mr.CloseMergeRequest(c.ctx, connect.NewRequest(&pb.CloseMergeRequestRequest{Repo: "proj", Number: 1}))
		assert.NoError(t, err)
	})
	n := c.openMR(c.root, "feat")
	job := runner.await()
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
	gh := c.ghContext(job)
	assert.Equal(t, "closed", gh["event"].(map[string]any)["action"])
	assert.Equal(t, f1, gh["sha"], "no test merge was published: the closed run uses the head")
	assert.Empty(t, c.ref(domain.PullMergeRef(n)), "the merge ref of a closed request is never published")
	assert.Empty(t, c.ref(domain.PullHeadRef(n)))
	time.Sleep(200 * time.Millisecond)
	assert.Len(t, c.runs(), 1, "no run for the opening")
}

// E1: a slow refresh of a merge request's refs does not delay the push
// workflows.
func TestPullSlowRefreshDoesNotDelayPushRuns(t *testing.T) {
	c, hook := newHookedHarness(t)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	both := "name: both\non:\n  push:\n    branches: [main]\n  pull_request:\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"
	c.push("main", map[string]string{".github/workflows/both.yml": both}, "")
	runner.runJob(runner.await(), pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	c.push("feat", map[string]string{"f.txt": "f\n"}, "")
	n := c.openMR(c.root, "feat")
	runner.runJob(runner.await(), pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
	c.waitFor("the merge ref", func() bool { return c.ref(domain.PullMergeRef(n)) != "" })

	reached, release := hook.block(t, "refresh")
	c.mustGit(c.wt, "checkout", "-q", "main")
	m2 := c.push("main", map[string]string{"m2.txt": "m\n"}, "") // the target moves
	await(t, reached, "the refresh")                             // the refresh worker is stuck
	c.waitFor("the push run of main, while the refresh waits", func() bool {
		for _, r := range c.runs() {
			if r.GetEvent() == "push" && r.GetSha() == m2 {
				return true
			}
		}
		return false
	})
	release()
	c.waitFor("the refresh then follows", func() bool {
		return strings.HasPrefix(c.parents(c.ref(domain.PullMergeRef(n))), m2+" ")
	})
}

// pushRuns counts the push runs, without failing the test (it runs in the hook
// of a worker).
func (c *ciHarness) pushRuns() int {
	res, err := c.root.run.ListRuns(c.ctx, connect.NewRequest(&pb.ListRunsRequest{Repo: c.repo}))
	if err != nil {
		return -1
	}
	n := 0
	for _, r := range res.Msg.GetRuns() {
		if r.GetEvent() == "push" {
			n++
		}
	}
	return n
}

// A push's workflows are queued before its merge requests are looked at: when
// the discovery starts, the push run is already being created, so a slow
// discovery never delays it.
func TestPullDiscoveryDoesNotDelayPushRuns(t *testing.T) {
	c, hook := newHookedHarness(t)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	both := "name: both\non:\n  push:\n    branches: [main]\n  pull_request:\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"
	c.push("main", map[string]string{".github/workflows/both.yml": both}, "")
	runner.runJob(runner.await(), pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	c.push("feat", map[string]string{"f.txt": "f\n"}, "")
	n := c.openMR(c.root, "feat")
	runner.runJob(runner.await(), pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
	c.waitFor("the merge ref", func() bool { return c.ref(domain.PullMergeRef(n)) != "" })
	c.waitFor("the workers are idle", func() bool { return len(c.runs()) == 2 })

	before := c.pushRuns()
	var sawRun atomic.Bool
	hook.run("discover", func() {
		// Inside the discovery of the push: its push run must come into being
		// while it goes on (the push handler waits here, up to the bound).
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if c.pushRuns() > before {
				sawRun.Store(true)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	c.mustGit(c.wt, "checkout", "-q", "main")
	m2 := c.push("main", map[string]string{"m2.txt": "m\n"}, "")
	assert.True(t, sawRun.Load(), "the push run was created while the discovery ran")
	c.waitFor("the merge ref follows the target", func() bool {
		return strings.HasPrefix(c.parents(c.ref(domain.PullMergeRef(n))), m2+" ")
	})
}

// Past the bound of the refresh queue the refs are repaired by a rescan, and
// the activities are never lost: every synchronize still runs.
func TestPullSaturatedRefreshQueueKeepsEventsAndRepairsRefs(t *testing.T) {
	h := &pullHook{}
	c := newCIHarness(t, func(cfg *usecase.CIConfig) { cfg.PullStage = h.call; cfg.PullRefreshes = 1 })
	runner := c.registerRunner("alpha", "ubuntu-latest")
	_ = runner
	c.push("main", pullWF("", ""), "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	c.push("feat", map[string]string{"f.txt": "f\n"}, "")
	n1 := c.openMR(c.root, "feat")
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat2", "main")
	c.push("feat2", map[string]string{"g.txt": "g\n"}, "")
	n2 := c.openMR(c.root, "feat2")
	c.waitFor("both opened runs and both merge refs", func() bool {
		return len(c.runs()) == 2 && c.ref(domain.PullMergeRef(n1)) != "" && c.ref(domain.PullMergeRef(n2)) != ""
	})

	reached, release := h.block(t, "refresh")
	c.mustGit(c.wt, "checkout", "-q", "feat")
	c.push("feat", map[string]string{"f2.txt": "2\n"}, "") // its refresh is taken by the worker, which stops
	await(t, reached, "the first refresh")
	c.mustGit(c.wt, "checkout", "-q", "feat2")
	g2 := c.push("feat2", map[string]string{"g2.txt": "2\n"}, "") // waits: the queue is at its bound of one
	c.mustGit(c.wt, "checkout", "-q", "feat")
	f3 := c.push("feat", map[string]string{"f3.txt": "3\n"}, "") // over the bound: the repository is marked dirty
	release()

	c.waitFor("the three synchronize runs, none lost to the cap", func() bool { return len(c.runs()) == 5 })
	c.waitFor("the refs end at the latest heads", func() bool {
		return c.ref(domain.PullHeadRef(n1)) == f3 && strings.HasSuffix(c.parents(c.ref(domain.PullMergeRef(n1))), " "+f3) &&
			c.ref(domain.PullHeadRef(n2)) == g2 && strings.HasSuffix(c.parents(c.ref(domain.PullMergeRef(n2))), " "+g2)
	})
}

// GO-010: the target tip an event is tested against is read when the push
// happens: a target that moves before the event is handled changes nothing for
// the run, which keeps the merge of the commits it was made for.
func TestPullEventKeepsTheTargetTipOfItsPush(t *testing.T) {
	c, hook := newHookedHarness(t)
	c.registerRunner("alpha", "ubuntu-latest")
	m1 := c.push("main", pullWF("", ""), "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	c.push("feat", map[string]string{"f.txt": "f\n"}, "")
	c.openMR(c.root, "feat")
	c.waitFor("the opened run", func() bool { return len(c.runs()) == 1 })

	reached, release := hook.block(t, "event")
	f2 := c.push("feat", map[string]string{"f2.txt": "2\n"}, "") // the synchronize event is taken and stops
	await(t, reached, "the synchronize event")
	c.mustGit(c.wt, "checkout", "-q", "main")
	m2 := c.push("main", map[string]string{"m2.txt": "m\n"}, "") // the target moves meanwhile
	release()

	c.waitFor("the synchronize run", func() bool { return len(c.runs()) == 2 })
	assert.Equal(t, m1+" "+f2, c.parents(c.runs()[0].GetSha()), "the run merges the target as it was at the push, not as it is now")
	assert.NotEqual(t, m2, strings.Fields(c.parents(c.runs()[0].GetSha()))[0])
	c.waitFor("the merge ref follows the target", func() bool {
		return c.parents(c.ref(domain.PullMergeRef(1))) == m2+" "+f2
	})
}

// countingPull counts the test merges the CI asks of Git.
type countingPull struct {
	ports.PullGit
	merges atomic.Int32
}

func (p *countingPull) TestMerge(ctx context.Context, repo domain.RepoName, base, head domain.ObjectID) (domain.ObjectID, bool, error) {
	p.merges.Add(1)
	return p.PullGit.TestMerge(ctx, repo, base, head)
}

// GO-009: conflicting merge requests are not tried again by a rescan until a
// branch moves, even when there are more of them than the memory holds, and the
// rescan neither loops nor marks its repository dirty again.
func TestPullRescanDoesNotRetryConflicts(t *testing.T) {
	p := &countingPull{}
	var rescans atomic.Int32
	c := newCIHarness(t, func(cfg *usecase.CIConfig) {
		p.PullGit = cfg.Pull
		cfg.Pull = p
		cfg.PullAttempts = 2
		cfg.PullStage = func(stage string) {
			if stage == "rescan" {
				rescans.Add(1)
			}
		}
	})
	c.push("main", map[string]string{"a.txt": "base\n"}, "")
	const conflicting = 4
	for i := range conflicting {
		b := "c" + string(rune('a'+i))
		c.mustGit(c.wt, "checkout", "-q", "-b", b, "main")
		c.push(b, map[string]string{"a.txt": b + "\n"}, "")
	}
	c.mustGit(c.wt, "checkout", "-q", "main")
	c.push("main", map[string]string{"a.txt": "main\n"}, "") // every branch now conflicts with main
	for i := range conflicting {
		c.openMR(c.root, "c"+string(rune('a'+i)))
	}
	c.waitFor("every conflict was tried", func() bool { return p.merges.Load() >= conflicting })
	time.Sleep(300 * time.Millisecond)
	for i := range conflicting {
		assert.Empty(t, c.ref(domain.PullMergeRef(int64(i+1))), "a conflict has no merge ref")
	}

	// A rescan (what a saturated queue or a restart leads to) tries again only
	// what the memory forgot: at most the number of merge requests, and it ends.
	before, scans := p.merges.Load(), rescans.Load()
	require.NoError(t, c.forge.CI.RescanPulls(c.ctx, c.name("proj")))
	assert.LessOrEqual(t, p.merges.Load()-before, int32(conflicting))
	time.Sleep(300 * time.Millisecond)
	after := p.merges.Load()
	assert.Equal(t, scans+1, rescans.Load(), "the rescan was not marked dirty again")
	assert.Equal(t, after, p.merges.Load(), "and nothing goes on trying")
}

// readingMRs counts what the CI asks of the merge request store: the full
// listings and the single reads of requests.
type readingMRs struct {
	ports.MergeRequestStore
	lists, opens, gets atomic.Int32
}

func (s *readingMRs) List(ctx context.Context, repo domain.RepoName) ([]domain.MergeRequest, error) {
	s.lists.Add(1)
	return s.MergeRequestStore.List(ctx, repo)
}

func (s *readingMRs) ListOpen(ctx context.Context, repo domain.RepoName) ([]domain.MergeRequest, error) {
	s.opens.Add(1)
	return s.MergeRequestStore.ListOpen(ctx, repo)
}

func (s *readingMRs) Get(ctx context.Context, repo domain.RepoName, n int64) (domain.MergeRequest, error) {
	s.gets.Add(1)
	return s.MergeRequestStore.Get(ctx, repo, n)
}

// GO-011: the snapshot of a push looks up the open merge requests of the pushed
// branches in the index and reads those only: the many closed ones of the
// repository are not read, and the repository's merge requests are not listed.
func TestPullPushDoesNotReadHistoricalMergeRequests(t *testing.T) {
	store := &readingMRs{}
	c := newCIHarness(t, func(cfg *usecase.CIConfig) {
		store.MergeRequestStore = cfg.MRs
		cfg.MRs = store
	})
	c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", pullWF("", ""), "")
	const closed = 30
	for i := range closed {
		b := "old" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		c.mustGit(c.wt, "checkout", "-q", "-b", b, "main")
		c.push(b, map[string]string{b + ".txt": "x\n"}, "")
		n := c.openMR(c.root, b)
		_, err := c.root.mr.CloseMergeRequest(c.ctx, connect.NewRequest(&pb.CloseMergeRequestRequest{Repo: "proj", Number: n}))
		require.NoError(t, err)
	}
	c.mustGit(c.wt, "checkout", "-q", "-b", "live", "main")
	c.push("live", map[string]string{"l.txt": "l\n"}, "")
	live := c.openMR(c.root, "live")
	runsOf := func() int {
		n := 0
		for _, r := range c.runs() {
			if r.GetRef() == domain.PullMergeRef(live) {
				n++
			}
		}
		return n
	}
	// (The closed requests may or may not have had their opened run: they were
	// closed at once.) The live one has its own.
	c.waitFor("the opened run of the live request", func() bool { return runsOf() == 1 })
	time.Sleep(300 * time.Millisecond)

	lists, gets := store.lists.Load(), store.gets.Load()
	c.push("live", map[string]string{"l2.txt": "2\n"}, "")
	c.waitFor("the synchronize run of the live request", func() bool { return runsOf() == 2 })
	time.Sleep(300 * time.Millisecond)
	assert.Zero(t, store.lists.Load()-lists, "the merge requests of the repository are not listed for a push")
	// The synchronize snapshot reads the one open request on the pushed branch (and
	// the workers read it again to handle the event and refresh the refs): a
	// small number of reads, nothing like the thirty closed ones.
	assert.Less(t, store.gets.Load()-gets, int32(closed), "the closed requests are not read")
}

// GO-011: past the bound of one push's snapshot the rest is skipped, an error
// naming the repository and the number skipped is logged, and the refs of the
// skipped requests are repaired by the rescan.
func TestPullSnapshotBoundSkipsLogsAndMarksDirty(t *testing.T) {
	c := newCIHarness(t, func(cfg *usecase.CIConfig) { cfg.PullSnapshot = 2 })
	c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", pullWF("", ""), "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "shared")
	c.push("shared", map[string]string{"s.txt": "s\n"}, "")
	const requests = 5
	var numbers []int64
	for i := range requests {
		// Several open requests on the same source branch (distinct targets).
		t0 := "t" + string(rune('a'+i))
		c.mustGit(c.wt, "checkout", "-q", "-b", t0, "main")
		c.push(t0, map[string]string{t0 + ".txt": "t\n"}, "")
		res, err := c.root.mr.CreateMergeRequest(c.ctx, connect.NewRequest(&pb.CreateMergeRequestRequest{
			Repo: "proj", Title: "t" + t0, SourceBranch: "shared", TargetBranch: t0,
		}))
		require.NoError(t, err)
		numbers = append(numbers, res.Msg.GetMergeRequest().GetNumber())
	}
	c.waitFor("the opened runs", func() bool { return len(c.runs()) == requests })

	c.mustGit(c.wt, "checkout", "-q", "shared")
	latest := c.push("shared", map[string]string{"s2.txt": "2\n"}, "")

	c.waitFor("the error naming the repository and the skipped requests", func() bool {
		logs := c.logs.String()
		return strings.Contains(logs, "snapshot of a push is cut short") && strings.Contains(logs, "more than the bound") &&
			strings.Contains(logs, `"bound":2`) && strings.Contains(logs, `"repo":"proj"`) && !strings.Contains(logs, `"skipped"`)
	})
	c.waitFor("only the bounded part got a synchronize run", func() bool { return len(c.runs()) == requests+2 })
	time.Sleep(300 * time.Millisecond)
	assert.Len(t, c.runs(), requests+2, "the three others were skipped")
	// The refs of every request still end at the pushed head: the repository was
	// marked dirty and the rescan refreshed them.
	for _, n := range numbers {
		c.waitFor("the head ref of a skipped request is repaired", func() bool {
			return c.ref(domain.PullHeadRef(n)) == latest
		})
	}
}

// GO-013: a push that touches no merge request (not a source, not a target)
// queues no refresh and marks nothing for a rescan; a push of a target queues the
// refresh of exactly the requests that target it.
func TestPullUnrelatedPushQueuesNoRefreshAndMarksNothing(t *testing.T) {
	var refreshes, rescans atomic.Int32
	c := newCIHarness(t, func(cfg *usecase.CIConfig) {
		cfg.PullStage = func(stage string) {
			switch stage {
			case "refresh":
				refreshes.Add(1)
			case "rescan":
				rescans.Add(1)
			}
		}
	})
	c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", pullWF("", ""), "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	c.push("feat", map[string]string{"f.txt": "f\n"}, "")
	n := c.openMR(c.root, "feat")
	c.waitFor("the merge ref", func() bool { return c.ref(domain.PullMergeRef(n)) != "" })
	time.Sleep(300 * time.Millisecond)

	// Pushes of branches that are neither the source nor the target of any open
	// merge request: nothing is queued, nothing is marked.
	beforeRefresh, beforeRescan := refreshes.Load(), rescans.Load()
	for _, b := range []string{"unrelated", "other/topic", "another"} {
		c.mustGit(c.wt, "checkout", "-q", "-b", b, "main")
		c.push(b, map[string]string{b + ".txt": "x\n"}, "")
	}
	time.Sleep(500 * time.Millisecond)
	assert.Equal(t, beforeRefresh, refreshes.Load(), "no refresh for pushes that touch no merge request")
	assert.Equal(t, beforeRescan, rescans.Load(), "and no repository marked for a rescan")

	// A push of the target queues the refresh of the request that targets it.
	c.mustGit(c.wt, "checkout", "-q", "main")
	m2 := c.push("main", map[string]string{"m2.txt": "m\n"}, "")
	c.waitFor("the merge ref follows the target", func() bool {
		return strings.HasPrefix(c.parents(c.ref(domain.PullMergeRef(n))), m2+" ")
	})
	assert.Greater(t, refreshes.Load(), beforeRefresh, "a refresh was queued for the target move")
	assert.Equal(t, beforeRescan, rescans.Load(), "through the index, not a rescan")
}

// GO-013: more merge requests target a pushed branch than the bound: the lookup
// is cut, an error is logged, and the repository is rescanned, so that every
// request ends with a merge ref on the new target.
func TestPullTargetLookupTruncationMarksDirty(t *testing.T) {
	c := newCIHarness(t, func(cfg *usecase.CIConfig) { cfg.PullSnapshot = 2 })
	c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", pullWF("", ""), "")
	const requests = 5
	var numbers []int64
	for i := range requests {
		b := "f" + string(rune('a'+i))
		c.mustGit(c.wt, "checkout", "-q", "-b", b, "main")
		c.push(b, map[string]string{b + ".txt": "x\n"}, "")
		numbers = append(numbers, c.openMR(c.root, b))
	}
	c.waitFor("the opened runs", func() bool { return len(c.runs()) == requests })
	c.mustGit(c.wt, "checkout", "-q", "main")
	m2 := c.push("main", map[string]string{"m2.txt": "m\n"}, "")
	c.waitFor("the truncation is logged", func() bool {
		logs := c.logs.String()
		return strings.Contains(logs, "refresh of a push is cut short") && strings.Contains(logs, "more than the bound") && strings.Contains(logs, `"repo":"proj"`)
	})
	for _, n := range numbers {
		c.waitFor("the merge ref of a request is on the new target", func() bool {
			return strings.HasPrefix(c.parents(c.ref(domain.PullMergeRef(n))), m2+" ")
		})
	}
}

// countingGit counts the diffs the CI asks of Git.
type countingGit struct {
	ports.MergeGit
	diffs atomic.Int32
}

func (g *countingGit) DiffCommits(ctx context.Context, repo domain.RepoName, base, head domain.ObjectID, l domain.DiffLimits) (domain.DiffStats, domain.Diff, error) {
	g.diffs.Add(1)
	return g.MergeGit.DiffCommits(ctx, repo, base, head, l)
}

// E2: the pages of one merge request share the diff.
func TestPullFilesPagesShareOneDiff(t *testing.T) {
	g := &countingGit{}
	c := newCIHarness(t, func(cfg *usecase.CIConfig) {
		g.MergeGit = cfg.Git
		cfg.Git = g
	})
	c.push("main", pullWF("", ""), "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	for i := range 5 {
		c.writeFile(c.wt, "d/"+string(rune('a'+i))+".txt", []byte("x\n"))
	}
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), "feat")
	c.openMR(c.root, "feat")
	for _, q := range []string{"?per_page=2&page=1", "?per_page=2&page=2", "?per_page=2&page=3", ""} {
		code, _, _ := c.apiGet(adminToken, "/repos/owner/proj/pulls/1/files"+q)
		require.Equal(t, http.StatusOK, code)
	}
	assert.EqualValues(t, 1, g.diffs.Load(), "one diff for four pages")
	// A push to the branch is another pair of commits: another diff.
	c.writeFile(c.wt, "d/z.txt", []byte("z\n"))
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), "feat")
	code, _, _ := c.apiGet(adminToken, "/repos/owner/proj/pulls/1/files")
	require.Equal(t, http.StatusOK, code)
	assert.EqualValues(t, 2, g.diffs.Load())
}
