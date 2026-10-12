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
