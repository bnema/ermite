// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"html/template"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

// codeView is a source file ready for the code view: one <span class="line"> per
// line, each with its number as an anchor, tokens as chroma CSS classes (the
// colours are in static/css/56-chroma.css, no inline style).
type codeView struct {
	html  template.HTML
	lines int
	// lang is the name of the language the file was highlighted as, "" for
	// plain text.
	lang string
}

// highlight renders src, the content of the file name, for the code view.
// The language comes from the file name. Whatever goes wrong while lexing
// (no lexer, a lexer error, panic or timeout, ctx done) degrades to plain
// text: the source is never lost or altered. Characters that can mislead a
// reader (bidi controls, invisible and control characters) are replaced by
// visible markers.
func highlight(ctx context.Context, name, src string) codeView {
	src = normaliseText(src)
	var lines [][]chroma.Token
	lang := ""
	if lx := lexerFor(name); lx != nil {
		if toks, ok := tokenise(ctx, lx, src); ok {
			lines, lang = chroma.SplitTokensIntoLines(toks), lx.Config().Name
		}
	}
	if lines == nil {
		for _, l := range splitSource(src) {
			lines = append(lines, []chroma.Token{{Type: chroma.Text, Value: l}})
		}
	}
	var b strings.Builder
	b.Grow(len(src) + len(lines)*64)
	for i, line := range lines {
		n := strconv.Itoa(i + 1)
		b.WriteString(`<span class="line" id="L` + n + `"><a class="ln" href="#L` + n + `">` + n + `</a>`)
		for _, tk := range line {
			v := strings.TrimRight(tk.Value, "\n")
			if v == "" {
				continue
			}
			if c := tokenClass(tk.Type); c != "" {
				b.WriteString(`<span class="` + c + `">`)
				writeMarked(&b, v)
				b.WriteString(`</span>`)
			} else {
				writeMarked(&b, v)
			}
		}
		b.WriteString("</span>\n")
	}
	// #nosec G203 -- every piece of the source goes through writeMarked, which
	// escapes it; the rest is markup and class names of this file.
	return codeView{html: template.HTML(b.String()), lines: len(lines), lang: lang}
}

// highlightBudget bounds the time spent lexing one file: a lexer on hostile
// input can be slow, and the page then shows the file as plain text.
const highlightBudget = 10 * time.Second

// lexSlots bounds the lexers running at once: hostile files requested over
// and over cannot take every CPU.
var lexSlots = make(chan struct{}, runtime.GOMAXPROCS(0))

// highlightCtx is highlight with a time budget and the cancellation of ctx:
// the lexer stops at the next token once either is over, and the file is
// shown as plain text. When every lexing slot is taken the file is shown as
// plain text right away.
func highlightCtx(ctx context.Context, name, src string) codeView {
	select {
	case lexSlots <- struct{}{}:
		defer func() { <-lexSlots }()
	default:
		return highlightPlain(src)
	}
	ctx, cancel := context.WithTimeout(ctx, highlightBudget)
	defer cancel()
	return highlight(ctx, name, src)
}

// highlightPlain is the code view of src with no language.
func highlightPlain(src string) codeView { return highlight(context.Background(), "", src) }

// maxViewLines bounds the lines of the code view: every line costs about a
// hundred bytes of HTML, and a megabyte of line feeds must not become a
// hundred megabytes of page.
const maxViewLines = 20000

// countLines is the number of lines of src, as the code view numbers them.
func countLines(src []byte) int {
	n := bytes.Count(src, []byte{'\n'})
	if len(src) > 0 && src[len(src)-1] != '\n' {
		n++
	}
	return n
}

// normaliseText makes src valid UTF-8 with "\n" line ends.
func normaliseText(src string) string {
	if !utf8.ValidString(src) {
		src = strings.ToValidUTF8(src, "\uFFFD")
	}
	if strings.Contains(src, "\r") {
		src = strings.ReplaceAll(src, "\r\n", "\n")
		src = strings.ReplaceAll(src, "\r", "\n")
	}
	return src
}

// lexerFor is the lexer for a file name, nil for plain text.
func lexerFor(name string) chroma.Lexer {
	lx := lexers.Match(path.Base(name))
	if lx == nil {
		return nil
	}
	return chroma.Coalesce(lx)
}

// tokenise lexes src, one token at a time so that it stops once ctx is done.
// It reports false then, when the lexer panics (chroma lexers can, on odd
// input), and when the tokens do not add up to the source, which is what a
// lexer that gave up half-way (its regexp timeout) leaves behind. A lexer
// may add a final line feed (EnsureNL), nothing else.
func tokenise(ctx context.Context, lx chroma.Lexer, src string) (toks []chroma.Token, ok bool) {
	defer func() {
		if recover() != nil {
			toks, ok = nil, false
		}
	}()
	it, err := lx.Tokenise(&chroma.TokeniseOptions{State: "root", EnsureLF: true}, src)
	if err != nil {
		return nil, false
	}
	for t := it(); t != chroma.EOF; t = it() {
		if len(toks)%256 == 0 && ctx.Err() != nil {
			return nil, false
		}
		toks = append(toks, t)
	}
	if !tokensMatch(toks, src) {
		return nil, false
	}
	return toks, true
}

// tokensMatch reports whether the values of the tokens, joined, are src but
// for the line feeds that end either: it compares piece by piece, where
// joining the tokens first would copy the whole file.
func tokensMatch(toks []chroma.Token, src string) bool {
	want := strings.TrimRight(src, "\n")
	pos := 0
	for _, t := range toks {
		v := t.Value
		rest := want[pos:]
		if len(v) <= len(rest) {
			if rest[:len(v)] != v {
				return false
			}
			pos += len(v)
			continue
		}
		// The token reaches past the end of the source: only line feeds may.
		if v[:len(rest)] != rest || strings.Trim(v[len(rest):], "\n") != "" {
			return false
		}
		pos = len(want)
	}
	return pos == len(want)
}

// splitSource splits plain text into lines, without the empty element after
// a final line feed.
func splitSource(src string) []string {
	if src == "" {
		return nil
	}
	ls := strings.Split(src, "\n")
	if ls[len(ls)-1] == "" {
		ls = ls[:len(ls)-1]
	}
	return ls
}

// tokenClass is the CSS class of a token type, "" for none, found the way
// chroma's HTML formatter finds it: the type, then its parents.
func tokenClass(t chroma.TokenType) string {
	for t != 0 {
		if c, ok := chroma.StandardTypes[t]; ok {
			return c
		}
		t = t.Parent()
	}
	return chroma.StandardTypes[t]
}

// invisibleNames name the characters of the code view that are shown as
// markers, for the tooltip. Any other invisible character is called by its
// category.
var invisibleNames = map[rune]string{
	0x061C: "arabic letter mark",
	0x200B: "zero width space",
	0x200C: "zero width non-joiner",
	0x200D: "zero width joiner",
	0x200E: "left-to-right mark",
	0x200F: "right-to-left mark",
	0x202A: "left-to-right embedding",
	0x202B: "right-to-left embedding",
	0x202C: "pop directional formatting",
	0x202D: "left-to-right override",
	0x202E: "right-to-left override",
	0x2028: "line separator",
	0x2029: "paragraph separator",
	0x2060: "word joiner",
	0x2066: "left-to-right isolate",
	0x2067: "right-to-left isolate",
	0x2068: "first strong isolate",
	0x2069: "pop directional isolate",
	0xFEFF: "byte order mark",
}

// codePoint is "U+202E".
func codePoint(r rune) string { return fmt.Sprintf("U+%04X", r) }

// invisibleName is what the tooltip of a marker says.
func invisibleName(r rune) string {
	if n, ok := invisibleNames[r]; ok {
		return n
	}
	if unicode.IsControl(r) {
		return "control character"
	}
	return "invisible character"
}

// writeMarked writes s escaped for HTML, each invisible character replaced
// by <span class="ctl" title="U+202E right-to-left override">&lt;U+202E&gt;
// </span>. The character itself is never output.
func writeMarked(b *strings.Builder, s string) {
	for {
		i := strings.IndexFunc(s, invisible)
		if i < 0 {
			b.WriteString(html.EscapeString(s))
			return
		}
		b.WriteString(html.EscapeString(s[:i]))
		r, size := utf8.DecodeRuneInString(s[i:])
		cp := codePoint(r)
		b.WriteString(`<span class="ctl" title="` + cp + ` ` + invisibleName(r) + `">&lt;` + cp + `&gt;</span>`)
		s = s[i+size:]
	}
}
