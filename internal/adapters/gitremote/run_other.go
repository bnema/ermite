// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux

package gitremote

import (
	"context"
	"os/exec"
)

// run starts cmd and waits for it, killing the process (not its group: only
// Linux has the guarantee that makes killing a group safe, see run_linux.go)
// when ctx is done. cmd.WaitDelay stays the backstop for helpers that hold the
// pipes.
func run(ctx context.Context, cmd *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
		case <-done:
		}
	}()
	return cmd.Wait()
}
