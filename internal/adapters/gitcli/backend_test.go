// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/gitcli"
	"bnema.dev/ermite/internal/domain"
)

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		m[k] = v
	}
	return m
}

// backendConf returns the GIT_CONFIG_KEY/VALUE pairs of a backend environment
// and checks that GIT_CONFIG_COUNT matches them.
func backendConf(e map[string]string) map[string]string {
	conf := map[string]string{}
	n := 0
	for {
		k, ok := e["GIT_CONFIG_KEY_"+strconv.Itoa(n)]
		if !ok {
			break
		}
		conf[k] = e["GIT_CONFIG_VALUE_"+strconv.Itoa(n)]
		n++
	}
	if e["GIT_CONFIG_COUNT"] != strconv.Itoa(n) {
		panic("GIT_CONFIG_COUNT does not match the keys")
	}
	return conf
}

func TestHTTPBackendReceiveMaxInputSize(t *testing.T) {
	ctx := context.Background()
	_, data := newGit(t)
	bin, err := exec.LookPath("git")
	require.NoError(t, err)
	g := gitcli.New(gitcli.Config{Bin: bin, DataDir: data, Path: os.Getenv("PATH"), MaxReceiveBytes: 4 << 30})

	push, err := g.HTTPBackend(ctx, true)
	require.NoError(t, err)
	assert.Equal(t, "4294967296", backendConf(envMap(push.Env))["receive.maxInputSize"])

	read, err := g.HTTPBackend(ctx, false)
	require.NoError(t, err)
	assert.NotContains(t, backendConf(envMap(read.Env)), "receive.maxInputSize", "a fetch does not receive a pack")
}

func TestHTTPBackendEnvironment(t *testing.T) {
	ctx := context.Background()
	g, data := newGit(t)

	read, err := g.HTTPBackend(ctx, false)
	require.NoError(t, err)
	execPath, err := exec.Command("git", "--exec-path").Output()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend"), read.Path)
	assert.Equal(t, filepath.Join(data, "repos"), read.Dir)

	e := envMap(read.Env)
	assert.Equal(t, "1", e["GIT_CONFIG_NOSYSTEM"])
	assert.Equal(t, filepath.Join(data, "run", "git-home"), e["HOME"])
	assert.Equal(t, "0", e["GIT_TERMINAL_PROMPT"])
	assert.Equal(t, filepath.Join(data, "repos"), e["GIT_PROJECT_ROOT"])
	assert.Equal(t, "1", e["GIT_HTTP_EXPORT_ALL"])
	assert.NotContains(t, e, "REMOTE_USER", "a read is not a push")

	assert.Equal(t, map[string]string{
		"core.hooksPath":         filepath.Join(data, "run", "hooks"),
		"receive.fsckObjects":    "true",
		"uploadpack.allowFilter": "true",
	}, backendConf(e))

	push, err := g.HTTPBackend(ctx, true)
	require.NoError(t, err)
	pe := envMap(push.Env)
	assert.Equal(t, "owner", pe["REMOTE_USER"])
	assert.Equal(t, map[string]string{
		"core.hooksPath":         filepath.Join(data, "run", "hooks"),
		"receive.fsckObjects":    "true",
		"uploadpack.allowFilter": "true",
		"http.receivepack":       "true",
	}, backendConf(pe), "http.receivepack only for a push; no size limit unless configured")

	// Nothing from the caller's environment leaks.
	t.Setenv("GIT_DIR", "/leak")
	again, err := g.HTTPBackend(ctx, true)
	require.NoError(t, err)
	assert.NotContains(t, envMap(again.Env), "GIT_DIR")

	// The slice is the caller's: appending does not corrupt the next call.
	_ = append(read.Env, "X=1")
	third, err := g.HTTPBackend(ctx, false)
	require.NoError(t, err)
	assert.NotContains(t, envMap(third.Env), "X")
}

func TestHTTPBackendMissing(t *testing.T) {
	data := t.TempDir()
	script := writeScript(t, `case "$1" in --exec-path) echo "`+data+`/nowhere";; *) echo "git version 2.50.0";; esac`)
	g := gitcli.New(gitcli.Config{Bin: script, DataDir: data, Path: "/usr/bin:/bin"})
	_, err := g.HTTPBackend(context.Background(), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "git-http-backend")

	// A non-executable file is refused too.
	require.NoError(t, os.MkdirAll(filepath.Join(data, "exec"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(data, "exec", "git-http-backend"), []byte("x"), 0o644))
	script = writeScript(t, `echo "`+data+`/exec"`)
	g = gitcli.New(gitcli.Config{Bin: script, DataDir: data, Path: "/usr/bin:/bin"})
	_, err = g.HTTPBackend(context.Background(), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not an executable")
}

func TestDefaultBranchAndMaintain(t *testing.T) {
	ctx := context.Background()
	g, data := newGit(t)
	n := name(t, "tools")
	id, err := g.Stage(ctx)
	require.NoError(t, err)
	require.NoError(t, g.Publish(ctx, id, n))
	dir := filepath.Join(data, "repos", "tools.git")

	branch, exists, err := g.DefaultBranch(ctx, n)
	require.NoError(t, err)
	assert.Equal(t, "main", branch)
	assert.False(t, exists, "no commit yet")

	_, _, err = g.DefaultBranch(ctx, name(t, "ghost"))
	assert.ErrorIs(t, err, domain.ErrNotFound)

	// Put a commit on "dev" through a work tree.
	wt := t.TempDir()
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@x"}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = wt, env
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	run("init", "-q", "--initial-branch=dev")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "f"), []byte("x"), 0o644))
	run("add", "f")
	run("commit", "-q", "-m", "one")
	run("push", "-q", dir, "dev")

	_, exists, err = g.DefaultBranch(ctx, n)
	require.NoError(t, err)
	assert.False(t, exists, "HEAD still points to main")

	require.NoError(t, g.SetDefaultBranch(ctx, n, "dev"))
	branch, exists, err = g.DefaultBranch(ctx, n)
	require.NoError(t, err)
	assert.Equal(t, "dev", branch)
	assert.True(t, exists)
	assert.Equal(t, "refs/heads/dev", gitOut(t, dir, "symbolic-ref", "HEAD"))

	for _, bad := range []string{"", "-x", "a b", "a..b"} {
		assert.Error(t, g.SetDefaultBranch(ctx, n, bad), bad)
	}
	assert.ErrorIs(t, g.SetDefaultBranch(ctx, name(t, "ghost"), "dev"), domain.ErrNotFound)

	require.NoError(t, g.Maintain(ctx, n))
	assert.FileExists(t, filepath.Join(dir, "objects", "info", "commit-graphs", "commit-graph-chain"))
	assert.ErrorIs(t, g.Maintain(ctx, name(t, "ghost")), domain.ErrNotFound)
}
