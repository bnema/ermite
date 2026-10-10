// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"html"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/a-h/templ"

	pb "bnema.dev/ermite-api/gen/ermite"
)

// Seg is a piece of a line of code: plain text, or the visible marker of an
// invisible character (Trojan Source, ADR 0017).
type Seg struct {
	Text string
	Mark bool
}

// DiffLine is one line of a hunk, in the unified view and, paired, in the
// split view.
type DiffLine struct {
	Kind     string // "ctx", "add", "del"
	Mark     string // " ", "+", "-": every change keeps its marker, not only a colour
	Old, New string // line numbers, "" when the side has none
	Segs     []Seg
	NoEOL    bool // the side ends without a line break
}

// SplitRow pairs a deleted line (left) with an added one (right); a context
// line fills both sides.
type SplitRow struct{ L, R *DiffLine }

// DiffHunk is a changed region.
type DiffHunk struct {
	Header string
	Lines  []DiffLine
	Split  []SplitRow
}

// Bar is the change bar of the file list, "+++--·····", in three runs so each
// can carry its colour.
type Bar struct{ Add, Del, Rest string }

// DiffFile is one changed file.
type DiffFile struct {
	ID       string
	Status   string // A D M R C T
	Path     string // display path, "old → new" for a rename
	Add, Del int
	Bar      Bar
	// Note is why there is no text to show, or what else changed: a binary
	// file, a mode change, a truncation, a similarity.
	Note  string
	Open  bool
	Hunks []DiffHunk
}

func lineNumber(n int32) string {
	if n <= 0 {
		return ""
	}
	return itoa(int(n))
}

// segments splits a line of code into plain text and markers. One trailing
// carriage return, the terminator of a CRLF file, is dropped.
func segments(text string) []Seg {
	text = strings.TrimSuffix(text, "\r")
	if !strings.ContainsFunc(text, invisible) {
		return []Seg{{Text: text}}
	}
	var out []Seg
	start := 0
	for i, r := range text {
		if !invisible(r) {
			continue
		}
		if i > start {
			out = append(out, Seg{Text: text[start:i]})
		}
		out = append(out, Seg{Text: marker(r), Mark: true})
		start = i + utf8.RuneLen(r)
	}
	if start < len(text) {
		out = append(out, Seg{Text: text[start:]})
	}
	return out
}

// messageText is a commit message for display: line breaks normalised, the
// invisible characters shown.
func messageText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return visibleText(strings.TrimRight(s, "\n"))
}

// hasMarker reports whether a line shows a marker of an invisible character.
func hasMarker(l DiffLine) bool {
	return slices.ContainsFunc(l.Segs, func(s Seg) bool { return s.Mark })
}

// pairLines builds the split view: a run of deletions lines up row by row
// with the run of additions that follows it; context fills both sides.
func pairLines(ls []DiffLine) []SplitRow {
	var out []SplitRow
	for i := 0; i < len(ls); {
		if ls[i].Kind == "ctx" {
			out = append(out, SplitRow{&ls[i], &ls[i]})
			i++
			continue
		}
		var dels, adds []*DiffLine
		for ; i < len(ls) && ls[i].Kind == "del"; i++ {
			dels = append(dels, &ls[i])
		}
		for ; i < len(ls) && ls[i].Kind == "add"; i++ {
			adds = append(adds, &ls[i])
		}
		for j := 0; j < len(dels) || j < len(adds); j++ {
			var row SplitRow
			if j < len(dels) {
				row.L = dels[j]
			}
			if j < len(adds) {
				row.R = adds[j]
			}
			out = append(out, row)
		}
	}
	return out
}

// changeBar draws the change bar of a file, scaled to the largest file.
func changeBar(add, del, most int) Bar {
	const cells = 10
	if most == 0 {
		return Bar{Rest: strings.Repeat("·", cells)}
	}
	scale := func(n int) int {
		c := (n*cells + most - 1) / most
		if n > 0 && c == 0 {
			c = 1
		}
		return c
	}
	a, d := scale(add), scale(del)
	if a+d > cells {
		d = cells - a
	}
	return Bar{strings.Repeat("+", a), strings.Repeat("-", d), strings.Repeat("·", cells-a-d)}
}

// statusLetter is the one-letter status of a file.
func statusLetter(s pb.FileStatus) string {
	switch s {
	case pb.FileStatus_FILE_STATUS_ADDED:
		return "A"
	case pb.FileStatus_FILE_STATUS_DELETED:
		return "D"
	case pb.FileStatus_FILE_STATUS_RENAMED:
		return "R"
	case pb.FileStatus_FILE_STATUS_COPIED:
		return "C"
	case pb.FileStatus_FILE_STATUS_TYPE_CHANGED:
		return "T"
	}
	return "M"
}

// hunkHeader is the "@@ -a,b +c,d @@ context" line of a hunk.
func hunkHeader(h *pb.Hunk) string {
	s := fmt.Sprintf("@@ -%d,%d +%d,%d @@", h.GetOldStart(), h.GetOldLines(), h.GetNewStart(), h.GetNewLines())
	if t := h.GetHeader(); t != "" {
		s += " " + visibleText(strings.TrimSpace(t))
	}
	return s
}

// diffLine converts a line of the API.
func diffLine(l *pb.Line) DiffLine {
	dl := DiffLine{Segs: segments(l.GetText()), NoEOL: l.GetNoNewlineAtEof()}
	switch l.GetKind() {
	case pb.LineKind_LINE_KIND_ADDITION:
		dl.Kind, dl.Mark, dl.New = "add", "+", lineNumber(l.GetNewNumber())
	case pb.LineKind_LINE_KIND_DELETION:
		dl.Kind, dl.Mark, dl.Old = "del", "-", lineNumber(l.GetOldNumber())
	default:
		dl.Kind, dl.Mark = "ctx", " "
		dl.Old, dl.New = lineNumber(l.GetOldNumber()), lineNumber(l.GetNewNumber())
	}
	return dl
}

// diffFiles converts the files of a diff for the page.
func diffFiles(files []*pb.FileDiff) []DiffFile {
	most := 0
	for _, f := range files {
		most = max(most, int(f.GetAdditions()+f.GetDeletions()))
	}
	out := make([]DiffFile, 0, len(files))
	for i, f := range files {
		df := DiffFile{
			ID: "f" + itoa(i+1), Status: statusLetter(f.GetStatus()),
			Add: int(f.GetAdditions()), Del: int(f.GetDeletions()),
			Bar: changeBar(int(f.GetAdditions()), int(f.GetDeletions()), most),
			// A file that is shown in full opens; a file with nothing to read
			// (binary, truncated) stays closed under its note.
			Open: len(f.GetHunks()) > 0 && !f.GetTruncated(),
		}
		switch f.GetStatus() {
		case pb.FileStatus_FILE_STATUS_DELETED:
			df.Path = visibleText(f.GetOldPath())
		case pb.FileStatus_FILE_STATUS_RENAMED, pb.FileStatus_FILE_STATUS_COPIED:
			df.Path = visibleText(f.GetOldPath()) + " → " + visibleText(f.GetNewPath())
		default:
			df.Path = visibleText(f.GetNewPath())
		}
		df.Note = fileNote(f)
		if f.GetBinary() {
			df.Bar = Bar{Rest: "bin"}
		}
		for _, h := range f.GetHunks() {
			dh := DiffHunk{Header: hunkHeader(h)}
			for _, l := range h.GetLines() {
				dh.Lines = append(dh.Lines, diffLine(l))
			}
			dh.Split = pairLines(dh.Lines)
			df.Hunks = append(df.Hunks, dh)
		}
		out = append(out, df)
	}
	return out
}

// fileNote says what the hunks cannot: the kind of change when there is no
// text to show, the mode change, the similarity of a rename.
func fileNote(f *pb.FileDiff) string {
	var parts []string
	switch {
	case f.GetBinary():
		parts = append(parts, "binary file: no text diff")
	case f.GetTruncated():
		parts = append(parts, fmt.Sprintf("diff too large to show (%d lines changed): collapsed", f.GetAdditions()+f.GetDeletions()))
	}
	if f.GetOldMode() != 0 && f.GetNewMode() != 0 && f.GetOldMode() != f.GetNewMode() {
		parts = append(parts, fmt.Sprintf("mode %06o → %06o", f.GetOldMode(), f.GetNewMode()))
	}
	switch f.GetStatus() {
	case pb.FileStatus_FILE_STATUS_RENAMED:
		parts = append(parts, fmt.Sprintf("renamed, %d%% similar", f.GetSimilarity()))
	case pb.FileStatus_FILE_STATUS_COPIED:
		parts = append(parts, fmt.Sprintf("copied, %d%% similar", f.GetSimilarity()))
	case pb.FileStatus_FILE_STATUS_TYPE_CHANGED:
		parts = append(parts, "type changed")
	}
	return strings.Join(parts, " · ")
}

// noEOL marks a side that ends without a line break.
const noEOL = "\u23ce\u0338"

// codeLine renders the text of a line of code. It writes exactly the text,
// escaped, and the markers: code cells keep their white space (pre), so the
// spaces a template adds between expressions would show up in the code.
func codeLine(ln DiffLine) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		var b strings.Builder
		for _, s := range ln.Segs {
			if s.Mark {
				b.WriteString(`<span class="inv" title="invisible character">` + html.EscapeString(s.Text) + `</span>`)
			} else {
				b.WriteString(html.EscapeString(s.Text))
			}
		}
		if ln.NoEOL {
			b.WriteString(`<span class="eol" title="no newline at end of file">` + noEOL + `</span>`)
		}
		_, err := io.WriteString(w, b.String())
		return err
	})
}
