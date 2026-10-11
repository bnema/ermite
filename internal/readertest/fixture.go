// SPDX-License-Identifier: AGPL-3.0-or-later

// Package readertest is the shared test kit of the RepoReader adapters: Git
// fixtures built with the git binary, the conformance table every adapter must
// pass, and the comparison of one adapter with another.
package readertest

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

// Fixture builds a repository with real git: a bare repository in the
// data directory, and a work tree beside it that the helpers edit and commit.
type Fixture struct {
	T    testing.TB
	Name domain.RepoName
	Bare string
	Work string
	When time.Time
}

// NewFixture creates an empty bare repository repos/<name>.git whose HEAD
// is refs/heads/main.
func NewFixture(t testing.TB, data, repo string) *Fixture {
	t.Helper()
	f := &Fixture{
		T:    t,
		Name: RepoName(t, repo),
		Bare: filepath.Join(data, "repos", repo+".git"),
		Work: t.TempDir(),
		When: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	}
	out, err := exec.Command("git", "init", "--quiet", "--bare", "--initial-branch=main", f.Bare).CombinedOutput()
	require.NoError(t, err, string(out))
	f.Git("config", "core.compression", "0")
	f.Git("config", "core.autocrlf", "false")
	return f
}

// Git runs git against the bare repository and the work tree.
func (f *Fixture) Git(args ...string) string {
	f.T.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = f.Work
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + f.Work,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_DIR=" + f.Bare,
		"GIT_WORK_TREE=" + f.Work,
		"GIT_AUTHOR_NAME=Ada Lovelace", "GIT_AUTHOR_EMAIL=ada@example.org",
		"GIT_COMMITTER_NAME=Grace Hopper", "GIT_COMMITTER_EMAIL=grace@example.org",
		"GIT_AUTHOR_DATE=" + f.When.Format(time.RFC3339),
		"GIT_COMMITTER_DATE=" + f.When.Format(time.RFC3339),
	}
	out, err := cmd.CombinedOutput()
	require.NoError(f.T, err, "git %s: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

// WriteObject stores a raw object of the given type (commit, tag...) built by
// hand, for what git itself refuses to write, and returns its id.
func (f *Fixture) WriteObject(typ, content string) string {
	f.T.Helper()
	cmd := exec.Command("git", "hash-object", "-w", "-t", typ, "--stdin")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.Work, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_DIR=" + f.Bare}
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.CombinedOutput()
	require.NoError(f.T, err, "git hash-object: %s", out)
	return strings.TrimSpace(string(out))
}

// Path returns the path of a file of the work tree.
func (f *Fixture) Path(p string) string { return filepath.Join(f.Work, filepath.FromSlash(p)) }

// Write creates or replaces a file with the content.
func (f *Fixture) Write(p, content string) *Fixture {
	f.T.Helper()
	require.NoError(f.T, os.MkdirAll(filepath.Dir(f.Path(p)), 0o755))
	require.NoError(f.T, os.WriteFile(f.Path(p), []byte(content), 0o644))
	return f
}

// WriteBytes is Write for binary content.
func (f *Fixture) WriteBytes(p string, content []byte) *Fixture {
	f.T.Helper()
	require.NoError(f.T, os.MkdirAll(filepath.Dir(f.Path(p)), 0o755))
	require.NoError(f.T, os.WriteFile(f.Path(p), content, 0o644))
	return f
}

// Lines writes n lines "<prefix> <i>" to the file.
func (f *Fixture) Lines(p, prefix string, n int) *Fixture {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "%s %d\n", prefix, i)
	}
	return f.Write(p, b.String())
}

func (f *Fixture) Remove(p string) *Fixture {
	f.T.Helper()
	require.NoError(f.T, os.Remove(f.Path(p)))
	return f
}

func (f *Fixture) Chmod(p string, mode os.FileMode) *Fixture {
	f.T.Helper()
	require.NoError(f.T, os.Chmod(f.Path(p), mode))
	return f
}

func (f *Fixture) Symlink(target, p string) *Fixture {
	f.T.Helper()
	require.NoError(f.T, os.Symlink(target, f.Path(p)))
	return f
}

func (f *Fixture) Mv(from, to string) *Fixture {
	f.T.Helper()
	require.NoError(f.T, os.MkdirAll(filepath.Dir(f.Path(to)), 0o755))
	require.NoError(f.T, os.Rename(f.Path(from), f.Path(to)))
	return f
}

// Commit stages everything and commits with the next timestamp; it returns
// the commit id.
func (f *Fixture) Commit(msg string) string {
	f.T.Helper()
	f.When = f.When.Add(time.Minute)
	f.Git("add", "-A")
	f.Git("commit", "--quiet", "--allow-empty", "-m", msg)
	return f.Git("rev-parse", "HEAD")
}

// CheckoutNew creates a branch from the current HEAD and switches to it.
func (f *Fixture) CheckoutNew(branch string) {
	f.T.Helper()
	f.Git("checkout", "--quiet", "-b", branch)
}

func (f *Fixture) Checkout(branch string) { f.T.Helper(); f.Git("checkout", "--quiet", branch) }

// Merge merges the branch with a merge commit.
func (f *Fixture) Merge(branch string) string {
	f.T.Helper()
	f.When = f.When.Add(time.Minute)
	f.Git("merge", "--quiet", "--no-ff", "-m", "merge "+branch, branch)
	return f.Git("rev-parse", "HEAD")
}

// RandomBytes returns n deterministic incompressible bytes.
func RandomBytes(n int, seed uint64) []byte {
	var s [32]byte
	for i := range 8 {
		s[i] = byte(seed >> (8 * i))
	}
	r := rand.NewChaCha8(s)
	b := make([]byte, n)
	_, _ = r.Read(b)
	return b
}

// RepoName parses a repository name.
func RepoName(t testing.TB, s string) domain.RepoName {
	t.Helper()
	n, err := domain.ParseRepoName(s)
	require.NoError(t, err)
	return n
}
