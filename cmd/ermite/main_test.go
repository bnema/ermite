// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/gitcli"
)

const adminToken = "0123456789abcdef0123456789abcdef-secret"

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// binary builds the ermite binary once per test run.
func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ermite-bin-")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "ermite")
		out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput()
		if err != nil {
			buildErr = errors.New("go build: " + err.Error() + "\n" + string(out))
		}
	})
	require.NoError(t, buildErr)
	return binPath
}

func TestMain(m *testing.M) {
	code := m.Run()
	if binPath != "" {
		_ = os.RemoveAll(filepath.Dir(binPath))
	}
	os.Exit(code)
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func baseEnv() []string {
	return []string{"PATH=" + os.Getenv("PATH")}
}

func TestVersion(t *testing.T) {
	out, err := exec.Command(binary(t), "version").Output()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(out), "ermite "), string(out))
	assert.Equal(t, 1, strings.Count(string(out), "\n"))
}

func TestUnknownCommandExitsNonZero(t *testing.T) {
	err := exec.Command(binary(t), "nope").Run()
	var exit *exec.ExitError
	require.True(t, errors.As(err, &exit))
	assert.Equal(t, 1, exit.ExitCode())
}

func TestServeMissingEnvListsEveryVariable(t *testing.T) {
	cmd := exec.Command(binary(t), "serve")
	cmd.Env = baseEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()

	var exit *exec.ExitError
	require.True(t, errors.As(err, &exit), "expected a failure, got %v", err)
	assert.Equal(t, 1, exit.ExitCode())
	for _, name := range []string{"ERMITE_DATA_DIR", "ERMITE_PUBLIC_URL", "ERMITE_ADMIN_TOKEN"} {
		assert.Contains(t, stderr.String(), name)
	}
}

func TestServeInvalidEnvNeverPrintsToken(t *testing.T) {
	cmd := exec.Command(binary(t), "serve")
	cmd.Env = append(baseEnv(),
		"ERMITE_DATA_DIR=relative",
		"ERMITE_PUBLIC_URL=ftp://x",
		"ERMITE_ADMIN_TOKEN=short-secret-value",
		"ERMITE_ADDR=bad",
	)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var exit *exec.ExitError
	require.True(t, errors.As(err, &exit))
	assert.Equal(t, 1, exit.ExitCode())
	for _, name := range []string{"ERMITE_DATA_DIR", "ERMITE_PUBLIC_URL", "ERMITE_ADMIN_TOKEN", "ERMITE_ADDR"} {
		assert.Contains(t, out.String(), name)
	}
	assert.NotContains(t, out.String(), "short-secret-value")
}

func TestServeGitTooOldNamesVariable(t *testing.T) {
	script := filepath.Join(t.TempDir(), "git")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\necho 'git version 2.44.0'\n"), 0o700))
	data := t.TempDir()

	cmd := exec.Command(binary(t), "serve")
	cmd.Env = append(baseEnv(),
		"ERMITE_DATA_DIR="+data,
		"ERMITE_PUBLIC_URL=http://localhost",
		"ERMITE_ADMIN_TOKEN="+adminToken,
		"ERMITE_ADDR="+freeAddr(t),
		"ERMITE_GIT="+script,
	)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var exit *exec.ExitError
	require.True(t, errors.As(err, &exit))
	assert.Equal(t, 1, exit.ExitCode())
	assert.Contains(t, out.String(), "ERMITE_GIT")
	assert.NotContains(t, out.String(), adminToken)
}

func TestServeMissingGitNamesVariable(t *testing.T) {
	cmd := exec.Command(binary(t), "serve")
	cmd.Env = append(baseEnv(),
		"ERMITE_DATA_DIR="+t.TempDir(),
		"ERMITE_PUBLIC_URL=http://localhost",
		"ERMITE_ADMIN_TOKEN="+adminToken,
		"ERMITE_ADDR="+freeAddr(t),
		"ERMITE_GIT=/nonexistent/git",
	)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var exit *exec.ExitError
	require.True(t, errors.As(err, &exit))
	assert.Equal(t, 1, exit.ExitCode())
	assert.Contains(t, out.String(), "ERMITE_GIT")
}

func TestServeHealthAndSigterm(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}
	probe := gitcli.New(gitcli.Config{Bin: gitBin, DataDir: t.TempDir(), Path: os.Getenv("PATH")})
	if _, err := probe.CheckVersion(context.Background()); err != nil {
		t.Skipf("git is not usable by ermite: %v", err)
	}
	data := t.TempDir()
	// State from a previous run: run/ is recreated, an orphan repository is
	// registered, a staging leftover is removed.
	require.NoError(t, os.MkdirAll(filepath.Join(data, "run", "stale"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(data, "repos", ".staging", "leftover"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(data, "repos", "orphan.git"), 0o700))
	addr := freeAddr(t)

	cmd := exec.Command(binary(t), "serve")
	cmd.Env = append(baseEnv(),
		"ERMITE_ADDR="+addr,
		"ERMITE_DATA_DIR="+data,
		"ERMITE_PUBLIC_URL=http://"+addr,
		"ERMITE_ADMIN_TOKEN="+adminToken,
		"ERMITE_LOG_LEVEL=debug",
		"ERMITE_LOG_FORMAT=json",
		"GIT_DIR=/nonexistent", // must not leak into git
	)
	var logs syncBuffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	require.NoError(t, cmd.Start())
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
	})

	base := "http://" + addr
	var resp *http.Response
	deadline := time.Now().Add(15 * time.Second)
	for {
		var err error
		resp, err = http.Get(base + "/-/health")
		if err == nil {
			break
		}
		select {
		case werr := <-exited:
			t.Fatalf("server exited before answering: %v\n%s", werr, logs.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("health never answered: %v\n%s", err, logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "ok\n", string(body))
	assert.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
	assert.Contains(t, resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'")

	resp404, err := http.Get(base + "/anything")
	require.NoError(t, err)
	_ = resp404.Body.Close()
	assert.Equal(t, 404, resp404.StatusCode)

	// Data directory layout and modes.
	for _, d := range []string{"repos", "repos/.staging", "trash", "kv", "run", "run/git-home"} {
		fi, err := os.Stat(filepath.Join(data, d))
		require.NoError(t, err, d)
		assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm(), d)
	}
	assert.NoDirExists(t, filepath.Join(data, "run", "stale"))
	assert.NoDirExists(t, filepath.Join(data, "repos", ".staging", "leftover"))
	assert.Contains(t, logs.String(), `"registered":1`)

	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-exited:
		require.NoError(t, err, "exit status 0 expected\n%s", logs.String())
	case <-time.After(15 * time.Second):
		t.Fatalf("server did not stop after SIGTERM\n%s", logs.String())
	}
	out := logs.String()
	assert.Contains(t, out, "shutting down")
	assert.NotContains(t, out, adminToken)
	assert.NotContains(t, out, strconv.Quote(adminToken))
	assert.False(t, strings.Contains(out, `"level":"error"`), "no error logged:\n%s", out)

	_, err = http.Get(base + "/-/health")
	assert.Error(t, err, "the port is closed")
}
