// SPDX-License-Identifier: AGPL-3.0-or-later

package readertest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

// Case is a repository the readers are compared on.
type Case struct {
	Name  string
	Build func(t *testing.T, data string) *Fixture
}

// Maintain runs what ermite runs after a push: a split commit-graph with
// changed-path Bloom filters.
func (f *Fixture) Maintain() {
	f.T.Helper()
	f.Git("commit-graph", "write", "--reachable", "--changed-paths", "--split")
}

// Cases are the fixtures of the comparison: everything the gitcli tests use,
// plus merges, tags, deep paths, a large blob, an unborn HEAD, repositories
// with and without commit-graph (Bloom filters v1 and v2), and one whose
// graph is stale because commits were pushed after the last maintenance.
func Cases() []Case {
	return []Case{
		{"history", func(t *testing.T, data string) *Fixture { return NewHistory(t, data).Fixture }},
		{"history with commit-graph v2", func(t *testing.T, data string) *Fixture {
			h := NewHistory(t, data)
			h.Git("config", "commitGraph.changedPathsVersion", "2")
			h.Maintain()
			return h.Fixture
		}},
		{"history with commit-graph v1", func(t *testing.T, data string) *Fixture {
			h := NewHistory(t, data)
			h.Git("config", "commitGraph.changedPathsVersion", "1")
			h.Maintain()
			return h.Fixture
		}},
		{"history packed", func(t *testing.T, data string) *Fixture {
			h := NewHistory(t, data)
			h.Git("repack", "-adf", "--depth=50", "--window=250")
			h.Git("pack-refs", "--all")
			h.Maintain()
			return h.Fixture
		}},
		{"unborn HEAD", func(t *testing.T, data string) *Fixture { return NewFixture(t, data, "empty") }},
		{"stale commit-graph", buildStale},
		{"merges", buildMerges},
		{"deep paths", buildDeep},
		{"large blobs", buildLarge},
		{"names and messages", buildNames},
		{"refs", buildRefs},
		{"dangling ref", buildDangling},
		{"submodule", buildSubmodule},
		{"paths", buildPaths},
		{"wide directory", buildWide},
		{"detached HEAD", func(t *testing.T, data string) *Fixture {
			h := NewHistory(t, data)
			h.Git("update-ref", "--no-deref", "HEAD", h.C2)
			return h.Fixture
		}},
	}
}

// buildStale writes the graph, then adds commits and refs that it does not
// know: a push followed by maintenance that has not run yet.
func buildStale(t *testing.T, data string) *Fixture {
	f := NewFixture(t, data, "stale")
	f.Git("config", "commitGraph.changedPathsVersion", "2")
	for i := range 12 {
		f.Write(fmt.Sprintf("dir%d/file.txt", i%3), fmt.Sprintf("v%d\n", i)).Write("root.txt", fmt.Sprintf("r%d\n", i/4))
		f.Commit(fmt.Sprintf("commit %d", i))
	}
	f.Maintain()
	f.Git("config", "commitGraph.changedPathsVersion", "1")
	f.CheckoutNew("late")
	for i := range 4 {
		f.Write("dir1/file.txt", fmt.Sprintf("late%d\n", i)).Write(fmt.Sprintf("late%d.txt", i), "x\n")
		f.Commit(fmt.Sprintf("late %d", i))
	}
	f.Checkout("main")
	f.Write("dir0/new.txt", "n\n")
	f.Commit("after maintenance")
	f.Merge("late")
	return f
}

// buildMerges has criss-cross merges, an octopus merge, a merge that takes
// one side of a file and one that is TREESAME to a parent, so that the path
// history simplification matters.
func buildMerges(t *testing.T, data string) *Fixture {
	f := NewFixture(t, data, "merges")
	f.Write("shared.txt", "base\n").Write("a/one.txt", "1\n").Write("b/one.txt", "1\n")
	f.Commit("base")
	f.CheckoutNew("left")
	f.Write("a/one.txt", "left\n")
	f.Commit("left a")
	f.Checkout("main")
	f.CheckoutNew("right")
	f.Write("b/one.txt", "right\n").Write("shared.txt", "right\n")
	f.Commit("right b")
	f.Checkout("main")
	f.Write("main.txt", "m\n")
	f.Commit("main")
	f.Merge("left")
	f.Merge("right")
	f.CheckoutNew("x1")
	f.Write("x.txt", "x1\n")
	f.Commit("x1")
	f.Checkout("main")
	f.CheckoutNew("x2")
	f.Write("y.txt", "y\n")
	f.Commit("x2")
	f.Checkout("main")
	f.Write("main.txt", "m2\n")
	f.Commit("main again")
	f.When = f.When.Add(time.Minute)
	f.Git("merge", "--quiet", "--no-ff", "-m", "octopus", "x1", "x2")
	// A merge whose result is the same as its first parent for shared.txt.
	f.CheckoutNew("same")
	f.Write("shared.txt", "same\n")
	f.Commit("same 1")
	f.Checkout("main")
	f.Git("merge", "--quiet", "--no-ff", "-s", "ours", "-m", "ours merge", "same")
	f.Write("tail.txt", "t\n")
	f.Commit("tail")
	f.Maintain()
	return f
}

func buildDeep(t *testing.T, data string) *Fixture {
	f := NewFixture(t, data, "deep")
	deep := strings.Repeat("d/", 30) + "leaf"
	f.Write(deep+"/file.txt", "deep\n").Write("top.txt", "top\n")
	f.Commit("deep")
	f.Write(deep+"/file.txt", "deeper\n")
	f.Commit("change deep")
	f.Write("top.txt", "top2\n")
	f.Commit("change top")
	f.Write("héllo/ünï.txt", "utf8\n").Write("sp ace/a b.txt", "sp\n")
	f.Commit("odd names")
	f.Maintain()
	return f
}

// buildLarge has blobs on both sides of the in-memory threshold of gitcli
// (256 KiB), loose, packed as is and packed as deltas.
func buildLarge(t *testing.T, data string) *Fixture {
	f := NewFixture(t, data, "large")
	big := RandomBytes(12<<20, 9)
	f.WriteBytes("big.bin", big).WriteBytes("mid.bin", RandomBytes(300<<10, 2)).
		Write("small.txt", "s\n")
	var text strings.Builder
	for i := range 20000 {
		fmt.Fprintf(&text, "line %d of a text that deltifies well\n", i)
	}
	f.Write("text.txt", text.String())
	f.Commit("big")
	f.Write("text.txt", text.String()+"one more line\n")
	f.Commit("big text, second version")
	f.Git("repack", "-adf", "--depth=50", "--window=250")
	f.Write("loose.bin", string(RandomBytes(400<<10, 4)))
	f.Commit("loose")
	f.Maintain()
	return f
}

// buildNames has messages and names git must convert or keep: no message,
// non-UTF-8, an encoding header, trailing blank lines, long summaries, CRLF.
func buildNames(t *testing.T, data string) *Fixture {
	f := NewFixture(t, data, "names")
	f.Write("a.txt", "a\n")
	f.When = f.When.Add(time.Minute)
	f.Git("add", "-A")
	f.Git("commit", "--quiet", "--allow-empty-message", "-m", "")
	f.Write("a.txt", "b\n")
	f.Commit("subject\n\n\n\nbody\n\n\n")
	f.Write("a.txt", "c\n")
	f.Commit("caf\xe9 latin-1 message")
	f.Write("a.txt", "d\n")
	f.Commit("crlf\r\nsecond\r\n")
	f.Write("a.txt", "e\n")
	f.Git("-c", "i18n.commitEncoding=ISO-8859-1", "add", "-A")
	f.When = f.When.Add(time.Minute)
	f.Git("-c", "i18n.commitEncoding=ISO-8859-1", "commit", "--quiet", "-m", "enc: caf\xe9")
	f.Write("a.txt", "f\n")
	f.When = f.When.Add(time.Minute)
	f.Git("add", "-A")
	f.Git("-c", "user.name=Zoë <Ünï>", "commit", "--quiet", "--author=Zoë Ünï <zoe@example.org>", "-m", strings.Repeat("long ", 400))
	f.Write("héllo wörld.txt", "x\n").Write("tab\tname.txt", "t\n").Write("-dash", "d\n")
	f.Commit("names")
	// Git trims the trailing whitespace of a name when it writes a commit but
	// shows one that has some as it is read: the readers must agree on it.
	head, tree := f.Git("rev-parse", "HEAD"), f.Git("rev-parse", "HEAD^{tree}")
	f.Git("update-ref", "refs/heads/main", f.WriteObject("commit", "tree "+tree+"\nparent "+head+"\n"+
		"author Ada\t <a@b> 1767268860 +0000\ncommitter Grace \t\r <g@b> 1767268860 +0000\n\nraw names\n"))
	f.Maintain()
	return f
}

// buildRefs has a branch and a tag with the same name, a branch named like a
// full commit id, a tag named like an abbreviation, nested tags, tags of
// non-commits, packed and loose refs, and refs outside heads and tags.
func buildRefs(t *testing.T, data string) *Fixture {
	f := NewFixture(t, data, "refs")
	f.Write("a.txt", "1\n")
	c1 := f.Commit("one")
	f.Write("a.txt", "2\n")
	c2 := f.Commit("two")
	f.Write("a.txt", "3\n")
	c3 := f.Commit("three")
	f.Git("branch", "same", c1)
	f.Git("tag", "same", c2)
	f.Git("tag", "-a", "-m", "annotated", "ann", c2)
	f.Git("tag", "-a", "-m", "nested", "ann2", "ann")
	f.Git("tag", "-a", "-m", "nested 3", "ann3", "ann2")
	f.Git("tag", "-a", "-m", "tree", "tree-tag", c3+"^{tree}")
	f.Git("tag", "-a", "-m", "blob", "blob-tag", c3+":a.txt")
	f.Git("-c", "advice.objectNameWarning=false", "branch", c1, c3)
	f.Git("-c", "advice.objectNameWarning=false", "tag", c2, c3)
	ghost := strings.Repeat("cd", 20)
	f.Git("-c", "advice.objectNameWarning=false", "branch", ghost, c3)
	f.Git("-c", "advice.objectNameWarning=false", "tag", strings.Repeat("ef", 20), c1)
	f.Git("branch", "dir/sub/branch", c2)
	f.Git("update-ref", "refs/remotes/origin/main", c1)
	f.Git("update-ref", "refs/notes/x", c1)
	f.Git("pack-refs", "--all")
	f.Git("branch", "loose-after-pack", c3)
	f.Git("tag", "loose-tag", c1)
	f.Git("branch", "-f", "same", c3) // loose overrides the packed one
	return f
}

// buildDangling has a loose branch and a loose tag whose objects do not exist,
// next to sound refs. git lists them, and fails when it must follow them.
func buildDangling(t *testing.T, data string) *Fixture {
	f := NewFixture(t, data, "dangling")
	f.Write("a.txt", "1\n")
	f.Commit("one")
	ghost := strings.Repeat("0123456789", 4)
	for _, ref := range []string{"refs/heads/ghost", "refs/tags/ghost"} {
		require.NoError(t, os.WriteFile(filepath.Join(f.Bare, filepath.FromSlash(ref)), []byte(ghost+"\n"), 0o644))
	}
	return f
}

// buildSubmodule has a gitlink entry, a symlink and an executable.
func buildSubmodule(t *testing.T, data string) *Fixture {
	f := NewFixture(t, data, "submod")
	f.Write("a.txt", "a\n").Write("run.sh", "#!/bin/sh\n").Chmod("run.sh", 0o755).Symlink("a.txt", "link")
	f.Commit("base")
	f.Git("update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("ab", 20)+",vendor/mod")
	f.Git("commit", "--quiet", "-m", "add submodule")
	f.Maintain()
	return f
}

// buildPaths has what ListPaths must order and skip: nested directories whose
// names sort around "/" (a/b/c.txt, a/b.txt, a-b.txt), an executable, a
// symlink, a submodule, and a name with a tab that no address can carry. The
// tag v1 points at the commit.
func buildPaths(t *testing.T, data string) *Fixture {
	f := NewFixture(t, data, "paths")
	f.Write("a/b/c.txt", "c\n").Write("a/b.txt", "b\n").Write("a-b.txt", "ab\n").Write("z.txt", "z\n")
	f.Write("run.sh", "#!/bin/sh\n").Chmod("run.sh", 0o755).Symlink("z.txt", "link")
	f.Write("tab\tname.txt", "t\n").Write("d/e/f/deep.txt", "d\n").Write("héllo wörld.txt", "h\n")
	f.Commit("base")
	f.Git("update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("ab", 20)+",vendor/mod")
	f.Git("commit", "--quiet", "-m", "add submodule")
	f.Git("tag", "v1")
	f.Maintain()
	return f
}

// buildWide has a directory over MaxTreeEntries entries and one over
// MaxLastCommitEntries, with commits that touch some of them.
func buildWide(t *testing.T, data string) *Fixture {
	f := NewFixture(t, data, "wide")
	for i := range domain.MaxTreeEntries + 7 {
		f.Write(fmt.Sprintf("d/f%05d", i), "x")
	}
	for i := range domain.MaxLastCommitEntries + 20 {
		f.Write(fmt.Sprintf("m/e%03d", i), "x\n")
		if i%7 == 0 {
			f.Write(fmt.Sprintf("m/sub%03d/x", i), "x\n")
		}
	}
	f.Commit("wide")
	f.Write("m/e003", "changed\n").Write("m/e250", "changed\n").Write("d/f00010", "y")
	f.Commit("touch some")
	f.Maintain()
	return f
}
