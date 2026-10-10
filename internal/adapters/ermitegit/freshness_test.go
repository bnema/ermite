// SPDX-License-Identifier: AGPL-3.0-or-later

package ermitegit_test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/ermitegit"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/readertest"
)

// push sends the work tree's HEAD to the bare repository of the fixture as a
// push does: git receive-pack writes a pack (or loose objects) and updates
// the ref, with no help from the reader.
func push(t *testing.T, f *readertest.Fixture, remote, branch string, loose bool) {
	t.Helper()
	args := []string{"push", "--quiet", remote, "HEAD:refs/heads/" + branch}
	if loose {
		args = append([]string{"-c", "receive.unpackLimit=1000"}, args...)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = f.Work
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.Work, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_DIR=" + f.Bare}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

// TestFreshness proves that a read that starts after a push sees the push
// with the adapter's own Refresh disabled (an interval of one hour: only the
// first read refreshes): new commits in a pack and as loose objects, new
// branches, a ref that moves, a repack that replaces the packs, a new
// commit-graph layer, a deleted branch, tags.
func TestFreshness(t *testing.T) {
	ctx := context.Background()
	a, g, data := newAdapter(t, ermitegit.AllRoutes(ermitegit.ErmiteGit), func(c *ermitegit.Config) { c.RefreshEvery = time.Hour })

	// The work tree is an ordinary clone-like repository; "remote" is the
	// bare repository the adapter reads.
	f := readertest.NewFixture(t, data, "pushed")
	bare := f.Bare
	// A separate repository makes the commits and pushes them.
	work := readertest.NewFixture(t, t.TempDir(), "work")
	work.Write("a.txt", "1\n")
	work.Commit("first")
	push(t, work, bare, "main", false)

	head := func(t *testing.T) string {
		t.Helper()
		res, err := a.ResolveRevision(ctx, f.Name, domain.Revision{})
		require.NoError(t, err)
		return res.CommitID.String()
	}
	rev := func(s string) domain.Revision {
		r, err := domain.ParseRevision(s)
		require.NoError(t, err)
		return r
	}
	cli := func(t *testing.T) string {
		t.Helper()
		res, err := g.ResolveRevision(ctx, f.Name, domain.Revision{})
		require.NoError(t, err)
		return res.CommitID.String()
	}
	require.Equal(t, work.Git("rev-parse", "HEAD"), head(t), "the first read")

	for i, step := range []struct {
		name  string
		loose bool
		after func()
	}{
		{"pack", false, nil},
		{"loose objects", true, nil},
		{"repack replaces the packs", false, func() { f.Git("repack", "-adf") }},
		{"commit-graph layer", false, func() { f.Git("commit-graph", "write", "--reachable", "--changed-paths", "--split") }},
		{"gc packs the refs", false, func() { f.Git("gc", "--quiet", "--prune=now") }},
	} {
		t.Run(step.name, func(t *testing.T) {
			work.Write("a.txt", strings.Repeat("x", i+2)+"\n").Write("dir/new.txt", step.name+"\n")
			c := work.Commit("push " + step.name)
			push(t, work, bare, "main", step.loose)
			// Not a single Refresh in between: the very next read sees it.
			assert.Equal(t, c, head(t))
			page, err := a.ListCommits(ctx, f.Name, ports.CommitQuery{Limit: 3})
			require.NoError(t, err)
			require.NotEmpty(t, page.Commits)
			assert.Equal(t, c, page.Commits[0].ID.String())
			tree, err := a.GetTree(ctx, f.Name, domain.Revision{}, domain.TreePath{}, true)
			require.NoError(t, err)
			require.NotNil(t, tree.Entries[0].LastCommit)
			assert.Equal(t, c, tree.Entries[0].LastCommit.ID.String(), "dir/ was last touched by the push")
			_, rc, err := a.OpenBlob(ctx, f.Name, domain.Revision{}, readertest.TreePath(t, "dir/new.txt"))
			require.NoError(t, err)
			body, err := io.ReadAll(rc)
			require.NoError(t, err)
			require.NoError(t, rc.Close())
			assert.Equal(t, step.name+"\n", string(body))
			assert.Equal(t, cli(t), head(t), "the same answer as git")

			if step.after != nil {
				step.after()
				assert.Equal(t, c, head(t), "after maintenance")
				tree, err := a.GetTree(ctx, f.Name, domain.Revision{}, domain.TreePath{}, true)
				require.NoError(t, err)
				assert.Equal(t, c, tree.Entries[0].LastCommit.ID.String())
			}
		})
	}

	t.Run("new and deleted refs", func(t *testing.T) {
		work.Git("branch", "-f", "feature/new", "HEAD")
		push(t, work, bare, "feature/new", false)
		res, err := a.ResolveRevision(ctx, f.Name, rev("feature/new"))
		require.NoError(t, err)
		assert.Equal(t, "refs/heads/feature/new", res.Ref)

		work.Git("tag", "-a", "-m", "release", "v9")
		work.Git("push", "--quiet", bare, "refs/tags/v9")
		refs, err := a.ListRefs(ctx, f.Name)
		require.NoError(t, err)
		var names []string
		for _, r := range refs {
			names = append(names, r.Name)
		}
		assert.Contains(t, names, "refs/tags/v9")
		want, err := g.ListRefs(ctx, f.Name)
		require.NoError(t, err)
		assert.Equal(t, want, refs)

		f.Git("update-ref", "-d", "refs/heads/feature/new")
		_, err = a.ResolveRevision(ctx, f.Name, rev("feature/new"))
		assert.ErrorIs(t, err, domain.ErrNotFound)
		f.Git("pack-refs", "--all")
		f.Git("update-ref", "-d", "refs/tags/v9")
		_, err = a.ResolveRevision(ctx, f.Name, rev("v9"))
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("the repository is deleted and recreated", func(t *testing.T) {
		require.NoError(t, g.Trash(ctx, f.Name))
		_, err := a.ListRefs(ctx, f.Name)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		assert.Equal(t, 0, a.OpenRepos(), "the trash hook dropped the repository")

		again := readertest.NewFixture(t, data, "pushed")
		again.Write("b.txt", "new repo\n")
		c := again.Commit("a new repository under the old name")
		assert.Equal(t, c, head(t), "no trace of the old repository")
	})
}

// TestReplacedDirectoryIsNotConfused: the cache is keyed by directory, so a
// directory replaced by another repository (a restore, a rename onto the same
// name) must not serve the old one: the inode tells.
func TestReplacedDirectoryIsNotConfused(t *testing.T) {
	ctx := context.Background()
	a, _, data := newAdapter(t, ermitegit.AllRoutes(ermitegit.ErmiteGit), func(c *ermitegit.Config) { c.RefreshEvery = time.Hour })
	one := readertest.NewFixture(t, data, "one")
	one.Write("a", "1\n")
	c1 := one.Commit("one")
	res, err := a.ResolveRevision(ctx, one.Name, domain.Revision{})
	require.NoError(t, err)
	require.Equal(t, c1, res.CommitID.String())

	two := readertest.NewFixture(t, t.TempDir(), "two")
	two.Write("b", "2\n")
	c2 := two.Commit("two")
	require.NoError(t, os.RemoveAll(one.Bare))
	require.NoError(t, os.Rename(two.Bare, one.Bare))
	res, err = a.ResolveRevision(ctx, one.Name, domain.Revision{})
	require.NoError(t, err)
	assert.Equal(t, c2, res.CommitID.String())
}
