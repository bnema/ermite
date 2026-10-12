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
func shellCommand(ctx context.Context, line string) *exec.Cmd {
	c := exec.CommandContext(ctx, "cmd")
	c.SysProcAttr = &syscall.SysProcAttr{CmdLine: "cmd /C " + line}
	return c
}

// openConfig opens the config file.
func openConfig(path string) (*os.File, error) { return os.Open(path) }

// checkPrivate: Windows has no Unix mode bits to check.
func checkPrivate(string, fs.FileInfo) error { return nil }
