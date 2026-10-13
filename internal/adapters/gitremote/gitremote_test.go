// SPDX-License-Identifier: AGPL-3.0-or-later

package gitremote_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/gitremote"
	"bnema.dev/ermite/internal/domain"
)

func gitBin(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	return bin
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x",
	}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s\n%s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

// fixture builds <base>/o/r with a branch, a lightweight tag and an annotated
// tag, and returns the base as a file:// URL.
func fixture(t *testing.T) (base, commit, tagObj string) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "o", "r")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	run(t, repo, "init", "-q", "--initial-branch=main")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a"), []byte("a"), 0o644))
	run(t, repo, "add", "a")
	run(t, repo, "commit", "-q", "-m", "a")
	commit = run(t, repo, "rev-parse", "HEAD")
	run(t, repo, "tag", "light")
	run(t, repo, "tag", "-a", "-m", "annotated", "v1")
	tagObj = run(t, repo, "rev-parse", "refs/tags/v1")
	return "file://" + root, commit, tagObj
}

func newRemote(t *testing.T, base string, mut func(*gitremote.Config)) *gitremote.Remote {
	t.Helper()
	cfg := gitremote.Config{Bin: gitBin(t), Path: os.Getenv("PATH"), Home: t.TempDir(), BaseURL: base, AllowedProtocols: "file"}
	if mut != nil {
		mut(&cfg)
	}
	return gitremote.New(cfg)
}

func TestListRefsPeelsAnnotatedTags(t *testing.T) {
	base, commit, tagObj := fixture(t)
	refs, err := newRemote(t, base, nil).ListRefs(context.Background(), "o", "r")
	require.NoError(t, err)
	byName := map[string][2]string{}
	for _, r := range refs {
		byName[r.Name] = [2]string{r.ID, r.Peeled}
	}
	assert.Equal(t, [2]string{commit, ""}, byName["refs/heads/main"])
	assert.Equal(t, [2]string{commit, ""}, byName["refs/tags/light"])
	assert.Equal(t, [2]string{tagObj, commit}, byName["refs/tags/v1"], "the annotated tag has its commit peeled")
	assert.Len(t, refs, 3)
}

// github.com answers a repository that does not exist, or that is private, with
// a 404 that git reports as a missing repository or as a prompt it may not
// show; both are "repository not found". Any other failure is unavailable.
func TestListRefsMapsWhatGitSays(t *testing.T) {
	fake := func(stderr string) string {
		bin := filepath.Join(t.TempDir(), "git")
		require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\necho '"+stderr+"' >&2\nexit 128\n"), 0o755))
		return bin
	}
	for _, tc := range []struct {
		stderr string
		want   error
	}{
		{"remote: Repository not found.\nfatal: repository 'https://github.com/o/x/' not found", domain.ErrNotFound},
		{"fatal: could not read Username for 'https://github.com': terminal prompts disabled", domain.ErrNotFound},
		{"fatal: unable to access 'https://github.com/o/r/': Could not resolve host: github.com", domain.ErrUnavailable},
		{"fatal: unable to access 'https://github.com/o/r/': The requested URL returned error: 503", domain.ErrUnavailable},
	} {
		_, err := newRemote(t, "https://github.com", func(c *gitremote.Config) { c.Bin = fake(tc.stderr) }).ListRefs(context.Background(), "o", "r")
		assert.ErrorIs(t, err, tc.want, tc.stderr)
	}
}

func TestListRefsRefusesBadNames(t *testing.T) {
	r := newRemote(t, "file:///nonexistent", nil)
	for _, bad := range [][2]string{{"", "r"}, {"o", ""}, {"-o", "r"}, {"o", "r/x"}, {"o", "r s"}, {"..", "r"}, {"o", "r?x=1"}} {
		_, err := r.ListRefs(context.Background(), bad[0], bad[1])
		assert.ErrorIs(t, err, domain.ErrNotFound, "%q/%q", bad[0], bad[1])
	}
}

func TestListRefsBoundsTheOutput(t *testing.T) {
	base, _, _ := fixture(t)
	_, err := newRemote(t, base, func(c *gitremote.Config) { c.MaxOutput = 40 }).ListRefs(context.Background(), "o", "r")
	assert.ErrorIs(t, err, domain.ErrUnavailable)
}

func TestListRefsTimesOut(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "git")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30\n"), 0o755))
	start := time.Now()
	_, err := newRemote(t, "file:///x", func(c *gitremote.Config) { c.Bin, c.Timeout = bin, 200*time.Millisecond }).ListRefs(context.Background(), "o", "r")
	assert.ErrorIs(t, err, domain.ErrUnavailable)
	assert.Less(t, time.Since(start), 10*time.Second)
}

// git asks for no credentials and runs with no configuration of the machine:
// the environment it gets is only what the adapter builds.
func TestListRefsGitEnvironment(t *testing.T) {
	out := filepath.Join(t.TempDir(), "env")
	bin := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\nenv > '" + out + "'\nprintf '%s\\n' \"$*\" >> '" + out + "'\nexit 0\n"
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))
	t.Setenv("GITHUB_TOKEN", "secret-from-the-process")
	_, err := newRemote(t, "https://example.test", func(c *gitremote.Config) { c.Bin, c.AllowedProtocols = bin, "" }).ListRefs(context.Background(), "o", "r")
	require.NoError(t, err)
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	env := string(b)
	assert.Contains(t, env, "GIT_TERMINAL_PROMPT=0")
	assert.Contains(t, env, "GIT_ALLOW_PROTOCOL=https")
	assert.NotContains(t, env, "secret-from-the-process")
	assert.Contains(t, env, "ls-remote -- https://example.test/o/r refs/heads/* refs/tags/*")
	assert.Contains(t, env, "credential.helper=")
}
