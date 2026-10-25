// SPDX-License-Identifier: AGPL-3.0-or-later

package ermitegit_test

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/ermitegit"
	"bnema.dev/ermite/internal/adapters/gitcli"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/readertest"
)

// benchTarget is what the benchmarks ask about: a repository and the names in
// it, found with git.
type benchTarget struct {
	name       domain.RepoName
	dir        string
	branch     string // a branch name
	head       string // the full id of HEAD
	abbrev     string // an abbreviated commit id
	refPath    string // "branch/dir/file"
	treeDir    string // a directory with many entries
	file       string // a text file of some KiB
	big        string // the largest blob of HEAD
	history    string // a path with a long history
	deepSkip   int    // a page far down the history
	commitsAll int
}

// newBenchTarget builds the fixture repository, or clones EnvBenchRepo when it
// is set, and looks for the targets with git.
func newBenchTarget(b *testing.B, g *gitcli.Git, data string) benchTarget {
	b.Helper()
	var t benchTarget
	if os.Getenv(EnvBenchRepo) != "" {
		r := cloneBench(b, g, data)
		t.name, t.dir = r.name, r.dir
	} else {
		f := benchFixture(b, data)
		t.name, t.dir = f.Name, f.Bare
		f.Maintain()
	}
	ageFiles(b, t.dir)
	git := func(args ...string) string { return gitOutB(b, t.dir, args...) }
	t.branch = git("symbolic-ref", "--short", "HEAD")
	head := git("rev-parse", "HEAD")
	t.head, t.abbrev = head, head[:10]
	t.commitsAll, _ = strconv.Atoi(git("rev-list", "--count", "HEAD"))
	t.deepSkip = min(5000, t.commitsAll*2/3)

	var bigSize int64
	dirCount := map[string]int{}
	var files []string
	for _, l := range strings.Split(git("ls-tree", "-r", "-l", "HEAD"), "\n") {
		meta, p, ok := strings.Cut(l, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 4 || f[1] != "blob" {
			continue
		}
		size, _ := strconv.ParseInt(f[3], 10, 64)
		if size > bigSize {
			bigSize, t.big = size, p
		}
		if i := strings.LastIndexByte(p, '/'); i >= 0 {
			dirCount[p[:i]]++
		}
		if size >= 2<<10 && size <= 100<<10 && strings.Count(p, "/") <= 2 && !strings.ContainsAny(p, " *") {
			files = append(files, p)
		}
	}
	for d, n := range dirCount {
		if n > dirCount[t.treeDir] || (n == dirCount[t.treeDir] && d < t.treeDir) {
			t.treeDir = d
		}
	}
	require.NotEmpty(b, files, "no text file to read")
	t.file = files[len(files)/2]
	t.history = t.file
	t.refPath = t.branch + "/" + t.file
	return t
}

// BenchmarkRepoReader compares the CLI reader and the ermite-git adapter,
// method by method, on a generated repository or, when ERMITE_GIT_BENCH_REPO
// is set, on a clone of that repository:
//
//	go test -run '^$' -bench RepoReader -benchmem ./internal/adapters/ermitegit
//	ERMITE_GIT_BENCH_REPO=/path/to/repo/.git go test -run '^$' -bench RepoReader -benchmem ...
func BenchmarkRepoReader(b *testing.B) {
	a, g, data := newAdapter(b, ermitegit.AllRoutes(ermitegit.ErmiteGit), nil)
	t := newBenchTarget(b, g, data)
	b.Logf("repository: %d commits, branch %s, dir %s, file %s, big %s, deep skip %d", t.commitsAll, t.branch, t.treeDir, t.file, t.big, t.deepSkip)
	ctx := context.Background()
	rev := func(s string) domain.Revision {
		r, err := domain.ParseRevision(s)
		require.NoError(b, err)
		return r
	}
	path := func(s string) domain.TreePath {
		p, err := domain.ParseTreePath(s)
		require.NoError(b, err)
		return p
	}
	drain := func(r ports.RepoReader, v domain.Revision, p domain.TreePath) error {
		_, rc, err := r.OpenBlob(ctx, t.name, v, p)
		if err != nil {
			return err
		}
		_, err = io.Copy(io.Discard, rc)
		_ = rc.Close()
		return err
	}
	// ermite-git re-reads a commit-graph it loaded less than two seconds ago on
	// every Refresh (see ageFiles). Open the repository and let that window
	// pass, as a long-lived server would have.
	require.NoError(b, drain(a, domain.Revision{}, path(t.file)))
	time.Sleep(2100 * time.Millisecond)
	headID, err := domain.ParseObjectID(t.head)
	require.NoError(b, err)
	type method struct {
		name string
		run  func(r ports.RepoReader) error
	}
	methods := []method{
		{"ListRefs", func(r ports.RepoReader) (err error) { _, err = r.ListRefs(ctx, t.name); return }},
		{"ResolveRevision/branch", func(r ports.RepoReader) (err error) {
			_, err = r.ResolveRevision(ctx, t.name, rev(t.branch))
			return
		}},
		{"ResolveRevision/fullid", func(r ports.RepoReader) (err error) {
			_, err = r.ResolveRevision(ctx, t.name, rev(t.head))
			return
		}},
		{"ResolveRevision/abbrev", func(r ports.RepoReader) (err error) {
			_, err = r.ResolveRevision(ctx, t.name, rev(t.abbrev))
			return
		}},
		{"ResolveRefPath", func(r ports.RepoReader) (err error) { _, err = r.ResolveRefPath(ctx, t.name, t.refPath); return }},
		{"GetTree/root", func(r ports.RepoReader) (err error) {
			_, err = r.GetTree(ctx, t.name, domain.Revision{}, domain.TreePath{}, false)
			return
		}},
		{"GetTree/dir", func(r ports.RepoReader) (err error) {
			_, err = r.GetTree(ctx, t.name, domain.Revision{}, path(t.treeDir), false)
			return
		}},
		{"GetTree/root+last", func(r ports.RepoReader) (err error) {
			_, err = r.GetTree(ctx, t.name, domain.Revision{}, domain.TreePath{}, true)
			return
		}},
		{"GetTree/dir+last", func(r ports.RepoReader) (err error) {
			_, err = r.GetTree(ctx, t.name, domain.Revision{}, path(t.treeDir), true)
			return
		}},
		{"ListPaths", func(r ports.RepoReader) (err error) {
			_, err = r.ListPaths(ctx, t.name, domain.Revision{}, domain.MaxPathEntries, domain.MaxPathBytes)
			return
		}},
		{"OpenBlob/small", func(r ports.RepoReader) error { return drain(r, domain.Revision{}, path(t.file)) }},
		{"OpenBlob/largest", func(r ports.RepoReader) error { return drain(r, domain.Revision{}, path(t.big)) }},
		{"ListCommits/page1", func(r ports.RepoReader) (err error) {
			_, err = r.ListCommits(ctx, t.name, ports.CommitQuery{Limit: 30})
			return
		}},
		{"ListCommits/deep", func(r ports.RepoReader) (err error) {
			_, err = r.ListCommits(ctx, t.name, ports.CommitQuery{Skip: t.deepSkip, Limit: 30})
			return
		}},
		{"ListCommits/path", func(r ports.RepoReader) (err error) {
			_, err = r.ListCommits(ctx, t.name, ports.CommitQuery{Path: path(t.history), Limit: 30})
			return
		}},
		{"ListCommits/dirpath", func(r ports.RepoReader) (err error) {
			_, err = r.ListCommits(ctx, t.name, ports.CommitQuery{Path: path(t.treeDir), Limit: 30})
			return
		}},
		{"TagDistance", func(r ports.RepoReader) (err error) {
			_, _, err = r.TagDistance(ctx, t.name, headID, domain.MaxTagDistance)
			return
		}},
	}
	// Each method runs three ways, for both readers: warm (the repository is
	// open, one caller), cold (the repository is closed before every call: the
	// price of the first read after the idle TTL, an eviction or a restart) and
	// parallel (b.RunParallel: the contention of the cache and of the library).
	// The CLI reader keeps no repository open, so its cold run is its warm run.
	for _, m := range methods {
		b.Run(m.name, func(b *testing.B) {
			for _, ad := range []struct {
				name string
				r    ports.RepoReader
				cold func() // closes what the reader holds open
			}{
				{"cli", g, func() {}},
				{"ermitegit", a, func() { a.Forget(t.dir) }},
			} {
				b.Run(ad.name, func(b *testing.B) {
					require.NoError(b, m.run(ad.r)) // warm-up: processes, caches, graph
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						if err := m.run(ad.r); err != nil {
							b.Fatal(err)
						}
					}
				})
				b.Run(ad.name+"_cold", func(b *testing.B) {
					require.NoError(b, m.run(ad.r))
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						ad.cold()
						if err := m.run(ad.r); err != nil {
							b.Fatal(err)
						}
					}
				})
				b.Run(ad.name+"_parallel", func(b *testing.B) {
					require.NoError(b, m.run(ad.r))
					b.ReportAllocs()
					b.ResetTimer()
					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							if err := m.run(ad.r); err != nil {
								b.Error(err)
								return
							}
						}
					})
				})
			}
		})
	}
}

// benchFixture builds the generated repository: a few hundred commits over a
// tree of directories, merges, tags, and a 10 MiB blob.
func benchFixture(b *testing.B, data string) *readertest.Fixture {
	b.Helper()
	f := readertest.NewFixture(b, data, "bench")
	f.Git("config", "commitGraph.changedPathsVersion", "2")
	f.Git("config", "core.compression", "1")
	for i := range 200 {
		for j := range 3 {
			f.Write(fmt.Sprintf("pkg%d/file%02d.go", (i+j)%8, (i*3+j)%40), strings.Repeat(fmt.Sprintf("// version %d of this file\n", i), 160))
		}
		f.Write("README.md", fmt.Sprintf("# project\n\nrevision %d\n", i))
		f.Commit(fmt.Sprintf("change %d\n\nbody of the message", i))
		if i%25 == 24 {
			f.CheckoutNew(fmt.Sprintf("topic%d", i))
			f.Write(fmt.Sprintf("topics/topic%d.txt", i), "t\n")
			f.Commit("topic work")
			f.Checkout("main")
			f.Write("pkg0/main-work.go", fmt.Sprintf("// main work %d\n", i))
			f.Commit("main work")
			f.Merge(fmt.Sprintf("topic%d", i))
			f.Git("tag", "-a", "-m", "release", fmt.Sprintf("v%d", i/25))
		}
	}
	f.WriteBytes("assets/big.bin", readertest.RandomBytes(10<<20, 5))
	f.Commit("add a 10 MiB asset")
	f.Git("gc", "--quiet")
	return f
}

// ageFiles sets the modification time of every file of the repository to an
// hour ago. ermite-git re-reads the files modified in the last two seconds (the
// racy window of git's index rule): a repository that was just built would
// measure that path. A repository that serves reads is not in that state, its
// last push is older.
func ageFiles(b *testing.B, dir string) {
	b.Helper()
	old := time.Now().Add(-time.Hour)
	require.NoError(b, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(p, old, old)
	}))
}

func gitOutB(b *testing.B, dir string, args ...string) string {
	b.Helper()
	return readertest.GitOut(b, dir, args...)
}
