// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/term"

	"bnema.dev/ermite-api/gen/ermite"
)

// writeConfigFile writes the config file of env with the given mode.
func writeConfigFile(t *testing.T, env *cliEnv, content string, mode os.FileMode) string {
	t.Helper()
	path, err := env.configPath()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
	require.NoError(t, os.Chmod(path, mode)) // umask
	return path
}

// tokenCmdStub makes runTokenCmd return out and err, and counts the runs.
func tokenCmdStub(env *cliEnv, out string, err error) *[]string {
	var ran []string
	env.runTokenCmd = func(_ context.Context, line string) (string, error) {
		ran = append(ran, line)
		return out, err
	}
	return &ran
}

func listFake() *fakeAPI {
	return &fakeAPI{listRepos: func(*ermite.ListReposRequest) (*ermite.ListReposResponse, error) {
		return &ermite.ListReposResponse{}, nil
	}}
}

func lastAuth(f *fakeAPI) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.header) == 0 {
		return "<no call>"
	}
	return f.header[len(f.header)-1].Get("Authorization")
}

func TestConfigFileSuppliesURLAndToken(t *testing.T) {
	f := listFake()
	url := f.serve(t)
	env := testEnv(t, nil)
	writeConfigFile(t, env, "# my server\nurl = "+url+"\ntoken.cmd = pass show ermite/token\n", 0o600)
	ran := tokenCmdStub(env, " ermite_fromcmd\n", nil)

	runCLIWith(t, env, "", "repo", "list").ok(t)
	assert.Equal(t, "Bearer ermite_fromcmd", lastAuth(f))
	assert.Equal(t, []string{"pass show ermite/token"}, *ran)
}

func TestConfigFileURLWithoutTokenCmdIsAnonymous(t *testing.T) {
	f := listFake()
	env := testEnv(t, nil)
	writeConfigFile(t, env, "url = "+f.serve(t)+"\n", 0o600)

	runCLIWith(t, env, "", "repo", "list").ok(t)
	assert.Empty(t, lastAuth(f))
}

func TestEnvironmentWinsOverConfigFile(t *testing.T) {
	fileSrv, envSrv := listFake(), listFake()
	env := testEnv(t, map[string]string{"ERMITE_URL": envSrv.serve(t), "ERMITE_TOKEN": "ermite_env"})
	writeConfigFile(t, env, "url = "+fileSrv.serve(t)+"\ntoken.cmd = echo nope\n", 0o600)

	runCLIWith(t, env, "", "repo", "list").ok(t) // the default runTokenCmd of testEnv fails the test if called
	assert.Equal(t, "Bearer ermite_env", lastAuth(envSrv))
	calls, _ := fileSrv.snapshot()
	assert.Empty(t, calls, "the url of the file is not used when ERMITE_URL is set")
}

func TestEnvURLDifferentFromFileKeepsTokenCmdOut(t *testing.T) {
	f := listFake()
	tests := []struct {
		name    string
		envURL  string // "" means the fake server
		file    string // the content of the config file
		wantErr bool
	}{
		{name: "other url in the file", file: "url = http://file.invalid\ntoken.cmd = get-token\n"},
		{name: "file without url", file: "token.cmd = get-token\n"},
		{name: "invalid ERMITE_URL", envURL: "nope", file: "url = http://file.invalid\ntoken.cmd = get-token\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			envURL := tt.envURL
			if envURL == "" {
				envURL = f.serve(t)
			}
			env := testEnv(t, map[string]string{"ERMITE_URL": envURL})
			writeConfigFile(t, env, tt.file, 0o600)
			ran := tokenCmdStub(env, "ermite_cmd", nil)

			res := runCLIWith(t, env, "", "repo", "list")
			assert.Empty(t, *ran, "the token of the file's server must not go to another server")
			if tt.wantErr {
				require.Error(t, res.err)
				return
			}
			require.NoError(t, res.err)
			assert.Empty(t, lastAuth(f))
			assert.Equal(t, 1, strings.Count(res.stderr, "token.cmd not run"), res.stderr)
			assert.Contains(t, res.stderr, "ERMITE_TOKEN")
		})
	}
}

func TestEnvURLSameAsFileUsesTokenCmd(t *testing.T) {
	f := listFake()
	url := f.serve(t)
	// the same address, written differently
	env := testEnv(t, map[string]string{"ERMITE_URL": " " + strings.ToUpper(url[:4]) + url[4:] + "/ "})
	writeConfigFile(t, env, "url = "+url+"\ntoken.cmd = get-token\n", 0o600)
	ran := tokenCmdStub(env, "ermite_cmd", nil)

	res := runCLIWith(t, env, "", "repo", "list").ok(t)
	assert.Equal(t, "Bearer ermite_cmd", lastAuth(f))
	assert.Len(t, *ran, 1)
	assert.NotContains(t, res.stderr, "token.cmd")
}

func TestTokenCmdNotRunWhenEnvTokenSet(t *testing.T) {
	f := listFake()
	env := testEnv(t, map[string]string{"ERMITE_TOKEN": "ermite_env"})
	writeConfigFile(t, env, "url = "+f.serve(t)+"\ntoken.cmd = get-token\n", 0o666) // even an unsafe file is not an issue
	ran := tokenCmdStub(env, "other", nil)

	runCLIWith(t, env, "", "repo", "list").ok(t)
	assert.Empty(t, *ran)
	assert.Equal(t, "Bearer ermite_env", lastAuth(f))
}

func TestTokenCmdNotRunForVersionNorCompletion(t *testing.T) {
	f := &fakeAPI{getVersion: func(*ermite.GetVersionRequest) (*ermite.GetVersionResponse, error) {
		return &ermite.GetVersionResponse{Version: "v0.4.0"}, nil
	}}
	url := f.serve(t)

	for name, args := range map[string][]string{
		"version":        {"version"},
		"version --json": {"version", "--json"},
		"completion":     {"completion", "bash"},
	} {
		t.Run(name, func(t *testing.T) {
			env := testEnv(t, nil)
			writeConfigFile(t, env, "url = "+url+"\ntoken.cmd = get-token\n", 0o600)
			ran := tokenCmdStub(env, "", errors.New("must not run"))
			runCLIWith(t, env, "", args...).ok(t)
			assert.Empty(t, *ran)
		})
	}

	t.Run("version without any url", func(t *testing.T) {
		env := testEnv(t, nil)
		writeConfigFile(t, env, "token.cmd = get-token\n", 0o600)
		ran := tokenCmdStub(env, "", errors.New("must not run"))
		res := runCLIWith(t, env, "", "version").ok(t)
		assert.Equal(t, "ermite v0.0.1\n", res.stdout)
		assert.Empty(t, *ran)
	})

	t.Run("version uses the url of the file and sends no token", func(t *testing.T) {
		env := testEnv(t, nil)
		writeConfigFile(t, env, "url = "+url+"\ntoken.cmd = get-token\n", 0o600)
		tokenCmdStub(env, "ermite_cmd", nil)
		res := runCLIWith(t, env, "", "version").ok(t)
		assert.Contains(t, res.stdout, "v0.4.0 at "+url)
		assert.Empty(t, lastAuth(f))
	})
}

func TestTokenCmdRunsOncePerProcess(t *testing.T) {
	f := listFake()
	env := testEnv(t, nil)
	writeConfigFile(t, env, "url = "+f.serve(t)+"\ntoken.cmd = get-token\n", 0o600)
	ran := tokenCmdStub(env, "ermite_cmd", nil)
	root := newRootCmdWith(env)
	for range 2 {
		c, _, err := root.Find([]string{"repo", "list"})
		require.NoError(t, err)
		_, err = env.client(c)
		require.NoError(t, err)
	}
	assert.Len(t, *ran, 1)
}

func TestTokenCmdRefusedWhenConfigFileIsWritableByOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix permission bits")
	}
	for _, mode := range []os.FileMode{0o666, 0o662, 0o620} {
		t.Run(mode.String(), func(t *testing.T) {
			env := testEnv(t, nil)
			path := writeConfigFile(t, env, "url = http://ermite.invalid:1\ntoken.cmd = get-token\n", mode)
			ran := tokenCmdStub(env, "ermite_cmd", nil)
			res := runCLIWith(t, env, "", "repo", "list")
			require.Error(t, res.err)
			assert.Equal(t, "config file "+path+" is writable by others: token.cmd not run", res.err.Error())
			assert.Empty(t, *ran)
		})
	}
	t.Run("group readable is fine", func(t *testing.T) {
		f := listFake()
		env := testEnv(t, nil)
		writeConfigFile(t, env, "url = "+f.serve(t)+"\ntoken.cmd = get-token\n", 0o644)
		tokenCmdStub(env, "ermite_cmd", nil)
		runCLIWith(t, env, "", "repo", "list").ok(t)
	})
}

func TestTokenCmdFailuresNeverShowOutput(t *testing.T) {
	const secret = "s3cret-token-value"
	tests := []struct {
		name string
		out  string
		err  error
		want string
	}{
		{"failure", secret, errors.New("exit status 3"), "token.cmd failed: exit status 3"},
		{"empty", "  \n", nil, "token.cmd printed nothing"},
		{"multiline", secret + "\n" + secret + "\n", nil, "token.cmd printed more than one line"},
		{"crlf multiline", secret + "\r\n" + secret, nil, "token.cmd printed more than one line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := testEnv(t, nil)
			writeConfigFile(t, env, "url = http://ermite.invalid:1\ntoken.cmd = get-token\n", 0o600)
			tokenCmdStub(env, tt.out, tt.err)
			res := runCLIWith(t, env, "", "repo", "list")
			require.Error(t, res.err)
			assert.Equal(t, tt.want, res.err.Error())
			assert.NotContains(t, res.err.Error()+res.stdout+res.stderr, secret)
		})
	}
}

func TestMissingURLIsReportedBeforeTokenCmdRuns(t *testing.T) {
	env := testEnv(t, nil)
	writeConfigFile(t, env, "token.cmd = get-token\n", 0o600)
	ran := tokenCmdStub(env, "ermite_cmd", nil)
	res := runCLIWith(t, env, "", "repo", "list")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "ERMITE_URL is not set")
	assert.Contains(t, res.err.Error(), "ermite config set url")
	assert.Empty(t, *ran)
}

func TestInvalidFileURLIsReported(t *testing.T) {
	env := testEnv(t, nil)
	path := writeConfigFile(t, env, "url = git.example.com\n", 0o600)
	res := runCLIWith(t, env, "", "repo", "list")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "url in "+filepath.Dir(path)) // the file named
	assert.Contains(t, res.err.Error(), "ermite config set url")
	assert.Contains(t, res.err.Error(), "http")
}

func TestConfigFileWarnings(t *testing.T) {
	f := listFake()
	env := testEnv(t, nil)
	writeConfigFile(t, env, "url = "+f.serve(t)+"\ncolour = blue\n", 0o600)
	res := runCLIWith(t, env, "", "repo", "list").ok(t)
	assert.Equal(t, 1, strings.Count(res.stderr, "ermite: warning: config: "), res.stderr)
	assert.Contains(t, res.stderr, "colour")
	assert.NotContains(t, res.stderr, "blue", "values are never echoed")
}

func TestCompletionStaysSilentAboutTheConfigFile(t *testing.T) {
	env := testEnv(t, nil)
	writeConfigFile(t, env, "colour = blue\nurl = nope\n", 0o600)
	res := runCLIWith(t, env, "", "__complete", "repo", "show", "").ok(t)
	assert.NotContains(t, res.stderr, "warning")
}

func TestERMITE_CONFIGSelectsTheFile(t *testing.T) {
	f := listFake()
	path := filepath.Join(t.TempDir(), "other", "conf")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("url = "+f.serve(t)+"\n"), 0o600))

	env := testEnv(t, map[string]string{"ERMITE_CONFIG": path})
	env.configPath = nil // the default resolution, which reads ERMITE_CONFIG
	runCLIWith(t, env, "", "repo", "list").ok(t)
}

func TestTokenCmdRunsThroughTheShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	f := listFake()
	env := testEnv(t, nil)
	writeConfigFile(t, env, "url = "+f.serve(t)+"\ntoken.cmd = printf secret\n", 0o600)
	env.runTokenCmd = runTokenShell // the real thing

	runCLIWith(t, env, "", "repo", "list").ok(t)
	assert.Equal(t, "Bearer secret", lastAuth(f))
}

func TestRunShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	tests := []struct {
		name    string
		line    string
		want    string // the output on success
		errHas  string // the error on failure
		errNot  string // what the error must not show
		wantErr bool
	}{
		{name: "exit status", line: "echo s3cret; exit 3", wantErr: true, errHas: "exit status 3", errNot: "s3cret"},
		{name: "too much output", line: "head -c 5000 /dev/zero | tr '\\0' a", wantErr: true, errHas: "more than 4096 bytes", errNot: "aaa"},
		{name: "output", line: "echo ermite_ok", want: "ermite_ok\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runShell(t.Context(), tt.line, time.Minute)
			if !tt.wantErr {
				require.NoError(t, err)
				assert.Equal(t, tt.want, out)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errHas)
			assert.NotContains(t, err.Error(), tt.errNot)
		})
	}
}

// requireNoTerminal skips a test of the non-terminal path when the tests run
// with a terminal as standard input.
func requireNoTerminal(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("standard input is a terminal")
	}
}

func TestTokenCmdTimesOut(t *testing.T) {
	requireNoTerminal(t)
	f := listFake()
	env := testEnv(t, nil)
	writeConfigFile(t, env, "url = "+f.serve(t)+"\ntoken.cmd = sleep 30; :\n", 0o600)
	env.runTokenCmd = func(ctx context.Context, line string) (string, error) {
		return runShell(ctx, line, 100*time.Millisecond)
	}

	start := time.Now()
	res := runCLIWith(t, env, "", "repo", "list")
	require.Error(t, res.err)
	assert.Equal(t, "token.cmd failed: timed out after 100ms", res.err.Error())
	assert.Less(t, time.Since(start), 10*time.Second)
}

func TestTokenCmdGetsNoInputWithoutATerminal(t *testing.T) {
	requireNoTerminal(t)
	out, err := runShell(t.Context(), "cat; echo done", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, "done\n", out, "the command read nothing")
}

func TestTokenCmdRefusedWhenDirectoryIsWritableByOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix permission bits")
	}
	env := testEnv(t, nil)
	path := writeConfigFile(t, env, "url = http://ermite.invalid:1\ntoken.cmd = get-token\n", 0o600)
	dir := filepath.Dir(path)
	require.NoError(t, os.Chmod(dir, 0o777))
	ran := tokenCmdStub(env, "ermite_cmd", nil)

	res := runCLIWith(t, env, "", "repo", "list")
	require.Error(t, res.err)
	assert.Equal(t, "config directory "+dir+" is writable by others: token.cmd not run", res.err.Error())
	assert.Empty(t, *ran)

	t.Run("sticky directory is fine", func(t *testing.T) {
		require.NoError(t, os.Chmod(dir, 0o777|os.ModeSticky))
		f := listFake()
		env := testEnvAt(t, path)
		require.NoError(t, os.WriteFile(path, []byte("url = "+f.serve(t)+"\ntoken.cmd = get-token\n"), 0o600))
		tokenCmdStub(env, "ermite_cmd", nil)
		runCLIWith(t, env, "", "repo", "list").ok(t)
	})
}

func TestTokenCmdFollowsASymlinkedConfigFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	f := listFake()
	env := testEnv(t, nil)
	real := writeConfigFile(t, env, "url = "+f.serve(t)+"\ntoken.cmd = get-token\n", 0o600)
	link := filepath.Join(t.TempDir(), "dotfile-link")
	require.NoError(t, os.Symlink(real, link))
	tokenCmdStub(env, "ermite_cmd", nil)
	env.configPath = func() (string, error) { return link, nil }
	runCLIWith(t, env, "", "repo", "list").ok(t)
	assert.Equal(t, "Bearer ermite_cmd", lastAuth(f))
}

func TestVersionWarnsAboutAnUnreadableConfigFile(t *testing.T) {
	env := testEnv(t, nil)
	path := writeConfigFile(t, env, "", 0o600)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))
	res := runCLIWith(t, env, "", "version").ok(t)
	assert.Equal(t, "ermite v0.0.1\n", res.stdout)
	assert.Contains(t, res.stderr, "ermite: warning: config:")
}

func TestCompletionNeverRunsTokenCmd(t *testing.T) {
	f := &fakeAPI{listRepos: func(*ermite.ListReposRequest) (*ermite.ListReposResponse, error) {
		return &ermite.ListReposResponse{Repos: []*ermite.Repo{repoAlpha}}, nil
	}}
	env := testEnv(t, nil)
	writeConfigFile(t, env, "url = "+f.serve(t)+"\ntoken.cmd = get-token\n", 0o600)
	ran := tokenCmdStub(env, "ermite_cmd", nil)
	res := runCLIWith(t, env, "", "__complete", "repo", "show", "").ok(t)
	assert.Empty(t, *ran)
	assert.Contains(t, res.stdout, "alpha")
	assert.Empty(t, lastAuth(f))

	// with ERMITE_TOKEN set the completion is authenticated
	env = testEnv(t, map[string]string{"ERMITE_TOKEN": "ermite_env"})
	writeConfigFile(t, env, "url = "+f.serve(t)+"\ntoken.cmd = get-token\n", 0o600)
	runCLIWith(t, env, "", "__complete", "repo", "show", "").ok(t)
	assert.Equal(t, "Bearer ermite_env", lastAuth(f))
}
