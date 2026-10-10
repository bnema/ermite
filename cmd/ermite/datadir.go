// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// prepareDataDir creates the directories of the data root (0700) and
// recreates run/ and run/git-home/, which hold nothing that survives a start.
func prepareDataDir(root string) error {
	for _, d := range []string{"", "repos", filepath.Join("repos", ".staging"), "trash", "kv"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			return fmt.Errorf("prepare data directory: %w", err)
		}
	}
	run := filepath.Join(root, "run")
	if err := os.RemoveAll(run); err != nil {
		return fmt.Errorf("reset %s: %w", run, err)
	}
	if err := os.MkdirAll(filepath.Join(run, "git-home"), 0o700); err != nil {
		return fmt.Errorf("prepare %s: %w", run, err)
	}
	return nil
}
