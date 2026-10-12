// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"strings"
	"testing"

	"github.com/alecthomas/chroma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestHighlightLines(t *testing.T) {
	t.Parallel()
	v := highlight(context.Background(), "a.go", "package main\n\nfunc main() {}\n")
	assert.Equal(t, 3, v.lines)
	assert.Equal(t, "Go", v.lang)
	h := string(v.html)
	assert.Contains(t, h, `<span class="line" id="L1"><a class="ln" href="#L1">1</a><span class="kn">package</span>`)
	assert.Contains(t, h, `<span class="line" id="L2"><a class="ln" href="#L2">2</a></span>`)
	assert.NotContains(t, h, "style=")

	// No final line feed, CRLF and lone CR are lines too.
	assert.Equal(t, 2, highlight(context.Background(), "x.txt", "a\nb").lines)
	assert.Equal(t, 3, highlight(context.Background(), "x.txt", "a\r\nb\rc").lines)
	assert.Equal(t, 0, highlight(context.Background(), "x.txt", "").lines)
}

func TestHighlightFallbackIsPlainAndEscaped(t *testing.T) {
	t.Parallel()
	v := highlight(context.Background(), "unknown.zzz", "<script>alert(1)</script> & \"q\"\n")
	assert.Equal(t, "", v.lang)
	h := string(v.html)
	assert.Contains(t, h, "&lt;script&gt;alert(1)&lt;/script&gt; &amp; &#34;q&#34;")
	assert.NotContains(t, h, "<script>")

	// Invalid UTF-8 is replaced, not passed on.
	assert.Contains(t, string(highlight(context.Background(), "x.txt", "a\xffb\n").html), "a\uFFFDb")
}

func TestHighlightMarksInvisibleCharacters(t *testing.T) {
	t.Parallel()
	for r, cp := range map[rune]string{
		0x202E: "U+202E", 0x2066: "U+2066", 0x2069: "U+2069", 0x200B: "U+200B", 0xFEFF: "U+FEFF",
		0x0000: "U+0000", 0x001B: "U+001B", 0x2028: "U+2028", 0x3164: "U+3164", 0x1F: "U+001F",
	} {
		h := string(highlight(context.Background(), "x.go", "a"+string(r)+"b\n").html)
		assert.NotContains(t, h, string(r), cp)
		assert.Contains(t, h, "&lt;"+cp+"&gt;</span>", cp)
		assert.Contains(t, h, `<span class="ctl" title="`+cp+" ", cp)
	}
	// A tab and ordinary non-ASCII text are left alone.
	h := string(highlight(context.Background(), "x.txt", "\tcafé 日本\n").html)
	assert.Contains(t, h, "\tcafé 日本")
	assert.NotContains(t, h, "ctl")
	// Inside a token of a lexer too.
	h = string(highlight(context.Background(), "x.go", "x := \"a\u202eb\"\n").html)
	assert.NotContains(t, h, "\u202e")
	assert.Contains(t, h, "&lt;U+202E&gt;")
}

// The lexers do not lose or add characters: the text of the lines is the
// source, whatever the language.
func TestHighlightKeepsTheSource(t *testing.T) {
	t.Parallel()
	srcs := map[string]string{
		"a.go":       "package a\n\nvar s = `multi\nline`\n// end",
		"b.py":       "def f():\n    '''doc\n    string'''\n    return 1\n",
		"c.md":       "# T\n\n```go\nx := 1\n```\n",
		"d.json":     "{\"a\": [1, 2, {\"b\": null}]}\n",
		"e.sh":       "echo \"$(date)\" | tr a b\n",
		"f.yaml":     "a:\n  - b\n  - c: d\n",
		"Makefile":   "all:\n\t@echo hi\n",
		"g.rs":       "fn main() { let _ = \"x\"; }\n",
		"h.html":     "<p class=\"a\">x &amp; y</p>\n",
		"i.templ":    "templ A() {\n\t<p>x</p>\n}\n",
		"Dockerfile": "FROM alpine\nRUN echo hi\n",
	}
	for name, src := range srcs {
		v := highlight(context.Background(), name, src)
		var text strings.Builder
		for _, l := range strings.Split(string(v.html), "\n") {
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			text.WriteString(stripTags(l) + "\n")
		}
		want := src
		assert.Equal(t, strings.Count(strings.TrimSuffix(src, "\n"), "\n")+1, v.lines, name)
		assert.Equal(t, normSpace(want), normSpace(unescape(text.String())), name)
	}
}

func TestFirstLinesAndCount(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "a\nb\n", firstLines("a\nb\nc\n", 2))
	assert.Equal(t, "a\nb", firstLines("a\nb", 5))
	assert.Equal(t, 3, countLines([]byte("a\nb\nc")))
	assert.Equal(t, 3, countLines([]byte("a\nb\nc\n")))
	assert.Equal(t, 0, countLines(nil))
}

func TestHighlightCtxCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v := highlightCtx(ctx, "a.go", "package a\n")
	assert.Equal(t, 1, v.lines, "a cancelled context gives the plain text")
	assert.Empty(t, v.lang, "the lexer stopped")
}

// panicLexer is a lexer that panics while lexing, as chroma lexers can.
type panicLexer struct{ chroma.Lexer }

func (panicLexer) Tokenise(*chroma.TokeniseOptions, string) (chroma.Iterator, error) {
	return func() chroma.Token { panic("lexer bug") }, nil
}

func TestTokeniseRecoversFromALexerPanic(t *testing.T) {
	t.Parallel()
	toks, ok := tokenise(context.Background(), panicLexer{}, "x\n")
	assert.False(t, ok)
	assert.Nil(t, toks)
}

func TestSniffImage(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "image/png", sniffImage([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")))
	assert.Equal(t, "image/gif", sniffImage([]byte("GIF89a....")))
	assert.Equal(t, "image/jpeg", sniffImage([]byte("\xff\xd8\xff\xe0\x00\x10JFIF")))
	assert.Equal(t, "", sniffImage([]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)))
	assert.Equal(t, "", sniffImage([]byte(`<?xml version="1.0"?><svg/>`)))
	assert.Equal(t, "", sniffImage([]byte("plain")))
	assert.Equal(t, "", sniffImage(nil))
}

func TestLocate(t *testing.T) {
	t.Parallel()
	b, tag := domain.RefKindBranch, domain.RefKindTag
	d := &repoData{
		branches: []domain.Ref{ref(b, "main"), ref(b, "feature/x")},
		tags:     []domain.Ref{ref(tag, "v1/rc")},
	}
	d.head = &d.branches[0]
	for _, tt := range []struct {
		in, name, path string
		detached, ok   bool
	}{
		{"", "main", "", false, true},
		{"main/a/b.go", "main", "a/b.go", false, true},
		{"feature/x/dir/f", "feature/x", "dir/f", false, true},
		{"v1/rc", "v1/rc", "", false, true},
		{"0123abc/f.txt", "0123abc", "f.txt", true, true},
		{"feature/y/f", "", "", false, false},
		{"abc/f", "", "", false, false},
		{"MAIN/f", "", "", false, false},
	} {
		got, ok := d.locate(tt.in)
		assert.Equal(t, tt.ok, ok, tt.in)
		if !ok {
			continue
		}
		assert.Equal(t, tt.name, got.name, tt.in)
		assert.Equal(t, tt.path, got.path, tt.in)
		assert.Equal(t, tt.detached, got.detached, tt.in)
	}
	empty := &repoData{}
	_, ok := empty.locate("")
	assert.False(t, ok, "an empty repository has no head")
	require.True(t, isCommitID(strings.Repeat("a", 40)))
	assert.False(t, isCommitID(strings.Repeat("a", 41)))
	assert.False(t, isCommitID("abcdef"))
}

func TestFileCrumbs(t *testing.T) {
	t.Parallel()
	cs := pathCrumbs("demo", "blob", target{name: "feature/x", path: "src/a.go"})
	require.Len(t, cs, 5)
	assert.Equal(t, "/demo/tree/feature/x/src", cs[3].Href)
	assert.Equal(t, "/demo/blob/feature/x/src/a.go", cs[4].Href)
	assert.True(t, cs[4].Current)
	cs = pathCrumbs("demo", "tree", target{name: "main", path: "src"})
	assert.Equal(t, "/demo/tree/main/src", cs[3].Href)
	assert.Equal(t, "a<U+202E>b", pathCrumbs("demo", "tree", target{name: "main", path: "a\u202eb"})[3].Label)
}

func stripTags(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func unescape(s string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&", "&#34;", `"`, "&#39;", "'").Replace(s)
}

// normSpace drops the leading numbers of the gutter and the white space, which
// the tests do not care about.
func normSpace(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		i := 0
		for i < len(l) && l[i] >= '0' && l[i] <= '9' {
			i++
		}
		if l = strings.Join(strings.Fields(l[i:]), ""); l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func TestTokensMatch(t *testing.T) {
	t.Parallel()
	toks := func(vs ...string) []chroma.Token {
		var out []chroma.Token
		for _, v := range vs {
			out = append(out, chroma.Token{Type: chroma.Text, Value: v})
		}
		return out
	}
	for _, tt := range []struct {
		name string
		toks []chroma.Token
		src  string
		want bool
	}{
		{"same pieces", toks("ab", "c\n", "d"), "abc\nd", true},
		{"lexer added a final line feed", toks("a\n", "b", "\n"), "a\nb", true},
		{"source ends with line feeds", toks("a", "\n"), "a\n\n", true},
		{"empty", nil, "", true},
		{"empty tokens", toks("", ""), "\n", true},
		{"only line feeds", toks("\n\n"), "", true},
		{"token differs", toks("a", "x"), "ab", false},
		{"tokens stop short", toks("a"), "ab", false},
		{"tokens go past the source", toks("abc"), "ab", false},
		{"extra text after the source", toks("ab", "\nx"), "ab", false},
		{"lexer dropped a line", toks("a\n", "c"), "a\nb\nc", false},
		{"unicode", toks("é", "日本"), "é日本", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tokensMatch(tt.toks, tt.src))
		})
	}
}
