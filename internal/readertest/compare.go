// SPDX-License-Identifier: AGPL-3.0-or-later

package readertest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Budget bounds how many revisions and paths Compare asks about, so that it
// stays fast on a large repository.
type Budget struct {
	Revs      int // revisions (refs and commits) for trees and blobs
	LogRevs   int // revisions for history pages
	Dirs      int // directories per revision
	Files     int // files per revision
	LogPaths  int // paths for path-limited history
	LastTrees int // trees listed with last commits
}

// SmallBudget suits the fixtures.
var SmallBudget = Budget{Revs: 24, LogRevs: 8, Dirs: 10, Files: 10, LogPaths: 6, LastTrees: 6}

// LargeBudget suits a repository of thousands of commits.
var LargeBudget = Budget{Revs: 4, LogRevs: 2, Dirs: 6, Files: 8, LogPaths: 5, LastTrees: 3}

// errClass names the kind of an error of the port, so that two readers agree
// on what went wrong without agreeing on the message.
func errClass(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, domain.ErrNotFound):
		return "not found"
	case errors.Is(err, domain.ErrNotFile):
		return "not a file"
	case errors.Is(err, domain.ErrNotDirectory):
		return "not a directory"
	case errors.Is(err, domain.ErrInvalidPageSize):
		return "invalid page size"
	case errors.Is(err, domain.ErrInvalidPath):
		return "invalid path"
	case errors.Is(err, domain.ErrInvalidObjectID):
		return "invalid object id"
	case errors.Is(err, domain.ErrInvalidRevision):
		return "invalid revision"
	case errors.Is(err, domain.ErrInvalidPageToken):
		return "invalid page token"
	case errors.Is(err, domain.ErrInvalidRefKind):
		return "invalid ref kind"
	case errors.Is(err, domain.ErrInvalid):
		return "invalid argument"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "other" // outside the domain errors: the messages of two readers differ
}

// Queries are the questions put to both readers about one repository.
type Queries struct {
	Revs     []string // revisions as a client writes them
	LogRevs  []string
	Dirs     []string
	Files    []string
	LogPaths []string
	RefPaths []string
}

// GitOut runs git in dir and returns its trimmed output.
func GitOut(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"--git-dir=" + dir}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "LC_ALL=C"}
	out, err := cmd.Output()
	require.NoError(t, err, "git %s", strings.Join(args, " "))
	return strings.TrimRight(string(out), "\n")
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// spread picks at most n elements of s, evenly, always the first.
func spread[T any](s []T, n int) []T {
	if len(s) <= n || n <= 0 {
		return s
	}
	out := make([]T, 0, n)
	for i := range n {
		out = append(out, s[i*len(s)/n])
	}
	return out
}

// Derive builds the questions for the repository in dir with git.
func Derive(t testing.TB, dir string, b Budget) Queries {
	t.Helper()
	var q Queries
	revs := []string{"", "HEAD", "nope", "deadbeefdeadbeef", strings.Repeat("ab", 20), "refs/heads", "refs/nope/x"}
	for _, l := range lines(GitOut(t, dir, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/tags")) {
		revs = append(revs, l)
		if short, ok := strings.CutPrefix(l, "refs/heads/"); ok {
			revs = append(revs, short)
		} else if short, ok := strings.CutPrefix(l, "refs/tags/"); ok {
			revs = append(revs, short)
		}
	}
	for _, l := range lines(GitOut(t, dir, "for-each-ref", "--format=%(objectname)", "refs/heads", "refs/tags")) {
		revs = append(revs, l, l[:7], l[:12]) // tag objects too: they are not commits when they lead elsewhere
	}
	var commits []string
	{
		// An empty repository has no commit: rev-list --all prints nothing.
		cmd := exec.Command("git", "--git-dir="+dir, "rev-list", "--all", "--missing=allow-any")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		out, err := cmd.Output()
		require.NoError(t, err)
		commits = lines(strings.TrimRight(string(out), "\n"))
	}
	for _, c := range spread(commits, b.Revs) {
		revs = append(revs, c, c[:7], c[:10])
	}
	if len(commits) > 0 {
		tree := GitOut(t, dir, "rev-parse", commits[0]+"^{tree}")
		revs = append(revs, tree, tree[:8]) // a tree is not a commit
	}
	q.Revs = dedupe(revs)
	var logRevs []string
	for _, r := range q.Revs {
		if r == "" || r == "HEAD" || strings.HasPrefix(r, "refs/") || len(r) == 40 {
			logRevs = append(logRevs, r)
		}
	}
	q.LogRevs = spread(logRevs, b.LogRevs+4)

	// Paths of the newest commit of HEAD, or of the first commit.
	if len(commits) > 0 {
		var dirs, files []string
		for _, l := range lines(GitOut(t, dir, "ls-tree", "-r", "-t", "--format=%(objecttype) %(path)", commits[0])) {
			typ, p, _ := strings.Cut(l, " ")
			switch typ {
			case "tree":
				dirs = append(dirs, p)
			case "blob":
				files = append(files, p)
			}
		}
		q.Dirs = append([]string{""}, spread(dirs, b.Dirs)...)
		q.Dirs = append(q.Dirs, "nope", "nope/deeper", "*")
		if len(files) > 0 {
			q.Dirs = append(q.Dirs, files[0]) // a file where a directory is wanted
		}
		q.Files = append(spread(files, b.Files), "", "nope", "nope/x")
		if len(dirs) > 0 {
			q.Files = append(q.Files, dirs[0]) // a directory where a file is wanted
		}
		q.LogPaths = append([]string{""}, spread(files, b.LogPaths/2+1)...)
		q.LogPaths = append(q.LogPaths, spread(dirs, b.LogPaths/2+1)...)
		q.LogPaths = append(q.LogPaths, "nope", "*")
		q.LogPaths = dedupe(q.LogPaths)
	}
	for _, r := range q.Revs {
		if r != "" {
			q.RefPaths = append(q.RefPaths, r, r+"/dir/file", r+"/x")
		}
	}
	q.RefPaths = append(q.RefPaths, "", "/", "nope", "nope/x", "a//b", "../x", "main/", "HEAD/x", "refs/heads/main/x")
	q.RefPaths = dedupe(spread(q.RefPaths, 4*b.Revs+12))
	return q
}

func dedupe(s []string) []string {
	seen := make(map[string]bool, len(s))
	out := s[:0:0]
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// Compare asks both readers the same questions about the repository and
// requires the same answers: the same values, and the same kind of error. want
// is the oracle (the CLI reader).
func Compare(t *testing.T, want, got ports.RepoReader, repo domain.RepoName, dir string, b Budget) {
	t.Helper()
	ctx := context.Background()
	q := Derive(t, dir, b)
	t.Logf("%d revisions, %d dirs, %d files, %d log paths", len(q.Revs), len(q.Dirs), len(q.Files), len(q.LogPaths))

	t.Run("ListRefs", func(t *testing.T) {
		w, werr := want.ListRefs(ctx, repo)
		g, gerr := got.ListRefs(ctx, repo)
		require.Equal(t, errClass(werr), errClass(gerr))
		assert.Equal(t, w, g)
	})
	t.Run("ResolveRevision", func(t *testing.T) {
		for _, s := range q.Revs {
			rev, err := domain.ParseRevision(s)
			if err != nil {
				continue
			}
			w, werr := want.ResolveRevision(ctx, repo, rev)
			g, gerr := got.ResolveRevision(ctx, repo, rev)
			if assert.Equal(t, errClass(werr), errClass(gerr), "revision %q", s) {
				assert.Equal(t, w, g, "revision %q", s)
			}
		}
	})
	t.Run("ResolveRefPath", func(t *testing.T) {
		for _, p := range q.RefPaths {
			w, werr := want.ResolveRefPath(ctx, repo, p)
			g, gerr := got.ResolveRefPath(ctx, repo, p)
			if assert.Equal(t, errClass(werr), errClass(gerr), "path %q", p) {
				assert.Equal(t, w, g, "path %q", p)
			}
		}
	})
	revs := make([]domain.Revision, 0, len(q.Revs))
	for _, s := range spread(q.Revs, b.Revs+8) {
		if rev, err := domain.ParseRevision(s); err == nil {
			revs = append(revs, rev)
		}
	}
	t.Run("GetTree", func(t *testing.T) {
		last := 0
		for _, rev := range revs {
			for _, p := range q.Dirs {
				path, err := domain.ParseTreePath(p)
				if err != nil {
					continue
				}
				w, werr := want.GetTree(ctx, repo, rev, path, false)
				g, gerr := got.GetTree(ctx, repo, rev, path, false)
				if assert.Equal(t, errClass(werr), errClass(gerr), "tree %q at %q", p, rev) {
					assert.Equal(t, w, g, "tree %q at %q", p, rev)
				}
				// Last commits for the first few revisions only: each is a history walk.
				if werr == nil && last < b.LastTrees*3 && len(w.Entries) > 0 {
					last++
					w, werr := want.GetTree(ctx, repo, rev, path, true)
					g, gerr := got.GetTree(ctx, repo, rev, path, true)
					if assert.Equal(t, errClass(werr), errClass(gerr), "tree+last %q at %q", p, rev) {
						assert.Equal(t, w, g, "tree+last %q at %q", p, rev)
					}
				}
			}
		}
	})
	t.Run("OpenBlob", func(t *testing.T) {
		read := func(r ports.RepoReader, rev domain.Revision, path domain.TreePath) (domain.BlobInfo, []byte, error) {
			info, rc, err := r.OpenBlob(ctx, repo, rev, path)
			if err != nil {
				require.Nil(t, rc)
				return info, nil, err
			}
			body, rerr := io.ReadAll(rc)
			require.NoError(t, rerr)
			require.NoError(t, rc.Close())
			return info, body, nil
		}
		for _, rev := range spread(revs, b.Revs/2+4) {
			for _, p := range q.Files {
				path, err := domain.ParseTreePath(p)
				if err != nil {
					continue
				}
				wi, wb, werr := read(want, rev, path)
				gi, gb, gerr := read(got, rev, path)
				if assert.Equal(t, errClass(werr), errClass(gerr), "blob %q at %q", p, rev) && werr == nil {
					assert.Equal(t, wi, gi, "blob %q at %q", p, rev)
					assert.True(t, bytes.Equal(wb, gb), "content of %q at %q differs", p, rev)
				}
			}
		}
	})
	t.Run("ListCommits", func(t *testing.T) {
		pages := [...][2]int{{0, 1000}, {0, 3}, {2, 3}, {5, 2}, {40, 7}, {5000, 20}}
		ids := func(p ports.CommitPage) string {
			var sb strings.Builder
			for _, c := range p.Commits {
				sb.WriteString(c.ID.String()[:8] + " ")
			}
			return sb.String()
		}
		for _, s := range q.LogRevs {
			rev, err := domain.ParseRevision(s)
			if err != nil {
				continue
			}
			for _, p := range q.LogPaths {
				path, err := domain.ParseTreePath(p)
				if err != nil {
					continue
				}
				for _, pg := range pages {
					cq := ports.CommitQuery{Revision: rev, Path: path, Skip: pg[0], Limit: pg[1]}
					w, werr := want.ListCommits(ctx, repo, cq)
					g, gerr := got.ListCommits(ctx, repo, cq)
					if assert.Equal(t, errClass(werr), errClass(gerr), "log %q -- %q %v", s, p, pg) {
						if !assert.Equal(t, w, g, "log %q -- %q %v\nwant %s\n got %s", s, p, pg, ids(w), ids(g)) {
							return
						}
					}
				}
			}
		}
		for _, cq := range []ports.CommitQuery{{Limit: 0}, {Limit: -1}, {Limit: ports.MaxCommitLimit + 1}, {Limit: 1, Skip: -1}} {
			_, werr := want.ListCommits(ctx, repo, cq)
			_, gerr := got.ListCommits(ctx, repo, cq)
			assert.Equal(t, errClass(werr), errClass(gerr), "%+v", cq)
		}
	})
	t.Run("missing repository", func(t *testing.T) {
		nope := RepoName(t, "no-such-repo")
		for name, call := range map[string]func(ports.RepoReader) error{
			"ListRefs":        func(r ports.RepoReader) error { _, err := r.ListRefs(ctx, nope); return err },
			"ResolveRevision": func(r ports.RepoReader) error { _, err := r.ResolveRevision(ctx, nope, domain.Revision{}); return err },
			"ResolveRefPath":  func(r ports.RepoReader) error { _, err := r.ResolveRefPath(ctx, nope, "main"); return err },
			"GetTree": func(r ports.RepoReader) error {
				_, err := r.GetTree(ctx, nope, domain.Revision{}, domain.TreePath{}, false)
				return err
			},
			"OpenBlob": func(r ports.RepoReader) error {
				_, _, err := r.OpenBlob(ctx, nope, domain.Revision{}, domain.TreePath{})
				return err
			},
			"ListCommits": func(r ports.RepoReader) error {
				_, err := r.ListCommits(ctx, nope, ports.CommitQuery{Limit: 1})
				return err
			},
		} {
			assert.Equal(t, errClass(call(want)), errClass(call(got)), name)
		}
	})
}
