// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli_test

import (
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

// fixtureRepo builds a repository with real git: a bare repository in the
// data directory, and a work tree beside it that the helpers edit and commit.
type fixtureRepo struct {
	t    *testing.T
	name domain.RepoName
	bare string
	work string
	when time.Time
}

// newFixtureRepo creates an empty bare repository repos/<name>.git whose HEAD
// is refs/heads/main.
func newFixtureRepo(t *testing.T, data, repo string) *fixtureRepo {
	t.Helper()
	f := &fixtureRepo{
		t:    t,
		name: name(t, repo),
		bare: filepath.Join(data, "repos", repo+".git"),
		work: t.TempDir(),
		when: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	}
	out, err := exec.Command("git", "init", "--quiet", "--bare", "--initial-branch=main", f.bare).CombinedOutput()
	require.NoError(t, err, string(out))
	f.git("config", "core.compression", "0")
	f.git("config", "core.autocrlf", "false")
	return f
}

// git runs git against the bare repository and the work tree.
func (f *fixtureRepo) git(args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = f.work
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + f.work,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_DIR=" + f.bare,
		"GIT_WORK_TREE=" + f.work,
		"GIT_AUTHOR_NAME=Ada Lovelace", "GIT_AUTHOR_EMAIL=ada@example.org",
		"GIT_COMMITTER_NAME=Grace Hopper", "GIT_COMMITTER_EMAIL=grace@example.org",
		"GIT_AUTHOR_DATE=" + f.when.Format(time.RFC3339),
		"GIT_COMMITTER_DATE=" + f.when.Format(time.RFC3339),
	}
	out, err := cmd.CombinedOutput()
	require.NoError(f.t, err, "git %s: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

func (f *fixtureRepo) path(p string) string { return filepath.Join(f.work, filepath.FromSlash(p)) }

// write creates or replaces a file with the content.
func (f *fixtureRepo) write(p, content string) *fixtureRepo {
	f.t.Helper()
	require.NoError(f.t, os.MkdirAll(filepath.Dir(f.path(p)), 0o755))
	require.NoError(f.t, os.WriteFile(f.path(p), []byte(content), 0o644))
	return f
}

// writeBytes is write for binary content.
func (f *fixtureRepo) writeBytes(p string, content []byte) *fixtureRepo {
	f.t.Helper()
	require.NoError(f.t, os.MkdirAll(filepath.Dir(f.path(p)), 0o755))
	require.NoError(f.t, os.WriteFile(f.path(p), content, 0o644))
	return f
}

// lines writes n lines "<prefix> <i>" to the file.
func (f *fixtureRepo) lines(p, prefix string, n int) *fixtureRepo {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "%s %d\n", prefix, i)
	}
	return f.write(p, b.String())
}

func (f *fixtureRepo) remove(p string) *fixtureRepo {
	f.t.Helper()
	require.NoError(f.t, os.Remove(f.path(p)))
	return f
}

func (f *fixtureRepo) chmod(p string, mode os.FileMode) *fixtureRepo {
	f.t.Helper()
	require.NoError(f.t, os.Chmod(f.path(p), mode))
	return f
}

func (f *fixtureRepo) symlink(target, p string) *fixtureRepo {
	f.t.Helper()
	require.NoError(f.t, os.Symlink(target, f.path(p)))
	return f
}

func (f *fixtureRepo) mv(from, to string) *fixtureRepo {
	f.t.Helper()
	require.NoError(f.t, os.MkdirAll(filepath.Dir(f.path(to)), 0o755))
	require.NoError(f.t, os.Rename(f.path(from), f.path(to)))
	return f
}

// commit stages everything and commits with the next timestamp; it returns
// the commit id.
func (f *fixtureRepo) commit(msg string) string {
	f.t.Helper()
	f.when = f.when.Add(time.Minute)
	f.git("add", "-A")
	f.git("commit", "--quiet", "--allow-empty", "-m", msg)
	return f.git("rev-parse", "HEAD")
}

// checkoutNew creates a branch from the current HEAD and switches to it.
func (f *fixtureRepo) checkoutNew(branch string) {
	f.t.Helper()
	f.git("checkout", "--quiet", "-b", branch)
}

func (f *fixtureRepo) checkout(branch string) { f.t.Helper(); f.git("checkout", "--quiet", branch) }

// merge merges the branch with a merge commit.
func (f *fixtureRepo) merge(branch string) string {
	f.t.Helper()
	f.when = f.when.Add(time.Minute)
	f.git("merge", "--quiet", "--no-ff", "-m", "merge "+branch, branch)
	return f.git("rev-parse", "HEAD")
}

// randomBytes returns n deterministic incompressible bytes.
func randomBytes(n int, seed uint64) []byte {
	var s [32]byte
	for i := range 8 {
		s[i] = byte(seed >> (8 * i))
	}
	r := rand.NewChaCha8(s)
	b := make([]byte, n)
	_, _ = r.Read(b)
	return b
}
