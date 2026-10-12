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

func TestChangeBar(t *testing.T) {
	t.Parallel()
	assert.Equal(t, Bar{Rest: "··········"}, changeBar(0, 0, 0))
	assert.Equal(t, Bar{Add: "++++++++++"}, changeBar(100, 0, 100))
	assert.Equal(t, Bar{Add: "+++++", Del: "-----", Rest: ""}, changeBar(50, 50, 100))
	assert.Equal(t, Bar{Add: "+", Rest: "·········"}, changeBar(1, 0, 1000), "a change is never rounded away")
	assert.Equal(t, 10, len(changeBar(7, 7, 8).Add+changeBar(7, 7, 8).Del+changeBar(7, 7, 8).Rest), "never wider than the bar")
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
		{"truncated", domain.FileDiff{Truncated: true, Additions: 2500}, "too large", false},
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
		{Status: domain.FileModified, NewPath: "big.go", Truncated: true, Additions: 3000},
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
