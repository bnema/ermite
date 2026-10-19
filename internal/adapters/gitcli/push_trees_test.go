// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/gitcli"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/readertest"
)

func TestChangedWorkflow(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	f := readertest.NewFixture(t, data, "proj")
	t.Cleanup(func() { _ = g.Close(ctx) })
	f.Write("README", "r\n")
	base := f.Commit("base")
	f.Write(".github/workflows/ci.yml", "on: push\n")
	withWF := f.Commit("add workflow")
	f.Write("docs/x.md", "x\n")
	docs := f.Commit("docs")
	f.Write(".github/workflows/ci.yml", "on: [push]\n")
	edited := f.Commit("edit workflow")
	f.Write(".github/workflows/other.yml", "on: push\n")
	two := f.Commit("a second workflow")
	f.Remove(".github/workflows/ci.yml")
	oneLeft := f.Commit("remove one of two")
	f.Remove(".github/workflows/other.yml")
	removed := f.Commit("remove the last workflow")
	f.Write(".github/workflows-notes/a.yml", "x\n")
	lookalike := f.Commit("a look-alike directory")
	oid := func(s string) string { return s[:40] }

	up := func(old, new string) domain.RefUpdate {
		return domain.RefUpdate{Old: old, New: new, Ref: "refs/heads/x"}
	}
	changed := func(ups ...domain.RefUpdate) string {
		p, err := changedWorkflow(g, ctx, f.Name, ups, ports.PushObjects{})
		require.NoError(t, err)
		return p
	}

	assert.Equal(t, ".github/workflows/ci.yml", changed(up(oid(base), oid(withWF))), "a file created")
	assert.Equal(t, "", changed(up(oid(withWF), oid(docs))), "another file changed")
	assert.Equal(t, ".github/workflows/ci.yml", changed(up(oid(docs), oid(edited))), "a file modified")
	assert.Equal(t, ".github/workflows/ci.yml", changed(up(oid(two), oid(oneLeft))), "a file removed from a directory that stays is a change too")
	assert.Equal(t, "", changed(up(oid(oneLeft), oid(removed))), "the directory removed with its last file is allowed")
	assert.Equal(t, "", changed(up(oid(removed), oid(lookalike))), "only .github/workflows/ counts")
	assert.Equal(t, ".github/workflows/ci.yml", changed(up(domain.ZeroOID, oid(withWF))), "a new ref carries the workflows of its commit")
	assert.Equal(t, "", changed(up(domain.ZeroOID, oid(base))), "a new ref without workflows")
	assert.Equal(t, "", changed(up(oid(withWF), domain.ZeroOID)), "a deleted ref changes nothing")
	assert.Equal(t, ".github/workflows/ci.yml", changed(up(oid(base), oid(docs)), up(oid(docs), oid(edited))), "the first ref that changes one")
	assert.Equal(t, "", changed(), "no update")

	_, err := changedWorkflow(g, ctx, f.Name, []domain.RefUpdate{up(oid(base), "1111111111111111111111111111111111111111")}, ports.PushObjects{})
	assert.Error(t, err, "an object git does not have is a failure, not a pass")
}

func TestPushSubtreesReadsTheQuarantineOfThePushOnly(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	f := readertest.NewFixture(t, data, "proj")
	t.Cleanup(func() { _ = g.Close(ctx) })
	f.Write("README", "r\n")
	f.Commit("base")
	objects := filepath.Join(f.Bare, "objects")
	up := []domain.RefUpdate{{Old: domain.ZeroOID, New: "1111111111111111111111111111111111111111", Ref: "refs/heads/x"}}

	for name, o := range map[string]ports.PushObjects{
		"outside the repository":   {Dir: filepath.Join(data, "elsewhere")},
		"a relative path":          {Dir: "objects/incoming"},
		"parent of the objects":    {Dir: f.Bare},
		"a dotdot out of objects":  {Dir: objects + "/../../other.git/objects"},
		"an alternate outside":     {Dir: objects + "/incoming-1", Alternates: "/tmp"},
		"alternates without a dir": {Alternates: objects},
	} {
		_, err := g.PushSubtrees(ctx, f.Name, up, o, domain.WorkflowDir, domain.DefaultPushWalkBounds())
		assert.ErrorIs(t, err, domain.ErrInvalid, name)
	}
	// An existing directory inside the repository is accepted as a location
	// (the object is missing: a refusal of the object, not of the path).
	require.NoError(t, os.Mkdir(filepath.Join(objects, "incoming-1"), 0o700))
	_, err := g.PushSubtrees(ctx, f.Name, up, ports.PushObjects{Dir: objects + "/incoming-1", Alternates: objects}, domain.WorkflowDir, domain.DefaultPushWalkBounds())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "outside the repository")
	// One that does not exist is refused.
	_, err = g.PushSubtrees(ctx, f.Name, up, ports.PushObjects{Dir: objects + "/incoming-2"}, domain.WorkflowDir, domain.DefaultPushWalkBounds())
	assert.ErrorIs(t, err, domain.ErrInvalid)
}

// A symbolic link in the way cannot take the objects of a push out of the
// repository, and a data directory reached through links still works.
func TestPushSubtreesResolvesSymbolicLinks(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	f := readertest.NewFixture(t, data, "proj")
	t.Cleanup(func() { _ = g.Close(ctx) })
	f.Write("README", "r\n")
	f.Commit("base")
	up := []domain.RefUpdate{{Old: domain.ZeroOID, New: "1111111111111111111111111111111111111111", Ref: "refs/heads/x"}}
	objects := filepath.Join(f.Bare, "objects")

	// Inside objects/, a link to elsewhere is outside once resolved.
	elsewhere := t.TempDir()
	require.NoError(t, os.Symlink(elsewhere, filepath.Join(objects, "incoming-link")))
	_, err := g.PushSubtrees(ctx, f.Name, up, ports.PushObjects{Dir: filepath.Join(objects, "incoming-link")}, domain.WorkflowDir, domain.DefaultPushWalkBounds())
	assert.ErrorIs(t, err, domain.ErrInvalid, "a link out of objects/")
	_, err = g.PushSubtrees(ctx, f.Name, up, ports.PushObjects{Dir: objects + "/pack", Alternates: filepath.Join(objects, "incoming-link")}, domain.WorkflowDir, domain.DefaultPushWalkBounds())
	assert.ErrorIs(t, err, domain.ErrInvalid, "an alternate that is a link out of objects/")

	// A link to somewhere inside objects/ is fine.
	require.NoError(t, os.Mkdir(filepath.Join(objects, "q1"), 0o700))
	require.NoError(t, os.Symlink(filepath.Join(objects, "q1"), filepath.Join(objects, "q-link")))
	_, err = g.PushSubtrees(ctx, f.Name, up, ports.PushObjects{Dir: filepath.Join(objects, "q-link")}, domain.WorkflowDir, domain.DefaultPushWalkBounds())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "outside the repository")
}

func TestPushSubtreesThroughALinkedDataDirectory(t *testing.T) {
	_, data := newGit(t)
	ctx := context.Background()
	f := readertest.NewFixture(t, data, "proj")
	f.Write("README", "r\n")
	base := f.Commit("base")
	f.Write(".github/workflows/ci.yml", "on: push\n")
	wf := f.Commit("workflow")
	// The server reaches the same data directory through a link, while git
	// (the hook) reports the real path: both must be accepted as inside.
	parent := t.TempDir()
	link := filepath.Join(parent, "data")
	require.NoError(t, os.Symlink(data, link))
	bin, err := exec.LookPath("git")
	require.NoError(t, err)
	g2 := gitcli.New(gitcli.Config{Bin: bin, DataDir: link, Path: os.Getenv("PATH")})
	t.Cleanup(func() { _ = g2.Close(ctx) })
	q := filepath.Join(f.Bare, "objects", "incoming-9")
	require.NoError(t, os.Mkdir(q, 0o700))
	for _, dir := range []string{q, filepath.Join(link, "repos", "proj.git", "objects", "incoming-9")} {
		p, err := g2.PushSubtrees(ctx, f.Name, []domain.RefUpdate{{Old: oidOf(t, base).String(), New: oidOf(t, wf).String(), Ref: "refs/heads/x"}},
			ports.PushObjects{Dir: dir, Alternates: filepath.Join(link, "repos", "proj.git", "objects")}, domain.WorkflowDir, domain.DefaultPushWalkBounds())
		require.NoError(t, err, dir)
		assert.Equal(t, domain.EntryDirectory, p.Subtrees[oidOf(t, wf).String()].Kind, dir)
	}
}

// treeOf writes a tree of the given entries ("mode type sha\tname") and returns it.
func treeOf(t *testing.T, f *readertest.Fixture, entries ...string) string {
	t.Helper()
	cmd := exec.Command("git", "--git-dir="+f.Bare, "mktree")
	cmd.Stdin = strings.NewReader(strings.Join(entries, "\n") + "\n")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

func commitOf(t *testing.T, f *readertest.Fixture, tree string, parents ...string) string {
	t.Helper()
	args := []string{"--git-dir=" + f.Bare, "commit-tree", tree, "-m", "c"}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	cmd := exec.Command("git", args...)
	cmd.Env = []string{"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b"}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

func TestFirstDifferentEntryDoesNotBufferTheDiff(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	f := readertest.NewFixture(t, data, "proj")
	t.Cleanup(func() { _ = g.Close(ctx) })
	blob := f.WriteObject("blob", "x\n")
	level := treeOf(t, f, "100644 blob "+blob+"\tf")
	for range 7 { // ten entries per level, all the same subtree: 10^7 paths
		var entries []string
		for i := range 10 {
			entries = append(entries, fmt.Sprintf("040000 tree %s\td%d", level, i))
		}
		level = treeOf(t, f, entries...)
	}
	amp := level

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	p, err := g.FirstDifferentEntry(ctx, f.Name, ports.PushObjects{}, domain.ObjectID{}, oidOf(t, level))
	took := time.Since(start)
	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(p, "d"), p)
	assert.Less(t, took, 2*time.Second, "answered from the first path")
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(4<<20), "bounded memory")
	// And no git is left behind.
	out, _ := exec.Command("pgrep", "-f", "git --no-replace-objects.*diff-tree.*"+amp).CombinedOutput()
	assert.Empty(t, strings.TrimSpace(string(out)), "git was reaped")
}

// A path longer than the bound fails closed: the push is refused, not read on.
func TestFirstDifferentEntryRefusesAnEndlessPath(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	f := readertest.NewFixture(t, data, "proj")
	t.Cleanup(func() { _ = g.Close(ctx) })
	level := treeOf(t, f, "100644 blob "+f.WriteObject("blob", "x\n")+"\tf")
	level = treeOf(t, f, "040000 tree "+level+"\t"+strings.Repeat("d", 5000)) // an entry name of 5000 bytes
	_, err := g.FirstDifferentEntry(ctx, f.Name, ports.PushObjects{}, domain.ObjectID{}, oidOf(t, level))
	assert.ErrorIs(t, err, domain.ErrInvalid)
}

// A tag may point to a blob, a tree or a commit; only the commits and trees can
// hold a workflow, and an object that is not there refuses the push.
func TestPushSubtreesPeelsAndClassifiesBothEnds(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	f := readertest.NewFixture(t, data, "proj")
	t.Cleanup(func() { _ = g.Close(ctx) })
	f.Write("README", "r\n")
	plain := oidOf(t, f.Commit("plain")).String()
	blob := f.WriteObject("blob", "hello\n")
	tag := func(id, kind string) string {
		return f.WriteObject("tag", "object "+id+"\ntype "+kind+"\ntag t\ntagger a <a@b> 1 +0000\n\nm\n")
	}
	tree := f.Git("rev-parse", plain+"^{tree}")
	for _, r := range []struct {
		id   string
		kind domain.ObjectType
	}{{plain, domain.ObjectCommit}, {tag(plain, "commit"), domain.ObjectCommit}, {blob, domain.ObjectBlob}, {tag(blob, "blob"), domain.ObjectBlob}, {tree, domain.ObjectTree}, {tag(tag(plain, "commit"), "tag"), domain.ObjectCommit}} {
		got, err := g.PushSubtrees(ctx, f.Name, []domain.RefUpdate{{Old: domain.ZeroOID, New: r.id}}, ports.PushObjects{}, domain.WorkflowDir, domain.DefaultPushWalkBounds())
		require.NoError(t, err)
		assert.Equal(t, r.kind, got.Ends[r.id].Type)
		if r.kind == domain.ObjectBlob {
			assert.NotContains(t, got.Subtrees, r.id)
		}
	}
	for _, r := range []domain.RefUpdate{{Old: domain.ZeroOID, New: strings.Repeat("1", 40)}, {Old: strings.Repeat("2", 40), New: plain}, {Old: domain.ZeroOID, New: "ZZZZ"}} {
		_, err := g.PushSubtrees(ctx, f.Name, []domain.RefUpdate{r}, ports.PushObjects{}, domain.WorkflowDir, domain.DefaultPushWalkBounds())
		assert.ErrorIs(t, err, domain.ErrInvalid)
	}
	got, err := g.PushSubtrees(ctx, f.Name, []domain.RefUpdate{{Old: "invalid but deleted", New: domain.ZeroOID}}, ports.PushObjects{}, domain.WorkflowDir, domain.DefaultPushWalkBounds())
	require.NoError(t, err)
	assert.Empty(t, got.Ends)
	assert.Empty(t, got.Commits)
}

// rawTree writes a tree object by hand: entries are "mode name hexid", in the
// order given (git sorts them; the callers give them sorted).
func rawTree(t *testing.T, f *readertest.Fixture, entries ...[3]string) string {
	t.Helper()
	var b strings.Builder
	for _, e := range entries {
		raw, err := hex.DecodeString(e[2])
		require.NoError(t, err)
		b.WriteString(strings.TrimPrefix(e[0], "0") + " " + e[1] + "\x00") // git writes 40000, not 040000
		b.Write(raw)
	}
	return f.WriteObject("tree", b.String())
}

// What counts as .github/workflows is what the loader lists: a directory of
// mode 040000 below a directory .github of mode 040000. The same tree id as a
// gitlink, a symbolic link or a file is nothing, and turning it into a
// directory activates it; the object id alone does not tell.
func TestPushSubtreesEntryKinds(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	f := readertest.NewFixture(t, data, "proj")
	t.Cleanup(func() { _ = g.Close(ctx) })
	blob := f.WriteObject("blob", "x\n")
	tree := rawTree(t, f, [3]string{"100644", "ci.yml", blob})
	for _, r := range []struct {
		mode, id string
		kind     domain.EntryKind
	}{{"040000", tree, domain.EntryDirectory}, {"160000", tree, domain.EntrySubmodule}, {"120000", blob, domain.EntrySymlink}, {"100644", blob, domain.EntryFile}, {"100755", blob, domain.EntryExecutable}} {
		t.Run(r.mode, func(t *testing.T) {
			gh := rawTree(t, f, [3]string{r.mode, "workflows", r.id})
			root := rawTree(t, f, [3]string{"040000", ".github", gh})
			oid := commitOf(t, f, root)
			got, err := g.PushSubtrees(ctx, f.Name, []domain.RefUpdate{{Old: domain.ZeroOID, New: oid}}, ports.PushObjects{}, domain.WorkflowDir, domain.DefaultPushWalkBounds())
			require.NoError(t, err)
			assert.Equal(t, r.kind, got.Subtrees[oid].Kind)
			assert.Equal(t, r.id, got.Subtrees[oid].ID.String())
			assert.NotContains(t, got.Subtrees, domain.HeadKey)
			if r.mode != "040000" {
				root = rawTree(t, f, [3]string{r.mode, ".github", r.id})
				oid = commitOf(t, f, root)
				got, err = g.PushSubtrees(ctx, f.Name, []domain.RefUpdate{{Old: domain.ZeroOID, New: oid}}, ports.PushObjects{}, domain.WorkflowDir, domain.DefaultPushWalkBounds())
				require.NoError(t, err)
				assert.NotContains(t, got.Subtrees, oid)
			}
		})
	}
}

// A history for the rules on the commits a push brings: main holds workflows A,
// old (a branch) holds B, and the commits made here are in no ref yet, like
// those of a push in quarantine.
type guardHistory struct {
	t      *testing.T
	f      *readertest.Fixture
	trees  map[string]string // workflows name -> tree id
	m, o   string            // main (A) and old (B)
	extraN int
	readme string
}

func newGuardHistory(t *testing.T, g *gitcli.Git, data string) *guardHistory {
	t.Helper()
	f := readertest.NewFixture(t, data, "proj")
	h := &guardHistory{t: t, f: f, trees: map[string]string{}}
	h.readme = f.WriteObject("blob", "r\n")
	for _, n := range []string{"A", "B", "C", "D"} {
		h.trees[n] = rawTree(t, f, [3]string{"100644", "ci.yml", f.WriteObject("blob", "on: push # "+n+"\n")})
	}
	h.m = h.commit("A", "")
	h.o = h.commit("B", "")
	f.Git("update-ref", "refs/heads/main", h.m)
	f.Git("update-ref", "refs/heads/old", h.o)
	return h
}

// commit makes a commit with the workflows named ("" for none), one more file
// each time (so that no two commits share a tree by accident), and the parents.
func (h *guardHistory) commit(workflows, _ string, parents ...string) string {
	h.t.Helper()
	h.extraN++
	extra := h.f.WriteObject("blob", fmt.Sprintf("file %d\n", h.extraN))
	entries := [][3]string{{"100644", "README", h.readme}, {"100644", fmt.Sprintf("x%d", h.extraN), extra}}
	if workflows != "" {
		gh := rawTree(h.t, h.f, [3]string{"040000", "workflows", h.trees[workflows]})
		entries = append([][3]string{{"040000", ".github", gh}}, entries...)
	}
	return commitOf(h.t, h.f, rawTree(h.t, h.f, entries...), parents...)
}

func TestPushSubtreesCommitsAndParents(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	h := newGuardHistory(t, g, data)
	t.Cleanup(func() { _ = g.Close(ctx) })
	side := h.commit("B", "", h.o)
	merge := h.commit("A", "", h.m, side)
	got, err := g.PushSubtrees(ctx, h.f.Name, []domain.RefUpdate{{Old: h.m, New: merge}, {Old: domain.ZeroOID, New: merge}}, ports.PushObjects{}, domain.WorkflowDir, domain.DefaultPushWalkBounds())
	require.NoError(t, err)
	want := h.f.Git("rev-list", "--parents", merge, "--not", "--all")
	var lines []string
	for _, c := range got.Commits {
		line := c.ID.String()
		for _, p := range c.Parents {
			line += " " + p.String()
		}
		lines = append(lines, line)
	}
	assert.Equal(t, strings.TrimSpace(want), strings.Join(lines, "\n"))
	assert.Len(t, got.Commits, 2)
	assert.Equal(t, h.trees["A"], got.Subtrees[domain.HeadKey].ID.String())
	assert.Equal(t, h.trees["B"], got.Subtrees[h.o].ID.String())
	// Any directory path is accepted; the reader has no workflow path policy.
	other, err := g.PushSubtrees(ctx, h.f.Name, []domain.RefUpdate{{Old: h.m, New: merge}}, ports.PushObjects{}, "README", domain.DefaultPushWalkBounds())
	require.NoError(t, err)
	assert.Equal(t, domain.EntryFile, other.Subtrees[merge].Kind)
	assert.NotContains(t, other.Subtrees, domain.HeadKey, "HEAD is lazy for non-creates")
}

// The commits of a push are bounded: more than the bound fails closed.
func TestPushSubtreesBoundsThePushedCommits(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	h := newGuardHistory(t, g, data)
	t.Cleanup(func() { _ = g.Close(ctx) })
	// A chain of plain commits on main, made with fast-import in a ref that is
	// removed afterwards: the objects stay, in no ref, like a push in quarantine.
	chain := func(n int) string {
		var b strings.Builder
		fmt.Fprintf(&b, "reset refs/tmp/chain\nfrom %s\n", h.m)
		for i := range n {
			fmt.Fprintf(&b, "commit refs/tmp/chain\ncommitter a <a@b> %d +0000\ndata 1\nc\nM 100644 inline f.txt\ndata 2\n%d\n\n", 1000+i, i%10)
		}
		cmd := exec.Command("git", "--git-dir="+h.f.Bare, "fast-import", "--quiet")
		cmd.Stdin = strings.NewReader(b.String())
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		tip := h.f.Git("rev-parse", "refs/tmp/chain")
		h.f.Git("update-ref", "-d", "refs/tmp/chain")
		return tip
	}
	// Plain commits carry the workflows of main (the fast-import tree has none,
	// so the chain is "removing" them): allowed, whatever their number, up to the bound.
	p, err := g.PushSubtrees(ctx, h.f.Name, []domain.RefUpdate{{Old: h.m, New: chain(20), Ref: "refs/heads/main"}}, ports.PushObjects{}, domain.WorkflowDir, domain.PushWalkBounds{Commits: 20, IDs: 80})
	require.NoError(t, err)
	assert.Len(t, p.Commits, 20)
	_, err = g.PushSubtrees(ctx, h.f.Name, []domain.RefUpdate{{Old: h.m, New: chain(21), Ref: "refs/heads/main"}}, ports.PushObjects{}, domain.WorkflowDir, domain.PushWalkBounds{Commits: 20, IDs: 80})
	assert.ErrorIs(t, err, domain.ErrInvalid, "more commits than the bound refuses the push")
}

// countingReader serves synthetic rev-list output and counts what was read.
type countingReader struct {
	lines, read int
	line        []byte
	pending     []byte
	made        int
}

func (c *countingReader) Read(p []byte) (int, error) {
	if len(c.pending) == 0 {
		if c.made == c.lines {
			return 0, io.EOF
		}
		c.made++
		c.pending = c.line
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	c.read += n
	return n, nil
}

// A hostile push: ten thousand commits of two hundred parents each is two
// million ids. The bound is met after the first lines, and nothing more is read
// or kept.
func TestParseRevListRejectsManyParentsEarly(t *testing.T) {
	id := func(n int) string { return fmt.Sprintf("%040x", n+1) }
	var b strings.Builder
	b.WriteString(id(0))
	for i := range 198 {
		b.WriteString(" " + id(1+i%3)) // few distinct parents, repeated: the distinct count alone would stay small
	}
	b.WriteString("\n")
	r := &countingReader{lines: domain.MaxPushedCommits, line: []byte(b.String())}
	kept, err := gitcli.ParseRevList(r, domain.MaxPushedCommits, domain.MaxPushedIDs)
	require.ErrorIs(t, err, domain.ErrInvalid)
	assert.Contains(t, err.Error(), "ids (commits and parents)", "refused by the bound on ids, not by the line length")
	assert.Zero(t, kept, "nothing is returned with the error")
	total := domain.MaxPushedCommits * len(b.String())
	assert.Less(t, r.read, 2<<20, "read only the head of %d bytes", total)
	assert.Less(t, r.read*20, total)

	// Within the bounds the same lines are kept: 100 commits of 198 parents.
	r = &countingReader{lines: 100, line: []byte(b.String())}
	kept, err = gitcli.ParseRevList(r, domain.MaxPushedCommits, domain.MaxPushedIDs)
	require.NoError(t, err)
	assert.Equal(t, 100, kept)

	// The other bounds and malformed output.
	_, err = gitcli.ParseRevList(strings.NewReader(id(0)+"\n"+id(1)+"\n"), 1, 100)
	assert.ErrorIs(t, err, domain.ErrInvalid, "commits")
	_, err = gitcli.ParseRevList(strings.NewReader(id(0)+" "+id(1)+"\n"+id(2)+"\n"), 10, 2)
	assert.ErrorIs(t, err, domain.ErrInvalid, "ids")
	_, err = gitcli.ParseRevList(strings.NewReader("nonsense\n"), 10, 10)
	assert.ErrorIs(t, err, domain.ErrInvalid, "an id that is none")
	_, err = gitcli.ParseRevList(strings.NewReader(id(0)+strings.Repeat(" "+id(1), 300)+"\n"), 10, 1000)
	assert.ErrorIs(t, err, domain.ErrInvalid, "a line over the bound")
}

func changedWorkflow(g *gitcli.Git, ctx context.Context, repo domain.RepoName, ups []domain.RefUpdate, objects ports.PushObjects) (string, error) {
	t, err := g.PushSubtrees(ctx, repo, ups, objects, domain.WorkflowDir, domain.DefaultPushWalkBounds())
	if err != nil {
		return "", err
	}
	c, found, err := domain.WorkflowChangeOf(ups, t)
	if err != nil || !found {
		return "", err
	}
	entry, err := g.FirstDifferentEntry(ctx, repo, objects, c.Old, c.New)
	if err != nil {
		return "", err
	}
	if entry == "" {
		return domain.WorkflowDir, nil
	}
	return domain.WorkflowDir + "/" + entry, nil
}

func TestFirstDifferentEntry(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	f := readertest.NewFixture(t, data, "proj")
	t.Cleanup(func() { _ = g.Close(ctx) })
	tree := oidOf(t, treeOf(t, f, "100644 blob "+f.WriteObject("blob", "x\n")+"\tci.yml"))
	entry, err := g.FirstDifferentEntry(ctx, f.Name, ports.PushObjects{}, domain.ObjectID{}, tree)
	require.NoError(t, err)
	assert.Equal(t, "ci.yml", entry)
	entry, err = g.FirstDifferentEntry(ctx, f.Name, ports.PushObjects{}, tree, tree)
	require.NoError(t, err)
	assert.Empty(t, entry)
	_, err = g.FirstDifferentEntry(ctx, f.Name, ports.PushObjects{Dir: t.TempDir()}, domain.ObjectID{}, tree)
	assert.ErrorIs(t, err, domain.ErrInvalid)
}
