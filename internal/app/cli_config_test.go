// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"os"
	"runtime"
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

func TestConfigHelpDocumentsTheFile(t *testing.T) {
	res := runCLI(t, nil, "", "--help").ok(t)
	for _, s := range []string{"ERMITE_URL", "ERMITE_TOKEN", "token.cmd", "ERMITE_CONFIG", "ermite config"} {
		assert.Contains(t, res.stdout, s)
	}
	res = runCLI(t, nil, "", "config", "--help").ok(t)
	assert.Contains(t, res.stdout, "pass show ermite/token")
	assert.Contains(t, res.stdout, "https://git.example.com")
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
