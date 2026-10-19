// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/config"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/usecase"
)

// CI-3 D1, S1: a scheduled run is run as whoever moved the default branch to
// its tip, with their trust.

const scheduleSecretWorkflow = `name: nightly
on:
  schedule:
    - cron: '* * * * *'
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ secrets.NIGHTLY_TOKEN }}
`

func newScheduleSecretsHarness(t *testing.T) (*ciHarness, *testClock, *fakeRunner) {
	t.Helper()
	clk := &testClock{}
	clk.set("2026-03-04 10:00:30")
	h := newHarnessWith(t, harnessOpts{env: map[string]string{config.EnvSecretsKey: secretsKeyA}}, func(o *options) {
		o.ci = func(cfg *usecase.CIConfig) {
			cfg.Clock = clk
			cfg.ScheduleTick = time.Hour
		}
	})
	c := &ciHarness{harness: h, root: h.api(adminToken), repo: "proj"}
	_, err := h.forge.Repos.Create(h.ctx, domain.Root(), ports.CreateRepoParams{Name: h.name("proj"), Visibility: domain.VisibilityPrivate})
	require.NoError(t, err)
	c.wt = h.workTree("main")
	c.setSecret(repoScope("proj"), "NIGHTLY_TOKEN", "s3cret")
	return c, clk, c.registerRunner("alpha", "ubuntu-latest")
}

// fireAt moves the clock to the minute, ticks the scheduler and returns the job
// of the scheduled run it started.
func (c *ciHarness) fireAt(clk *testClock, runner *fakeRunner, at string) (*pb.Run, *pb.Job) {
	c.t.Helper()
	before := len(scheduleRuns(c.runs()))
	clk.set(at)
	c.forge.CI.ScheduleNow(c.ctx)
	runs := scheduleRuns(c.runs())
	require.Len(c.t, runs, before+1, "one scheduled run")
	job := runner.await()
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
	return runs[0], job // newest first
}

func (c *ciHarness) repoToken(label string) string {
	return c.token(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite), "proj")
}

func TestScheduleOnABranchAnAgentMadeTheDefaultIsNotTrusted(t *testing.T) {
	c, clk, runner := newScheduleSecretsHarness(t)
	// The repository is empty: the first branch pushed becomes the default
	// branch, whoever pushes it.
	secret, _ := c.branchToken(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite), []string{"agent/"}, "proj")
	c.mustGit(c.wt, "checkout", "-q", "-b", "agent/x")
	c.writeFile(c.wt, ".github/workflows/nightly.yml", []byte(scheduleSecretWorkflow))
	c.mustGit(c.wt, "push", "-q", c.urlAuth(secret, "/proj.git"), "agent/x")
	def, _, err := c.forge.git.DefaultBranch(c.ctx, c.name("proj"))
	require.NoError(t, err)
	require.Equal(t, "agent/x", def, "the agent's branch is the default branch")

	run, job := c.fireAt(clk, runner, "2026-03-04 10:01:00")
	assert.False(t, run.GetTrusted(), "the owner's secrets do not run for an agent's workflow")
	assert.Equal(t, "claude", run.GetActor(), "the actor is who pushed the tip")
	assert.False(t, job.GetTrusted())
	assert.Empty(t, job.GetSecrets())
}

func TestScheduleActorFollowsTheLastPusherOfTheDefaultBranch(t *testing.T) {
	c, clk, runner := newScheduleSecretsHarness(t)

	// The owner pushes main: trusted, with the secrets.
	c.push("main", map[string]string{".github/workflows/nightly.yml": scheduleSecretWorkflow}, "")
	run, job := c.fireAt(clk, runner, "2026-03-04 10:01:00")
	assert.True(t, run.GetTrusted())
	assert.Equal(t, "root", run.GetActor())
	assert.Equal(t, map[string]string{"NIGHTLY_TOKEN": "s3cret"}, job.GetSecrets())

	// A token scoped to the repository (not the owner) pushes main: untrusted.
	tok := c.repoToken("scoped")
	c.writeFile(c.wt, "x.txt", []byte("x"))
	c.mustGit(c.wt, "push", "-q", c.urlAuth(tok, "/proj.git"), "main")
	run, job = c.fireAt(clk, runner, "2026-03-04 10:02:00")
	assert.False(t, run.GetTrusted(), "a repository-scoped token is not the owner")
	assert.Empty(t, job.GetSecrets())

	// The owner pushes again: trusted again.
	c.push("main", map[string]string{"y.txt": "y"}, "")
	run, _ = c.fireAt(clk, runner, "2026-03-04 10:03:00")
	assert.True(t, run.GetTrusted())

	// The record is for one commit: a tip it does not name is not trusted.
	require.NoError(t, c.forge.store.CI().PutRefTip(c.ctx, domain.CIRefTip{
		Repo: c.name("proj"), Ref: "refs/heads/main", Sha: "1111111111111111111111111111111111111111", Actor: "root", Trusted: true,
	}))
	run, job = c.fireAt(clk, runner, "2026-03-04 10:04:00")
	assert.False(t, run.GetTrusted(), "the record names another commit")
	assert.Empty(t, job.GetSecrets())
}

func TestScheduleAfterAMergeIsTheMergersToo(t *testing.T) {
	c, clk, runner := newScheduleSecretsHarness(t)
	c.push("main", map[string]string{".github/workflows/nightly.yml": scheduleSecretWorkflow}, "")
	tok := c.repoToken("scoped")
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	c.writeFile(c.wt, "f.txt", []byte("f"))
	c.mustGit(c.wt, "push", "-q", c.urlAuth(tok, "/proj.git"), "feat")
	n := c.openMR(c.root, "feat")
	c.waitFor("the head ref", func() bool { return c.ref(domain.PullHeadRef(n)) != "" })
	head := c.ref(domain.PullHeadRef(n))

	// The owner merges: main is theirs from now on.
	_, err := c.root.mr.MergeMergeRequest(c.ctx, connect.NewRequest(&pb.MergeMergeRequestRequest{Repo: "proj", Number: n, HeadId: head}))
	require.NoError(t, err)
	run, job := c.fireAt(clk, runner, "2026-03-04 10:01:00")
	assert.True(t, run.GetTrusted(), "merged by the owner")
	assert.Equal(t, map[string]string{"NIGHTLY_TOKEN": "s3cret"}, job.GetSecrets())
}

func TestRefTipRecordsGoWithTheRepository(t *testing.T) {
	c, _, _ := newScheduleSecretsHarness(t)
	c.push("main", map[string]string{"a.txt": "a"}, "")
	tips := c.forge.store.CI()
	got, err := tips.GetRefTip(c.ctx, c.name("proj"), "refs/heads/main")
	require.NoError(t, err)
	assert.True(t, got.Trusted)
	assert.Equal(t, c.ref("refs/heads/main"), got.Sha)

	_, err = c.root.repo.DeleteRepo(c.ctx, connect.NewRequest(&pb.DeleteRepoRequest{Repo: "proj"}))
	require.NoError(t, err)
	_, err = tips.GetRefTip(c.ctx, c.name("proj"), "refs/heads/main")
	assert.ErrorIs(t, err, domain.ErrNotFound, "deleted with the repository")
}

// GO-004: the record is written before the ref moves, so the sequence owner A,
// someone else B, someone else A (the same commit as the owner's) never leaves
// the owner's record on the tip, however late the post-receive of the last push
// is. A push that is refused after its record was written leaves a record that
// matches no tip.
func TestScheduleABAOfThePusherIsNeverTrusted(t *testing.T) {
	c, clk, runner := newScheduleSecretsHarness(t)
	tips := c.forge.store.CI()
	c.push("main", map[string]string{".github/workflows/nightly.yml": scheduleSecretWorkflow}, "")
	commitA := c.ref("refs/heads/main")
	run, _ := c.fireAt(clk, runner, "2026-03-04 10:01:00")
	require.True(t, run.GetTrusted(), "the owner's A is trusted")

	// B and A again, by a repository-scoped token.
	tok := c.repoToken("scoped")
	c.writeFile(c.wt, "b.txt", []byte("b"))
	c.mustGit(c.wt, "push", "-q", c.urlAuth(tok, "/proj.git"), "main")
	rec, err := tips.GetRefTip(c.ctx, c.name("proj"), "refs/heads/main")
	require.NoError(t, err)
	assert.False(t, rec.Trusted, "B was recorded as the token's before the ref moved")
	c.mustGit(c.wt, "push", "-q", "-f", c.urlAuth(tok, "/proj.git"), commitA+":refs/heads/main")
	require.Equal(t, commitA, c.ref("refs/heads/main"), "main is back at the owner's commit")

	// The record of that push is the token's, not the owner's: A is untrusted.
	rec, err = tips.GetRefTip(c.ctx, c.name("proj"), "refs/heads/main")
	require.NoError(t, err)
	assert.Equal(t, commitA, rec.Sha)
	assert.False(t, rec.Trusted)
	run, job := c.fireAt(clk, runner, "2026-03-04 10:02:00")
	assert.False(t, run.GetTrusted(), "owner A, token B, token A: not the owner's")
	assert.Empty(t, job.GetSecrets())

	// A push that moves nothing (the owner pushes A where A already is) is not a
	// push for git: it endorses nothing.
	c.mustGit(c.wt, "push", "-q", "-f", c.urlAuth(adminToken, "/proj.git"), commitA+":refs/heads/main")
	run, _ = c.fireAt(clk, runner, "2026-03-04 10:03:00")
	assert.False(t, run.GetTrusted(), "still the token's")

	// The owner moves the tip: trusted again.
	c.push("main", map[string]string{"owner.txt": "o"}, "")
	run, _ = c.fireAt(clk, runner, "2026-03-04 10:04:00")
	assert.True(t, run.GetTrusted())
	tip := c.ref("refs/heads/main")

	// A push refused after the decision recorded it (here the check alone runs,
	// then git never moves the ref): the record names a commit the tip does not
	// have, and the schedule fails closed.
	scoped, err := c.forge.Auth.Authenticate(c.ctx, tok)
	require.NoError(t, err)
	never := strings.Repeat("2", 40)
	require.NoError(t, c.forge.Pushes.Check(c.ctx, scoped, c.name("proj"),
		[]domain.RefUpdate{{Ref: "refs/heads/main", Old: tip, New: never}}, ports.PushObjects{}))
	left, err := c.forge.store.CI().GetRefTip(c.ctx, c.name("proj"), "refs/heads/main")
	require.NoError(t, err)
	assert.Equal(t, never, left.Sha, "the refused push left its record behind")
	run, _ = c.fireAt(clk, runner, "2026-03-04 10:05:00")
	assert.False(t, run.GetTrusted(), "a record for a commit that never became the tip: fail closed")
}

// GO-006: a merge that deletes its source branch drops the record of that
// branch, and a merge records the target before it moves.
func TestMergeRecordsTheTargetAndDropsTheSourceRecord(t *testing.T) {
	c, _, _ := newScheduleSecretsHarness(t)
	tips := c.forge.store.CI()
	c.push("main", map[string]string{"a.txt": "a"}, "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	c.push("feat", map[string]string{"f.txt": "f"}, "")
	_, err := tips.GetRefTip(c.ctx, c.name("proj"), "refs/heads/feat")
	require.NoError(t, err, "the pushed branch has a record")
	n := c.openMR(c.root, "feat")
	c.waitFor("the head ref", func() bool { return c.ref(domain.PullHeadRef(n)) != "" })

	_, err = c.root.mr.MergeMergeRequest(c.ctx, connect.NewRequest(&pb.MergeMergeRequestRequest{
		Repo: "proj", Number: n, HeadId: c.ref(domain.PullHeadRef(n)), DeleteSourceBranch: true,
	}))
	require.NoError(t, err)

	// The deletion and the removal of the record run after the merge returned.
	c.waitFor("the deleted source branch has no record", func() bool {
		_, err := tips.GetRefTip(c.ctx, c.name("proj"), "refs/heads/feat")
		return errors.Is(err, domain.ErrNotFound)
	})
	main, err := tips.GetRefTip(c.ctx, c.name("proj"), "refs/heads/main")
	require.NoError(t, err)
	assert.Equal(t, c.ref("refs/heads/main"), main.Sha, "the merge recorded the new tip")
	assert.True(t, main.Trusted)

	// A pushed branch that is deleted by a push loses its record too.
	c.mustGit(c.wt, "checkout", "-q", "-b", "gone")
	c.push("gone", map[string]string{"g.txt": "g"}, "")
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), ":refs/heads/gone")
	_, err = tips.GetRefTip(c.ctx, c.name("proj"), "refs/heads/gone")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

// GO-005: a dispatched run is trusted only when the caller is and the ref was
// moved to its commit by a trusted principal, for every caller.
func TestDispatchTrustFollowsTheRefsPusher(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/deploy.yml": dispatchWorkflow, ".github/workflows/d.yml": jobTokenDispatcher}, "")
	jobs := runner.jobsByKey(4)
	in := map[string]string{"version": "1"}

	// An agent's branch.
	secret, _ := c.branchToken(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite), []string{"agent/"}, "proj")
	c.mustGit(c.wt, "checkout", "-q", "-b", "agent/x")
	c.writeFile(c.wt, "x.txt", []byte("x"))
	c.mustGit(c.wt, "push", "-q", c.urlAuth(secret, "/proj.git"), "agent/x")
	c.waitFor("the agent's push run", func() bool { return len(c.runsOf("dispatcher")) >= 2 })

	run, err := c.dispatch(c.root, "deploy.yml", "main", in)
	require.NoError(t, err)
	assert.True(t, run.GetTrusted(), "the owner dispatching what the owner pushed")

	run, err = c.dispatch(c.root, "deploy.yml", "agent/x", in)
	require.NoError(t, err)
	assert.False(t, run.GetTrusted(), "the owner dispatching an agent's branch")

	run, err = c.dispatch(c.api(jobs["with"].GetGithubToken()), "deploy.yml", "agent/x", in)
	require.NoError(t, err)
	assert.False(t, run.GetTrusted(), "a trusted job token dispatching an agent's branch")
	run, err = c.dispatch(c.api(jobs["with"].GetGithubToken()), "deploy.yml", "main", in)
	require.NoError(t, err)
	assert.True(t, run.GetTrusted(), "a trusted job token dispatching the owner's branch")

	// What the jobs of those runs received: the untrusted ones got no secrets.
	got := map[int64]*pb.Job{}
	for range 4 {
		j := runner.await()
		got[j.GetRunId()] = j
	}
	for _, r := range c.runs() {
		if r.GetEvent() != "workflow_dispatch" {
			continue
		}
		if j := got[r.GetId()]; j != nil {
			assert.Equal(t, r.GetTrusted(), j.GetTrusted(), "the job follows its run")
			if !r.GetTrusted() {
				assert.Empty(t, j.GetSecrets())
			}
		}
	}
}

// GO-007: the trust of a dispatch is judged on the object and the commit it
// resolved, not on the tag as it is read afterwards. The tag is a lightweight
// one at commit A when the dispatch resolves it; before the trust is judged it
// is retargeted by the owner to an annotated tag on commit B, whose record is
// trusted: A runs, and must not borrow that record.
func TestDispatchOnATagRetargetedMeanwhileIsNotTrusted(t *testing.T) {
	var retarget func()
	c := newCIHarness(t, func(cfg *usecase.CIConfig) {
		cfg.DispatchResolved = func() {
			if retarget != nil {
				retarget()
			}
		}
	})
	c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/deploy.yml": dispatchWorkflow}, "")
	c.mustGit(c.wt, "tag", "v1")
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), "v1")
	commitA := c.ref("refs/heads/main")
	in := map[string]string{"version": "1"}

	run, err := c.dispatch(c.root, "deploy.yml", "v1", in)
	require.NoError(t, err)
	require.True(t, run.GetTrusted(), "without a retarget the owner's own tag is trusted")

	retarget = func() {
		retarget = nil // once
		c.writeFile(c.wt, "b.txt", []byte("b"))
		c.mustGit(c.wt, "tag", "-a", "-f", "-m", "annotated", "v1")
		c.mustGit(c.wt, "push", "-q", "-f", c.urlAuth(adminToken, "/proj.git"), "v1")
	}
	run, err = c.dispatch(c.root, "deploy.yml", "v1", in)
	require.NoError(t, err)
	assert.Equal(t, commitA, run.GetSha(), "the dispatch runs the commit it resolved")
	assert.False(t, run.GetTrusted(), "the tag moved to another object meanwhile: the record of the new one is not A's")

	// The tag really moved: dispatching it now is trusted again, on the new commit.
	run, err = c.dispatch(c.root, "deploy.yml", "v1", in)
	require.NoError(t, err)
	assert.NotEqual(t, commitA, run.GetSha())
	assert.True(t, run.GetTrusted())
}

// A tag is judged by its own record, like a branch.
func TestDispatchOnATagFollowsItsPusher(t *testing.T) {
	c := newCIHarness(t, nil)
	c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/deploy.yml": dispatchWorkflow}, "")
	c.mustGit(c.wt, "tag", "v1")
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), "v1")
	c.mustGit(c.wt, "tag", "-a", "-m", "annotated", "v2")
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), "v2")
	in := map[string]string{"version": "1"}

	run, err := c.dispatch(c.root, "deploy.yml", "v1", in)
	require.NoError(t, err)
	assert.True(t, run.GetTrusted(), "a lightweight tag the owner pushed")
	run, err = c.dispatch(c.root, "deploy.yml", "v2", in)
	require.NoError(t, err)
	assert.True(t, run.GetTrusted(), "an annotated tag the owner pushed (the record names the tag object)")

	tok := c.repoToken("scoped")
	c.writeFile(c.wt, "t.txt", []byte("t"))
	c.mustGit(c.wt, "tag", "v3")
	c.mustGit(c.wt, "push", "-q", c.urlAuth(tok, "/proj.git"), "v3")
	run, err = c.dispatch(c.root, "deploy.yml", "v3", in)
	require.NoError(t, err)
	assert.False(t, run.GetTrusted(), "a tag a scoped token pushed")
}
