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

	"bnema.dev/ermite/internal/domain"
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
	// OldHref and NewHref link the line number of a side to the comment form
	// of that side; they are set on a merge request that can be commented on.
	OldHref, NewHref string
	// Notes are the comments of a merge request on this line, and Form the
	// form to write one, when the reader asked for it.
	Notes []CommentView
	Form  *CommentForm

	// oldN and newN are the line numbers of the sides, 0 for none.
	oldN, newN int
}

// SplitRow pairs a deleted line (left) with an added one (right); a context
// line fills both sides.
type SplitRow struct{ L, R *DiffLine }

// Notes are the comments under a row of the split view: those of its left
// line, then those of its right line; a context line, on both sides, counts
// once.
func (r SplitRow) Notes() []CommentView {
	switch {
	case r.L == nil && r.R == nil:
		return nil
	case r.L == nil:
		return r.R.Notes
	case r.R == nil || r.L == r.R:
		return r.L.Notes
	}
	return append(append([]CommentView(nil), r.L.Notes...), r.R.Notes...)
}

// Form is the comment form of the row, nil when there is none.
func (r SplitRow) Form() *CommentForm {
	switch {
	case r.R != nil && r.R.Form != nil:
		return r.R.Form
	case r.L != nil:
		return r.L.Form
	}
	return nil
}

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
	ID     string
	Status string // A D M R C T
	// Key is the path comments name: the new path, the old path of a deleted
	// file. It is not fit to display (Path is).
	Key      string
	Path     string // display path, "old → new" for a rename
	Add, Del int
	Bar      Bar
	// Note is why there is no text to show, or what else changed: a binary
	// file, a mode change, a truncation, a similarity.
	Note  string
	Open  bool
	Hunks []DiffHunk
}

func lineNumber(n int) string {
	if n <= 0 {
		return ""
	}
	return itoa(n)
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
func statusLetter(s domain.FileStatus) string {
	switch s {
	case domain.FileAdded:
		return "A"
	case domain.FileDeleted:
		return "D"
	case domain.FileRenamed:
		return "R"
	case domain.FileCopied:
		return "C"
	case domain.FileTypeChanged:
		return "T"
	}
	return "M"
}

// hunkHeader is the "@@ -a,b +c,d @@ context" line of a hunk.
func hunkHeader(h domain.Hunk) string {
	s := fmt.Sprintf("@@ -%d,%d +%d,%d @@", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
	if t := h.Header; t != "" {
		s += " " + visibleText(strings.TrimSpace(t))
	}
	return s
}

// diffLine converts a line of a hunk.
func diffLine(l domain.Line) DiffLine {
	dl := DiffLine{Segs: segments(l.Text), NoEOL: l.NoNewlineAtEOF, oldN: l.OldNumber, newN: l.NewNumber}
	switch l.Kind {
	case domain.LineAddition:
		dl.Kind, dl.Mark, dl.New = "add", "+", lineNumber(l.NewNumber)
	case domain.LineDeletion:
		dl.Kind, dl.Mark, dl.Old = "del", "-", lineNumber(l.OldNumber)
	default:
		dl.Kind, dl.Mark = "ctx", " "
		dl.Old, dl.New = lineNumber(l.OldNumber), lineNumber(l.NewNumber)
	}
	return dl
}

// diffFiles converts the files of a diff for the page.
func diffFiles(files []domain.FileDiff) []DiffFile {
	most := 0
	for _, f := range files {
		most = max(most, f.Additions+f.Deletions)
	}
	out := make([]DiffFile, 0, len(files))
	for i, f := range files {
		df := DiffFile{
			ID: "f" + itoa(i+1), Status: statusLetter(f.Status),
			Add: f.Additions, Del: f.Deletions,
			Bar: changeBar(f.Additions, f.Deletions, most),
			// A file that is shown in full opens; a file with nothing to read
			// (binary, truncated) stays closed under its note.
			Open: len(f.Hunks) > 0 && !f.Truncated,
		}
		df.Key = f.NewPath
		switch f.Status {
		case domain.FileDeleted:
			df.Key = f.OldPath
			df.Path = visibleText(f.OldPath)
		case domain.FileRenamed, domain.FileCopied:
			df.Path = visibleText(f.OldPath) + " → " + visibleText(f.NewPath)
		default:
			df.Path = visibleText(f.NewPath)
		}
		df.Note = fileNote(f)
		if f.Binary {
			df.Bar = Bar{Rest: "bin"}
		}
		for _, h := range f.Hunks {
			dh := DiffHunk{Header: hunkHeader(h)}
			for _, l := range h.Lines {
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
func fileNote(f domain.FileDiff) string {
	var parts []string
	switch {
	case f.Binary:
		parts = append(parts, "binary file: no text diff")
	case f.Truncated:
		parts = append(parts, fmt.Sprintf("diff too large to show (%d lines changed): collapsed", f.Additions+f.Deletions))
	}
	if f.OldMode != 0 && f.NewMode != 0 && f.OldMode != f.NewMode {
		parts = append(parts, fmt.Sprintf("mode %06o → %06o", f.OldMode, f.NewMode))
	}
	switch f.Status {
	case domain.FileRenamed:
		parts = append(parts, fmt.Sprintf("renamed, %d%% similar", f.Similarity))
	case domain.FileCopied:
		parts = append(parts, fmt.Sprintf("copied, %d%% similar", f.Similarity))
	case domain.FileTypeChanged:
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
