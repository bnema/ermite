// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/config"
	"bnema.dev/ermite/internal/domain"
)

func TestParseEnvFile(t *testing.T) {
	got, err := parseEnvFile(strings.NewReader(`# comment

A=1
export B = two words
C='single $quoted\n'
D="double\nquoted \"q\" \\ end"
E=
F=a=b
G="unterminated
`))
	require.NoError(t, err)
	assert.Equal(t, [][2]string{
		{"A", "1"}, {"B", "two words"}, {"C", `single $quoted\n`}, {"D", "double\nquoted \"q\" \\ end"},
		{"E", ""}, {"F", "a=b"}, {"G", `"unterminated`},
	}, got)

	for _, bad := range []string{"no equals sign\n", "=value\n", "  = x\n"} {
		_, err := parseEnvFile(strings.NewReader(bad))
		require.Error(t, err, bad)
		assert.Contains(t, err.Error(), "line 1")
	}
}

func newSecretsCLI(t *testing.T) *cliHarness {
	t.Helper()
	h := newHarnessWith(t, harnessOpts{env: map[string]string{config.EnvSecretsKey: secretsKeyA}}, nil)
	h.createRepo("proj", domain.VisibilityPrivate)
	return &cliHarness{harness: h, url: h.srv.URL}
}

// `ermite secret|variable|environment` against a real server: scopes, value
// sources, listings, and that no command ever prints a secret value.
func TestCLISecretsVariablesEnvironments(t *testing.T) {
	h := newSecretsCLI(t)
	run := func(stdin string, args ...string) result {
		return runCLI(t, h.vars(adminToken), stdin, args...)
	}
	const value = "S3CRET-VALUE-do-not-print"
	var outputs strings.Builder
	record := func(r result) result { outputs.WriteString(r.stdout + "\n" + r.stderr + "\n"); return r }

	// A value from standard input, with its trailing line break dropped; one
	// from --body; and a multi-line one.
	r := record(run(value+"\n", "secret", "set", "api_token", "--repo", "proj").ok(t))
	assert.Equal(t, "set secret API_TOKEN for proj\n", r.stdout)
	record(run("", "secret", "set", "BODY_ONE", "--repo", "proj", "--body", value+"-body").ok(t))
	record(run("line one\nline two\n", "secret", "set", "MULTI", "-R", "proj").ok(t))
	record(run(value+"-inst", "secret", "set", "INST", "--instance").ok(t))

	// An environment: created by the command, then used with --env.
	r = run("", "environment", "create", "Production", "--repo", "proj").ok(t)
	assert.Equal(t, "environment Production of proj\n", r.stdout)
	run("", "environment", "create", "production", "-R", "proj").ok(t)
	record(run(value+"-env", "secret", "set", "ENV_ONE", "--repo", "proj", "--env", "PRODUCTION").ok(t))

	// list: names and dates only, ordered, per scope; --json too.
	r = record(run("", "secret", "list", "--repo", "proj").ok(t))
	assert.Contains(t, r.stdout, "NAME")
	assert.Regexp(t, `(?s)API_TOKEN.*BODY_ONE.*MULTI`, r.stdout)
	assert.NotContains(t, r.stdout, "ENV_ONE", "the environment's own scope is not mixed in")
	r = record(run("", "secret", "list", "--repo", "proj", "--env", "production", "--json").ok(t))
	secrets := decode[[]jsonSecret](t, r.stdout)
	require.Len(t, secrets, 1)
	assert.Equal(t, "ENV_ONE", secrets[0].Name)
	assert.NotNil(t, secrets[0].UpdatedAt)
	assert.Equal(t, []string{"name", "updated_at"}, keys(t, strings.TrimSpace(strings.Trim(strings.TrimSpace(r.stdout), "[]"))))
	r = record(run("", "secret", "list", "--instance", "--json").ok(t))
	assert.Equal(t, "INST", decode[[]jsonSecret](t, r.stdout)[0].Name)

	// delete
	r = run("", "secret", "delete", "body_one", "--repo", "proj").ok(t)
	assert.Equal(t, "deleted secret BODY_ONE from proj\n", r.stdout)
	gone := run("", "secret", "delete", "BODY_ONE", "--repo", "proj")
	require.Error(t, gone.err)
	r = record(run("", "secret", "list", "--repo", "proj", "--json").ok(t))
	assert.Len(t, decode[[]jsonSecret](t, r.stdout), 2)

	// --env-file sets several at once, in a repository and on the instance.
	envFile := filepath.Join(t.TempDir(), ".env")
	require.NoError(t, os.WriteFile(envFile, []byte("# secrets\nFROM_FILE_A="+value+"-a\nexport FROM_FILE_B='"+value+"-b'\n"), 0o600))
	r = record(run("", "secret", "set", "--env-file", envFile, "--repo", "proj").ok(t))
	assert.Equal(t, "set secret FROM_FILE_A for proj\nset secret FROM_FILE_B for proj\n", r.stdout)
	record(run("X_STDIN="+value+"-x\n", "secret", "set", "-f", "-", "--instance").ok(t))
	r = record(run("", "secret", "list", "--instance", "--json").ok(t))
	assert.Len(t, decode[[]jsonSecret](t, r.stdout), 2)

	// Variables are readable.
	run("hello world\n", "variable", "set", "greeting", "--repo", "proj").ok(t)
	run("", "variable", "set", "GREETING", "--instance", "--body", "hi").ok(t)
	run("", "variable", "set", "ENV_VAR", "--repo", "proj", "--env", "Production", "--body", "e").ok(t)
	run("", "variable", "set", "EMPTY", "--repo", "proj", "--body", "").ok(t)
	assert.Equal(t, "hello world\n", run("", "variable", "get", "GREETING", "--repo", "proj").ok(t).stdout)
	assert.Equal(t, "hi\n", run("", "variable", "get", "greeting", "--instance").ok(t).stdout)
	r = run("", "variable", "get", "GREETING", "--repo", "proj", "--json").ok(t)
	v := decode[jsonVariable](t, r.stdout)
	assert.Equal(t, jsonVariable{Name: "GREETING", Value: "hello world", UpdatedAt: v.UpdatedAt}, v)
	r = run("", "variable", "list", "--repo", "proj").ok(t)
	assert.Regexp(t, `(?s)EMPTY.*GREETING\s+hello world`, r.stdout)
	assert.Equal(t, "e\n", run("", "variable", "get", "ENV_VAR", "-R", "proj", "-e", "production").ok(t).stdout)
	r = run("", "variable", "delete", "empty", "--repo", "proj").ok(t)
	assert.Equal(t, "deleted variable EMPTY from proj\n", r.stdout)
	require.Error(t, run("", "variable", "get", "EMPTY", "--repo", "proj").err)

	// environments: list, delete (with their content).
	r = run("", "environment", "list", "--repo", "proj", "--json").ok(t)
	envs := decode[[]jsonEnvironment](t, r.stdout)
	require.Len(t, envs, 1)
	assert.Equal(t, "Production", envs[0].Name)
	assert.Equal(t, "proj", envs[0].Repo)
	assert.Contains(t, run("", "environment", "list", "--repo", "proj").ok(t).stdout, "Production")
	r = run("", "environment", "delete", "production", "--repo", "proj").ok(t)
	assert.Equal(t, "deleted environment production of proj\n", r.stdout)
	assert.Contains(t, run("", "environment", "list", "--repo", "proj").ok(t).stderr, "no environments")
	require.Error(t, run("", "secret", "list", "--repo", "proj", "--env", "production").err, "its secrets went with it")

	// No command printed a value, and none of them was logged by the server.
	for _, secret := range []string{value, "line one", "line two"} {
		assert.NotContains(t, outputs.String(), secret)
		assert.NotContains(t, h.logs.String(), secret)
	}
}

func TestCLISecretsArgumentsAndErrors(t *testing.T) {
	h := newSecretsCLI(t)
	run := func(stdin string, args ...string) result {
		return runCLI(t, h.vars(adminToken), stdin, args...)
	}
	for name, args := range map[string][]string{
		"no scope":                 {"secret", "set", "A", "--body", "v"},
		"env without repo":         {"secret", "set", "A", "--body", "v", "--env", "prod"},
		"instance with repo":       {"secret", "set", "A", "--body", "v", "--instance", "--repo", "proj"},
		"instance with env":        {"secret", "set", "A", "--body", "v", "--instance", "--env", "prod"},
		"no name":                  {"secret", "set", "--repo", "proj", "--body", "v"},
		"name and env-file":        {"secret", "set", "A", "--env-file", "x", "--repo", "proj"},
		"body and env-file":        {"secret", "set", "--body", "v", "--env-file", "x", "--repo", "proj"},
		"missing env-file":         {"secret", "set", "--env-file", "/nonexistent/.env", "--repo", "proj"},
		"environment without repo": {"environment", "list"},
	} {
		require.Error(t, run("", args...).err, name)
	}

	// A bad name or value is the server's refusal, naming the field, not the value.
	r := run("", "secret", "set", "GITHUB_TOKEN", "--repo", "proj", "--body", "leak-me")
	require.Error(t, r.err)
	assert.Contains(t, r.err.Error(), "GITHUB_")
	assert.NotContains(t, r.err.Error()+r.stderr+r.stdout, "leak-me")
	r = run("", "secret", "set", "BIG", "--repo", "proj", "--body", strings.Repeat("x", domain.MaxSettingValueBytes+1))
	require.Error(t, r.err)
	assert.Contains(t, r.err.Error(), "49,152")
	// A file with one bad name sets the others and reports the bad one.
	f := filepath.Join(t.TempDir(), "e")
	require.NoError(t, os.WriteFile(f, []byte("GOOD=1\n9BAD=2\nALSO_GOOD=3\n"), 0o600))
	r = run("", "secret", "set", "-f", f, "--repo", "proj")
	require.Error(t, r.err)
	assert.Contains(t, r.err.Error(), "9BAD")
	assert.Equal(t, "set secret GOOD for proj\nset secret ALSO_GOOD for proj\n", r.stdout)
	r = run("", "secret", "set", "X", "--repo", "ghost", "--body", "v")
	require.Error(t, r.err)

	// Not authorised: a token with the scope on another repository only.
	reader := h.token([]domain.Scope{domain.ScopeRepoRead}, "proj")
	r = runCLI(t, h.vars(reader), "", "secret", "list", "--repo", "proj")
	require.Error(t, r.err)
	assert.Contains(t, r.err.Error(), "secrets:write")
}

// Standard input larger than the cap is refused with a size error, also when
// the excess is only line breaks: it is never silently cut. The exact maximum
// (and its line break) still goes through to the server.
func TestCLISecretSetStdinSize(t *testing.T) {
	h := newSecretsCLI(t)
	run := func(stdin string, args ...string) result {
		return runCLI(t, h.vars(adminToken), stdin, args...)
	}
	max := domain.MaxSettingValueBytes
	for name, in := range map[string]string{
		"value over the cap":         strings.Repeat("x", max+2000),
		"newline-padded over cap":    strings.Repeat("x", max) + strings.Repeat("\n", 5000),
		"newlines only, over cap":    strings.Repeat("\n", max+2000),
		"cap plus slack plus a byte": strings.Repeat("x", max+1025),
	} {
		r := run(in, "secret", "set", "BIG", "--repo", "proj")
		require.Error(t, r.err, name)
		assert.Contains(t, r.err.Error(), "larger than", name)
		assert.Empty(t, r.stdout, name)
	}
	r := run(strings.Repeat("x", max)+"\n", "secret", "set", "EXACT", "--repo", "proj").ok(t)
	assert.Equal(t, "set secret EXACT for proj\n", r.stdout)
}

// Secret and variable `set` share the reading of their pairs.
func TestCLISetPairModes(t *testing.T) {
	h := newSecretsCLI(t)
	run := func(stdin string, args ...string) result {
		return runCLI(t, h.vars(adminToken), stdin, args...)
	}
	for _, noun := range []string{"secret", "variable"} {
		r := run("", noun, "set", "--repo", "proj")
		require.Error(t, r.err, noun)
		assert.Contains(t, r.err.Error(), "give the NAME of the "+noun)
		r = run("", noun, "set", "A", "--env-file", "x", "--repo", "proj")
		require.Error(t, r.err, noun)
		assert.Contains(t, r.err.Error(), "not both")
		empty := filepath.Join(t.TempDir(), "empty")
		require.NoError(t, os.WriteFile(empty, []byte("# nothing\n"), 0o600))
		r = run("", noun, "set", "-f", empty, "--repo", "proj")
		require.Error(t, r.err, noun)
		assert.Contains(t, r.err.Error(), "holds no NAME=value")
		run("viaStdin\n", noun, "set", "S"+strings.ToUpper(noun[:1]), "--repo", "proj").ok(t)
	}
}
