// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package gitpush

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// Tests replace these two.
var (
	waitid    = unix.Waitid
	killGroup = func(pgid int) error { return syscall.Kill(-pgid, syscall.SIGKILL) }
)

// run starts cmd, waits for it, and kills what it leaves running.
//
// git runs git-remote-https, which is the process that connects and that holds
// the pipes of git's output. Killing git alone leaves it running, and Wait
// blocked on the pipes. So git leads a process group of its own, and the group
// is killed when ctx is done and also when git has exited.
//
// The invariant that makes killing a group safe: the group is only signalled
// while its leader is still not reaped. Linux does not reuse a PID as long as
// the process is a zombie nobody waited for, nor while it is the group ID of a
// live member, so -pid is still our group and cannot be another one. The
// leader is therefore waited for with waitid(WNOWAIT), which observes its exit
// without reaping it; the group is signalled; and only then cmd.Wait reaps. A
// kill(-pid) after Wait, or from Cmd.Cancel which races with it, would not be
// safe. When waitid fails, whatever the reason, nothing proves the leader is
// unreaped: the group is not signalled, only the leader is killed (through
// its pidfd, which cannot hit a reused PID), and the error is returned.
// cmd.WaitDelay (set by the caller) stays the backstop for a descendant that
// escaped the group and holds the pipes.
func run(ctx context.Context, cmd *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	observed := make(chan error, 1)
	go func() { observed <- waitExit(pid) }()

	var werr error
	select {
	case err := <-observed:
		if err != nil {
			werr = err // the leader may have been reaped: no group signal
		} else {
			signalGroup(cmd, pid) // exited, not reaped: a zombie
		}
	case <-ctx.Done():
		select {
		case err := <-observed:
			// waitid answered at the same time: trust it as above.
			if err != nil {
				werr = err
			} else {
				signalGroup(cmd, pid)
			}
		default:
			// No waitid error so far: the leader is alive or a zombie, unreaped
			// (only cmd.Wait below reaps it).
			signalGroup(cmd, pid)
			if err := <-observed; err != nil {
				werr = err // after the signals: reported, nothing more to do
			}
		}
	}
	if werr != nil {
		_ = cmd.Process.Kill()
	}
	err := cmd.Wait()
	if werr != nil {
		return fmt.Errorf("waiting for the process %d: %w", pid, werr)
	}
	return err
}

// signalGroup kills the group of the leader, and the leader alone should it
// have left the group. The leader must not be reaped yet.
func signalGroup(cmd *exec.Cmd, pid int) {
	_ = killGroup(pid)
	_ = cmd.Process.Kill()
}

// waitExit returns when the child pid has exited, without reaping it, or the
// error of waitid.
func waitExit(pid int) error {
	var info unix.Siginfo
	for {
		err := waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
