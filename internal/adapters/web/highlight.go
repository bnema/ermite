// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"html"
	"html/template"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

// code is a source file ready for the code view: one <span class="line"> per
// line, each with its number as an anchor, tokens as chroma CSS classes (the
// colours are in static/css/55-chroma.css, no inline style).
type code struct {
	html  template.HTML
	lines int
	// lang is the name of the language the file was highlighted as, "" for
	// plain text.
	lang string
}

// highlight renders src, the content of the file name, for the code view.
// The language comes from the file name. Whatever goes wrong while lexing
// (no lexer, a lexer error or timeout) degrades to plain text: the source is
// never lost or altered. Characters that can mislead a reader (bidi
// controls, invisible and control characters) are replaced by visible
// markers.
func highlight(name, src string) code {
	src = normaliseText(src)
	var lines [][]chroma.Token
	lang := ""
	if lx := lexerFor(name); lx != nil {
		if toks, ok := tokenise(lx, src); ok {
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
	return code{html: template.HTML(b.String()), lines: len(lines), lang: lang}
}

// maxViewLines bounds the lines of the code view: every line costs about a
// hundred bytes of HTML, and a megabyte of line feeds must not become a
// hundred megabytes of page.
const maxViewLines = 20000

// countLines is the number of lines of src, as the code view numbers them.
func countLines(src []byte) int {
	n := strings.Count(string(src), "\n")
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

// tokenise lexes src. It reports false when the tokens do not add up to the
// source, which is what a lexer that gave up half-way (its regexp timeout)
// leaves behind.
func tokenise(lx chroma.Lexer, src string) ([]chroma.Token, bool) {
	it, err := lx.Tokenise(&chroma.TokeniseOptions{State: "root", EnsureLF: true}, src)
	if err != nil {
		return nil, false
	}
	toks := it.Tokens()
	n := 0
	for _, t := range toks {
		n += len(t.Value)
	}
	if strings.TrimRight(src, "\n") == "" {
		return nil, n == len(src) || n == len(src)+1
	}
	// A lexer may add a final line feed (EnsureNL) but nothing else.
	return toks, len(strings.TrimRight(joinTokens(toks), "\n")) == len(strings.TrimRight(src, "\n"))
}

func joinTokens(toks []chroma.Token) string {
	var b strings.Builder
	for _, t := range toks {
		b.WriteString(t.Value)
	}
	return b.String()
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

// misleading are the characters, other than the controls, that the code
// view shows as markers instead of itself: the bidirectional controls of
// "Trojan Source" attacks (CVE-2021-42574) and the characters with no
// glyph.
var misleading = map[rune]string{
	0x061C: "arabic letter mark",
	0x200B: "zero width space",
	0x200E: "left-to-right mark",
	0x200F: "right-to-left mark",
	0x202A: "left-to-right embedding",
	0x202B: "right-to-left embedding",
	0x202C: "pop directional formatting",
	0x202D: "left-to-right override",
	0x202E: "right-to-left override",
	0x2060: "word joiner",
	0x2066: "left-to-right isolate",
	0x2067: "right-to-left isolate",
	0x2068: "first strong isolate",
	0x2069: "pop directional isolate",
	0xFEFF: "zero width no-break space (byte order mark)",
}

// markerName says whether r is shown as a marker, and how it is called.
// Tab is ordinary; the line feed never reaches here.
func markerName(r rune) (string, bool) {
	if n, ok := misleading[r]; ok {
		return n, true
	}
	switch {
	case r == '\t':
		return "", false
	case r == 0x2028 || r == 0x2029:
		return "line or paragraph separator", true
	case unicode.IsControl(r):
		return "control character", true
	}
	return "", false
}

func isMarked(r rune) bool { _, ok := markerName(r); return ok }

// writeMarked writes s escaped for HTML, with each misleading character
// replaced by <span class="ctl" title="U+202E right-to-left override">
// &lt;U+202E&gt;</span>. The character itself is not output.
func writeMarked(b *strings.Builder, s string) {
	for {
		i := strings.IndexFunc(s, isMarked)
		if i < 0 {
			b.WriteString(html.EscapeString(s))
			return
		}
		b.WriteString(html.EscapeString(s[:i]))
		r, size := utf8.DecodeRuneInString(s[i:])
		name, _ := markerName(r)
		code := "U+" + strings.ToUpper(strconv.FormatInt(int64(r)+0x10000, 16)[1:])
		if r > 0xFFFF {
			code = "U+" + strings.ToUpper(strconv.FormatInt(int64(r), 16))
		}
		b.WriteString(`<span class="ctl" title="` + code + ` ` + name + `">&lt;` + code + `&gt;</span>`)
		s = s[i+size:]
	}
}
