// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package app

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"syscall"
)

// shellCommand runs line with `cmd /C`. The command line is passed as is:
// Go's argument quoting would mangle quotes cmd.exe means to read itself.
//
// The command is killed with exec.CommandContext on a timeout or a signal:
// only cmd.exe itself, not what it started (there is no process group kill
// here); foreground is ignored.
func shellCommand(ctx context.Context, line string, _ bool) *exec.Cmd {
	c := exec.CommandContext(ctx, "cmd")
	c.SysProcAttr = &syscall.SysProcAttr{CmdLine: "cmd /C " + line}
	return c
}

// openConfig opens the config file.
func openConfig(path string) (*os.File, error) { return os.Open(path) }

// checkPrivate is a no-op on Windows: it has no Unix mode bits or owner ids to
// check, so token.cmd is run from a config file whoever can write it. The
// per-user config directory (%AppData%) is private to the user by default;
// keep ERMITE_CONFIG out of shared folders.
func checkPrivate(string, fs.FileInfo, int) error { return nil }
