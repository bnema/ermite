// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
)

// Trojan Source (ADR 0017): direction controls, zero-width and control
// characters are shown as markers; ordinary text, tabs and non-Latin letters
// are not touched.
func TestSegmentsShowInvisibleCharacters(t *testing.T) {
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
			assert.Equal(t, tt.want, segments(tt.in))
		})
	}
	assert.Equal(t, "a<U+202E>b.go", visibleText("a\u202eb.go"))
	assert.Equal(t, "plain", visibleText("plain"))
	assert.Equal(t, "l1\nl<U+200B>2", messageText("l1\r\nl\u200b2\n\n"))
}

func line(kind pb.LineKind, oldN, newN int32, text string) *pb.Line {
	return &pb.Line{Kind: kind, OldNumber: oldN, NewNumber: newN, Text: text}
}

// The split view lines a run of deletions up with the additions after it;
// context fills both sides; the extra lines of the longer run have an empty
// opposite side.
func TestPairLines(t *testing.T) {
	ctx, add, del := pb.LineKind_LINE_KIND_CONTEXT, pb.LineKind_LINE_KIND_ADDITION, pb.LineKind_LINE_KIND_DELETION
	var ls []DiffLine
	for _, l := range []*pb.Line{
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
	assert.Equal(t, DiffLine{Kind: "del", Mark: "-", Old: "2", Segs: []Seg{{Text: "d1"}}}, ls[1])
	assert.Equal(t, DiffLine{Kind: "add", Mark: "+", New: "2", Segs: []Seg{{Text: "a1"}}}, ls[4])
}

func TestChangeBar(t *testing.T) {
	assert.Equal(t, Bar{Rest: "··········"}, changeBar(0, 0, 0))
	assert.Equal(t, Bar{Add: "++++++++++"}, changeBar(100, 0, 100))
	assert.Equal(t, Bar{Add: "+++++", Del: "-----", Rest: ""}, changeBar(50, 50, 100))
	assert.Equal(t, Bar{Add: "+", Rest: "·········"}, changeBar(1, 0, 1000), "a change is never rounded away")
	assert.Equal(t, 10, len(changeBar(7, 7, 8).Add+changeBar(7, 7, 8).Del+changeBar(7, 7, 8).Rest), "never wider than the bar")
}

func TestFileNotes(t *testing.T) {
	assert.Equal(t, "binary file: no text diff", fileNote(&pb.FileDiff{Binary: true, Status: pb.FileStatus_FILE_STATUS_MODIFIED}))
	assert.Equal(t, "mode 100644 → 100755", fileNote(&pb.FileDiff{Status: pb.FileStatus_FILE_STATUS_MODIFIED, OldMode: 0o100644, NewMode: 0o100755}))
	assert.Empty(t, fileNote(&pb.FileDiff{Status: pb.FileStatus_FILE_STATUS_ADDED, NewMode: 0o100644}), "an added file has no mode change")
	assert.Equal(t, "renamed, 90% similar", fileNote(&pb.FileDiff{Status: pb.FileStatus_FILE_STATUS_RENAMED, Similarity: 90, OldMode: 0o100644, NewMode: 0o100644}))
	assert.Contains(t, fileNote(&pb.FileDiff{Truncated: true, Additions: 2500}), "too large")
}

func TestDiffFilesPaths(t *testing.T) {
	fs := diffFiles([]*pb.FileDiff{
		{Status: pb.FileStatus_FILE_STATUS_ADDED, NewPath: "a.go", Additions: 1, Hunks: []*pb.Hunk{{Lines: []*pb.Line{line(pb.LineKind_LINE_KIND_ADDITION, 0, 1, "x")}}}},
		{Status: pb.FileStatus_FILE_STATUS_DELETED, OldPath: "b.go", Deletions: 1},
		{Status: pb.FileStatus_FILE_STATUS_RENAMED, OldPath: "c\u202e.go", NewPath: "d.go"},
		{Status: pb.FileStatus_FILE_STATUS_MODIFIED, NewPath: "big.go", Truncated: true, Additions: 3000},
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
	var b strings.Builder
	ln := diffLine(&pb.Line{Kind: pb.LineKind_LINE_KIND_ADDITION, NewNumber: 1, Text: "a <b> \u202e c  d", NoNewlineAtEof: true})
	require.NoError(t, codeLine(ln).Render(context.Background(), &b))
	assert.Equal(t, `a &lt;b&gt; <span class="inv" title="invisible character">&lt;U+202E&gt;</span> c  d`+
		`<span class="eol" title="no newline at end of file">`+noEOL+`</span>`, b.String())
}
