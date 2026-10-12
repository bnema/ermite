// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// processGone reports whether pid is gone (a zombie that nobody reaped counts).
func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	_, after, _ := strings.Cut(string(stat), ") ")
	return strings.HasPrefix(after, "Z")
}

// readPid waits for the pid file written by a grandchild.
func readPid(t *testing.T, path string) int {
	t.Helper()
	var pid int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil && pid > 0
	}, 5*time.Second, 10*time.Millisecond)
	return pid
}

func TestRunShellKillsTheWholeProcessTreeOnTimeoutAndCancel(t *testing.T) {
	requireNoTerminal(t)
	for name, tc := range map[string]struct {
		ctx     func(t *testing.T) context.Context
		timeout time.Duration
		want    string
	}{
		"timeout": {func(t *testing.T) context.Context { return t.Context() }, 700 * time.Millisecond, "timed out after 700ms"},
		"cancel": {func(t *testing.T) context.Context {
			ctx, cancel := context.WithCancel(t.Context())
			time.AfterFunc(700*time.Millisecond, cancel)
			return ctx
		}, time.Minute, "interrupted"},
	} {
		t.Run(name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			// sh starts a grandchild, records its pid and waits for it
			line := "sleep 30 & echo $! > '" + pidFile + "'; wait"
			_, err := runShell(tc.ctx(t), line, tc.timeout)
			require.Error(t, err)
			assert.Equal(t, tc.want, err.Error())

			pid := readPid(t, pidFile)
			require.Eventually(t, func() bool { return processGone(pid) }, 5*time.Second, 20*time.Millisecond,
				"the grandchild %d must not outlive the command", pid)
		})
	}
}

func TestRunShellStopsOnSIGTERM(t *testing.T) {
	requireNoTerminal(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	line := "sleep 30 & echo $! > '" + pidFile + "'; wait"
	errc := make(chan error, 1)
	go func() {
		_, err := runShell(t.Context(), line, time.Minute)
		errc <- err
	}()
	pid := readPid(t, pidFile)
	// the signal is delivered to the test process itself: runShell catches it
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))
	select {
	case err := <-errc:
		require.Error(t, err)
		assert.Equal(t, "interrupted", err.Error())
	case <-time.After(10 * time.Second):
		t.Fatal("runShell did not stop on SIGTERM")
	}
	require.Eventually(t, func() bool { return processGone(pid) }, 5*time.Second, 20*time.Millisecond)
}

// fakeInfo is a file whose owner and mode are chosen by the test.
type fakeInfo struct {
	fs.FileInfo
	mode fs.FileMode
	uid  uint32
}

func (f fakeInfo) Mode() fs.FileMode { return f.mode }
func (f fakeInfo) Sys() any          { return &syscall.Stat_t{Uid: f.uid} }

func TestCheckPrivate(t *testing.T) {
	dir := t.TempDir() // owned by the test user, mode 0700
	path := filepath.Join(dir, "config")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	real, err := os.Stat(path)
	require.NoError(t, err)
	me := os.Geteuid()

	tests := []struct {
		name string
		mode fs.FileMode
		uid  int // owner of the file
		euid int
		want string // error text, "" for accepted
	}{
		{"own private file", 0o600, me, me, ""},
		{"file of another user", 0o600, me + 1, me, "is owned by another user"},
		{"file owned by root, not me", 0o600, 0, me + 1, "is owned by another user"},
		{"group writable", 0o620, me, me, "is writable by others"},
		{"world writable", 0o602, me, me, "is writable by others"},
		{"group readable", 0o640, me, me, ""},
		// the directory is the real one: owned by me
		{"directory of another user", 0o600, me + 1, me + 1, "config directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.want == "config directory" && me == 0 {
				t.Skip("root owns every directory it creates")
			}
			err := checkPrivate(path, fakeInfo{FileInfo: real, mode: tt.mode, uid: uint32(tt.uid)}, tt.euid)
			if tt.want == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}

	t.Run("directory owned by root is fine", func(t *testing.T) {
		// /tmp and friends are root's; use a root-owned directory when there is one
		fi, err := os.Stat("/")
		require.NoError(t, err)
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Uid != 0 || fi.Mode().Perm()&0o022 != 0 {
			t.Skip("/ is not a private root directory here")
		}
		assert.NoError(t, checkPrivateDir("/", me+1))
	})
}
