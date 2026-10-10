// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/gitcli"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in      string
		want    gitcli.Version
		wantErr bool
	}{
		{in: "git version 2.45.0\n", want: gitcli.Version{Major: 2, Minor: 45}},
		{in: "git version 2.56.0", want: gitcli.Version{Major: 2, Minor: 56}},
		{in: "git version 2.39.5 (Apple Git-154)\n", want: gitcli.Version{Major: 2, Minor: 39, Patch: 5}},
		{in: "git version 2.46.0.rc1\n", want: gitcli.Version{Major: 2, Minor: 46}},
		{in: "git version 2.47.1.windows.1", want: gitcli.Version{Major: 2, Minor: 47, Patch: 1}},
		{in: "git version 3.0", want: gitcli.Version{Major: 3}},
		{in: "git version", wantErr: true},
		{in: "", wantErr: true},
		{in: "hg version 6.0", wantErr: true},
		{in: "git version two.five", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := gitcli.ParseVersion(tt.in)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestVersionLess(t *testing.T) {
	min := gitcli.MinVersion
	assert.True(t, gitcli.Version{Major: 2, Minor: 44, Patch: 9}.Less(min))
	assert.True(t, gitcli.Version{Major: 1, Minor: 99}.Less(min))
	assert.False(t, gitcli.Version{Major: 2, Minor: 45}.Less(min))
	assert.False(t, gitcli.Version{Major: 2, Minor: 46}.Less(min))
	assert.False(t, gitcli.Version{Major: 3}.Less(min))
}

// newGit prepares a data dir and an adapter; it skips when git is missing.
func newGit(t *testing.T) (*gitcli.Git, string) {
	t.Helper()
	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}
	data := t.TempDir()
	for _, d := range []string{"repos/.staging", "trash", "run/git-home", "kv"} {
		require.NoError(t, os.MkdirAll(filepath.Join(data, d), 0o700))
	}
	return gitcli.New(gitcli.Config{Bin: bin, DataDir: data, Path: os.Getenv("PATH")}), data
}

func name(t *testing.T, s string) domain.RepoName {
	t.Helper()
	n, err := domain.ParseRepoName(s)
	require.NoError(t, err)
	return n
}

func TestCheckVersion(t *testing.T) {
	g, _ := newGit(t)
	v, err := g.CheckVersion(context.Background())
	require.NoError(t, err)
	assert.False(t, v.Less(gitcli.MinVersion))
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "git")
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700))
	return p
}

func TestCheckVersionRejects(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	cfg := func(bin string) gitcli.Config { return gitcli.Config{Bin: bin, DataDir: data, Path: "/usr/bin:/bin"} }

	t.Run("too old", func(t *testing.T) {
		_, err := gitcli.New(cfg(writeScript(t, `echo "git version 2.44.9"`))).CheckVersion(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "older than")
	})
	t.Run("garbage output", func(t *testing.T) {
		_, err := gitcli.New(cfg(writeScript(t, `echo hello`))).CheckVersion(ctx)
		assert.Error(t, err)
	})
	t.Run("missing binary", func(t *testing.T) {
		_, err := gitcli.New(cfg(filepath.Join(data, "no-such-git"))).CheckVersion(ctx)
		assert.Error(t, err)
	})
	t.Run("failing binary", func(t *testing.T) {
		_, err := gitcli.New(cfg(writeScript(t, `echo broken >&2; exit 3`))).CheckVersion(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "broken")
	})
}

func TestIsolatedEnvironment(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	out := filepath.Join(data, "env.txt")
	script := writeScript(t, `env > "`+out+`"; echo "git version 2.50.0"`)
	// Variables that must not reach git.
	t.Setenv("GIT_DIR", "/nonexistent")
	t.Setenv("GIT_AUTHOR_NAME", "leak")
	t.Setenv("GIT_CONFIG_GLOBAL", "/leak")
	t.Setenv("XDG_CONFIG_HOME", "/leak")
	t.Setenv("SECRET_TOKEN", "leak")

	g := gitcli.New(gitcli.Config{Bin: script, DataDir: data, Path: "/usr/bin:/bin"})
	_, err := g.CheckVersion(ctx)
	require.NoError(t, err)

	raw, err := os.ReadFile(out)
	require.NoError(t, err)
	got := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		k, v, _ := strings.Cut(l, "=")
		got[k] = v
	}
	assert.Equal(t, "1", got["GIT_CONFIG_NOSYSTEM"])
	assert.Equal(t, filepath.Join(data, "run", "git-home"), got["HOME"])
	assert.Equal(t, "0", got["GIT_TERMINAL_PROMPT"])
	assert.Equal(t, "/usr/bin:/bin", got["PATH"])
	for k, v := range got {
		assert.NotContains(t, v, "leak", k)
		switch k {
		case "GIT_DIR", "GIT_AUTHOR_NAME", "GIT_CONFIG_GLOBAL", "XDG_CONFIG_HOME", "SECRET_TOKEN":
			t.Errorf("variable %s leaked into git", k)
		}
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1"}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

func TestStagePublish(t *testing.T) {
	ctx := context.Background()
	g, data := newGit(t)

	id, err := g.Stage(ctx)
	require.NoError(t, err)
	staged := filepath.Join(data, "repos", ".staging", string(id))
	require.DirExists(t, staged)

	assert.Equal(t, "true", gitOut(t, staged, "rev-parse", "--is-bare-repository"))
	assert.Equal(t, "sha1", gitOut(t, staged, "rev-parse", "--show-object-format"))
	assert.Equal(t, "files", gitOut(t, staged, "rev-parse", "--show-ref-format"))
	assert.Equal(t, "true", gitOut(t, staged, "config", "--local", "repack.writeBitmaps"))
	assert.Equal(t, "true", gitOut(t, staged, "config", "--local", "core.commitGraph"))
	assert.Equal(t, "refs/heads/main", gitOut(t, staged, "symbolic-ref", "HEAD"))

	n := name(t, "tools")
	require.NoError(t, g.Publish(ctx, id, n))
	assert.NoDirExists(t, staged)
	assert.Equal(t, "true", gitOut(t, filepath.Join(data, "repos", "tools.git"), "rev-parse", "--is-bare-repository"))

	// A second staged repo cannot take the same name and is left for Discard.
	id2, err := g.Stage(ctx)
	require.NoError(t, err)
	err = g.Publish(ctx, id2, n)
	assert.ErrorIs(t, err, domain.ErrAlreadyExists)
	assert.DirExists(t, filepath.Join(data, "repos", ".staging", string(id2)))
	require.NoError(t, g.Discard(ctx, id2))
	assert.NoDirExists(t, filepath.Join(data, "repos", ".staging", string(id2)))
	require.NoError(t, g.Discard(ctx, id2), "discarding twice is not an error")
}

func TestPublishOverEmptyDirectoryDoesNotReplace(t *testing.T) {
	ctx := context.Background()
	g, data := newGit(t)
	require.NoError(t, os.Mkdir(filepath.Join(data, "repos", "taken.git"), 0o700))
	id, err := g.Stage(ctx)
	require.NoError(t, err)
	assert.ErrorIs(t, g.Publish(ctx, id, name(t, "taken")), domain.ErrAlreadyExists)
}

func TestInvalidStagedID(t *testing.T) {
	ctx := context.Background()
	g, data := newGit(t)
	for _, id := range []ports.StagedID{"", "../../trash", "zz", ports.StagedID(strings.Repeat("g", 32))} {
		assert.Error(t, g.Discard(ctx, id), string(id))
		assert.Error(t, g.Publish(ctx, id, name(t, "x")), string(id))
	}
	assert.DirExists(t, filepath.Join(data, "trash"))
}

func TestConcurrentPublishSameName(t *testing.T) {
	ctx := context.Background()
	g, data := newGit(t)
	n := name(t, "race")

	const workers = 6
	ids := make([]ports.StagedID, workers)
	for i := range ids {
		id, err := g.Stage(ctx)
		require.NoError(t, err)
		ids[i] = id
	}
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- g.Publish(ctx, id, n)
		}()
	}
	wg.Wait()
	close(errs)
	ok, exists := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, domain.ErrAlreadyExists):
			exists++
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	assert.Equal(t, 1, ok)
	assert.Equal(t, workers-1, exists)
	for _, id := range ids {
		require.NoError(t, g.Discard(ctx, id))
	}
	entries, err := os.ReadDir(filepath.Join(data, "repos", ".staging"))
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestStageFailureLeavesNothing(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(data, "repos", ".staging"), 0o700))
	// A git that creates the directory and then fails.
	script := writeScript(t, `for last; do :; done; mkdir -p "$last"; echo nope >&2; exit 1`)
	g := gitcli.New(gitcli.Config{Bin: script, DataDir: data, Path: "/usr/bin:/bin"})
	_, err := g.Stage(ctx)
	require.Error(t, err)
	entries, rerr := os.ReadDir(filepath.Join(data, "repos", ".staging"))
	require.NoError(t, rerr)
	assert.Empty(t, entries)
}

func TestPurgeStagingAndList(t *testing.T) {
	ctx := context.Background()
	g, data := newGit(t)
	repos := filepath.Join(data, "repos")

	_, err := g.Stage(ctx)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(repos, ".staging", "stray"), []byte("x"), 0o600))
	for _, d := range []string{"good.git", "also-good.git", "Bad.git", "no-suffix", ".hidden.git", "x..y.git"} {
		require.NoError(t, os.Mkdir(filepath.Join(repos, d), 0o700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(repos, "file.git"), []byte("x"), 0o600))

	require.NoError(t, g.PurgeStaging(ctx))
	entries, err := os.ReadDir(filepath.Join(repos, ".staging"))
	require.NoError(t, err)
	assert.Empty(t, entries)
	assert.DirExists(t, filepath.Join(repos, ".staging"))

	dirs, err := g.List(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []domain.RepoName{name(t, "good"), name(t, "also-good")}, dirs.Names)
	assert.ElementsMatch(t, []string{"Bad.git", "no-suffix", ".hidden.git", "x..y.git", "file.git"}, dirs.Invalid)
}

func TestTrash(t *testing.T) {
	ctx := context.Background()
	g, data := newGit(t)
	n := name(t, "old")

	assert.ErrorIs(t, g.Trash(ctx, n), domain.ErrNotFound)

	for range 2 {
		id, err := g.Stage(ctx)
		require.NoError(t, err)
		require.NoError(t, g.Publish(ctx, id, n))
		require.NoError(t, g.Trash(ctx, n))
	}
	assert.NoDirExists(t, filepath.Join(data, "repos", "old.git"))
	entries, err := os.ReadDir(filepath.Join(data, "trash"))
	require.NoError(t, err)
	require.Len(t, entries, 2, "two trashings never overwrite each other")
	for _, e := range entries {
		assert.True(t, strings.HasPrefix(e.Name(), "old-") && strings.HasSuffix(e.Name(), ".git"), e.Name())
	}
}
