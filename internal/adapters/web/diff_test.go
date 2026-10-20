// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

// Trojan Source (ADR 0017): direction controls, zero-width and control
// characters are shown as markers; ordinary text, tabs and non-Latin letters
// are not touched.
func TestSegmentsShowInvisibleCharacters(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in string
		want     []Seg
	}{
		{"plain", "return x", []Seg{{Text: "return x"}}},
		{"tab and unicode", "\tπ := “é”", []Seg{{Text: "\tπ := “é”"}}},
		{"empty", "", []Seg{{Text: ""}}},
		{"crlf", "a\r", []Seg{{Text: "a"}}},
		{"rlo", "if x { // \u202eadmin", []Seg{{Text: "if x { // "}, {Text: "<U+202E>", Mark: true}, {Text: "admin"}}},
		{"isolates", "\u2066a\u2069", []Seg{{Text: "<U+2066>", Mark: true}, {Text: "a"}, {Text: "<U+2069>", Mark: true}}},
		{"zero width", "a\u200bb\ufeff", []Seg{{Text: "a"}, {Text: "<U+200B>", Mark: true}, {Text: "b"}, {Text: "<U+FEFF>", Mark: true}}},
		{"control", "a\x00\x1bb", []Seg{{Text: "a"}, {Text: "<U+0000>", Mark: true}, {Text: "<U+001B>", Mark: true}, {Text: "b"}}},
		{"tag character", "a\U000E0041", []Seg{{Text: "a"}, {Text: "<U+E0041>", Mark: true}}},
		{"hangul filler", "\u3164", []Seg{{Text: "<U+3164>", Mark: true}}},
		{"replacement is visible already", "a\ufffdb", []Seg{{Text: "a\ufffdb"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, segments(tt.in))
		})
	}
	assert.Equal(t, "a<U+202E>b.go", visibleText("a\u202eb.go"))
	assert.Equal(t, "plain", visibleText("plain"))
	assert.Equal(t, "l1\nl<U+200B>2", messageText("l1\r\nl\u200b2\n\n"))
}

func line(kind domain.LineKind, oldN, newN int, text string) domain.Line {
	return domain.Line{Kind: kind, OldNumber: oldN, NewNumber: newN, Text: text}
}

// The split view lines a run of deletions up with the additions after it;
// context fills both sides; the extra lines of the longer run have an empty
// opposite side.
func TestPairLines(t *testing.T) {
	t.Parallel()
	ctx, add, del := domain.LineContext, domain.LineAddition, domain.LineDeletion
	var ls []DiffLine
	for _, l := range []domain.Line{
		line(ctx, 1, 1, "c"),
		line(del, 2, 0, "d1"), line(del, 3, 0, "d2"), line(del, 4, 0, "d3"),
		line(add, 0, 2, "a1"),
		line(ctx, 5, 3, "c2"),
		line(add, 0, 4, "a2"), line(add, 0, 5, "a3"),
	} {
		ls = append(ls, diffLine(l))
	}
	rows := pairLines(ls)
	type pair struct{ l, r string }
	var got []pair
	for _, r := range rows {
		p := pair{}
		if r.L != nil {
			p.l = r.L.Mark + r.L.Segs[0].Text
		}
		if r.R != nil {
			p.r = r.R.Mark + r.R.Segs[0].Text
		}
		got = append(got, p)
	}
	assert.Equal(t, []pair{
		{" c", " c"}, {"-d1", "+a1"}, {"-d2", ""}, {"-d3", ""}, {" c2", " c2"}, {"", "+a2"}, {"", "+a3"},
	}, got)
	assert.Equal(t, DiffLine{Kind: "del", Mark: "-", Old: "2", Segs: []Seg{{Text: "d1"}}, oldN: 2}, ls[1])
	assert.Equal(t, DiffLine{Kind: "add", Mark: "+", New: "2", Segs: []Seg{{Text: "a1"}}, newN: 2}, ls[4])
}

func TestFileNotes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		file domain.FileDiff
		want string // a substring; empty means no note at all
		eq   bool   // want is the whole note
	}{
		{"binary", domain.FileDiff{Binary: true, Status: domain.FileModified}, "binary file: no text diff", true},
		{"mode change", domain.FileDiff{Status: domain.FileModified, OldMode: 0o100644, NewMode: 0o100755}, "mode 100644 → 100755", true},
		{"an added file has no mode change", domain.FileDiff{Status: domain.FileAdded, NewMode: 0o100644}, "", true},
		{"rename", domain.FileDiff{Status: domain.FileRenamed, Similarity: 90, OldMode: 0o100644, NewMode: 0o100644}, "renamed, 90% similar", true},
		{"copy", domain.FileDiff{Status: domain.FileCopied, Similarity: 80}, "copied, 80% similar", true},
		{"type change", domain.FileDiff{Status: domain.FileTypeChanged}, "type changed", true},
		{"generated", domain.FileDiff{Omitted: domain.OmitGenerated, Additions: 5}, "generated file: collapsed", true},
		{"large", domain.FileDiff{Omitted: domain.OmitLarge, Additions: 2000, Deletions: 500}, "large diff (2500 lines changed): collapsed", true},
		{"budget", domain.FileDiff{Omitted: domain.OmitBudget, Additions: 40}, "collapsed to keep the page light", true},
		{"unavailable", domain.FileDiff{Omitted: domain.OmitUnavailable, Additions: 40}, "too large to show in the browser", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := fileNote(tt.file)
			if tt.eq {
				assert.Equal(t, tt.want, got)
				return
			}
			assert.Contains(t, got, tt.want)
		})
	}
}

func TestDiffFilesPaths(t *testing.T) {
	t.Parallel()
	fs := diffFiles([]domain.FileDiff{
		{Status: domain.FileAdded, NewPath: "a.go", Additions: 1, Hunks: []domain.Hunk{{Lines: []domain.Line{line(domain.LineAddition, 0, 1, "x")}}}},
		{Status: domain.FileDeleted, OldPath: "b.go", Deletions: 1},
		{Status: domain.FileRenamed, OldPath: "c\u202e.go", NewPath: "d.go"},
		{Status: domain.FileModified, NewPath: "big.go", Omitted: domain.OmitLarge, Additions: 3000},
	})
	assert.Equal(t, "a.go", fs[0].Path)
	assert.True(t, fs[0].Open)
	assert.Equal(t, "b.go", fs[1].Path)
	assert.False(t, fs[1].Open, "nothing to read: stays closed")
	assert.Equal(t, "c<U+202E>.go → d.go", fs[2].Path)
	assert.False(t, fs[3].Open, "a truncated file is collapsed")
	assert.Equal(t, []string{"f1", "f2", "f3", "f4"}, []string{fs[0].ID, fs[1].ID, fs[2].ID, fs[3].ID})
}

// A code cell holds exactly the text: escaped, with markers, and nothing
// added between the pieces (the cell keeps its white space).
func TestCodeLine(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	ln := diffLine(domain.Line{Kind: domain.LineAddition, NewNumber: 1, Text: "a <b> \u202e c  d", NoNewlineAtEOF: true})
	require.NoError(t, codeLine(ln).Render(context.Background(), &b))
	assert.Equal(t, `a &lt;b&gt; <span class="inv" title="invisible character">&lt;U+202E&gt;</span> c  d`+
		`<span class="eol" title="no newline at end of file">`+noEOL+`</span>`, b.String())
}

// The gaps of a file: before a hunk the unchanged lines it does not show
// (the same number on each side), after the last hunk the rest of the file;
// none for a file whose lines are all in the diff.
func TestContextGaps(t *testing.T) {
	t.Parallel()
	ctx := func(o, n int) domain.Line { return line(domain.LineContext, o, n, "x") }
	files := []domain.FileDiff{
		{Status: domain.FileModified, NewPath: "a b.go", Hunks: []domain.Hunk{
			{OldStart: 1, OldLines: 2, NewStart: 1, NewLines: 2, Lines: []domain.Line{ctx(1, 1), ctx(2, 2)}},
			{OldStart: 10, OldLines: 3, NewStart: 10, NewLines: 3, Lines: []domain.Line{ctx(10, 10), ctx(11, 11), ctx(12, 12)}},
		}},
		{Status: domain.FileModified, NewPath: "b.go", Hunks: []domain.Hunk{
			// a pure deletion: the new side has no line, "+4,0" names line 4
			{OldStart: 5, OldLines: 1, NewStart: 4, NewLines: 0, Lines: []domain.Line{line(domain.LineDeletion, 5, 0, "x")}},
		}},
		{Status: domain.FileAdded, NewPath: "c.go", Hunks: []domain.Hunk{{OldStart: 0, NewStart: 1, NewLines: 1, Lines: []domain.Line{line(domain.LineAddition, 0, 1, "x")}}}},
		{Status: domain.FileModified, NewPath: "d.go", Hunks: []domain.Hunk{
			{OldStart: 3, OldLines: 1, NewStart: 3, NewLines: 1, Lines: []domain.Line{{Kind: domain.LineAddition, NewNumber: 3, Text: "end", NoNewlineAtEOF: true}}},
		}},
	}
	out := diffFiles(files)
	contextGaps(out, files, "/r/context/c")

	a := out[0]
	assert.Nil(t, a.Hunks[0].Gap, "the first hunk starts at line 1")
	require.NotNil(t, a.Hunks[1].Gap)
	assert.Equal(t, 7, a.Hunks[1].Gap.Lines, "lines 3 to 9 on both sides")
	assert.Equal(t, "/r/context/c?key=g1&n=7&new=3&old=3&path=a+b.go", a.Hunks[1].Gap.Src)
	require.NotNil(t, a.Tail)
	assert.Equal(t, "/r/context/c?key=tail&n=0&new=13&old=13&path=a+b.go", a.Tail.Src)

	b := out[1]
	require.NotNil(t, b.Hunks[0].Gap)
	assert.Equal(t, 4, b.Hunks[0].Gap.Lines, "lines 1 to 4 on both sides before the deleted line 5")
	assert.Nil(t, b.Tail, "a hunk that ends on a change ends the file")

	assert.Nil(t, out[2].Tail, "an added file shows every line")
	assert.Nil(t, out[3].Tail, "the last hunk holds the end of the file")
	assert.NotNil(t, a.Tail, "three unchanged lines after the change: the file may go on")
	assert.NotNil(t, out[3].Hunks[0].Gap)

	// Sides that do not move together (a parse the page cannot trust): no gap.
	odd := []domain.FileDiff{{Status: domain.FileModified, NewPath: "e.go", Hunks: []domain.Hunk{
		{OldStart: 10, OldLines: 1, NewStart: 12, NewLines: 1, Lines: []domain.Line{ctx(10, 12)}},
	}}}
	o := diffFiles(odd)
	contextGaps(o, odd, "/r/context/c")
	assert.Nil(t, o[0].Hunks[0].Gap)
}

func TestDiffNote(t *testing.T) {
	t.Parallel()
	files := func(w ...domain.FileOmission) domain.Diff {
		var d domain.Diff
		for _, o := range w {
			d.Files = append(d.Files, domain.FileDiff{Omitted: o})
		}
		return d
	}
	assert.Empty(t, diffNote(files(domain.OmitGenerated, domain.OmitLarge, domain.OmitBudget)), "collapsed files have no global note")
	assert.Equal(t, "this diff is incomplete: 2 files are too large to show. the totals above count everything; clone the repository to read the rest.",
		diffNote(files(domain.OmitUnavailable, domain.OmitLarge, domain.OmitUnavailable)))
}

func TestCollapsedFileMarkup(t *testing.T) {
	t.Parallel()
	view := DiffView{ShowUnified: true}
	src := "/demo/commit/abc1234/files"
	for _, why := range []domain.FileOmission{domain.OmitGenerated, domain.OmitLarge, domain.OmitBudget} {
		raw := []domain.FileDiff{{Status: domain.FileModified, NewPath: "go.sum", Additions: 3, Omitted: why}}
		fs := diffFiles(raw)
		collapseFiles(fs, raw, fileLinks{src: src, repo: "demo", rev: "abc1234"})
		out := renderToString(t, DiffFileView(fs[0], view))
		assert.NotRegexp(t, `<details[^>]*\sopen[\s>]`, out, "omission %d", why)
		assert.Regexp(t, `<details `, out, "the file is a details element")
		assert.Contains(t, out, `<div class="dlazy" data-src="/demo/commit/abc1234/files?one=1&amp;from=0" data-manual>`)
		assert.Contains(t, out, `<a href="?file=0#f1">load diff</a>`)
		assert.NotContains(t, out, "<table")
	}
	raw := []domain.FileDiff{{Status: domain.FileModified, NewPath: "huge.txt", Additions: 3, Omitted: domain.OmitUnavailable}}
	fs := diffFiles(raw)
	collapseFiles(fs, raw, fileLinks{src: src, repo: "demo", rev: "abc1234"})
	out := renderToString(t, DiffFileView(fs[0], view))
	assert.Contains(t, out, "too large to show in the browser")
	assert.NotContains(t, out, "load diff")
	assert.NotContains(t, out, "data-manual")
	assert.Contains(t, out, `<a href="/demo/raw/abc1234/huge.txt">view raw</a>`)

	// Past the first window of a merge request the link keeps the window.
	raw = []domain.FileDiff{{NewPath: "a", Additions: 1, Omitted: domain.OmitLarge}}
	fs = diffFiles(raw)
	collapseFiles(fs, raw, fileLinks{src: "/d/merge-requests/1/files?base=b&head=h", from: 25})
	assert.Equal(t, "/d/merge-requests/1/files?base=b&head=h&one=1&from=0", fs[0].Src)
	assert.Equal(t, "?from=25&file=0#f1", fs[0].Href)
}
