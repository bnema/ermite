// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
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
	// Class is the CSS class of the syntax token the text belongs to (the
	// classes of 56-chroma.css), "" for plain text and for markers.
	Class string
}

// DiffLine is one line of a hunk, in the unified view and, paired, in the
// split view.
type DiffLine struct {
	Selected bool
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
	// Gap is the unchanged lines before the hunk that the diff does not show,
	// nil when there are none or they cannot be loaded.
	Gap *DiffGap
}

// DiffGap is a run of unchanged lines a diff hides: a row of the diff that
// loads them (diffContext) and takes their place.
type DiffGap struct {
	// Key tells the gap apart within its file: the unified and the split
	// tables hold one row each for it.
	Key string
	// Lines is the number of hidden lines, 0 for the rest of the file.
	Lines int
	Src   string
}

// Label is the text of the row of the gap.
func (g DiffGap) Label() string {
	if g.Lines == 0 {
		return "show the rest of the file"
	}
	return plural(g.Lines, "hidden line", "hidden lines")
}

// DiffFile is one changed file.
type DiffFile struct {
	ID     string
	Status string // A D M R C T
	// Key is the path comments name: the new path, the old path of a deleted
	// file. It is not fit to display (Path is).
	Key      string
	Path     string // display path, "old → new" for a rename
	Add, Del int
	// Bin is true for a binary file: no lines to count.
	Bin bool
	// Note is why there is no text to show, or what else changed: a binary
	// file, a mode change, a truncation, a similarity.
	Note  string
	Open  bool
	Hunks []DiffHunk
	// Manual is true for a collapsed file (generated, large, over the page
	// budget): no hunks, a placeholder with a "load diff" control that reads
	// that one file (Src, empty when it cannot be loaded in place; Href is the
	// page without scripts).
	Manual bool
	Href   string
	// Raw is the address of the raw blob of a file too large to show.
	Raw string
	// Digits is the width of the largest line number of the file, at least 2
	// and at most 7: the gutter is as wide as the file needs (class lnw-N).
	Digits int
	// Name and Dir are the last element of the path and the directory before
	// it ("" at the root), for the compact file list.
	Name, Dir string
	// Notes is the number of comments on lines of the file, Open those not
	// resolved; the file list shows them.
	Notes, OpenNotes int
	// Lazy is true when the hunks of the file were not read: the page shows a
	// placeholder that loads them (Src, the window of files it belongs to).
	Lazy bool
	Src  string
	// From is the index of the first file of the window of a lazy file: the
	// page without scripts links to ?from=From.
	From int
	// Height is the size class of a lazy file (1 to 4, by its changed lines),
	// so that the placeholder takes about the room of the diff.
	Height int
	// Comments are the ids of the comments on the file ("c12"): a link to a
	// comment of a lazy file loads that file first.
	Comments string
	// Loose are the comments of a file loaded with its window that are on a
	// line its diff does not show: listed at the end of the file.
	Loose []CommentView
	// Tail is the rest of the file after its last hunk, nil when it cannot be
	// loaded. Its length is not known until it is.
	Tail *DiffGap
}

// diffContextLines is the number of unchanged lines Git shows around a change
// (the -U3 of the diffs of the Git adapter, Git's default).
const diffContextLines = 3

// contextGaps gives the files of a diff the gaps around their hunks, which
// load the hidden lines of the new side from base (/<repo>/context/<commit>,
// the commit of the new side). Only a file whose two sides hold text has
// them: an added or a deleted file shows every line already.
func contextGaps(out []DiffFile, files []domain.FileDiff, base string) {
	for i, f := range files {
		if i >= len(out) || f.Binary || f.Omitted != domain.OmitNone || len(f.Hunks) == 0 || len(out[i].Hunks) != len(f.Hunks) {
			continue
		}
		switch f.Status {
		case domain.FileModified, domain.FileRenamed, domain.FileCopied:
		default:
			continue
		}
		gap := func(key string, oldFrom, newFrom, n int) *DiffGap {
			q := url.Values{}
			q.Set("path", f.NewPath)
			q.Set("old", itoa(oldFrom))
			q.Set("new", itoa(newFrom))
			q.Set("n", itoa(n))
			q.Set("key", key)
			return &DiffGap{Key: key, Lines: n, Src: base + "?" + q.Encode()}
		}
		// first is the first line of a side of a hunk: a side without lines
		// names the line before the hunk ("-5,0").
		first := func(start, lines int) int {
			if lines == 0 {
				return start + 1
			}
			return start
		}
		oldNext, newNext := 1, 1
		for j, h := range f.Hunks {
			oldFirst, newFirst := first(h.OldStart, h.OldLines), first(h.NewStart, h.NewLines)
			if n := newFirst - newNext; n > 0 && n == oldFirst-oldNext {
				out[i].Hunks[j].Gap = gap("g"+itoa(j), oldNext, newNext, n)
			}
			oldNext, newNext = oldFirst+h.OldLines, newFirst+h.NewLines
		}
		// Git ends a hunk diffContextLines lines after its last change, or at
		// the end of the file: fewer lines after the last change mean the
		// file ends there.
		last := f.Hunks[len(f.Hunks)-1].Lines
		trailing := 0
		for k := len(last) - 1; k >= 0 && last[k].Kind == domain.LineContext && !last[k].NoNewlineAtEOF; k-- {
			trailing++
		}
		if trailing >= diffContextLines {
			out[i].Tail = gap("tail", oldNext, newNext, 0)
		}
	}
}

// gutterDigits is the number of digits of the largest line number of the
// hunks, clamped to the widths the style sheet has a class for.
func gutterDigits(hunks []domain.Hunk) int {
	most := 0
	for _, h := range hunks {
		most = max(most, h.OldStart+h.OldLines, h.NewStart+h.NewLines)
	}
	return min(max(len(itoa(most)), 2), 7)
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
	return appendSegs(nil, strings.TrimSuffix(text, "\r"), "")
}

// appendSegs appends to out the pieces of text, a piece of code of the
// syntax class class: plain text of that class, and the markers of its
// invisible characters.
func appendSegs(out []Seg, text, class string) []Seg {
	if !strings.ContainsFunc(text, invisible) {
		return append(out, Seg{Text: text, Class: class})
	}
	start := 0
	for i, r := range text {
		if !invisible(r) {
			continue
		}
		if i > start {
			out = append(out, Seg{Text: text[start:i], Class: class})
		}
		out = append(out, Seg{Text: marker(r), Mark: true})
		start = i + utf8.RuneLen(r)
	}
	if start < len(text) {
		out = append(out, Seg{Text: text[start:], Class: class})
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

// fileFilterMin is the number of files above which the file list has a
// filter.
const fileFilterMin = 10

// statusName says what a status letter means, for its tooltip.
func statusName(letter string) string {
	switch letter {
	case "A":
		return "added"
	case "D":
		return "deleted"
	case "R":
		return "renamed"
	case "C":
		return "copied"
	case "T":
		return "type changed"
	}
	return "modified"
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
	out := make([]DiffFile, 0, len(files))
	for i, f := range files {
		df := DiffFile{
			ID: "f" + itoa(i+1), Status: statusLetter(f.Status),
			Add: f.Additions, Del: f.Deletions, Bin: f.Binary,
			// A file that is shown in full opens; a file with nothing to read
			// (binary, truncated) stays closed under its note.
			Open:   len(f.Hunks) > 0 && f.Omitted == domain.OmitNone,
			Manual: f.Omitted.Collapsed(),
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
		df.Digits = gutterDigits(f.Hunks)
		df.Name, df.Dir = splitPath(df.Key)
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
	case f.Omitted == domain.OmitGenerated:
		parts = append(parts, "generated file: collapsed")
	case f.Omitted == domain.OmitLarge:
		parts = append(parts, fmt.Sprintf("large diff (%d lines changed): collapsed", f.Additions+f.Deletions))
	case f.Omitted == domain.OmitBudget:
		parts = append(parts, "collapsed to keep the page light")
	case f.Omitted == domain.OmitUnavailable:
		parts = append(parts, "too large to show in the browser")
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
			switch {
			case s.Mark:
				b.WriteString(`<span class="inv" title="invisible character">` + html.EscapeString(s.Text) + `</span>`)
			case s.Class != "":
				b.WriteString(`<span class="` + html.EscapeString(s.Class) + `">` + html.EscapeString(s.Text) + `</span>`)
			default:
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

// fileLinks says where the collapsed files of a diff are loaded from.
type fileLinks struct {
	// src is the address of the fragment route of the diff ("" when files
	// cannot be loaded in place); one file is src plus one=1&from=i.
	src string
	// from is the window of the page, kept in the links of a page without
	// scripts.
	from int
	// repo and rev name the raw blob of a file that cannot be shown.
	repo, rev string
}

// oneFileSrc is the address of the fragment holding file i alone.
func oneFileSrc(src string, i int) string {
	sep := "?"
	if strings.Contains(src, "?") {
		sep = "&"
	}
	return src + sep + "one=1&from=" + itoa(i)
}

// collapseFiles gives the collapsed files of a diff their load address, and
// the files too large to show a link to their raw blob. Nothing is read.
func collapseFiles(out []DiffFile, files []domain.FileDiff, l fileLinks) {
	for i, f := range files {
		if i >= len(out) {
			break
		}
		switch {
		case f.Omitted.Collapsed():
			if l.src != "" {
				out[i].Src = oneFileSrc(l.src, i)
			}
			q := "?file=" + itoa(i)
			if l.from > 0 {
				q = "?from=" + itoa(l.from) + "&file=" + itoa(i)
			}
			out[i].Href = q + "#" + out[i].ID
		default:
			out[i].Raw = rawLink(f, l)
		}
	}
}

// rawLink is the address of the raw blob of a file too large to show, "" for
// any other file.
func rawLink(f domain.FileDiff, l fileLinks) string {
	if f.Omitted != domain.OmitUnavailable || f.Status == domain.FileDeleted || l.rev == "" {
		return ""
	}
	return repoHref(l.repo, "raw", l.rev, f.NewPath)
}

// fileParam reads ?file= of a page without scripts: the index of a collapsed
// file to read in full, in [0, n).
func fileParam(r *http.Request, n int) (int, bool) {
	s := r.URL.Query().Get("file")
	i, err := strconv.Atoi(s)
	if err != nil || i < 0 || i >= n || strconv.Itoa(i) != s {
		return 0, false
	}
	return i, true
}

// fragmentView lays out a one-file fragment like the page that asks for it:
// ?view= is the layout shown and ?layouts= says whether the page holds both.
func fragmentView(r *http.Request, lines int) DiffView {
	v := newDiffView(r, lines, "", "")
	switch r.URL.Query().Get("layouts") {
	case "both":
		v.Both = true
	case "one":
		v.Both = false
	}
	v.ShowUnified, v.ShowSplit = v.Both || v.View == "unified", v.Both || v.View == "split"
	return v
}
