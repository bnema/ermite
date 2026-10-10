// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/readertest"
)

// unified renders hunks the way git prints them after the "+++" line.
func unified(hunks []domain.Hunk) string {
	var b strings.Builder
	rng := func(start, n int) string {
		if n == 1 {
			return strconv.Itoa(start)
		}
		return fmt.Sprintf("%d,%d", start, n)
	}
	for _, h := range hunks {
		fmt.Fprintf(&b, "@@ -%s +%s @@", rng(h.OldStart, h.OldLines), rng(h.NewStart, h.NewLines))
		if h.Header != "" {
			b.WriteString(" " + h.Header)
		}
		b.WriteByte('\n')
		for _, l := range h.Lines {
			b.WriteString(map[domain.LineKind]string{domain.LineContext: " ", domain.LineAddition: "+", domain.LineDeletion: "-"}[l.Kind])
			b.WriteString(l.Text + "\n")
			if l.NoNewlineAtEOF {
				b.WriteString("\\ No newline at end of file\n")
			}
		}
	}
	return b.String()
}

// gitSections splits the output of `git show -p` into the text each
// "diff --git" section has from its first hunk on ("" for a section without
// hunk: binary, pure rename, mode change).
func gitSections(out string) []string {
	var sections []string
	for _, s := range strings.Split("\n"+out, "\ndiff --git ")[1:] {
		_, hunks, ok := strings.Cut(s, "\n@@ ")
		if !ok {
			sections = append(sections, "")
			continue
		}
		sections = append(sections, "@@ "+strings.TrimSuffix(hunks, "\n")+"\n")
	}
	return sections
}

// showSections is git's own patch text of a commit against its first parent.
func showSections(f *readertest.Fixture, id string) []string {
	f.T.Helper()
	out := f.Git("show", "--format=", "--no-color", "-M", "-m", "--first-parent", "-U3", id)
	return gitSections(strings.ToValidUTF8(out, "\uFFFD"))
}

// showNumstat is git's own numstat of a commit against its first parent.
func showNumstat(f *readertest.Fixture, id string) []string {
	f.T.Helper()
	out := f.Git("show", "--format=", "-M", "-m", "--first-parent", "--numstat", id)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// assertMatchesGitShow checks the structured diff of a commit against the
// text of `git show`: the patch of every file, its counts, and the totals.
func assertMatchesGitShow(t *testing.T, f *readertest.Fixture, id string, d domain.CommitDetail) {
	t.Helper()
	sections := showSections(f, id)
	var adds, dels int
	numstat := showNumstat(f, id)
	require.Len(t, numstat, len(d.Diff.Files), "one numstat line per file")
	si := 0
	for i, file := range d.Diff.Files {
		label := file.OldPath + ">" + file.NewPath
		fields := strings.SplitN(numstat[i], "\t", 3)
		if fields[0] == "-" {
			assert.True(t, file.Binary, label)
			assert.Zero(t, file.Additions+file.Deletions, label)
		} else {
			assert.False(t, file.Binary, label)
			assert.Equal(t, fields[0], strconv.Itoa(file.Additions), label)
			assert.Equal(t, fields[1], strconv.Itoa(file.Deletions), label)
			a, _ := strconv.Atoi(fields[0])
			del, _ := strconv.Atoi(fields[1])
			adds, dels = adds+a, dels+del
		}
		// A type change is two sections: the deletion, then the addition.
		n := 1
		if file.Status == domain.FileTypeChanged {
			n = 2
		}
		require.LessOrEqual(t, si+n, len(sections), label)
		assert.Equal(t, strings.Join(sections[si:si+n], ""), unified(file.Hunks), "patch of %s", label)
		si += n
	}
	assert.Equal(t, len(sections), si, "every git section is accounted for")
	assert.Equal(t, domain.DiffStats{FilesChanged: len(numstat), Additions: adds, Deletions: dels}, d.Stats)
}

func TestGetCommitMatchesGitShow(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	h := readertest.NewHistory(t, data)

	for _, tt := range []struct{ name, id string }{
		{"root commit: adds, binary, empty file", h.C1},
		{"rename with change, binary, mode change, add, delete", h.C2},
		{"type change", h.C3},
		{"merge, against the first parent", h.Merge},
		{"side branch", h.Side},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, err := g.GetCommit(ctx, h.Name, domain.RevisionOf(readertest.MustID(t, tt.id)), domain.DefaultDiffLimits())
			require.NoError(t, err)
			assert.Equal(t, tt.id, d.Commit.ID.String())
			assert.False(t, d.Diff.Truncated)
			assertMatchesGitShow(t, h.Fixture, tt.id, d)
		})
	}
}

func TestGetCommitFileMetadata(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	h := readertest.NewHistory(t, data)

	byPath := func(d domain.Diff) map[string]domain.FileDiff {
		m := map[string]domain.FileDiff{}
		for _, f := range d.Files {
			m[f.OldPath+">"+f.NewPath] = f
		}
		return m
	}
	id := func(spec string) domain.ObjectID { return readertest.MustID(t, h.Git("rev-parse", spec)) }

	d, err := g.GetCommit(ctx, h.Name, readertest.Rev(t, "v2"), domain.DefaultDiffLimits())
	require.NoError(t, err)
	assert.Equal(t, h.C2, d.Commit.ID.String())
	files := byPath(d.Diff)
	require.Len(t, files, 5)

	ren := files["a.txt>b.txt"]
	assert.Equal(t, domain.FileRenamed, ren.Status)
	assert.Equal(t, 73, ren.Similarity)
	assert.Equal(t, 1, ren.Additions)
	assert.Equal(t, 0, ren.Deletions)
	assert.Equal(t, uint32(0o100644), ren.OldMode)
	assert.Equal(t, uint32(0o100644), ren.NewMode)
	assert.Equal(t, id(h.C1+":a.txt"), ren.OldID)
	assert.Equal(t, id(h.C2+":b.txt"), ren.NewID)
	require.Len(t, ren.Hunks, 1)
	assert.Equal(t, domain.Hunk{
		OldStart: 1, OldLines: 3, NewStart: 1, NewLines: 4,
		Lines: []domain.Line{
			{Kind: domain.LineContext, OldNumber: 1, NewNumber: 1, Text: "one"},
			{Kind: domain.LineContext, OldNumber: 2, NewNumber: 2, Text: "two"},
			{Kind: domain.LineContext, OldNumber: 3, NewNumber: 3, Text: "three"},
			{Kind: domain.LineAddition, NewNumber: 4, Text: "four"},
		},
	}, ren.Hunks[0])

	bin := files["bin>bin"]
	assert.Equal(t, domain.FileModified, bin.Status)
	assert.True(t, bin.Binary)
	assert.Empty(t, bin.Hunks)
	assert.False(t, bin.Truncated)

	mode := files["d/f>d/f"]
	assert.Equal(t, domain.FileModified, mode.Status)
	assert.Equal(t, uint32(0o100644), mode.OldMode)
	assert.Equal(t, uint32(0o100755), mode.NewMode)
	assert.Empty(t, mode.Hunks)
	assert.False(t, mode.Binary)
	assert.False(t, mode.Truncated)

	del := files["README.md>"]
	assert.Equal(t, domain.FileDeleted, del.Status)
	assert.Equal(t, uint32(0), del.NewMode)
	assert.True(t, del.NewID.IsZero())
	assert.Equal(t, 1, del.Deletions)
	require.Len(t, del.Hunks, 1)
	assert.Equal(t, domain.LineDeletion, del.Hunks[0].Lines[0].Kind)
	assert.Equal(t, 0, del.Hunks[0].NewStart, "a deleted file has no new side")

	add := files[">n.txt"]
	assert.Equal(t, domain.FileAdded, add.Status)
	assert.Equal(t, uint32(0), add.OldMode)
	assert.True(t, add.OldID.IsZero())
	require.Len(t, add.Hunks, 1)
	last := add.Hunks[0].Lines[1]
	assert.Equal(t, domain.Line{Kind: domain.LineAddition, NewNumber: 2, Text: "nonl", NoNewlineAtEOF: true}, last)

	d, err = g.GetCommit(ctx, h.Name, readertest.Rev(t, h.C3), domain.DefaultDiffLimits())
	require.NoError(t, err)
	require.Len(t, d.Diff.Files, 1)
	tc := d.Diff.Files[0]
	assert.Equal(t, domain.FileTypeChanged, tc.Status)
	assert.Equal(t, uint32(0o120000), tc.NewMode)
	assert.Equal(t, 1, tc.Additions)
	assert.Equal(t, 2, tc.Deletions)
}

func TestGetCommitMergeDiffIsAgainstFirstParent(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	h := readertest.NewHistory(t, data)

	d, err := g.GetCommit(ctx, h.Name, readertest.Rev(t, "main"), domain.DefaultDiffLimits())
	require.NoError(t, err)
	require.Len(t, d.Commit.Parents, 2)
	require.Len(t, d.Diff.Files, 1)
	assert.Equal(t, "side.txt", d.Diff.Files[0].NewPath)
	assert.Equal(t, domain.FileAdded, d.Diff.Files[0].Status)
}

func TestGetCommitEmptyCommit(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	f := readertest.NewFixture(t, data, "empty-commit")
	id := f.Commit("nothing")

	d, err := g.GetCommit(ctx, f.Name, domain.Revision{}, domain.DefaultDiffLimits())
	require.NoError(t, err)
	assert.Equal(t, id, d.Commit.ID.String())
	assert.Empty(t, d.Diff.Files)
	assert.False(t, d.Diff.Truncated)
	assert.Equal(t, domain.DiffStats{}, d.Stats)
}

func TestGetCommitErrors(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	h := readertest.NewHistory(t, data)

	_, err := g.GetCommit(ctx, h.Name, readertest.Rev(t, "nope"), domain.DefaultDiffLimits())
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = g.GetCommit(ctx, name(t, "nope"), domain.Revision{}, domain.DefaultDiffLimits())
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestGetCommitHunkHeaderAndNonUTF8(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	f := readertest.NewFixture(t, data, "hunks")
	var src strings.Builder
	src.WriteString("package x\n\nfunc Alpha() {\n")
	for i := range 40 {
		fmt.Fprintf(&src, "\tline(%d)\n", i)
	}
	src.WriteString("}\n")
	f.Write("x.go", src.String()).Write("latin", "caf\xe9\nok\n")
	f.Commit("v1")
	f.Write("x.go", strings.Replace(src.String(), "\tline(30)", "\tchanged(30)", 1)).Write("latin", "caf\xe9\nOK\n")
	id := f.Commit("v2")

	d, err := g.GetCommit(ctx, f.Name, domain.Revision{}, domain.DefaultDiffLimits())
	require.NoError(t, err)
	assertMatchesGitShow(t, f, id, d)
	var gofile, latin domain.FileDiff
	for _, file := range d.Diff.Files {
		switch file.NewPath {
		case "x.go":
			gofile = file
		case "latin":
			latin = file
		}
	}
	require.Len(t, gofile.Hunks, 1)
	assert.Equal(t, "func Alpha() {", gofile.Hunks[0].Header, "git's default function name rule")
	assert.Equal(t, 31, gofile.Hunks[0].OldStart)
	require.Len(t, latin.Hunks, 1)
	assert.Equal(t, "caf\uFFFD", latin.Hunks[0].Lines[0].Text, "invalid UTF-8 is replaced")
}

// manyLines commits files and returns the id.
func commitFiles(f *readertest.Fixture, msg string, files map[string]int) string {
	for p, n := range files {
		f.Lines(p, strings.ReplaceAll(p, "/", "_"), n)
	}
	return f.Commit(msg)
}

func TestDiffLimits(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	defaults := domain.DefaultDiffLimits()
	find := func(d domain.Diff, p string) domain.FileDiff {
		for _, f := range d.Files {
			if f.NewPath == p {
				return f
			}
		}
		t.Fatalf("file %s not in the diff", p)
		return domain.FileDiff{}
	}

	t.Run("a file at the line limit is kept, one line more is truncated", func(t *testing.T) {
		f := readertest.NewFixture(t, data, "limit-file")
		commitFiles(f, "files", map[string]int{"exact": 2000, "over": 2001, "small": 3})
		d, err := g.GetCommit(ctx, f.Name, domain.Revision{}, defaults)
		require.NoError(t, err)
		exact, over, small := find(d.Diff, "exact"), find(d.Diff, "over"), find(d.Diff, "small")

		assert.False(t, exact.Truncated)
		require.Len(t, exact.Hunks, 1)
		assert.Len(t, exact.Hunks[0].Lines, 2000)
		assert.True(t, over.Truncated)
		assert.Empty(t, over.Hunks)
		assert.Equal(t, 2001, over.Additions, "counts stay accurate")
		assert.False(t, small.Truncated)
		assert.Len(t, small.Hunks, 1)
		assert.True(t, d.Diff.Truncated)
		assert.Equal(t, domain.DiffStats{FilesChanged: 3, Additions: 4004}, d.Stats)
	})

	t.Run("context counts towards the file limit", func(t *testing.T) {
		f := readertest.NewFixture(t, data, "limit-context")
		f.Lines("big", "l", 3000).Commit("v1")
		// Every 7th line changes: ~428 changed lines, but the hunks merge
		// into one that spans the file with its context.
		var b strings.Builder
		for i := range 3000 {
			if i%7 == 0 {
				fmt.Fprintf(&b, "changed %d\n", i)
			} else {
				fmt.Fprintf(&b, "l %d\n", i)
			}
		}
		f.Write("big", b.String())
		f.Write("tiny", "x\n")
		id := f.Commit("v2")
		d, err := g.GetCommit(ctx, f.Name, domain.Revision{}, defaults)
		require.NoError(t, err)
		big, tiny := find(d.Diff, "big"), find(d.Diff, "tiny")
		assert.True(t, big.Truncated)
		assert.Empty(t, big.Hunks)
		assert.Equal(t, 429, big.Additions)
		assert.Equal(t, 429, big.Deletions)
		assert.False(t, tiny.Truncated, "the file after a truncated one is still read")
		assert.Equal(t, "x\n", strings.TrimPrefix(unified(tiny.Hunks), "@@ -0,0 +1 @@\n+"))
		assert.Equal(t, id, d.Commit.ID.String())
	})

	t.Run("the total limit truncates the file that does not fit", func(t *testing.T) {
		f := readertest.NewFixture(t, data, "limit-total")
		files := map[string]int{}
		for i := range 11 {
			files[fmt.Sprintf("f%02d", i)] = 1900 // 11 x 1900 = 20,900 > 20,000
		}
		files["g-small"] = 50 // sorts after f10: still fits in the 1,000 lines left
		files["h-big"] = 1500 // does not fit
		files["i-small"] = 10 // fits
		commitFiles(f, "total", files)
		d, err := g.GetCommit(ctx, f.Name, domain.Revision{}, defaults)
		require.NoError(t, err)
		require.Len(t, d.Diff.Files, 14)
		total := 0
		for _, file := range d.Diff.Files {
			for _, h := range file.Hunks {
				total += len(h.Lines)
			}
		}
		assert.LessOrEqual(t, total, 20000)
		for i := range 10 {
			assert.False(t, find(d.Diff, fmt.Sprintf("f%02d", i)).Truncated)
		}
		assert.True(t, find(d.Diff, "f10").Truncated, "1,900 lines do not fit in the 1,000 left")
		assert.False(t, find(d.Diff, "g-small").Truncated)
		assert.True(t, find(d.Diff, "h-big").Truncated)
		assert.False(t, find(d.Diff, "i-small").Truncated)
		assert.True(t, d.Diff.Truncated)
		assert.Equal(t, 11*1900+50+1500+10, d.Stats.Additions)
		assert.Equal(t, 14, d.Stats.FilesChanged)
	})

	t.Run("files past the file limit are omitted but counted", func(t *testing.T) {
		f := readertest.NewFixture(t, data, "limit-files")
		for i := range 305 {
			f.Write(fmt.Sprintf("many/f%03d", i), "x\n")
		}
		f.Commit("many")
		d, err := g.GetCommit(ctx, f.Name, domain.Revision{}, defaults)
		require.NoError(t, err)
		assert.Len(t, d.Diff.Files, 300)
		assert.True(t, d.Diff.Truncated)
		assert.Equal(t, domain.DiffStats{FilesChanged: 305, Additions: 305}, d.Stats)
		assert.Equal(t, "many/f000", d.Diff.Files[0].NewPath)
		assert.Equal(t, "many/f299", d.Diff.Files[299].NewPath)
		for _, file := range d.Diff.Files {
			assert.False(t, file.Truncated)
			assert.Len(t, file.Hunks, 1)
		}
	})

	t.Run("exactly 300 files is not truncated", func(t *testing.T) {
		f := readertest.NewFixture(t, data, "limit-300")
		for i := range 300 {
			f.Write(fmt.Sprintf("f%03d", i), "x\n")
		}
		f.Commit("300")
		d, err := g.GetCommit(ctx, f.Name, domain.Revision{}, defaults)
		require.NoError(t, err)
		assert.Len(t, d.Diff.Files, 300)
		assert.False(t, d.Diff.Truncated)
	})

	t.Run("custom limits and the byte limit", func(t *testing.T) {
		f := readertest.NewFixture(t, data, "limit-custom")
		commitFiles(f, "c", map[string]int{"a": 10, "b": 10, "c": 10})
		d, err := g.GetCommit(ctx, f.Name, domain.Revision{}, domain.DiffLimits{MaxFiles: 2, MaxFileLines: 100, MaxTotalLines: 15, MaxTotalBytes: 1 << 20})
		require.NoError(t, err)
		require.Len(t, d.Diff.Files, 2)
		assert.False(t, d.Diff.Files[0].Truncated)
		assert.True(t, d.Diff.Files[1].Truncated, "10 lines do not fit in the 5 left")
		assert.Equal(t, 3, d.Stats.FilesChanged)

		d, err = g.GetCommit(ctx, f.Name, domain.Revision{}, domain.DiffLimits{MaxFiles: 10, MaxFileLines: 100, MaxTotalLines: 100, MaxTotalBytes: 50})
		require.NoError(t, err)
		require.Len(t, d.Diff.Files, 3)
		assert.False(t, d.Diff.Files[0].Truncated, "30 bytes of text fit in 50")
		for _, file := range d.Diff.Files[1:] {
			assert.True(t, file.Truncated, "the byte budget has 20 bytes left for 30")
			assert.Empty(t, file.Hunks)
		}
	})

	t.Run("a very long line truncates its file instead of being cut", func(t *testing.T) {
		f := readertest.NewFixture(t, data, "limit-line")
		f.Write("long", strings.Repeat("x", 200<<10)+"\n").Write("short", "ok\n").Commit("long")
		d, err := g.GetCommit(ctx, f.Name, domain.Revision{}, defaults)
		require.NoError(t, err)
		long, short := find(d.Diff, "long"), find(d.Diff, "short")
		assert.True(t, long.Truncated)
		assert.Empty(t, long.Hunks)
		assert.False(t, short.Truncated)
		assert.Len(t, short.Hunks, 1)
	})

	t.Run("binary files and mode changes in a truncated commit", func(t *testing.T) {
		f := readertest.NewFixture(t, data, "limit-mixed")
		f.Lines("a-big", "x", 2500).WriteBytes("b-bin", []byte{0, 1, 2}).Write("c-mode", "m\n").Write("d-text", "t\n")
		f.Commit("v1")
		f.Lines("a-big", "y", 2500).WriteBytes("b-bin", []byte{0, 3}).Chmod("c-mode", 0o755).Write("d-text", "t2\n")
		id := f.Commit("v2")
		d, err := g.GetCommit(ctx, f.Name, domain.Revision{}, defaults)
		require.NoError(t, err)
		assert.True(t, find(d.Diff, "a-big").Truncated)
		assert.True(t, find(d.Diff, "b-bin").Binary)
		assert.False(t, find(d.Diff, "b-bin").Truncated)
		assert.False(t, find(d.Diff, "c-mode").Truncated)
		assert.Len(t, find(d.Diff, "d-text").Hunks, 1)
		// What is kept matches git.
		sections := showSections(f, id)
		require.Len(t, sections, 4)
		assert.Equal(t, sections[3], unified(find(d.Diff, "d-text").Hunks))
	})
}

func TestGetCommitStopsGitEarly(t *testing.T) {
	ctx := context.Background()
	g, data := newReader(t, nil)
	f := readertest.NewFixture(t, data, "early")
	f.Lines("a", "x", 5).Lines("z-huge", "y", 100000).Commit("c")

	// z-huge is truncated by the numstat alone, so nothing of it is read.
	d, err := g.GetCommit(ctx, f.Name, domain.Revision{}, domain.DefaultDiffLimits())
	require.NoError(t, err)
	require.Len(t, d.Diff.Files, 2)
	assert.False(t, d.Diff.Files[0].Truncated)
	assert.True(t, d.Diff.Files[1].Truncated)
	assert.Equal(t, 100005, d.Stats.Additions)
}
