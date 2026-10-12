// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigShowWithoutFile(t *testing.T) {
	env := testEnv(t, nil)
	path, _ := env.configPath()
	res := runCLIWith(t, env, "", "config", "show").ok(t)
	assert.Equal(t, "file       "+path+"\nurl        (not set)\ntoken.cmd  (not set)\n", res.stdout)
	_, err := os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist, "show does not create the file")
}

func TestConfigSetUnsetShowRoundTrip(t *testing.T) {
	env := testEnv(t, nil)
	path, _ := env.configPath()
	writeConfigFile(t, env, "# my forge\nurl = https://old.example.com\n\n# secrets stay in pass\n", 0o600)

	runCLIWith(t, env, "", "config", "set", "url", "https://git.example.com/").ok(t)
	runCLIWith(t, env, "", "config", "set", "token.cmd", "pass show ermite/token").ok(t)
	// a fresh process sees the new content
	res := runCLIWith(t, testEnvAt(t, path), "", "config", "show").ok(t)
	assert.Contains(t, res.stdout, "url        https://git.example.com\n")
	assert.Contains(t, res.stdout, "token.cmd  pass show ermite/token\n")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "# my forge\n")
	assert.Contains(t, string(data), "# secrets stay in pass\n")

	runCLIWith(t, testEnvAt(t, path), "", "config", "unset", "token.cmd").ok(t)
	runCLIWith(t, testEnvAt(t, path), "", "config", "unset", "url").ok(t)
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "token.cmd")
	assert.NotContains(t, string(data), "url")
	assert.Contains(t, string(data), "# my forge\n", "comments survive")

	res = runCLIWith(t, testEnvAt(t, path), "", "config", "show").ok(t)
	assert.Contains(t, res.stdout, "url        (not set)\n")
	assert.Contains(t, res.stdout, "token.cmd  (not set)\n")
}

// testEnvAt is testEnv with the config file at path: a new process.
func testEnvAt(t *testing.T, path string) *cliEnv {
	t.Helper()
	env := testEnv(t, nil)
	env.configPath = func() (string, error) { return path, nil }
	return env
}

func TestConfigSetCreatesPrivateFile(t *testing.T) {
	env := testEnv(t, nil)
	path, _ := env.configPath()
	res := runCLIWith(t, env, "", "config", "set", "token.cmd", "gopass show -o ermite").ok(t)
	assert.Contains(t, res.stderr, path)
	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	assert.Equal(t, "token.cmd = gopass show -o ermite\n", string(data))
}

func TestConfigSetRefusesBadInput(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"unknown key", []string{"set", "token", "x"}, `unknown key "token": the keys are url and token.cmd`},
		{"unknown key unset", []string{"unset", "colour"}, `unknown key "colour": the keys are url and token.cmd`},
		{"no scheme", []string{"set", "url", "git.example.com"}, "invalid url"},
		{"credentials", []string{"set", "url", "https://u:s3cret@git.example.com"}, "must not contain credentials"},
		{"ftp", []string{"set", "url", "ftp://git.example.com"}, "invalid url"},
		{"empty token.cmd", []string{"set", "token.cmd", "  "}, "token.cmd is empty"},
		{"multiline token.cmd", []string{"set", "token.cmd", "a\nb"}, "single line"},
		{"missing value", []string{"set", "url"}, "accepts 2 arg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := testEnv(t, nil)
			path, _ := env.configPath()
			res := runCLIWith(t, env, "", append([]string{"config"}, tt.args...)...)
			require.Error(t, res.err)
			assert.Contains(t, res.err.Error(), tt.want)
			assert.NotContains(t, res.err.Error(), "s3cret")
			_, err := os.Stat(path)
			assert.ErrorIs(t, err, os.ErrNotExist, "nothing is written")
		})
	}
}

func TestConfigUnsetWithoutFile(t *testing.T) {
	env := testEnv(t, nil)
	path, _ := env.configPath()
	res := runCLIWith(t, env, "", "config", "unset", "url").ok(t)
	assert.Contains(t, res.stderr, "does not exist")
	_, err := os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestConfigShowNeverRunsTokenCmdAndNotesPrecedence(t *testing.T) {
	env := testEnv(t, map[string]string{"ERMITE_URL": "https://env.example.com", "ERMITE_TOKEN": "ermite_x"})
	writeConfigFile(t, env, "url = https://file.example.com\ntoken.cmd = get-token\n", 0o666)
	res := runCLIWith(t, env, "", "config", "show").ok(t) // testEnv fails the test if token.cmd runs
	assert.Contains(t, res.stdout, "url        https://file.example.com\n", "show prints what is stored")
	assert.Contains(t, res.stderr, "ERMITE_URL is set and takes precedence")
	assert.Contains(t, res.stderr, "ERMITE_TOKEN is set: token.cmd is not used")
	assert.False(t, strings.Contains(res.stdout+res.stderr, "ermite_x"), "the token is never printed")
}

func TestHelpDocumentsTheConfigFileContract(t *testing.T) {
	root := runCLI(t, nil, "", "--help").ok(t).stdout
	cfg := runCLI(t, nil, "", "config", "--help").ok(t).stdout
	for name, help := range map[string]string{"root": root, "config": cfg} {
		assert.Contains(t, help, "ermite/config", "%s help names the file", name)
		assert.Contains(t, help, "ERMITE_CONFIG", "%s help names the override", name)
		assert.Contains(t, help, "ERMITE_URL", "%s help states the precedence", name)
		assert.Contains(t, help, "ERMITE_TOKEN", "%s help states the precedence", name)
	}
	assert.Contains(t, cfg, "token.cmd = ", "the config help has an example")
	assert.Contains(t, cfg, "Windows", "the config help states the Windows limit")
}

func TestConfigShowNamesTheFileItCannotRead(t *testing.T) {
	env := testEnv(t, nil)
	path := writeConfigFile(t, env, "url = https://git.example.com\n", 0o600)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700)) // a directory is not a config file
	res := runCLIWith(t, env, "", "config", "show")
	require.Error(t, res.err)
	assert.Contains(t, res.stdout, path)
	assert.Contains(t, res.err.Error(), "config:")
}

func TestConfigUnsetOfAnAbsentKey(t *testing.T) {
	env := testEnv(t, nil)
	path := writeConfigFile(t, env, "# only a comment\nurl = https://git.example.com\n", 0o600)
	res := runCLIWith(t, env, "", "config", "unset", "token.cmd").ok(t)
	assert.Equal(t, "ermite: token.cmd is not set\n", res.stderr)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "# only a comment\nurl = https://git.example.com\n", string(data), "the file is untouched")
}

func TestConfigShowJSON(t *testing.T) {
	env := testEnv(t, nil)
	path, _ := env.configPath()
	res := runCLIWith(t, env, "", "config", "show", "--json").ok(t)
	assert.JSONEq(t, `{"file":`+strconv.Quote(path)+`,"url":null,"token_cmd":null}`, res.stdout)

	env = testEnv(t, nil)
	path = writeConfigFile(t, env, "url = https://git.example.com\ntoken.cmd = pass show ermite/token\n", 0o600)
	res = runCLIWith(t, env, "", "config", "show", "--json").ok(t) // testEnv fails the test if token.cmd runs
	assert.JSONEq(t, `{"file":`+strconv.Quote(path)+`,"url":"https://git.example.com","token_cmd":"pass show ermite/token"}`, res.stdout)
}

func TestConfigShowAndSetWarnWhenTokenCmdWouldBeRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix permission bits")
	}
	env := testEnv(t, nil)
	path := writeConfigFile(t, env, "url = https://git.example.com\ntoken.cmd = get-token\n", 0o666)

	res := runCLIWith(t, env, "", "config", "show").ok(t)
	assert.Contains(t, res.stderr, "ermite: warning: config file "+path+" is writable by others: token.cmd not run")

	// a fresh process; set keeps the mode of an existing file
	res = runCLIWith(t, testEnvAt(t, path), "", "config", "set", "url", "https://git2.example.com").ok(t)
	assert.Contains(t, res.stderr, "writable by others")

	// no warning without a token.cmd, or with a private file
	env = testEnv(t, nil)
	writeConfigFile(t, env, "url = https://git.example.com\n", 0o666)
	assert.NotContains(t, runCLIWith(t, env, "", "config", "show").ok(t).stderr, "warning")
	res = runCLIWith(t, testEnv(t, nil), "", "config", "set", "token.cmd", "get-token").ok(t)
	assert.NotContains(t, res.stderr, "warning")
}

func TestDefaultConfigPathUsesXDGConfigHome(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("ERMITE_CONFIG", "")
	f := listFake()

	env := defaultCLIEnv() // the real path resolution, the real environment
	res := runCLIWith(t, env, "", "config", "set", "url", f.serve(t)).ok(t)
	want := filepath.Join(xdg, "ermite", "config")
	assert.Contains(t, res.stderr, want)
	info, err := os.Stat(want)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	env = defaultCLIEnv()
	env.runTokenCmd = func(context.Context, string) (string, error) { return "", errors.New("must not run") }
	t.Setenv("ERMITE_URL", "")
	t.Setenv("ERMITE_TOKEN", "")
	runCLIWith(t, env, "", "repo", "list").ok(t)
	assert.Equal(t, 1, callCount(f, "ListRepos"))

	// ERMITE_CONFIG wins over the default
	other := filepath.Join(t.TempDir(), "other")
	t.Setenv("ERMITE_CONFIG", other)
	res = runCLIWith(t, defaultCLIEnv(), "", "config", "show").ok(t)
	assert.Contains(t, res.stdout, other)
}

func callCount(f *fakeAPI, name string) int {
	calls, _ := f.snapshot()
	n := 0
	for _, c := range calls {
		if c == name {
			n++
		}
	}
	return n
}
