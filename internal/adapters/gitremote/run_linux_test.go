// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package gitremote

import (
	"context"
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
	"golang.org/x/sys/unix"
)

// recordKills replaces killGroup with one that records the groups it is asked
// to kill, and does not kill them.
func recordKills(t *testing.T, really bool) func() []int {
	t.Helper()
	var mu sync.Mutex
	var got []int
	old := killGroup
	killGroup = func(pgid int) error {
		mu.Lock()
		got = append(got, pgid)
		mu.Unlock()
		if really {
			return old(pgid)
		}
		return nil
	}
	t.Cleanup(func() { killGroup = old })
	return func() []int {
		mu.Lock()
		defer mu.Unlock()
		return append([]int(nil), got...)
	}
}

func helperScript(t *testing.T) (cmd *exec.Cmd, pidFile string) {
	t.Helper()
	dir := t.TempDir()
	pidFile = filepath.Join(dir, "pid")
	// A leader with a helper in its group, which keeps its stdout open.
	script := "sleep 30 &\necho $! > '" + pidFile + "'\nwait\n"
	return exec.Command("/bin/sh", "-c", script), pidFile
}

func killPidFile(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// When waitid fails nothing proves that the leader is not reaped: the group
// is not signalled, the leader alone is killed, and the error is returned.
func TestRunDoesNotSignalTheGroupWhenWaitidFails(t *testing.T) {
	calls := recordKills(t, true)
	old := waitid
	cmd, pidFile := helperScript(t)
	// Fails once the helper is started, so that the test can tell it lives.
	waitid = func(int, int, *unix.Siginfo, int, *unix.Rusage) error {
		for i := 0; i < 200; i++ {
			if _, err := os.Stat(pidFile); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		return unix.ECHILD
	}
	t.Cleanup(func() { waitid = old })
	t.Cleanup(func() { killPidFile(t, pidFile) })
	cmd.WaitDelay = 100 * time.Millisecond
	start := time.Now()
	err := run(context.Background(), cmd)
	require.ErrorIs(t, err, unix.ECHILD)
	assert.Empty(t, calls(), "the group was signalled")
	assert.Less(t, time.Since(start), 2*time.Second)
	// The helper of the group was not killed.
	require.Eventually(t, func() bool { _, err := os.Stat(pidFile); return err == nil }, time.Second, 10*time.Millisecond)
	raw, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, syscall.Kill(pid, 0), "the helper should still be alive")
}

// Same when the context is done: the group is signalled once, while the
// leader is not reaped, and the leader is gone after run.
func TestRunSignalsTheGroupBeforeTheLeaderIsReaped(t *testing.T) {
	calls := recordKills(t, true)
	cmd, pidFile := helperScript(t)
	t.Cleanup(func() { killPidFile(t, pidFile) })
	cmd.WaitDelay = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	require.Error(t, run(ctx, cmd))
	got := calls()
	require.Len(t, got, 1)
	assert.Equal(t, cmd.Process.Pid, got[0])
	assert.True(t, cmd.ProcessState.Exited() || cmd.ProcessState.Sys() != nil)
}
