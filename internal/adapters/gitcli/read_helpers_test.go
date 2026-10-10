// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/gitcli"
)

// newReader prepares a data dir and an adapter like newGit, with the pool
// settings of cfg applied. Without a usable git it skips locally and fails
// when CI is set. The adapter is closed when the test ends.
func newReader(t *testing.T, tune func(*gitcli.Config)) (*gitcli.Git, string) {
	t.Helper()
	bin, err := exec.LookPath("git")
	if err != nil {
		unusable(t, "git not found")
	}
	data := t.TempDir()
	for _, d := range []string{"repos/.staging", "trash", "run/git-home", "kv"} {
		require.NoError(t, os.MkdirAll(filepath.Join(data, d), 0o700))
	}
	cfg := gitcli.Config{Bin: bin, DataDir: data, Path: os.Getenv("PATH")}
	if tune != nil {
		tune(&cfg)
	}
	g := gitcli.New(cfg)
	if _, err := g.CheckVersion(context.Background()); err != nil {
		unusable(t, "git is not usable by ermite: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, g.Close(ctx))
	})
	return g, data
}
