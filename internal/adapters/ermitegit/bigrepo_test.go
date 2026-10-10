// SPDX-License-Identifier: AGPL-3.0-or-later

package ermitegit_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/gitcli"
	"bnema.dev/ermite/internal/domain"
)

// EnvBenchRepo names a real repository (its .git directory) for the
// comparison test and the benchmarks, which skip without it. It is never
// modified: it is cloned.
const EnvBenchRepo = "ERMITE_GIT_BENCH_REPO"

// bigRepo is a bare clone of the repository of EnvBenchRepo, configured like
// a repository ermite creates and maintained the way ermite does.
type bigRepo struct {
	name domain.RepoName
	dir  string
}

// cloneBench clones the repository of EnvBenchRepo into the data directory with
// --no-local (a real pack transfer, so that the clone is packed as a push
// would leave it), applies the settings of gitcli.Stage and runs
// gitcli.Maintain. It skips the test when the variable is unset.
func cloneBench(tb testing.TB, g *gitcli.Git, data string) bigRepo {
	tb.Helper()
	src := os.Getenv(EnvBenchRepo)
	if src == "" {
		tb.Skipf("%s is not set", EnvBenchRepo)
	}
	name, err := domain.ParseRepoName("bench")
	require.NoError(tb, err)
	dir := gitcli.RepoDir(data, name)
	run := func(args ...string) {
		tb.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(data, "run", "git-home"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		out, err := cmd.CombinedOutput()
		require.NoError(tb, err, "git %v: %s", args, out)
	}
	run("clone", "--quiet", "--bare", "--no-local", src, dir)
	for _, kv := range [][2]string{{"repack.writeBitmaps", "true"}, {"core.commitGraph", "true"}, {"commitGraph.changedPathsVersion", "2"}} {
		run("-C", dir, "config", "--local", kv[0], kv[1])
	}
	run("-C", dir, "gc", "--quiet", "--prune=now")
	require.NoError(tb, g.Maintain(context.Background(), name))
	return bigRepo{name: name, dir: dir}
}
