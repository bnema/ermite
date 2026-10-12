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
func shellCommand(ctx context.Context, line string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", line)
}

// openConfig opens the config file without blocking on a FIFO swapped in for it.
func openConfig(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// checkPrivate says whether token.cmd may be run from the config file at
// path, whose open handle reported info: whoever can change the file can make
// token.cmd any command. It returns nil when the file belongs to the current
// user and cannot be written by group or others, and when its directory
// belongs to the current user or root and cannot be written by group or
// others (a sticky directory excepted), as they could replace the file.
func checkPrivate(path string, info fs.FileInfo) error {
	euid := uint32(os.Geteuid())
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid != euid {
		return fmt.Errorf("config file %s is owned by another user: token.cmd not run", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("config file %s is writable by others: token.cmd not run", path)
	}
	dir := filepath.Dir(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		dir = filepath.Dir(resolved) // a symlinked dotfile: the directory of the target
	}
	di, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("config directory %s: %w: token.cmd not run", dir, err)
	}
	if st, ok := di.Sys().(*syscall.Stat_t); ok && st.Uid != euid && st.Uid != 0 {
		return fmt.Errorf("config directory %s is owned by another user: token.cmd not run", dir)
	}
	if di.Mode().Perm()&0o022 != 0 && di.Mode()&fs.ModeSticky == 0 {
		return fmt.Errorf("config directory %s is writable by others: token.cmd not run", dir)
	}
	return nil
}
