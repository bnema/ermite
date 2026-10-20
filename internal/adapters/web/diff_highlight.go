// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2"

	"bnema.dev/ermite/internal/domain"
)

// Syntax highlighting of the diffs. The tokens are the ones of the code view
// (highlight.go, the same lexers, the same classes, the colours of
// static/css/56-chroma.css); what differs is the input. A diff holds pieces
// of files, not files, so each hunk is lexed on its own, as two sequences:
// the old side (context and deleted lines) and the new side (context and
// added lines). A multi-line construct that starts and ends inside a hunk
// (a block comment, a raw string) is then coloured on all its lines, and a
// deleted line gets the colours it had in the old file, not the ones the
// added lines around it would give it. A hunk is lexed from its first line,
// so a construct that opens above it is not known: the first lines may be
// coloured as code (the usual limit of a diff view). The lines that /context
// loads come from the whole file, which is lexed as a whole.
//
// The work is bounded, since a page is served under load and the lexers are
// not cheap: a line, a file and a request have size limits (over them
// the text stays plain), a request lexes with one of the lexing slots of the
// code view (none free: plain) and a time budget, and the language is the
// one of the file name (none: plain). Text that is not lexed, or whose tokens
// do not add up to it, is shown as before.
//
// What a lexer finds is kept (hlcache.go) by the blobs the diff compares, so
// that the visitors of one merge request or commit, and its live updates, lex
// each file once. The colours are stored as runs (an end offset and a class a
// piece of line), not as text: they are put back on the lines of the diff
// that is being rendered.

// maxDiffHLTotalBytes is the most text lexed for one request, the files of
// the diff together: the files after it are plain. A variable for the tests.
var maxDiffHLTotalBytes = 256 << 10

const (
	// maxDiffHLFileBytes is the most text of one file of a diff that is
	// lexed. The diff limits (domain.DefaultDiffLimits) allow 2,000 changed lines a
	// file; this keeps a file of very long lines out.
	maxDiffHLFileBytes = 128 << 10
	// maxDiffHLLine is the longest line that is lexed, in bytes: a minified
	// file is the slowest input for a lexer and gains nothing from colour.
	maxDiffHLLine = 2000
	// diffHLBudget is the time a request spends lexing; once over, the rest
	// of the diff is plain.
	diffHLBudget = 2 * time.Second
)

// styledClasses are the token classes that 56-chroma.css gives a style. A
// token of any other class has none worth sending: its text joins the plain
// text around it. TestStyledClassesMatchCSS keeps it equal to the style sheet.
var styledClasses = map[string]bool{
	"c": true, "c1": true, "ch": true, "cm": true, "cs": true, "dl": true,
	"k": true, "kc": true, "kd": true, "kn": true, "kr": true,
	"nc": true, "ne": true, "nn": true, "ow": true,
	"s": true, "s1": true, "s2": true, "sa": true, "sb": true, "sc": true, "sd": true,
	"se": true, "sh": true, "si": true, "sr": true, "ss": true, "sx": true,
}

// diffClass is the CSS class of a token type, "" when it has no style.
func diffClass(t chroma.TokenType) string {
	if c := tokenClass(t); styledClasses[c] {
		return c
	}
	return ""
}

// lexerMemoMax bounds the memo of lexers by file name: the names come from
// the repositories, so it is emptied when full.
const lexerMemoMax = 1024

var lexerMemo struct {
	sync.Mutex
	m map[string]chroma.Lexer
}

// diffLexer is the lexer of a file name, nil for plain text. A name without
// a language, or whose lexer only lexes plain text, has none. Finding a lexer
// matches the name against every pattern chroma knows (it is as dear as lexing
// a few hundred bytes), so the answer is kept by file name; lexers are safe to
// share.
func diffLexer(name string) chroma.Lexer {
	lexerMemo.Lock()
	lx, ok := lexerMemo.m[name]
	lexerMemo.Unlock()
	if ok {
		return lx
	}
	if lx = lexerFor(name); lx != nil && lx.Config().Name == "plaintext" {
		lx = nil
	}
	lexerMemo.Lock()
	if lexerMemo.m == nil || len(lexerMemo.m) >= lexerMemoMax {
		lexerMemo.m = map[string]chroma.Lexer{}
	}
	lexerMemo.m[name] = lx
	lexerMemo.Unlock()
	return lx
}

// lexingSlot takes one of the lexing slots of the code view without waiting;
// release gives it back. ok is false when they are all taken.
func lexingSlot() (release func(), ok bool) {
	select {
	case lexSlots <- struct{}{}:
		return func() { <-lexSlots }, true
	default:
		return nil, false
	}
}

// lexable reports whether lines can be lexed and shown with the colours put
// back on them: valid UTF-8, no carriage return inside a line (the lexer
// would take it for a line end and the lines would no longer line up), no
// line too long.
func lexable(lines []string) bool {
	for _, l := range lines {
		if len(l) > maxDiffHLLine || strings.ContainsRune(l, '\r') || !utf8.ValidString(l) {
			return false
		}
	}
	return true
}

// tokenBufs are the token buffers of lexLines.
var tokenBufs = sync.Pool{New: func() any { b := make([]chroma.Token, 0, 4096); return &b }}

// lexLines lexes lines, texts without their line end, as one sequence and
// returns the tokens of each line. nil means no colours: the lexer failed,
// ran out of time, or its tokens do not line up with the lines.
func lexLines(ctx context.Context, lx chroma.Lexer, lines []string) [][]chroma.Token {
	if len(lines) == 0 || !lexable(lines) {
		return nil
	}
	n := 0
	for _, l := range lines {
		n += len(l) + 1
	}
	var b strings.Builder
	b.Grow(n)
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	// The tokens of the whole text are a big slice, grown by doubling: one
	// buffer is reused from a lexing to the next. The lines get copies of the
	// tokens, so it is free again once they are split.
	buf := tokenBufs.Get().(*[]chroma.Token)
	toks, ok := tokeniseInto(ctx, lx, b.String(), (*buf)[:0])
	if !ok {
		tokenBufs.Put(buf)
		return nil
	}
	per := chroma.SplitTokensIntoLines(toks)
	clear(toks)
	*buf = toks[:0]
	tokenBufs.Put(buf)
	if len(per) != len(lines) {
		return nil
	}
	return per
}

// classList are the classes a run can have, "" (plain) first; a run holds the
// index.
var classList = func() []string {
	l := []string{""}
	for c := range styledClasses {
		l = append(l, c)
	}
	slices.Sort(l[1:])
	return l
}()

var classIndex = func() map[string]uint8 {
	m := make(map[string]uint8, len(classList))
	for i, c := range classList {
		m[c] = uint8(i)
	}
	return m
}()

// hlRun is a piece of a line of one class: it ends at byte end.
type hlRun struct {
	end   uint16
	class uint8
}

// hlData are the colours of the lines of a file, the lines of its hunks in
// order or all of them: the runs of line i are runs[lineEnd[i-1]:lineEnd[i]],
// none for a plain line. It is read only once built, so it is shared by the
// requests that use it.
type hlData struct {
	lineEnd []uint32
	runs    []hlRun
}

// size is what the data weighs, for the budget of the cache: what its
// arrays hold, not what they use (computeFile trims them).
func (d *hlData) size() int { return 64 + 4*cap(d.lineEnd) + 4*cap(d.runs) }

// line is the runs of line i.
func (d *hlData) line(i int) []hlRun {
	lo := uint32(0)
	if i > 0 {
		lo = d.lineEnd[i-1]
	}
	return d.runs[lo:d.lineEnd[i]]
}

// segs puts the runs on the text of a line, in the room buf gives (nil: it
// allocates); a line with invisible characters needs more and grows apart.
// nil means plain: no run, or runs that do not cover the text (data of
// another text, which no key of the cache lets through, but a wrong colour is
// worse than none).
func segs(text string, runs []hlRun, buf []Seg) []Seg {
	if len(runs) == 0 || int(runs[len(runs)-1].end) != len(text) {
		return nil
	}
	var out []Seg
	if buf == nil {
		out = make([]Seg, 0, len(runs))
	} else {
		out = buf[:0:len(runs)]
	}
	start := 0
	for _, r := range runs {
		end := int(r.end)
		if end < start || end > len(text) || int(r.class) >= len(classList) {
			return nil
		}
		out = appendSegs(out, text[start:end], classList[r.class])
		start = end
	}
	return out
}

// hlBuilder builds the hlData of a file, line by line.
type hlBuilder struct{ d hlData }

// line adds the next line: its text and its tokens (nil for none). Tokens
// that do not add up to the text leave the line plain.
func (b *hlBuilder) line(text string, toks []chroma.Token) {
	mark := len(b.d.runs)
	if text != "" && len(toks) > 0 && !b.runs(text, toks) {
		b.d.runs = b.d.runs[:mark]
	}
	b.d.lineEnd = append(b.d.lineEnd, uint32(len(b.d.runs)))
}

// runs appends the runs of a line, one for each stretch of tokens of the same
// class; false if the tokens do not add up to the text.
func (b *hlBuilder) runs(text string, toks []chroma.Token) bool {
	pos, class := 0, ""
	for _, t := range toks {
		v := strings.TrimSuffix(t.Value, "\n")
		if v == "" {
			continue
		}
		if c := diffClass(t.Type); c != class {
			if pos > 0 {
				b.d.runs = append(b.d.runs, hlRun{end: uint16(pos), class: classIndex[class]})
			}
			class = c
		}
		pos += len(v)
		if pos > len(text) {
			return false
		}
	}
	if pos != len(text) {
		return false
	}
	b.d.runs = append(b.d.runs, hlRun{end: uint16(pos), class: classIndex[class]})
	return true
}

// hunk adds the lines of a hunk, lexed as two sequences, one a side. Context
// lines take their colours from the new side, or from the old side when the
// hunk adds nothing: a side that no line needs is not lexed.
func (b *hlBuilder) hunk(ctx context.Context, lx chroma.Lexer, src []domain.Line) {
	var oldTexts, newTexts []string
	hasAdd, hasDel := false, false
	for _, l := range src {
		t := strings.TrimSuffix(l.Text, "\r")
		if l.Kind != domain.LineAddition {
			oldTexts = append(oldTexts, t)
		} else {
			hasAdd = true
		}
		if l.Kind != domain.LineDeletion {
			newTexts = append(newTexts, t)
		} else {
			hasDel = true
		}
	}
	var oldToks, newToks [][]chroma.Token
	if hasDel || !hasAdd {
		oldToks = lexLines(ctx, lx, oldTexts)
	}
	if hasAdd {
		newToks = lexLines(ctx, lx, newTexts)
	}
	tokens := func(all [][]chroma.Token, i int) []chroma.Token {
		if i < len(all) {
			return all[i]
		}
		return nil
	}
	oi, ni := 0, 0
	for _, l := range src {
		switch l.Kind {
		case domain.LineDeletion:
			b.line(oldTexts[oi], tokens(oldToks, oi))
			oi++
		case domain.LineAddition:
			b.line(newTexts[ni], tokens(newToks, ni))
			ni++
		default:
			if hasAdd {
				b.line(newTexts[ni], tokens(newToks, ni))
			} else {
				b.line(oldTexts[oi], tokens(oldToks, oi))
			}
			oi++
			ni++
		}
	}
}

// computeFile lexes the hunks of a file. ok is false when ctx ended, whatever
// was lexed is then not the whole of it and is not kept.
//
//hexcheck:ignore suspicious-business-logic-in-adapter cuts the lines of a diff into coloured runs for the page; no rule is decided here
func computeFile(ctx context.Context, lx chroma.Lexer, hunks []domain.Hunk) (d *hlData, ok bool) {
	n := 0
	for _, h := range hunks {
		n += len(h.Lines)
	}
	b := hlBuilder{d: hlData{lineEnd: make([]uint32, 0, n), runs: make([]hlRun, 0, 4*n)}}
	for _, h := range hunks {
		if ctx.Err() != nil {
			return nil, false
		}
		b.hunk(ctx, lx, h.Lines)
	}
	if ctx.Err() != nil {
		return nil, false
	}
	// The runs were reserved for four a line; a cached entry keeps only what
	// it uses (size counts the capacity).
	if cap(b.d.runs) > len(b.d.runs)+len(b.d.runs)/4+16 {
		b.d.runs = slices.Clone(b.d.runs)
	}
	return &b.d, true
}

// applyFile puts the colours on the lines of a file; false when the data is
// not the one of this file (another number of lines), nothing is changed.
func applyFile(d *hlData, f *DiffFile, hunks []domain.Hunk) bool {
	n := 0
	for _, h := range hunks {
		n += len(h.Lines)
	}
	if len(d.lineEnd) != n || len(f.Hunks) != len(hunks) {
		return false
	}
	i := 0
	// The segments of all the lines come from one slab, not one allocation a
	// line: that is most of what a cached page costs.
	slab := make([]Seg, len(d.runs))
	for j, h := range hunks {
		if len(f.Hunks[j].Lines) != len(h.Lines) {
			return false
		}
		for k, l := range h.Lines {
			runs := d.line(i)
			if s := segs(strings.TrimSuffix(l.Text, "\r"), runs, slab[:len(runs)]); s != nil {
				f.Hunks[j].Lines[k].Segs = s
				slab = slab[len(runs):]
			}
			i++
		}
	}
	return true
}

// diffHLBytes is the size of the text of the lines of a file.
func diffHLBytes(f domain.FileDiff) int {
	n, lines := 0, 0
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			n += len(l.Text) + 1
		}
		lines += len(h.Lines)
	}
	return n
}

// diffKey names the colours of a file of a diff: its blobs name the text of
// both sides, so the hunks, and the language names the lexer. The number of
// hunks and lines is a check on top. "" for a file with no blob ids, which is
// not kept.
func diffKey(lang string, f domain.FileDiff) string {
	if f.OldID.IsZero() && f.NewID.IsZero() {
		return ""
	}
	lines := 0
	for _, h := range f.Hunks {
		lines += len(h.Lines)
	}
	return "d\x00" + lang + "\x00" + f.OldID.String() + "\x00" + f.NewID.String() + "\x00" +
		strconv.Itoa(len(f.Hunks)) + ":" + strconv.Itoa(lines)
}

// files is diffFiles with the syntax colours: the files of a diff for the
// page, each one coloured from the cache or lexed. A nil cache lexes every
// time.
//
// A file is left plain when it is binary, truncated, of no known language or
// over a limit, and when it cannot be lexed now: every lexing slot is taken,
// the request has used up its share of text or its time. A file that is in
// the cache costs none of these.
func (c *hlCache) files(ctx context.Context, files []domain.FileDiff) []DiffFile {
	out := diffFiles(files)
	deadline := time.Now().Add(diffHLBudget)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	// A lexing outlives the request that started it: the others that wait for
	// it, and the cache, want the result. It ends at the same deadline.
	bctx, bcancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer bcancel()

	left := maxDiffHLTotalBytes
	for i, f := range files {
		if i >= len(out) || f.Binary || f.Omitted != domain.OmitNone || len(f.Hunks) == 0 || len(out[i].Hunks) != len(f.Hunks) {
			continue
		}
		if ctx.Err() != nil {
			return out
		}
		name := f.NewPath
		if f.Status == domain.FileDeleted {
			name = f.OldPath
		}
		// The size first: the language lookup is not free, and a file over a
		// limit does not need it.
		size := diffHLBytes(f)
		if size > maxDiffHLFileBytes {
			continue
		}
		// One lexer a file, set up once for all its lines and hunks.
		lx := diffLexer(name)
		if lx == nil {
			continue
		}
		d := c.load(ctx, diffKey(lx.Config().Name, f), func() *hlData {
			if size > left {
				return nil
			}
			release, ok := lexingSlot()
			if !ok {
				return nil
			}
			defer release()
			left -= size
			d, _ := computeFile(bctx, lx, f.Hunks)
			return d
		})
		if d != nil {
			applyFile(d, &out[i], f.Hunks)
		}
	}
	return out
}

// highlightContext colours the lines [from, to) of a file, the lines that
// /context loads. The whole file is lexed (it is in memory already), not only
// the lines shown: a comment or a string that opens above them, or closes
// below them, is known. blob is the id of the file, what its colours are kept
// under. A file over the limits is plain, as is nil.
func (c *hlCache) highlightContext(ctx context.Context, blob, name string, lines []string, from, to int) [][]Seg {
	if from >= to || to > len(lines) || len(lines) > maxViewLines {
		return nil
	}
	lx := diffLexer(name)
	if lx == nil {
		return nil
	}
	size := 0
	trimmed := make([]string, len(lines))
	for i := range lines {
		trimmed[i] = strings.TrimSuffix(lines[i], "\r")
		size += len(trimmed[i]) + 1
	}
	if size > maxDiffHLFileBytes {
		return nil
	}
	key := ""
	if blob != "" {
		key = "c\x00" + lx.Config().Name + "\x00" + blob + "\x00" + strconv.Itoa(len(lines))
	}
	deadline := time.Now().Add(diffHLBudget)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	bctx, bcancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer bcancel()
	d := c.load(ctx, key, func() *hlData {
		release, ok := lexingSlot()
		if !ok {
			return nil
		}
		defer release()
		toks := lexLines(bctx, lx, trimmed)
		if bctx.Err() != nil {
			return nil
		}
		b := hlBuilder{d: hlData{lineEnd: make([]uint32, 0, len(lines)), runs: make([]hlRun, 0, 4*len(lines))}}
		for i, t := range trimmed {
			if toks != nil {
				b.line(t, toks[i])
			} else {
				b.line(t, nil)
			}
		}
		return &b.d
	})
	if d == nil || len(d.lineEnd) != len(lines) {
		return nil
	}
	out := make([][]Seg, to-from)
	for i := from; i < to; i++ {
		out[i-from] = segs(trimmed[i], d.line(i), nil)
	}
	return out
}
