// SPDX-License-Identifier: AGPL-3.0-or-later

package ermitegit_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/ermitegit"
	"bnema.dev/ermite/internal/adapters/gitcli"
	"bnema.dev/ermite/internal/domain"
)

// newAdapter prepares a data directory and an ermitegit adapter around a
// gitcli reader, with the given routes. Without git it skips locally and
// fails when CI is set. Both are closed when the test ends.
func newAdapter(t testing.TB, routes ermitegit.Routes, tune func(*ermitegit.Config)) (*ermitegit.Adapter, *gitcli.Git, string) {
	t.Helper()
	bin, err := exec.LookPath("git")
	if err != nil {
		unusable(t, "git not found")
	}
	data := t.TempDir()
	for _, d := range []string{"repos/.staging", "trash", "run/git-home", "kv"} {
		require.NoError(t, os.MkdirAll(filepath.Join(data, d), 0o700))
	}
	g := gitcli.New(gitcli.Config{Bin: bin, DataDir: data, Path: os.Getenv("PATH")})
	if _, err := g.CheckVersion(context.Background()); err != nil {
		unusable(t, "git is not usable by ermite: %v", err)
	}
	cfg := ermitegit.Config{
		Fallback: g,
		RepoDir:  func(n domain.RepoName) string { return gitcli.RepoDir(data, n) },
		Routes:   &routes,
	}
	if tune != nil {
		tune(&cfg)
	}
	a := ermitegit.New(cfg)
	g.OnTrash(a.Forget)
	t.Cleanup(func() {
		a.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, g.Close(ctx))
	})
	return a, g, data
}

// unusable skips the test, or fails it in CI so that CI cannot pass vacuously.
func unusable(t testing.TB, format string, args ...any) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}
