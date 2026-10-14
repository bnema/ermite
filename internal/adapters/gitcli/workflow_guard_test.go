// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli_test

import (
	"context"
	"encoding/hex"
	"fmt"
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
		p, err := g.ChangedWorkflow(ctx, f.Name, ups, ports.PushObjects{})
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

	_, err := g.ChangedWorkflow(ctx, f.Name, []domain.RefUpdate{up(oid(base), "1111111111111111111111111111111111111111")}, ports.PushObjects{})
	assert.Error(t, err, "an object git does not have is a failure, not a pass")
}

func TestChangedWorkflowReadsTheQuarantineOfThePushOnly(t *testing.T) {
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
		_, err := g.ChangedWorkflow(ctx, f.Name, up, o)
		assert.ErrorIs(t, err, domain.ErrInvalid, name)
	}
	// An existing directory inside the repository is accepted as a location
	// (the object is missing: a refusal of the object, not of the path).
	require.NoError(t, os.Mkdir(filepath.Join(objects, "incoming-1"), 0o700))
	_, err := g.ChangedWorkflow(ctx, f.Name, up, ports.PushObjects{Dir: objects + "/incoming-1", Alternates: objects})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "outside the repository")
	// One that does not exist is refused.
	_, err = g.ChangedWorkflow(ctx, f.Name, up, ports.PushObjects{Dir: objects + "/incoming-2"})
	assert.ErrorIs(t, err, domain.ErrInvalid)
}

// A symbolic link in the way cannot take the objects of a push out of the
// repository, and a data directory reached through links still works.
func TestChangedWorkflowResolvesSymbolicLinks(t *testing.T) {
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
	_, err := g.ChangedWorkflow(ctx, f.Name, up, ports.PushObjects{Dir: filepath.Join(objects, "incoming-link")})
	assert.ErrorIs(t, err, domain.ErrInvalid, "a link out of objects/")
	_, err = g.ChangedWorkflow(ctx, f.Name, up, ports.PushObjects{Dir: objects + "/pack", Alternates: filepath.Join(objects, "incoming-link")})
	assert.ErrorIs(t, err, domain.ErrInvalid, "an alternate that is a link out of objects/")

	// A link to somewhere inside objects/ is fine.
	require.NoError(t, os.Mkdir(filepath.Join(objects, "q1"), 0o700))
	require.NoError(t, os.Symlink(filepath.Join(objects, "q1"), filepath.Join(objects, "q-link")))
	_, err = g.ChangedWorkflow(ctx, f.Name, up, ports.PushObjects{Dir: filepath.Join(objects, "q-link")})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "outside the repository")
}

func TestChangedWorkflowThroughALinkedDataDirectory(t *testing.T) {
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
		p, err := g2.ChangedWorkflow(ctx, f.Name, []domain.RefUpdate{{Old: oidOf(t, base).String(), New: oidOf(t, wf).String(), Ref: "refs/heads/x"}},
			ports.PushObjects{Dir: dir, Alternates: filepath.Join(link, "repos", "proj.git", "objects")})
		require.NoError(t, err, dir)
		assert.Equal(t, ".github/workflows/ci.yml", p, dir)
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

// withWorkflows is a commit whose .github/workflows is the tree given.
func withWorkflows(t *testing.T, f *readertest.Fixture, workflows string) string {
	t.Helper()
	gh := treeOf(t, f, "040000 tree "+workflows+"\tworkflows")
	return commitOf(t, f, treeOf(t, f, "040000 tree "+gh+"\t.github"))
}

// The trees of a push are the pusher's: a workflows directory that fans out
// into ten million paths is answered with its first path, at once, by a git
// that is stopped, without the diff ever being held in memory.
func TestChangedWorkflowDoesNotBufferTheDiff(t *testing.T) {
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
	amp := withWorkflows(t, f, level)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	p, err := g.ChangedWorkflow(ctx, f.Name, []domain.RefUpdate{{Old: domain.ZeroOID, New: amp, Ref: "refs/heads/x"}}, ports.PushObjects{})
	took := time.Since(start)
	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(p, ".github/workflows/d"), p)
	assert.Less(t, took, 2*time.Second, "answered from the first path")
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(4<<20), "bounded memory")
	// And no git is left behind.
	out, _ := exec.Command("pgrep", "-f", "git --no-replace-objects.*diff-tree.*"+amp).CombinedOutput()
	assert.Empty(t, strings.TrimSpace(string(out)), "git was reaped")
}

// A path longer than the bound fails closed: the push is refused, not read on.
func TestChangedWorkflowRefusesAnEndlessPath(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	f := readertest.NewFixture(t, data, "proj")
	t.Cleanup(func() { _ = g.Close(ctx) })
	level := treeOf(t, f, "100644 blob "+f.WriteObject("blob", "x\n")+"\tf")
	level = treeOf(t, f, "040000 tree "+level+"\t"+strings.Repeat("d", 5000)) // an entry name of 5000 bytes
	_, err := g.ChangedWorkflow(ctx, f.Name, []domain.RefUpdate{{Old: domain.ZeroOID, New: withWorkflows(t, f, level), Ref: "refs/heads/x"}}, ports.PushObjects{})
	assert.ErrorIs(t, err, domain.ErrInvalid)
}

// A tag may point to a blob, a tree or a commit; only the commits and trees can
// hold a workflow, and an object that is not there refuses the push.
func TestChangedWorkflowPeelsAndClassifiesBothEnds(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	f := readertest.NewFixture(t, data, "proj")
	t.Cleanup(func() { _ = g.Close(ctx) })
	f.Write("README", "r\n")
	plain := f.Commit("plain")
	f.Write(".github/workflows/ci.yml", "on: push\n")
	wf := f.Commit("workflow")
	blob := f.WriteObject("blob", "hello\n")
	annotatedBlob := f.WriteObject("tag", "object "+blob+"\ntype blob\ntag b\ntagger a <a@b> 1 +0000\n\nm\n")
	annotatedWF := f.WriteObject("tag", "object "+oidOf(t, wf).String()+"\ntype commit\ntag c\ntagger a <a@b> 1 +0000\n\nm\n")
	rootTree := f.Git("rev-parse", wf+"^{tree}")
	ref := func(old, new string) []domain.RefUpdate {
		return []domain.RefUpdate{{Old: old, New: new, Ref: "refs/tags/t"}}
	}
	changed := func(ups []domain.RefUpdate) (string, error) {
		return g.ChangedWorkflow(ctx, f.Name, ups, ports.PushObjects{})
	}
	for name, ups := range map[string][]domain.RefUpdate{
		"create a lightweight tag on a blob":           ref(domain.ZeroOID, blob),
		"create an annotated tag on a blob":            ref(domain.ZeroOID, annotatedBlob),
		"a tag from a blob to another blob":            ref(blob, annotatedBlob),
		"a tag from a commit with workflows to a blob": ref(oidOf(t, wf).String(), blob),
	} {
		p, err := changed(ups)
		require.NoError(t, err, name)
		assert.Empty(t, p, name)
	}
	for name, ups := range map[string][]domain.RefUpdate{
		"a tag from a blob to a commit with workflows":            ref(blob, oidOf(t, wf).String()),
		"a tag from an annotated blob to a commit with workflows": ref(annotatedBlob, oidOf(t, wf).String()),
		"an annotated tag on a commit with workflows":             ref(domain.ZeroOID, annotatedWF),
		"a tag on a tree with workflows":                          ref(domain.ZeroOID, rootTree),
		"from a commit without to one with":                       ref(oidOf(t, plain).String(), annotatedWF),
	} {
		p, err := changed(ups)
		require.NoError(t, err, name)
		assert.Equal(t, ".github/workflows/ci.yml", p, name)
	}
	for name, ups := range map[string][]domain.RefUpdate{
		"a missing new object": ref(domain.ZeroOID, "1111111111111111111111111111111111111111"),
		"a missing old object": ref("2222222222222222222222222222222222222222", oidOf(t, wf).String()),
		"a malformed object":   ref(domain.ZeroOID, "ZZZZ"),
	} {
		_, err := changed(ups)
		assert.Error(t, err, name)
	}
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
func TestChangedWorkflowComparesTheModeOfTheEntries(t *testing.T) {
	g, data := newGit(t)
	ctx := context.Background()
	f := readertest.NewFixture(t, data, "proj")
	t.Cleanup(func() { _ = g.Close(ctx) })
	f.Write("README", "r\n")
	f.Commit("base")
	yml := f.WriteObject("blob", "on: push\n")
	wfTree := rawTree(t, f, [3]string{"100644", "ci.yml", yml}) // the workflows
	link := f.WriteObject("blob", ".github/other")
	file := f.WriteObject("blob", "not a directory\n")
	readme := f.WriteObject("blob", "r\n")

	// A commit whose .github/workflows has the mode and id given.
	withWorkflows := func(mode, id string) string {
		gh := rawTree(t, f, [3]string{mode, "workflows", id})
		return commitOf(t, f, rawTree(t, f, [3]string{"040000", ".github", gh}))
	}
	// A commit whose .github has the mode and id given.
	withGithub := func(mode, id string) string {
		return commitOf(t, f, rawTree(t, f, [3]string{mode, ".github", id}))
	}
	asDir := withWorkflows("040000", wfTree)
	asGitlink := withWorkflows("160000", wfTree) // the same id, a submodule
	asSymlink := withWorkflows("120000", link)
	asFile := withWorkflows("100644", file)
	none := commitOf(t, f, rawTree(t, f, [3]string{"100644", "README", readme}))
	ghDir := rawTree(t, f, [3]string{"040000", "workflows", wfTree})
	githubDir := withGithub("040000", ghDir)
	githubGitlink := withGithub("160000", ghDir) // the same id, a submodule
	githubFile := withGithub("100644", file)
	githubLink := withGithub("120000", link)

	for name, c := range map[string]struct {
		old, new string
		want     string
	}{
		"gitlink to a directory, same id (the bypass)": {asGitlink, asDir, ".github/workflows/ci.yml"},
		"a directory to a gitlink removes it":          {asDir, asGitlink, ""},
		"symlink to a directory":                       {asSymlink, asDir, ".github/workflows/ci.yml"},
		"a directory to a symlink removes it":          {asDir, asSymlink, ""},
		"file to a directory":                          {asFile, asDir, ".github/workflows/ci.yml"},
		"a directory to a file removes it":             {asDir, asFile, ""},
		"nothing to a directory":                       {none, asDir, ".github/workflows/ci.yml"},
		"a directory to nothing":                       {asDir, none, ""},
		"the same gitlink":                             {asGitlink, asGitlink, ""},
		"gitlink to symlink":                           {asGitlink, asSymlink, ""},
		".github a gitlink to a directory, same id":    {githubGitlink, githubDir, ".github/workflows/ci.yml"},
		".github a directory to a gitlink":             {githubDir, githubGitlink, ""},
		".github a file to a directory":                {githubFile, githubDir, ".github/workflows/ci.yml"},
		".github a symlink to a directory":             {githubLink, githubDir, ".github/workflows/ci.yml"},
		".github a directory to a symlink":             {githubDir, githubLink, ""},
		"a new ref with a gitlink":                     {domain.ZeroOID, asGitlink, ""},
		"a new ref with a symlink":                     {domain.ZeroOID, asSymlink, ""},
		"a new ref with .github a gitlink":             {domain.ZeroOID, githubGitlink, ""},
		"a new ref with a directory":                   {domain.ZeroOID, asDir, ".github/workflows/ci.yml"},
	} {
		p, err := g.ChangedWorkflow(ctx, f.Name, []domain.RefUpdate{{Old: c.old, New: c.new, Ref: "refs/heads/x"}}, ports.PushObjects{})
		require.NoError(t, err, name)
		assert.Equal(t, c.want, p, name)
	}

	// A tag that peels to the commit is judged the same way.
	tag := f.WriteObject("tag", "object "+asDir+"\ntype commit\ntag t\ntagger a <a@b> 1 +0000\n\nm\n")
	p, err := g.ChangedWorkflow(ctx, f.Name, []domain.RefUpdate{{Old: asGitlink, New: tag, Ref: "refs/tags/t"}}, ports.PushObjects{})
	require.NoError(t, err)
	assert.Equal(t, ".github/workflows/ci.yml", p)
}
