// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package app

import (
	"context"
	"os/exec"
)

// shellCommand runs line with `sh -c`.
func shellCommand(ctx context.Context, line string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", line)
}
