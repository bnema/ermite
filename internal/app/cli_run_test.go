// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
)

// The CLI over a real server: `run list|view|watch|cancel|rerun`, `runner
// list|remove`, and the runner token preset.
func TestCLIRunsAndRunners(t *testing.T) {
	ci := newCIHarness(t, nil)
	h := &cliHarness{harness: ci.harness, url: ci.srv.URL}
	runner := ci.registerRunner("alpha", "ubuntu-latest")
	ci.push("main", map[string]string{".github/workflows/ci.yml": ciWorkflow}, "")
	ci.waitFor("a run", func() bool { return len(ci.runs()) == 1 })

	job := runner.await()
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "hello from the job")
	next := runner.await()
	runner.runJob(next, pb.CiConclusion_CI_CONCLUSION_FAILURE, nil, "this one failed")
	ci.waitFor("the run completes", func() bool { return ci.runs()[0].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })
	id := "1"

	// list
	out := h.cli(t, adminToken, "run", "list", "proj")
	require.Empty(t, out.err, out.stderr)
	assert.Contains(t, out.stdout, "RESULT")
	assert.Contains(t, out.stdout, "failure")
	assert.Contains(t, out.stdout, "ci")

	js := h.cli(t, adminToken, "run", "list", "proj", "--json")
	require.NoError(t, js.err)
	runs := decode[[]jsonRun](t, js.stdout)
	require.Len(t, runs, 1)
	assert.Equal(t, "failure", runs[0].Conclusion)
	assert.Equal(t, "completed", runs[0].Status)
	assert.Equal(t, "refs/heads/main", runs[0].Ref)
	assert.Equal(t, []string{"actor", "attempt", "completed_at", "conclusion", "created_at", "error", "event", "id", "name", "number", "ref", "repo", "sha", "started_at", "status", "title", "workflow", "workflow_path"}, keys(t, strings.TrimSpace(strings.Trim(strings.TrimSpace(js.stdout), "[]"))))

	none := h.cli(t, adminToken, "run", "list", "proj", "--status", "queued")
	require.NoError(t, none.err)
	assert.Contains(t, none.stderr, "no runs")
	bad := h.cli(t, adminToken, "run", "list", "proj", "--status", "weird")
	require.Error(t, bad.err)

	// view
	view := h.cli(t, adminToken, "run", "view", "proj", id)
	require.NoError(t, view.err, view.stderr)
	assert.Contains(t, view.stdout, "result")
	assert.Contains(t, view.stdout, "build")
	assert.Contains(t, view.stdout, "[alpha]")
	assert.Contains(t, view.stdout, "Set up job")

	vjs := h.cli(t, adminToken, "run", "view", "proj", id, "--json")
	detail := decode[jsonRunDetail](t, vjs.stdout)
	require.Len(t, detail.Jobs, 2)
	assert.Equal(t, "build", detail.Jobs[0].Key)
	assert.Equal(t, "vm", detail.Jobs[0].Isolation)
	assert.Equal(t, int32(1), detail.LatestAttempt)

	// --log, all jobs and one job
	all := h.cli(t, adminToken, "run", "view", "proj", id, "--log")
	require.NoError(t, all.err, all.stderr)
	assert.Contains(t, all.stdout, "== build ==")
	assert.Contains(t, all.stdout, "hello from the job")
	assert.Contains(t, all.stdout, "this one failed")
	one := h.cli(t, adminToken, "run", "view", "proj", id, "--log", "--job", "test")
	require.NoError(t, one.err)
	assert.Contains(t, one.stdout, "this one failed")
	assert.NotContains(t, one.stdout, "hello from the job")
	missing := h.cli(t, adminToken, "run", "view", "proj", id, "--log", "--job", "nope")
	require.Error(t, missing.err)
	assert.Contains(t, missing.err.Error(), `no job "nope"`)

	// watch: a completed failing run prints once and exits with an error.
	w := h.cli(t, adminToken, "run", "watch", "proj", id)
	require.Error(t, w.err)
	assert.Contains(t, w.err.Error(), "failure")
	assert.Contains(t, w.stdout, "build success")

	// rerun --failed, then cancel the new attempt.
	re := h.cli(t, adminToken, "run", "rerun", "proj", id, "--failed")
	require.NoError(t, re.err, re.stderr)
	assert.Contains(t, re.stderr, "attempt 2 started")
	ca := h.cli(t, adminToken, "run", "cancel", "proj", id, "--json")
	require.NoError(t, ca.err, ca.stderr)
	assert.Equal(t, int32(2), decode[jsonRun](t, ca.stdout).Attempt)
	// No runner holds a job of attempt 2, so the cancel ended it at once: a
	// completed run cannot be cancelled again.
	again := h.cli(t, adminToken, "run", "cancel", "proj", id)
	require.Error(t, again.err)
	assert.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(again.err))

	// A reader may look but not cancel or rerun.
	reader := h.token(scopes(domain.ScopeRepoRead))
	assert.NoError(t, h.cli(t, reader, "run", "view", "proj", id).err)
	denied := h.cli(t, reader, "run", "cancel", "proj", id)
	require.Error(t, denied.err)
	assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(denied.err))
	assert.Error(t, h.cli(t, adminToken, "run", "view", "proj", "abc").err)

	// runners
	rl := h.cli(t, adminToken, "runner", "list")
	require.NoError(t, rl.err, rl.stderr)
	assert.Contains(t, rl.stdout, "alpha")
	assert.Contains(t, rl.stdout, "vm")
	rjs := decode[[]jsonRunner](t, h.cli(t, adminToken, "runner", "list", "--json").stdout)
	require.Len(t, rjs, 1)
	assert.Equal(t, []string{"linux", "self-hosted", "ubuntu-latest", "x64"}, rjs[0].Labels)
	rm := h.cli(t, adminToken, "runner", "remove", rjs[0].ID)
	require.NoError(t, rm.err, rm.stderr)
	assert.Error(t, h.cli(t, adminToken, "runner", "remove", rjs[0].ID).err, "already removed")
	assert.Contains(t, h.cli(t, adminToken, "runner", "list").stderr, "no runners")

	// The runner token preset makes a token that registers runners and nothing
	// else.
	tok := h.cli(t, adminToken, "token", "create", "runner host", "--preset", "runner", "--json")
	require.NoError(t, tok.err, tok.stderr)
	created := decode[jsonCreatedToken](t, tok.stdout)
	assert.Equal(t, []string{"runner:register"}, created.Scopes)
	assert.NoError(t, h.cli(t, created.Secret, "runner", "list").err)
	assert.Error(t, h.cli(t, created.Secret, "run", "cancel", "proj", id).err)
}
