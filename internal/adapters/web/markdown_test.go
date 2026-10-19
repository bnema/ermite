// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func renderMD(t *testing.T, src string) (string, []Heading) {
	t.Helper()
	h, outline, err := newMarkdown().render([]byte(src), "demo", "main", "docs")
	require.NoError(t, err)
	return string(h), outline
}

// AC4: raw HTML, javascript: links and SVG are neutralised.
func TestMarkdownNeutralisation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, src string
		absent    []string
		present   []string
	}{
		{
			"raw html block", "<script>alert(1)</script>\n\n<div onclick=\"x()\">hi</div>\n",
			[]string{"<script", "<div", "onclick", "alert(1)</"}, nil,
		},
		{
			"raw inline html", "a <img src=x onerror=alert(1)> b <b>c</b> <iframe src=//evil></iframe>\n",
			[]string{"<img", "onerror", "<iframe", "<b>"}, []string{"<p>a "},
		},
		{
			"javascript link", "[x](javascript:alert(1)) [y](JaVaScRiPt:alert(1)) [z](java\tscript:alert(1))\n",
			[]string{"javascript", "JaVaScRiPt", "href=\"java"}, []string{"<p>x y "},
		},
		{
			"data and other schemes", "[a](data:text/html,<script>1</script>) [b](vbscript:x) [c](file:///etc/passwd) [d](mailto:a@b.c) [e](//evil.example/x) [f](ftp://h/x)\n",
			[]string{"href=\"data", "vbscript", "file:", "mailto", "evil.example", "ftp:"}, []string{"<p>a b c d e f</p>"},
		},
		{
			"autolinks", "<javascript:alert(1)> <https://ok.example/a> <me@example.org>\n",
			[]string{"href=\"javascript", "mailto:"}, []string{`href="https://ok.example/a"`, "me@example.org"},
		},
		{
			"svg and images", "![logo](x.svg) ![remote](https://evil.example/p.png) ![](data:image/svg+xml;base64,PHN2Zz4=)\n\n<svg onload=alert(1)><circle/></svg>\n",
			[]string{"<img", "<svg", "onload", "evil.example", "base64", "src="}, []string{`<span class="alt">[logo]</span>`, `<span class="alt">[remote]</span>`, `<span class="alt">[image]</span>`},
		},
		{
			"escaped text", "# <b>x</b> & \"q\"\n\nplain <u>tags</u> & \"quotes\"\n",
			[]string{"<b>", "<u>"}, []string{"&amp;", "&quot;"},
		},
		{
			"title injection", "[x](https://a.example \"t\\\" onmouseover=\\\"alert(1)\")\n",
			[]string{"onmouseover=\"", " title="}, []string{`href="https://a.example"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, _ := renderMD(t, tt.src)
			for _, s := range tt.absent {
				assert.NotContains(t, out, s)
			}
			for _, s := range tt.present {
				assert.Contains(t, out, s)
			}
			// Whatever the input: no inline script, no event handler, no
			// style, no element that loads something.
			for _, banned := range []string{"<script", "<style", "<svg", "<img", "<iframe", "<object", "<embed", "<form", " on", "style="} {
				if banned == " on" {
					assert.NotRegexp(t, ` on[a-z]+=`, out)
					continue
				}
				assert.NotContains(t, strings.ToLower(out), banned)
			}
		})
	}
}

func TestMarkdownLinks(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, src, href string }{
		{"relative from the file's directory", "[a](other.md)", `href="/demo/blob/main/docs/other.md"`},
		{"parent", "[a](../LICENSE)", `href="/demo/blob/main/LICENSE"`},
		{"cannot climb out of the repository", "[a](../../../../etc/passwd)", `href="/demo/blob/main/etc/passwd"`},
		{"root relative", "[a](/src/main.go)", `href="/demo/blob/main/src/main.go"`},
		{"fragment on a file", "[a](x.md#part)", `href="/demo/blob/main/docs/x.md#part"`},
		{"dot is the repository", "[a](../)", `href="/demo"`},
		{"escaped segments", "[a](a%20b/c%3Fd.md)", `href="/demo/blob/main/docs/a%20b/c%3Fd.md"`},
		{"https is external", "[a](https://example.org/p?q=1)", `href="https://example.org/p?q=1" rel="nofollow noopener noreferrer"`},
		{"http too", "[a](http://example.org/)", `rel="nofollow noopener noreferrer"`},
		{"heading anchor", "[a](#Install Now)", ``},
		{"heading anchor ok", "[a](#install)", `href="#md-install"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, _ := renderMD(t, tt.src)
			if tt.href == "" {
				assert.NotContains(t, out, "<a ")
				return
			}
			assert.Contains(t, out, tt.href)
		})
	}
}

func TestMarkdownOutline(t *testing.T) {
	t.Parallel()
	out, outline := renderMD(t, "# Title\n\n## Install `now`\n\n### Install `now`\n\ntext\n\n#### raw *em*\n")
	assert.Equal(t, []Heading{
		{Level: 1, ID: "md-title", Text: "Title"},
		{Level: 2, ID: "md-install-now", Text: "Install now"},
		{Level: 3, ID: "md-install-now-1", Text: "Install now"},
		{Level: 4, ID: "md-raw-em", Text: "raw em"},
	}, outline, "ids are prefixed and unique, text has no markup")
	for _, h := range outline {
		assert.Contains(t, out, `id="`+h.ID+`"`)
	}
	// A heading cannot take the id of a page element.
	out, outline = renderMD(t, "# main\n\n# keys\n")
	assert.Equal(t, "md-main", outline[0].ID)
	assert.NotContains(t, out, `id="main"`)
	assert.NotContains(t, out, `id="keys"`)
}

func TestMarkdownGFM(t *testing.T) {
	t.Parallel()
	out, _ := renderMD(t, "| a | b |\n|:-|-:|\n| 1 | 2 |\n\n~~gone~~\n\n- [x] task\n")
	assert.Contains(t, out, `<th align="right">b</th>`, "alignment is an attribute, never a style")
	assert.NotContains(t, out, "style=")
	assert.Contains(t, out, "<del>gone</del>")
	assert.Contains(t, out, `type="checkbox"`)
}

func renderUntrustedMD(t *testing.T, src string) string {
	t.Helper()
	h, err := newMarkdown().renderUntrusted([]byte(src), "issue-desc-")
	require.NoError(t, err)
	return string(h)
}

// An issue from the public form is rendered with the strictest policy.
func TestMarkdownUntrusted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, src string
		absent    []string
		present   []string
	}{
		{"script", "<script>alert(1)</script>\n", []string{"<script", "alert(1)</"}, nil},
		{"img onerror", "a <img src=x onerror=alert(1)> b\n", []string{"<img", "onerror"}, []string{"<p>a "}},
		{"javascript link", "[x](javascript:alert(1))\n", []string{"javascript", "href"}, []string{"<p>x</p>"}},
		{"data link", "[x](data:text/html,<script>alert(1)</script>)\n", []string{"data:", "href", "<script"}, nil},
		{"vbscript and file", "[a](vbscript:x) [b](file:///etc/passwd)\n", []string{"vbscript", "file:", "href"}, []string{"a b"}},
		{"relative links lose the link", "[a](../../etc) [b](/proj/blob/main/x) [c](#frag)\n", []string{"href"}, []string{"a b c"}},
		{"protocol relative", "[x](//evil.test/p)\n", []string{"href", "evil"}, nil},
		{"https is kept with ugc", "[x](https://example.org/p?q=1)\n", nil,
			[]string{`href="https://example.org/p?q=1"`, `rel="nofollow noopener noreferrer ugc"`}},
		{"http is kept", "[x](http://example.org/)\n", nil, []string{`href="http://example.org/"`, "ugc"}},
		{"credentials are refused", "[x](https://user:pw@example.org/)\n", []string{"href", "pw"}, nil},
		{"mailto is kept", "[x](mailto:a@example.org)\n", nil, []string{`href="mailto:a@example.org"`, "ugc"}},
		{"mailto with query is refused", "[x](mailto:a@example.org?cc=b@example.org)\n", []string{"href"}, nil},
		{"autolink url", "<https://example.org/a> and <javascript:alert(1)>\n", []string{"href=\"javascript"}, []string{`rel="nofollow noopener noreferrer ugc"`}},
		{"autolink email", "<a@example.org>\n", nil, []string{`href="mailto:a@example.org"`, "ugc"}},
		{"bare url (GFM)", "see https://example.org/x now\n", nil, []string{`href="https://example.org/x"`, "ugc"}},
		{"image is its alt text", "![pwn](https://evil.test/p.png)\n", []string{"<img", "evil.test"}, []string{`<span class="alt">[pwn]</span>`}},
		{"reference link to javascript", "[x][r]\n\n[r]: javascript:alert(1)\n", []string{"javascript", "href"}, nil},
		{"entity-encoded scheme", "[x](&#106;avascript:alert(1))\n", []string{"avascript:", "href"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := renderUntrustedMD(t, tt.src)
			for _, a := range tt.absent {
				assert.NotContains(t, out, a)
			}
			for _, p := range tt.present {
				assert.Contains(t, out, p)
			}
		})
	}
}

func TestMarkdownUntrustedBigInputs(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 10000)
	out := renderUntrustedMD(t, long)
	assert.Contains(t, out, long)

	rows := "| a | b |\n|---|---|\n" + strings.Repeat("| <b>x</b> | [l](javascript:1) |\n", 500)
	out = renderUntrustedMD(t, rows)
	assert.NotContains(t, out, "<b>")
	assert.NotContains(t, out, "javascript")
	assert.NotContains(t, out, "style=")

	deep := strings.Repeat("> ", 200) + "[x](javascript:1)\n"
	assert.NotContains(t, renderUntrustedMD(t, deep), "javascript")

	assert.NotContains(t, renderUntrustedMD(t, "[x](https://a.test/\"onmouseover=\"alert(1))\n"), "onmouseover=\"")
}

func TestNoteMarkdown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		source string
		links  int
	}{
		{"^2 ^99 a^2 _^2 ^^2 1^2", 1},
		{"`^2` [^2](https://example.org) [^2](javascript:alert(1))", 0},
		{"```\n^2\n```", 0},
		{"^2, ^2\n^2", 3},
	}
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			t.Parallel()
			h, err := newMarkdown().renderNote([]byte(tt.source), "demo", "main", "c1-", func(n int64) bool { return n == 2 })
			require.NoError(t, err)
			assert.Equal(t, tt.links, strings.Count(string(h), `href="#c2"`), string(h))
			assert.NotContains(t, string(h), `href="#c99"`)
			assert.NotContains(t, string(h), "javascript:")
		})
	}
	h, err := newMarkdown().renderNote([]byte("# Heading\n\n<script>alert(1)</script>\n\n**bold** ^2"), "demo", "main", "c1-", func(n int64) bool { return n == 2 })
	require.NoError(t, err)
	assert.Contains(t, string(h), `id="c1-heading"`)
	assert.Contains(t, string(h), "<strong>bold</strong>")
	assert.NotContains(t, string(h), "<script")
	assert.Contains(t, string(h), `title="note 2"`)
}
