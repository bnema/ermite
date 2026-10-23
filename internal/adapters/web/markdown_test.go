// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"strings"
	"testing"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
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
	h, err := newMarkdown().renderUntrusted([]byte(src), "issue-desc-", nil)
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
			h, err := newMarkdown().renderNote([]byte(tt.source), "demo", "main", "c1-", refResolver{notes: func(n int64) bool { return n == 2 }})
			require.NoError(t, err)
			assert.Equal(t, tt.links, strings.Count(string(h), `href="#c2"`), string(h))
			assert.NotContains(t, string(h), `href="#c99"`)
			assert.NotContains(t, string(h), "javascript:")
		})
	}
	h, err := newMarkdown().renderNote([]byte("# Heading\n\n<script>alert(1)</script>\n\n**bold** ^2"), "demo", "main", "c1-", refResolver{notes: func(n int64) bool { return n == 2 }})
	require.NoError(t, err)
	assert.Contains(t, string(h), `id="c1-heading"`)
	assert.Contains(t, string(h), "<strong>bold</strong>")
	assert.NotContains(t, string(h), "<script")
	assert.Contains(t, string(h), `title="note 2"`)
}

func TestNumberReferences(t *testing.T) {
	t.Parallel()
	numbers := func(n int64) (string, domain.NumberKind, bool) {
		switch n {
		case 3:
			return "/demo/issues/3", domain.NumberIssue, true
		case 4:
			return "/demo/merge-requests/4", domain.NumberMergeRequest, true
		}
		return "", domain.NumberNone, false
	}
	tests := []struct {
		source string
		want   []string
		not    []string
	}{
		{"fix #3 and #4, not #9", []string{`<a href="/demo/issues/3" title="issue 3" class="ref">#3</a>`, `title="merge request 4"`}, []string{"/issues/9", "#9</a>"}},
		{"abc#3 _#3 ##3 1#3", nil, []string{"<a "}},
		{"&#39; &#3;", nil, []string{"<a "}},
		{"`#3`\n\n```\n#3\n```", nil, []string{"<a "}},
		{"[#3](https://example.org)", []string{`href="https://example.org"`}, []string{"/demo/issues/3"}},
		{"#3\n#3", []string{`href="/demo/issues/3"`}, nil},
		{"#03 #12345678901", nil, []string{"<a "}},
	}
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			t.Parallel()
			h, err := newMarkdown().renderIssue([]byte(tt.source), "demo", "main", "i-", numbers)
			require.NoError(t, err)
			for _, w := range tt.want {
				assert.Contains(t, string(h), w)
			}
			for _, w := range tt.not {
				assert.NotContains(t, string(h), w)
			}
			u, err := newMarkdown().renderUntrusted([]byte(tt.source), "i-", numbers)
			require.NoError(t, err)
			assert.Equal(t, strings.Count(string(h), `class="ref"`), strings.Count(string(u), `class="ref"`))
		})
	}
	// Without a resolver nothing links.
	h, err := newMarkdown().renderUntrusted([]byte("#3"), "i-", nil)
	require.NoError(t, err)
	assert.NotContains(t, string(h), "<a ")
}

func TestNumberAndNoteReferencesTogether(t *testing.T) {
	t.Parallel()
	h, err := newMarkdown().renderNote([]byte("see ^2 and #3."), "demo", "main", "c1-", refResolver{
		notes:   func(n int64) bool { return n == 2 },
		numbers: func(n int64) (string, domain.NumberKind, bool) { return "/demo/issues/3", domain.NumberIssue, n == 3 },
	})
	require.NoError(t, err)
	assert.Contains(t, string(h), `href="#c2"`)
	assert.Contains(t, string(h), `href="/demo/issues/3"`)
	assert.Contains(t, string(h), "and ")
}

func TestNumberResolverCapsLookups(t *testing.T) {
	t.Parallel()
	nums := mocks.NewIssueNumberResolver(t)
	nums.EXPECT().Kind(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(domain.NumberIssue, nil).Times(maxNumberLookups)
	srv := &Server{cfg: Config{IssueNumbers: nums}}
	resolve := srv.numberResolver(t.Context(), "demo")
	require.NotNil(t, resolve)
	for n := int64(1); n <= maxNumberLookups+10; n++ {
		_, _, ok := resolve(n)
		assert.Equal(t, n <= maxNumberLookups, ok, n)
	}
	_, _, ok := resolve(1) // memoized
	assert.True(t, ok)
}

func TestNumberReferenceTitleFromKindNotHref(t *testing.T) {
	t.Parallel()
	numbers := func(n int64) (string, domain.NumberKind, bool) {
		return "/owner/merge-requests/issues/3", domain.NumberIssue, true
	}
	h, err := newMarkdown().renderIssue([]byte("see #3"), "merge-requests", "main", "i-", numbers)
	require.NoError(t, err)
	assert.Contains(t, string(h), `title="issue 3"`)
	assert.NotContains(t, string(h), "merge request 3")
}

func TestMarkdownFencedHighlight(t *testing.T) {
	tests := []struct {
		name, info, code string
		highlighted      bool
	}{
		{"go", "go", "package main\nfunc main() {}\n", true},
		{"info suffix", "go example", "package main\n", true},
		{"unknown", "not-a-real-language", "<script>\n", false},
		{"empty", "", "<script>\n", false},
		{"html", "html", "<script>alert(1)</script>\n", true},
		{"injection", `go" onmouseover=x`, "package main\n", false},
		{"oversize", "go", strings.Repeat("x", markdownBlockBytes+1) + "\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := "```" + tt.info + "\n" + tt.code + "```\n"
			for _, untrusted := range []bool{false, true} {
				var h string
				if untrusted {
					out, err := newMarkdown().renderUntrusted([]byte(src), "test-", nil)
					require.NoError(t, err)
					h = string(out)
				} else {
					h, _ = renderMD(t, src)
				}
				assert.Equal(t, tt.highlighted, strings.Contains(h, `<pre class="chroma">`))
				assert.NotContains(t, h, "<script>")
				assert.NotContains(t, h, `style=`)
				assert.NotContains(t, h, `onmouseover`)
				if tt.name == "go" {
					assert.Contains(t, h, `<span class="kn">package</span>`)
				}
				if tt.name == "html" {
					assert.Contains(t, h, "&lt;")
					assert.Contains(t, h, "script")
				}
				if !tt.highlighted {
					assert.NotContains(t, h, "<span")
					assert.Contains(t, h, "<pre><code")
				}
			}
		})
	}
}

func TestMarkdownHighlightMarkers(t *testing.T) {
	h, _ := renderMD(t, "```go\n// invisible \u202e\n```\n")
	assert.Contains(t, h, `class="ctl"`)
	assert.Contains(t, h, "U+202E")
	assert.NotContains(t, h, "\u202e")
}

func TestMarkdownHighlightDocumentBudget(t *testing.T) {
	// Even thousands of tiny blocks may only attempt 128 lexes per document.
	h, _ := renderMD(t, strings.Repeat("```go\npackage main\n```\n\n", 1000))
	assert.LessOrEqual(t, strings.Count(h, `<pre class="chroma">`), 128)
	assert.Contains(t, h, "<pre><code")
	// The byte budget also stops blocks before the block-count budget.
	block := "```go\n//" + strings.Repeat("x", markdownBlockBytes-4) + "\n```\n\n"
	h, _ = renderMD(t, strings.Repeat(block, 6))
	assert.LessOrEqual(t, strings.Count(h, `<pre class="chroma">`), 4)
	assert.Contains(t, h, "<pre><code")
}

func TestMarkdownVideos(t *testing.T) {
	for _, tc := range []struct {
		name, source, want string
		video              bool
	}{
		{"relative", `![demo](clips/demo.mp4)`, `src="/demo/raw/main/docs/clips/demo.mp4" aria-label="demo"`, true},
		{"root mov", `![demo](/clip.MOV)`, `src="/demo/raw/main/clip.MOV"`, true},
		{"webm", `![demo](clip.webm)`, `src="/demo/raw/main/docs/clip.webm"`, true},
		{"m4v", `![demo](clip.m4v)`, `src="/demo/raw/main/docs/clip.m4v"`, true},
		{"ogv", `![demo](clip.ogv)`, `src="/demo/raw/main/docs/clip.ogv"`, true},
		{"external image", `![demo](https://github.com/demo.mp4)`, `src="https://github.com/demo.mp4"`, true},
		{"autolink", `https://github.com/user-attachments/assets/uuid`, `src="https://github.com/user-attachments/assets/uuid"`, true},
		{"angle autolink", `<https://github.com/user-attachments/assets/uuid>`, `src="https://github.com/user-attachments/assets/uuid"`, true},
		{"standalone link", `[clip](https://github.com/user-attachments/assets/uuid)`, `aria-label="clip"`, true},
		{"repo link", `[Source](https://github.com/org/project)`, `<a href="https://github.com/org/project"`, false},
		{"issue link", `[Issue](https://github.com/org/project/issues/123)`, `<a href="https://github.com/org/project/issues/123"`, false},
		{"root autolink", `https://github.com`, `<a href="https://github.com"`, false},
		{"root link", `[GitHub](https://github.com/)`, `<a href="https://github.com/"`, false},
		{"standalone video extension", `[clip](https://user-images.githubusercontent.com/demo.MP4?download=1)`, `<video controls`, true},
		{"video autolink", `https://github.com/demo.webm`, `<video controls`, true},
		{"attachment missing id", `https://github.com/user-attachments/assets/`, `<a href=`, false},
		{"attachment extra segment", `https://github.com/user-attachments/assets/id/extra`, `<a href=`, false},
		{"attachment nonvideo extension", `https://github.com/user-attachments/assets/id.png`, `<a href=`, false},
		{"attachment other host", `https://user-images.githubusercontent.com/user-attachments/assets/id`, `<a href=`, false},
		{"inline link", `see [clip](https://github.com/user-attachments/assets/uuid) here`, `<a href=`, false},
		{"blocked image", `![demo](https://evil.example/demo.mp4)`, `<span class="alt">[demo]</span>`, false},
		{"blocked link", `https://evil.example/demo.mp4`, `<a href=`, false},
		{"host suffix", `![demo](https://github.com.evil.example/demo.mp4)`, `<span class="alt">`, false},
		{"credentials", `![demo](https://evil@github.com/demo.mp4)`, `<span class="alt">`, false},
		{"protocol relative", `![demo](//github.com/demo.mp4)`, `<span class="alt">`, false},
		{"javascript", `![demo](javascript:demo.mp4)`, `<span class="alt">`, false},
		{"ordinary image", `![demo](demo.png)`, `<span class="alt">`, false},
		{"escaped alt", `![a " & \<b\>](clip.mp4)`, `aria-label="a &quot; &amp; \&lt;b\&gt;"`, true},
		{"escaped path", `![demo](<a"&b.mp4>)`, `src="/demo/raw/main/docs/a%22&amp;b.mp4"`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := renderMD(t, tc.source)
			assert.Contains(t, h, tc.want)
			assert.Equal(t, tc.video, strings.Contains(h, `<video controls preload="metadata"`))
			untrusted, err := newMarkdown().renderUntrusted([]byte(tc.source), "u-", nil)
			require.NoError(t, err)
			assert.NotContains(t, string(untrusted), "<video")
		})
	}
	t.Run("configured host", func(t *testing.T) {
		m := newMarkdown("clips.example")
		h, _, err := m.render([]byte(`![demo](https://clips.example/a.mp4)`), "demo", "main", "")
		require.NoError(t, err)
		assert.Contains(t, string(h), "<video")
		h, _, err = m.render([]byte(`![demo](https://github.com/a.mp4)`), "demo", "main", "")
		require.NoError(t, err)
		assert.NotContains(t, string(h), "<video")
	})
}

func TestMarkdownCommitAndMRReferences(t *testing.T) {
	t.Parallel()
	full := "f25742a" + strings.Repeat("b", 33)
	for _, tt := range []struct {
		source         string
		links, lookups int
	}{
		{"f25742a", 1, 1}, {full, 1, 1}, {"abcdef0", 0, 1},
		{"`" + full + " !37`", 0, 0}, {"```\n" + full + " !37\n```", 0, 0},
		{"[" + full + " !37](https://example.com)", 0, 0},
		{"[" + full + "](javascript:bad)", 0, 0},
		{"word" + full, 0, 0}, {full + "word", 0, 0}, {full + "a", 0, 0},
		{"é" + full, 0, 0}, {"https://example.com/" + full, 0, 0}, {"path/" + full, 0, 0},
		{"(!37)", 1, 0}, {"!38", 0, 0}, {"!39", 0, 0}, {"word!37", 0, 0},
		{"f25742a !37 #37", 3, 1},
	} {
		t.Run(tt.source, func(t *testing.T) {
			t.Parallel()
			calls := 0
			res := refResolver{mergeRequests: true, commits: func(sha string) (string, string, bool) {
				calls++
				return "/demo/commit/" + full, full[:7], strings.HasPrefix(sha, "f25742a")
			}, numbers: func(n int64) (string, domain.NumberKind, bool) {
				if n == 39 {
					return "/demo/issues/39", domain.NumberIssue, true
				}
				return "/demo/merge-requests/37", domain.NumberMergeRequest, n == 37
			}}
			h, err := newMarkdown().renderNote([]byte(tt.source), "demo", "main", "r-", res)
			require.NoError(t, err)
			assert.Equal(t, tt.links, strings.Count(string(h), `class="ref"`), string(h))
			assert.Equal(t, tt.lookups, calls)
			if tt.source == full {
				assert.Contains(t, string(h), `href="/demo/commit/`+full+`" class="ref">f25742a</a>`)
			}
		})
	}
}

func TestMarkdownUntrustedNoNewReferences(t *testing.T) {
	t.Parallel()
	h, err := newMarkdown().renderUntrusted([]byte("f25742a !37"), "u-", func(int64) (string, domain.NumberKind, bool) {
		t.Fatal("untrusted !N must not look up a number")
		return "", domain.NumberNone, false
	})
	require.NoError(t, err)
	assert.Equal(t, "<p>f25742a !37</p>\n", string(h))
}
