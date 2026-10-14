// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/config"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/usecase"
)

var secretsKeyA = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
var secretsKeyB = base64.StdEncoding.EncodeToString([]byte("fedcba9876543210fedcba9876543210"))

// newSecretsHarness is a CI harness whose server has ERMITE_SECRETS_KEY.
func newSecretsHarness(t *testing.T, key string) *ciHarness {
	t.Helper()
	o := harnessOpts{}
	if key != "" {
		o.env = map[string]string{config.EnvSecretsKey: key}
	}
	h := newHarnessWith(t, o, nil)
	c := &ciHarness{harness: h, root: h.api(adminToken), repo: "proj"}
	_, err := h.forge.Repos.Create(h.ctx, domain.Root(), ports.CreateRepoParams{Name: h.name("proj"), Visibility: domain.VisibilityPrivate})
	require.NoError(t, err)
	c.wt = h.workTree("main")
	return c
}

func instanceScope() *pb.SettingsScope {
	return &pb.SettingsScope{Where: &pb.SettingsScope_Instance{Instance: true}}
}
func repoScope(r string) *pb.SettingsScope {
	return &pb.SettingsScope{Where: &pb.SettingsScope_Repo{Repo: r}}
}
func envScope(r, e string) *pb.SettingsScope {
	return &pb.SettingsScope{Where: &pb.SettingsScope_Environment{Environment: &pb.EnvironmentRef{Repo: r, Name: e}}}
}

func (c *ciHarness) setSecret(sc *pb.SettingsScope, name, value string) {
	c.t.Helper()
	_, err := c.root.secret.SetSecret(c.ctx, connect.NewRequest(&pb.SetSecretRequest{Scope: sc, Name: name, Value: value}))
	require.NoError(c.t, err)
}

func (c *ciHarness) setVar(sc *pb.SettingsScope, name, value string) {
	c.t.Helper()
	_, err := c.root.vars.SetVariable(c.ctx, connect.NewRequest(&pb.SetVariableRequest{Scope: sc, Name: name, Value: value}))
	require.NoError(c.t, err)
}

const settingsWorkflow = `name: s
on:
  push:
    branches: ['**']
env:
  FROM_WORKFLOW: ${{ secrets.WF_SECRET }}
jobs:
  plain:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ vars.GREETING }}
  uses-one:
    runs-on: ubuntu-latest
    steps:
      - run: echo "$TOKEN"
        env:
          TOKEN: ${{ secrets.api_token }}
          OTHER: ${{ secrets['Bracket_One'] }}
  uses-all:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ toJSON(secrets) }}
  deploy:
    runs-on: ubuntu-latest
    environment: Production
    steps:
      - run: echo ${{ secrets.ENV_ONLY }} ${{ vars.GREETING }}
`

// jobsByKey fetches jobs from the runner until it holds one per key.
func (r *fakeRunner) jobsByKey(want int) map[string]*pb.Job {
	r.c.t.Helper()
	out := map[string]*pb.Job{}
	for len(out) < want {
		j := r.await()
		out[j.GetJobKey()] = j
	}
	return out
}

func secretNames(j *pb.Job) []string {
	var out []string
	for k := range j.GetSecrets() {
		out = append(out, k)
	}
	return out
}

// AC1/AC2: what a job receives. The owner's run gets the secrets its job
// refers to and no others, variables merged with environment over repository
// over instance, and the environment is created on first use.
func TestCIJobReceivesOnlyTheSecretsItRefersTo(t *testing.T) {
	c := newSecretsHarness(t, secretsKeyA)
	runner := c.registerRunner("alpha", "ubuntu-latest")

	c.setSecret(instanceScope(), "api_token", "instance-token")
	c.setSecret(repoScope("proj"), "API_TOKEN", "repo-token")
	c.setSecret(repoScope("proj"), "bracket_one", "bracket-value")
	c.setSecret(repoScope("proj"), "WF_SECRET", "workflow-level-secret")
	c.setSecret(repoScope("proj"), "UNUSED", "never-sent")
	c.setSecret(instanceScope(), "ENV_ONLY", "instance-env-only")
	c.setVar(instanceScope(), "GREETING", "from-instance")
	c.setVar(instanceScope(), "ONLY_INSTANCE", "i")
	c.setVar(repoScope("proj"), "GREETING", "from-repo")

	c.push("main", map[string]string{".github/workflows/s.yml": settingsWorkflow}, "")
	jobs := runner.jobsByKey(4)

	// A job that refers to no secret gets none, and the workflow-level env
	// counts for every job.
	assert.ElementsMatch(t, []string{"WF_SECRET"}, secretNames(jobs["plain"]))
	assert.Equal(t, "workflow-level-secret", jobs["plain"].GetSecrets()["WF_SECRET"])

	// secrets.NAME and secrets['NAME'], case-insensitive, repository over instance.
	one := jobs["uses-one"].GetSecrets()
	assert.ElementsMatch(t, []string{"API_TOKEN", "BRACKET_ONE", "WF_SECRET"}, secretNames(jobs["uses-one"]))
	assert.Equal(t, "repo-token", one["API_TOKEN"], "repository over instance")
	assert.Equal(t, "bracket-value", one["BRACKET_ONE"])
	assert.NotContains(t, one, "UNUSED")

	// toJSON(secrets) is any other use: every secret of the scopes.
	assert.ElementsMatch(t, []string{"API_TOKEN", "BRACKET_ONE", "WF_SECRET", "UNUSED", "ENV_ONLY"}, secretNames(jobs["uses-all"]))

	// The serialized Job holds only the values that were sent: the unused
	// secret is nowhere in the bytes, and no job but the one that reads
	// everything holds it.
	for key, j := range jobs {
		raw, err := proto.Marshal(j)
		require.NoError(t, err)
		if key == "uses-all" {
			assert.Contains(t, string(raw), "never-sent")
			continue
		}
		assert.NotContains(t, string(raw), "never-sent", key)
		assert.NotContains(t, string(raw), "instance-token", key)
	}

	// Variables are merged, and the same values are in the vars context.
	for key, j := range jobs {
		want := "from-repo"
		if key == "deploy" {
			assert.Equal(t, "Production", j.GetEnvironment())
		} else {
			assert.Empty(t, j.GetEnvironment(), key)
		}
		assert.Equal(t, want, j.GetVars()["GREETING"], key)
		assert.Equal(t, "i", j.GetVars()["ONLY_INSTANCE"], key)
		assert.Contains(t, j.GetContexts(), `"GREETING":"from-repo"`, key)
		assert.True(t, j.GetTrusted(), key)
	}

	// The environment was created by its first use, and a job that names one
	// reads the secrets and variables set in it afterwards.
	envs, err := c.root.env.ListEnvironments(c.ctx, connect.NewRequest(&pb.ListEnvironmentsRequest{Repo: "proj"}))
	require.NoError(t, err)
	require.Len(t, envs.Msg.GetEnvironments(), 1)
	assert.Equal(t, "Production", envs.Msg.GetEnvironments()[0].GetName())
}

func TestCIEnvironmentOverridesVariablesAndSecrets(t *testing.T) {
	c := newSecretsHarness(t, secretsKeyA)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	_, err := c.root.env.CreateEnvironment(c.ctx, connect.NewRequest(&pb.CreateEnvironmentRequest{Repo: "proj", Name: "production"}))
	require.NoError(t, err)

	c.setSecret(instanceScope(), "ENV_ONLY", "instance")
	c.setSecret(repoScope("proj"), "ENV_ONLY", "repo")
	c.setSecret(envScope("proj", "PRODUCTION"), "ENV_ONLY", "environment")
	c.setVar(instanceScope(), "GREETING", "instance")
	c.setVar(repoScope("proj"), "GREETING", "repo")
	c.setVar(envScope("proj", "Production"), "GREETING", "environment")

	c.push("main", map[string]string{".github/workflows/s.yml": settingsWorkflow}, "")
	jobs := runner.jobsByKey(4)
	deploy := jobs["deploy"]
	assert.Equal(t, "environment", deploy.GetSecrets()["ENV_ONLY"], "environment over repository over instance")
	assert.Equal(t, "environment", deploy.GetVars()["GREETING"])
	assert.Equal(t, "repo", jobs["plain"].GetVars()["GREETING"], "a job without the environment does not see its variables")
	assert.NotContains(t, jobs["plain"].GetSecrets(), "ENV_ONLY")
	assert.Contains(t, deploy.GetContexts(), `"GREETING":"environment"`)
	envs, err := c.root.env.ListEnvironments(c.ctx, connect.NewRequest(&pb.ListEnvironmentsRequest{Repo: "proj"}))
	require.NoError(t, err)
	assert.Len(t, envs.Msg.GetEnvironments(), 1, "an existing environment is not created again, whatever the case")
}

// D1 / AC1: the run of a token limited to branch prefixes is not trusted: no
// secret, and an owner who runs it again does not unlock them. An owner's run
// that an agent runs again loses them too.
func TestCIUntrustedRunsGetNoSecretsAndRerunsNeverRaiseTrust(t *testing.T) {
	c := newSecretsHarness(t, secretsKeyA)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.setSecret(repoScope("proj"), "API_TOKEN", "repo-token")
	c.setSecret(repoScope("proj"), "WF_SECRET", "wf")
	c.setVar(repoScope("proj"), "GREETING", "hello")

	agent, _ := c.branchToken(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite, domain.ScopeCIWrite), []string{"agent/x/"}, "proj")
	agentAPI := c.api(agent)

	// The owner's push: trusted.
	c.push("main", map[string]string{".github/workflows/s.yml": settingsWorkflow}, "")
	for _, j := range runner.jobsByKey(4) {
		assert.True(t, j.GetTrusted())
		assert.NotEmpty(t, j.GetSecrets(), j.GetJobKey())
		runner.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	}
	c.waitFor("main done", func() bool { return len(c.runs()) == 1 && c.runs()[0].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })
	assert.True(t, c.runs()[0].GetTrusted(), "Run.trusted")
	mainRun := c.runs()[0]

	// The agent's push: not trusted. It still gets the variables.
	c.pushAs(agent, "agent/x/a", map[string]string{"f.txt": "x"})
	c.waitFor("the agent run", func() bool { return len(c.runs()) == 2 })
	var agentRun *pb.Run
	for _, r := range c.runs() {
		if r.GetRef() == "refs/heads/agent/x/a" {
			agentRun = r
		}
	}
	require.NotNil(t, agentRun)
	assert.False(t, agentRun.GetTrusted())
	for _, j := range runner.jobsByKey(4) {
		assert.False(t, j.GetTrusted(), j.GetJobKey())
		assert.Empty(t, j.GetSecrets(), "no secret for an agent's run: "+j.GetJobKey())
		raw, err := proto.Marshal(j)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "repo-token", "untrusted: no secret value anywhere in the job")
		assert.Equal(t, "hello", j.GetVars()["GREETING"], "variables are not secret")
		runner.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	}
	c.waitFor("agent run done", func() bool { return c.getRun(agentRun.GetId()).GetRun().GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })

	// The owner re-runs the agent's run: still not trusted.
	_, err := c.root.run.RerunRun(c.ctx, connect.NewRequest(&pb.RerunRunRequest{Repo: "proj", RunId: agentRun.GetId()}))
	require.NoError(t, err)
	assert.False(t, c.getRun(agentRun.GetId()).GetRun().GetTrusted(), "a re-run never raises trust")
	for _, j := range runner.jobsByKey(4) {
		assert.Empty(t, j.GetSecrets(), "the owner re-running an agent's run does not unlock its secrets: "+j.GetJobKey())
		assert.False(t, j.GetTrusted())
		runner.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	}

	// An agent re-running the owner's run on its branch: only the branches under
	// its prefix are allowed, so a re-run of main is refused, and the trusted
	// flag of main is untouched.
	_, err = agentAPI.run.RerunRun(c.ctx, connect.NewRequest(&pb.RerunRunRequest{Repo: "proj", RunId: mainRun.GetId()}))
	assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	assert.True(t, c.getRun(mainRun.GetId()).GetRun().GetTrusted())

	// The owner re-running their own run keeps it.
	_, err = c.root.run.RerunRun(c.ctx, connect.NewRequest(&pb.RerunRunRequest{Repo: "proj", RunId: mainRun.GetId()}))
	require.NoError(t, err)
	assert.True(t, c.getRun(mainRun.GetId()).GetRun().GetTrusted())
	for _, j := range runner.jobsByKey(4) {
		assert.NotEmpty(t, j.GetSecrets(), j.GetJobKey())
		runner.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	}
}

// Without ERMITE_SECRETS_KEY secrets cannot be set (failed_precondition) and
// jobs get none; variables and environments work.
func TestCIWithoutASecretsKey(t *testing.T) {
	c := newSecretsHarness(t, "")
	runner := c.registerRunner("alpha", "ubuntu-latest")
	_, err := c.root.secret.SetSecret(c.ctx, connect.NewRequest(&pb.SetSecretRequest{Scope: repoScope("proj"), Name: "A", Value: "v"}))
	assert.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	c.setVar(repoScope("proj"), "GREETING", "still works")
	_, err = c.root.secret.ListSecrets(c.ctx, connect.NewRequest(&pb.ListSecretsRequest{Scope: repoScope("proj")}))
	require.NoError(t, err, "names can be listed")

	c.push("main", map[string]string{".github/workflows/s.yml": settingsWorkflow}, "")
	for _, j := range runner.jobsByKey(4) {
		assert.Empty(t, j.GetSecrets(), j.GetJobKey())
		assert.Equal(t, "still works", j.GetVars()["GREETING"])
	}
}

// The server refuses to start with another key than the one the secrets were
// sealed with, and starts again with the right one or after they are gone.
func TestAnotherSecretsKeyRefusesToStart(t *testing.T) {
	c := newSecretsHarness(t, secretsKeyA)
	c.setSecret(repoScope("proj"), "A", "value")

	cfg := c.forge.cfg
	c.srv.Close()
	c.forge.Close(c.ctx)

	other, err := base64.StdEncoding.DecodeString(secretsKeyB)
	require.NoError(t, err)
	bad := cfg
	bad.SecretsKey = other
	_, err = newForge(c.ctx, bad, options{hookExe: binary(t)})
	require.ErrorIs(t, err, domain.ErrSecretsKeyMismatch)
	assert.NotContains(t, err.Error(), secretsKeyB)
	assert.NotContains(t, err.Error(), secretsKeyA)

	f, err := newForge(c.ctx, cfg, options{hookExe: binary(t)})
	require.NoError(t, err, "the right key starts")
	srv := httptest.NewServer(f.server.Handler())
	t.Cleanup(func() { srv.Close(); f.Close(context.WithoutCancel(c.ctx)) })
	list, err := newClients(srv.URL+"/-/api", adminToken).secret.ListSecrets(c.ctx, connect.NewRequest(&pb.ListSecretsRequest{Scope: repoScope("proj")}))
	require.NoError(t, err)
	assert.Len(t, list.Msg.GetSecrets(), 1)
}

// Secret values never appear in a response of the settings services, in the
// ermite log, or in the run record.
func TestCISecretValuesAreNeverReturnedOrLogged(t *testing.T) {
	c := newSecretsHarness(t, secretsKeyA)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	const value = "SUPER-SECRET-VALUE-0123456789"
	c.setSecret(instanceScope(), "I", value+"-i")
	c.setSecret(repoScope("proj"), "R", value+"-r")
	_, err := c.root.env.CreateEnvironment(c.ctx, connect.NewRequest(&pb.CreateEnvironmentRequest{Repo: "proj", Name: "prod"}))
	require.NoError(t, err)
	c.setSecret(envScope("proj", "prod"), "E", value+"-e")

	var seen []proto.Message
	set, err := c.root.secret.SetSecret(c.ctx, connect.NewRequest(&pb.SetSecretRequest{Scope: repoScope("proj"), Name: "r", Value: value + "-r2"}))
	require.NoError(t, err)
	seen = append(seen, set.Msg)
	for _, sc := range []*pb.SettingsScope{instanceScope(), repoScope("proj"), envScope("proj", "PROD")} {
		list, err := c.root.secret.ListSecrets(c.ctx, connect.NewRequest(&pb.ListSecretsRequest{Scope: sc}))
		require.NoError(t, err)
		require.NotEmpty(t, list.Msg.GetSecrets())
		seen = append(seen, list.Msg)
		for _, s := range list.Msg.GetSecrets() {
			assert.Equal(t, strings.ToUpper(s.GetName()), s.GetName(), "names are stored in upper case")
			assert.NotNil(t, s.GetUpdatedAt())
		}
	}
	for _, m := range seen {
		assert.NotContains(t, protojson.Format(m), value)
	}

	// An invalid request echoes no value either.
	_, err = c.root.secret.SetSecret(c.ctx, connect.NewRequest(&pb.SetSecretRequest{Scope: repoScope("proj"), Name: "GITHUB_X", Value: value}))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), value)

	c.push("main", map[string]string{".github/workflows/s.yml": "on: push\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ${{ secrets.R }}\n"}, "")
	job := runner.await()
	assert.Equal(t, value+"-r2", job.GetSecrets()["R"])
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
	c.waitFor("done", func() bool { return len(c.runs()) == 1 && c.runs()[0].GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED })

	// Nothing the owner can read about the run holds the value.
	run := c.getRun(c.runs()[0].GetId())
	assert.NotContains(t, protojson.Format(run), value)
	src, err := c.forge.store.CI().GetRunSource(c.ctx, c.runs()[0].GetId())
	require.NoError(t, err)
	assert.NotContains(t, src.GithubJSON, value)
	assert.NotContains(t, src.WorkflowYAML, value)
	jobs, err := c.forge.store.CI().ListJobs(c.ctx, c.runs()[0].GetId(), 1)
	require.NoError(t, err)
	for _, j := range jobs {
		assert.NotContains(t, j.ContextsJSON, value)
	}
	assert.NotContains(t, c.logs.String(), value, "the ermite log")
}

// Branch-limited tokens never manage secrets, whatever their scopes (ADR 0027);
// a token needs secrets:write on the repository, and the instance scope needs
// one that is not limited to repositories.
func TestSettingsServicesAccessRules(t *testing.T) {
	c := newSecretsHarness(t, secretsKeyA)
	_, err := c.forge.Repos.Create(c.ctx, domain.Root(), ports.CreateRepoParams{Name: c.name("other"), Visibility: domain.VisibilityPublic})
	require.NoError(t, err)

	limited, _ := c.branchToken(scopes(domain.ScopeSecretsWrite, domain.ScopeRepoRead), []string{"agent/"})
	reader := c.token(scopes(domain.ScopeRepoRead), "proj")
	writer := c.token(scopes(domain.ScopeSecretsWrite, domain.ScopeRepoRead), "proj")
	writerAll := c.token(scopes(domain.ScopeSecretsWrite))

	code := func(secret string, sc *pb.SettingsScope) (set, list, vset, vget, del connect.Code) {
		api := c.api(secret)
		codeOf := func(err error) connect.Code {
			if err == nil {
				return 0 // ok
			}
			return connect.CodeOf(err)
		}
		_, err := api.secret.SetSecret(c.ctx, connect.NewRequest(&pb.SetSecretRequest{Scope: sc, Name: "A", Value: "v"}))
		set = codeOf(err)
		_, err = api.secret.ListSecrets(c.ctx, connect.NewRequest(&pb.ListSecretsRequest{Scope: sc}))
		list = codeOf(err)
		_, err = api.vars.SetVariable(c.ctx, connect.NewRequest(&pb.SetVariableRequest{Scope: sc, Name: "A", Value: "v"}))
		vset = codeOf(err)
		_, err = api.vars.GetVariable(c.ctx, connect.NewRequest(&pb.GetVariableRequest{Scope: sc, Name: "A"}))
		vget = codeOf(err)
		_, err = api.secret.DeleteSecret(c.ctx, connect.NewRequest(&pb.DeleteSecretRequest{Scope: sc, Name: "ZZ"}))
		del = codeOf(err)
		return
	}
	allOf := func(want connect.Code, got ...connect.Code) {
		t.Helper()
		for i, g := range got {
			assert.Equal(t, want, g, "call %d", i)
		}
	}

	// A branch-limited token with secrets:write is refused everywhere.
	for _, sc := range []*pb.SettingsScope{repoScope("proj"), instanceScope(), envScope("proj", "x")} {
		a, b, d, e, f := code(limited, sc)
		allOf(connect.CodePermissionDenied, a, b, d, e, f)
	}
	_, err = c.api(limited).env.CreateEnvironment(c.ctx, connect.NewRequest(&pb.CreateEnvironmentRequest{Repo: "proj", Name: "x"}))
	assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))

	// Without secrets:write, a repository the token reads: permission denied.
	a, b, d, e, f := code(reader, repoScope("proj"))
	allOf(connect.CodePermissionDenied, a, b, d, e, f)
	// Anonymous.
	a, b, d, e, f = code("", repoScope("proj"))
	allOf(connect.CodeUnauthenticated, a, b, d, e, f)
	// With it: allowed (the delete of a missing secret is not found).
	a, b, d, e, f = code(writer, repoScope("proj"))
	assert.Equal(t, []connect.Code{0, 0, 0, 0, connect.CodeNotFound}, []connect.Code{a, b, d, e, f})
	// The writer is limited to one repository: not the instance, not another one.
	a, b, d, e, f = code(writer, instanceScope())
	allOf(connect.CodePermissionDenied, a, b, d, e, f)
	a, b, d, e, f = code(writer, repoScope("other"))
	allOf(connect.CodePermissionDenied, a, b, d, e, f) // can read it (public), may not write there
	// Not restricted to repositories: the instance scope works.
	a, b, d, e, f = code(writerAll, instanceScope())
	assert.Equal(t, []connect.Code{0, 0, 0, 0, connect.CodeNotFound}, []connect.Code{a, b, d, e, f})

	// A repository the caller cannot see is not found.
	_, err = c.forge.Repos.Create(c.ctx, domain.Root(), ports.CreateRepoParams{Name: c.name("hidden"), Visibility: domain.VisibilityPrivate})
	require.NoError(t, err)
	a, b, d, e, f = code(writer, repoScope("hidden"))
	allOf(connect.CodeNotFound, a, b, d, e, f)
	a, _, _, _, _ = code(writer, repoScope("ghost"))
	assert.Equal(t, connect.CodeNotFound, a)

	// Validation.
	root := c.root
	_, err = root.secret.SetSecret(c.ctx, connect.NewRequest(&pb.SetSecretRequest{Scope: repoScope("proj"), Name: "1BAD", Value: "v"}))
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
	_, err = root.secret.SetSecret(c.ctx, connect.NewRequest(&pb.SetSecretRequest{Scope: repoScope("proj"), Name: "github_token", Value: "v"}))
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
	_, err = root.secret.SetSecret(c.ctx, connect.NewRequest(&pb.SetSecretRequest{Scope: repoScope("proj"), Name: "OK", Value: ""}))
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "an empty secret")
	_, err = root.secret.SetSecret(c.ctx, connect.NewRequest(&pb.SetSecretRequest{Scope: repoScope("proj"), Name: "OK", Value: strings.Repeat("x", 49153)}))
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "over 48 KiB")
	_, err = root.secret.SetSecret(c.ctx, connect.NewRequest(&pb.SetSecretRequest{Scope: repoScope("proj"), Name: "OK", Value: strings.Repeat("x", 49152)}))
	assert.NoError(t, err, "exactly 48 KiB")
	_, err = root.secret.SetSecret(c.ctx, connect.NewRequest(&pb.SetSecretRequest{Name: "OK", Value: "v"}))
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "no scope")
	_, err = root.secret.SetSecret(c.ctx, connect.NewRequest(&pb.SetSecretRequest{Scope: &pb.SettingsScope{Where: &pb.SettingsScope_Instance{}}, Name: "OK", Value: "v"}))
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "instance: false")
	_, err = root.vars.SetVariable(c.ctx, connect.NewRequest(&pb.SetVariableRequest{Scope: repoScope("proj"), Name: "EMPTY", Value: ""}))
	assert.NoError(t, err, "an empty variable is allowed")
	_, err = root.secret.SetSecret(c.ctx, connect.NewRequest(&pb.SetSecretRequest{Scope: envScope("proj", "missing"), Name: "OK", Value: "v"}))
	assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err), "an environment that does not exist")
	_, err = root.env.CreateEnvironment(c.ctx, connect.NewRequest(&pb.CreateEnvironmentRequest{Repo: "proj", Name: "bad\nname"}))
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
	_, err = root.env.DeleteEnvironment(c.ctx, connect.NewRequest(&pb.DeleteEnvironmentRequest{Repo: "proj", Name: "missing"}))
	assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
}

func TestSettingsServicesPagingAndCrud(t *testing.T) {
	c := newSecretsHarness(t, secretsKeyA)
	for _, n := range []string{"c", "a", "b"} {
		c.setSecret(repoScope("proj"), n, "v")
		c.setVar(repoScope("proj"), n, "val-"+n)
	}
	var names []string
	token := ""
	for range 5 {
		res, err := c.root.secret.ListSecrets(c.ctx, connect.NewRequest(&pb.ListSecretsRequest{Scope: repoScope("proj"), PageSize: 2, PageToken: token}))
		require.NoError(t, err)
		for _, s := range res.Msg.GetSecrets() {
			names = append(names, s.GetName())
		}
		if token = res.Msg.GetNextPageToken(); token == "" {
			break
		}
	}
	assert.Equal(t, []string{"A", "B", "C"}, names)

	v, err := c.root.vars.GetVariable(c.ctx, connect.NewRequest(&pb.GetVariableRequest{Scope: repoScope("proj"), Name: "b"}))
	require.NoError(t, err)
	assert.Equal(t, "val-b", v.Msg.GetVariable().GetValue())
	vl, err := c.root.vars.ListVariables(c.ctx, connect.NewRequest(&pb.ListVariablesRequest{Scope: repoScope("proj")}))
	require.NoError(t, err)
	require.Len(t, vl.Msg.GetVariables(), 3)
	assert.Equal(t, "A", vl.Msg.GetVariables()[0].GetName())
	_, err = c.root.vars.DeleteVariable(c.ctx, connect.NewRequest(&pb.DeleteVariableRequest{Scope: repoScope("proj"), Name: "A"}))
	require.NoError(t, err)
	_, err = c.root.vars.GetVariable(c.ctx, connect.NewRequest(&pb.GetVariableRequest{Scope: repoScope("proj"), Name: "a"}))
	assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
	_, err = c.root.secret.DeleteSecret(c.ctx, connect.NewRequest(&pb.DeleteSecretRequest{Scope: repoScope("proj"), Name: "b"}))
	require.NoError(t, err)
	_, err = c.root.secret.ListSecrets(c.ctx, connect.NewRequest(&pb.ListSecretsRequest{Scope: repoScope("proj"), PageToken: "garbage"}))
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))

	// Environments: create twice (any case), list, delete with their content.
	e1, err := c.root.env.CreateEnvironment(c.ctx, connect.NewRequest(&pb.CreateEnvironmentRequest{Repo: "proj", Name: "Staging"}))
	require.NoError(t, err)
	e2, err := c.root.env.CreateEnvironment(c.ctx, connect.NewRequest(&pb.CreateEnvironmentRequest{Repo: "proj", Name: "STAGING"}))
	require.NoError(t, err)
	assert.Equal(t, "Staging", e2.Msg.GetEnvironment().GetName(), "the case it was given is kept")
	assert.Equal(t, e1.Msg.GetEnvironment().GetCreatedAt().AsTime(), e2.Msg.GetEnvironment().GetCreatedAt().AsTime())
	c.setSecret(envScope("proj", "staging"), "S", "v")
	_, err = c.root.env.DeleteEnvironment(c.ctx, connect.NewRequest(&pb.DeleteEnvironmentRequest{Repo: "proj", Name: "staging"}))
	require.NoError(t, err)
	_, err = c.root.secret.ListSecrets(c.ctx, connect.NewRequest(&pb.ListSecretsRequest{Scope: envScope("proj", "Staging")}))
	assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err))

	// Deleting the repository removes its scopes.
	require.NoError(t, c.forge.Repos.Delete(c.ctx, domain.Root(), c.name("proj")))
	_, err = c.forge.Repos.Create(c.ctx, domain.Root(), ports.CreateRepoParams{Name: c.name("proj"), Visibility: domain.VisibilityPrivate})
	require.NoError(t, err)
	res, err := c.root.secret.ListSecrets(c.ctx, connect.NewRequest(&pb.ListSecretsRequest{Scope: repoScope("proj")}))
	require.NoError(t, err)
	assert.Empty(t, res.Msg.GetSecrets())
	vl, err = c.root.vars.ListVariables(c.ctx, connect.NewRequest(&pb.ListVariablesRequest{Scope: repoScope("proj")}))
	require.NoError(t, err)
	assert.Empty(t, vl.Msg.GetVariables())
}

// environment: as a mapping (the url is ignored) and as an expression of the
// matrix; an untrusted run does not add environments to the repository.
func TestCIEnvironmentForms(t *testing.T) {
	c := newSecretsHarness(t, secretsKeyA)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	_, err := c.root.vars.SetVariable(c.ctx, connect.NewRequest(&pb.SetVariableRequest{Scope: envScope("proj", "alpha"), Name: "V", Value: "x"}))
	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err), "not an environment yet")
	_, err = c.root.env.CreateEnvironment(c.ctx, connect.NewRequest(&pb.CreateEnvironmentRequest{Repo: "proj", Name: "Alpha"}))
	require.NoError(t, err)
	c.setVar(envScope("proj", "alpha"), "V", "from-alpha")

	c.push("main", map[string]string{".github/workflows/e.yml": `on: push
jobs:
  mapping:
    runs-on: ubuntu-latest
    environment:
      name: alpha
      url: https://example.test
    steps:
      - run: echo
  matrix:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        env: [Alpha, beta]
    environment: ${{ matrix.env }}
    steps:
      - run: echo
`}, "")
	jobs := map[string]*pb.Job{}
	for len(jobs) < 3 {
		j := runner.await()
		key := j.GetJobKey() + "/" + j.GetEnvironment()
		jobs[key] = j
	}
	assert.Equal(t, "from-alpha", jobs["mapping/alpha"].GetVars()["V"])
	assert.Equal(t, "from-alpha", jobs["matrix/Alpha"].GetVars()["V"])
	assert.NotContains(t, jobs["matrix/beta"].GetVars(), "V")

	names := func() []string {
		res, err := c.root.env.ListEnvironments(c.ctx, connect.NewRequest(&pb.ListEnvironmentsRequest{Repo: "proj"}))
		require.NoError(t, err)
		var out []string
		for _, e := range res.Msg.GetEnvironments() {
			out = append(out, e.GetName())
		}
		return out
	}
	assert.Equal(t, []string{"Alpha", "beta"}, names(), "beta was created on first use; alpha already existed")

	// An agent's run names a new environment: it is not created.
	agent, _ := c.branchToken(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite), []string{"agent/x/"}, "proj")
	c.pushAs(agent, "agent/x/a", map[string]string{".github/workflows/e2.yml": "on: push\njobs:\n  j:\n    runs-on: ubuntu-latest\n    environment: agent-made\n    steps:\n      - run: echo\n"})
	var seen *pb.Job
	for seen == nil || seen.GetEnvironment() != "agent-made" {
		seen = runner.await()
	}
	assert.False(t, seen.GetTrusted())
	assert.Equal(t, []string{"Alpha", "beta"}, names())
}

// flakySettings fails the next n resolutions, then works again.
type flakySettings struct {
	inner ports.CIJobSettings
	fail  atomic.Int32
	// err is what a failure returns; nil is a plain error.
	err error
}

func (f *flakySettings) Resolve(ctx context.Context, req ports.JobSettingsRequest) (ports.JobSettings, error) {
	if f.fail.Add(-1) >= 0 {
		if f.err != nil {
			return ports.JobSettings{}, f.err
		}
		return ports.JobSettings{}, errors.New("injected failure")
	}
	f.fail.Store(0)
	return f.inner.Resolve(ctx, req)
}

func newFlakyHarness(t *testing.T) (*ciHarness, *flakySettings) {
	t.Helper()
	flaky := &flakySettings{}
	h := newHarnessWith(t, harnessOpts{env: map[string]string{config.EnvSecretsKey: secretsKeyA}}, func(o *options) {
		o.ci = func(cfg *usecase.CIConfig) { flaky.inner = cfg.Settings; cfg.Settings = flaky }
	})
	c := &ciHarness{harness: h, root: h.api(adminToken), repo: "proj"}
	_, err := h.forge.Repos.Create(h.ctx, domain.Root(), ports.CreateRepoParams{Name: h.name("proj"), Visibility: domain.VisibilityPrivate})
	require.NoError(t, err)
	c.wt = h.workTree("main")
	return c, flaky
}

// jobRecord reads the stored record of the only job.
func (c *ciHarness) jobRecord(runID int64) domain.CIJob {
	c.t.Helper()
	jobs, err := c.forge.store.CI().ListJobs(c.ctx, runID, 1)
	require.NoError(c.t, err)
	require.Len(c.t, jobs, 1)
	return jobs[0]
}

const oneSecretJob = "on: push\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ${{ secrets.A }}\n"

// A settings resolver that fails at the assignment leaves the job ready: no
// write, no token; the runner's retry gets it. The same holds for the repeat of
// a poll, which reads the settings again.
func TestCIAssignmentSurvivesASettingsFailure(t *testing.T) {
	c, flaky := newFlakyHarness(t)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.setSecret(repoScope("proj"), "A", "value")
	c.push("main", map[string]string{".github/workflows/s.yml": oneSecretJob}, "")
	c.waitFor("a ready job", func() bool {
		ready, err := c.forge.store.CI().ListReadyJobs(c.ctx, 0)
		return err == nil && len(ready) == 1
	})
	runID := c.runs()[0].GetId()
	before := c.jobRecord(runID)
	require.Equal(t, domain.CIPhaseReady, before.Phase)

	flaky.fail.Store(1)
	_, err := runner.fetch("")
	require.Error(t, err, "the poll fails instead of losing the job")
	after := c.jobRecord(runID)
	assert.Equal(t, domain.CIPhaseReady, after.Phase)
	assert.Equal(t, before.Version, after.Version, "no assignment write")
	assert.False(t, after.HasToken(), "no job token")
	ready, err := c.forge.store.CI().ListReadyJobs(c.ctx, 0)
	require.NoError(t, err)
	assert.Len(t, ready, 1, "still in the queue")
	assert.Zero(t, c.repoTokens(), "no repository token issued")

	// The retry works.
	job, err := runner.fetch("poll-1")
	require.NoError(t, err)
	require.NotNil(t, job)
	assert.Equal(t, "value", job.GetSecrets()["A"])
	assigned := c.jobRecord(runID)
	assert.Equal(t, domain.CIPhaseAssigned, assigned.Phase)
	assert.Equal(t, 1, c.repoTokens())

	// A repeat of the poll reads the settings again: a failure is an error, the
	// assignment stays, and the next repeat returns the very same job.
	flaky.fail.Store(1)
	_, err = runner.fetch("poll-1")
	require.Error(t, err)
	same := c.jobRecord(runID)
	assert.Equal(t, assigned.Version, same.Version)
	assert.Equal(t, assigned.TokenHash, same.TokenHash)
	assert.Equal(t, 1, c.repoTokens())
	again, err := runner.fetch("poll-1")
	require.NoError(t, err)
	require.NotNil(t, again)
	assert.Equal(t, job.GetJobToken(), again.GetJobToken())
	assert.Equal(t, job.GetGithubToken(), again.GetGithubToken())
	assert.Equal(t, "value", again.GetSecrets()["A"])
}

// repoTokens counts the job repository tokens stored for the repository.
func (c *ciHarness) repoTokens() int {
	c.t.Helper()
	n, err := c.forge.store.CI().CountRepoTokens(c.ctx, c.name("proj"))
	require.NoError(c.t, err)
	return n
}

const varsEverywhereWorkflow = `name: v
run-name: run ${{ vars.RUN }}
on: push
jobs:
  j:
    if: ${{ vars.GATE == 'on' }}
    runs-on: ${{ vars.LABEL }}
    strategy:
      matrix:
        n: ${{ fromJSON(vars.LEGS) }}
    environment: ${{ vars.ENVNAME }}
    steps:
      - run: echo ${{ vars.LATE }}
`

// Variables are read where the workflow reads them: in run-name, a job's if,
// strategy, runs-on and environment name, when the job is planned; and again
// when the job is assigned, without planning it again.
func TestCIVarsInPlanningAndAtAssignment(t *testing.T) {
	c := newSecretsHarness(t, secretsKeyA)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	for k, v := range map[string]string{"RUN": "first", "GATE": "on", "LABEL": "ubuntu-latest", "LEGS": "[1, 2]", "ENVNAME": "Prod", "LATE": "before"} {
		c.setVar(repoScope("proj"), k, v)
	}
	c.push("main", map[string]string{".github/workflows/v.yml": varsEverywhereWorkflow}, "")
	c.waitFor("two ready legs", func() bool {
		ready, err := c.forge.store.CI().ListReadyJobs(c.ctx, 0)
		return err == nil && len(ready) == 2
	})
	assert.Equal(t, "run first", c.runs()[0].GetName(), "run-name")

	// Change variables after the jobs are ready and before they are fetched.
	c.setVar(repoScope("proj"), "LATE", "after")
	c.setVar(repoScope("proj"), "GATE", "off")
	c.setVar(repoScope("proj"), "LABEL", "gpu")
	c.setVar(repoScope("proj"), "LEGS", "[1, 2, 3, 4]")
	c.setVar(repoScope("proj"), "RUN", "second")

	got := map[string]*pb.Job{}
	for len(got) < 2 {
		j := runner.await()
		got[j.GetJobId()] = j
	}
	assert.Len(t, got, 2, "the strategy is not planned again: two legs, not four")
	for _, j := range got {
		assert.Equal(t, "after", j.GetVars()["LATE"], "the assignment sees the new value")
		assert.Contains(t, j.GetContexts(), `"LATE":"after"`)
		assert.Contains(t, j.GetContexts(), `"GATE":"off"`, "all the variables are current")
		assert.Equal(t, []string{"ubuntu-latest"}, j.GetLabels(), "runs-on is the planned one")
		assert.Equal(t, "Prod", j.GetEnvironment())
	}
	assert.Equal(t, "run first", c.runs()[0].GetName(), "the run is not renamed")
	// The environment named by a variable is created on first use, at assignment.
	envs, err := c.root.env.ListEnvironments(c.ctx, connect.NewRequest(&pb.ListEnvironmentsRequest{Repo: "proj"}))
	require.NoError(t, err)
	require.Len(t, envs.Msg.GetEnvironments(), 1)
	assert.Equal(t, "Prod", envs.Msg.GetEnvironments()[0].GetName(), "environment name")
}

// One rule for environment names: what the API accepts is what a workflow can
// name, with the same spelling; a name with surrounding whitespace is refused by
// the API and fails the job with a clear error instead of being trimmed.
func TestCIEnvironmentNameRoundTrip(t *testing.T) {
	c := newSecretsHarness(t, secretsKeyA)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	const name = "Prod EU/2"
	_, err := c.root.env.CreateEnvironment(c.ctx, connect.NewRequest(&pb.CreateEnvironmentRequest{Repo: "proj", Name: name}))
	require.NoError(t, err)
	c.setVar(envScope("proj", name), "WHERE", "eu")
	for _, bad := range []string{" Prod", "Prod ", "\tProd", "Prod\u00a0"} {
		_, err := c.root.env.CreateEnvironment(c.ctx, connect.NewRequest(&pb.CreateEnvironmentRequest{Repo: "proj", Name: bad}))
		assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "%q", bad)
	}

	c.push("main", map[string]string{
		".github/workflows/ok.yml":  "on: push\njobs:\n  ok:\n    runs-on: ubuntu-latest\n    environment: '" + name + "'\n    steps:\n      - run: echo\n",
		".github/workflows/bad.yml": "on: push\njobs:\n  bad:\n    runs-on: ubuntu-latest\n    environment: ' Prod EU/2 '\n    steps:\n      - run: echo\n",
	}, "")
	job := runner.await()
	assert.Equal(t, "ok", job.GetJobKey())
	assert.Equal(t, name, job.GetEnvironment())
	assert.Equal(t, "eu", job.GetVars()["WHERE"], "the job reads the environment the API created")

	c.waitFor("the bad job fails", func() bool {
		for _, r := range c.runs() {
			if r.GetWorkflowPath() == ".github/workflows/bad.yml" && r.GetStatus() == pb.CiStatus_CI_STATUS_COMPLETED {
				return true
			}
		}
		return false
	})
	for _, r := range c.runs() {
		if r.GetWorkflowPath() != ".github/workflows/bad.yml" {
			continue
		}
		j := c.getRun(r.GetId()).GetJobs()[0]
		assert.Equal(t, pb.CiConclusion_CI_CONCLUSION_FAILURE, j.GetConclusion())
		assert.Contains(t, j.GetError(), "environment name")
		assert.Contains(t, j.GetError(), "whitespace")
	}
	envs, err := c.root.env.ListEnvironments(c.ctx, connect.NewRequest(&pb.ListEnvironmentsRequest{Repo: "proj"}))
	require.NoError(t, err)
	assert.Len(t, envs.Msg.GetEnvironments(), 1, "no trimmed copy was created")
}

// A settings failure that wraps ErrConflict (an environment creation that ran
// out of retries) or ErrNotFound is not "nothing to repeat" and not "taken
// meanwhile": the repeated poll fails, and no other job is assigned in its
// place. Fetching without a poll id fails too and leaves every job ready.
func TestCIRepeatedPollPropagatesSettingsConflicts(t *testing.T) {
	for name, injected := range map[string]error{
		"conflict":  fmt.Errorf("create environment: %w", domain.ErrConflict),
		"not found": fmt.Errorf("environment: %w", domain.ErrNotFound),
	} {
		t.Run(name, func(t *testing.T) {
			c, flaky := newFlakyHarness(t)
			runner := c.registerRunner("alpha", "ubuntu-latest")
			c.setSecret(repoScope("proj"), "A", "value")
			c.push("main", map[string]string{
				".github/workflows/one.yml": oneSecretJob,
				".github/workflows/two.yml": oneSecretJob,
			}, "")
			c.waitFor("two ready jobs", func() bool {
				ready, err := c.forge.store.CI().ListReadyJobs(c.ctx, 0)
				return err == nil && len(ready) == 2
			})

			first, err := runner.fetch("poll-1")
			require.NoError(t, err)
			require.NotNil(t, first)
			ready, err := c.forge.store.CI().ListReadyJobs(c.ctx, 0)
			require.NoError(t, err)
			require.Len(t, ready, 1, "the other job waits")

			flaky.err = injected
			flaky.fail.Store(1)
			res, err := runner.fetch("poll-1")
			require.Error(t, err, "the repeat fails")
			assert.Nil(t, res, "and gives no other job")
			ready, err = c.forge.store.CI().ListReadyJobs(c.ctx, 0)
			require.NoError(t, err)
			assert.Len(t, ready, 1, "the second job was not assigned in its place")
			assert.Equal(t, 1, c.repoTokens(), "no token was issued for another job")

			// A poll without an id fails the same way and assigns nothing.
			flaky.fail.Store(1)
			_, err = runner.fetch("")
			require.Error(t, err)
			ready, err = c.forge.store.CI().ListReadyJobs(c.ctx, 0)
			require.NoError(t, err)
			assert.Len(t, ready, 1)

			// Once it works again, the repeat is the first job, unchanged.
			flaky.err = nil
			again, err := runner.fetch("poll-1")
			require.NoError(t, err)
			require.NotNil(t, again)
			assert.Equal(t, first.GetJobId(), again.GetJobId())
			assert.Equal(t, first.GetJobToken(), again.GetJobToken())
		})
	}
}
