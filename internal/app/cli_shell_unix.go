// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package app

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// shellCommand runs line with `sh -c`.
//
// When foreground is false (no terminal: a script or an agent), the command
// gets its own process group and a cancellation, a timeout or a signal kills
// the whole group, so a grandchild such as `sleep` or a secret manager helper
// cannot outlive ermite or keep the output pipe open.
//
// When foreground is true (standard input is a terminal) the command stays in
// the foreground process group of the terminal: a pinentry or a password
// prompt then works, and a background group would be stopped by SIGTTIN
// when it reads the terminal. Ctrl+C reaches the whole group. The limit: on a
// timeout or a SIGTERM only sh is killed, not what it started.
func shellCommand(ctx context.Context, line string, foreground bool) *exec.Cmd {
	c := exec.CommandContext(ctx, "sh", "-c", line)
	if !foreground {
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		c.Cancel = func() error {
			// A negative pid is the process group, led by sh.
			return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		}
	}
	return c
}

// openConfig opens the config file without blocking on a FIFO swapped in for it.
func openConfig(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// checkPrivate says whether token.cmd may be run from the config file at
// path, whose open handle reported info: whoever can change the file can make
// token.cmd any command. It returns nil when the file belongs to euid (the
// effective user id of the process) and cannot be written by group or others,
// and when its directory belongs to euid or root and cannot be written by
// group or others (a sticky directory excepted), as they could replace the
// file.
func checkPrivate(path string, info fs.FileInfo, euid int) error {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != euid {
		return fmt.Errorf("config file %s is owned by another user: token.cmd not run", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("config file %s is writable by others: token.cmd not run", path)
	}
	dir := filepath.Dir(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		dir = filepath.Dir(resolved) // a symlinked dotfile: the directory of the target
	}
	return checkPrivateDir(dir, euid)
}

// checkPrivateDir is the directory half of checkPrivate.
func checkPrivateDir(dir string, euid int) error {
	di, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("config directory %s: %w: token.cmd not run", dir, err)
	}
	if st, ok := di.Sys().(*syscall.Stat_t); ok && int(st.Uid) != euid && st.Uid != 0 {
		return fmt.Errorf("config directory %s is owned by another user: token.cmd not run", dir)
	}
	if di.Mode().Perm()&0o022 != 0 && di.Mode()&fs.ModeSticky == 0 {
		return fmt.Errorf("config directory %s is writable by others: token.cmd not run", dir)
	}
	return nil
}
