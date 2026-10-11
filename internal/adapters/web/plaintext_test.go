// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Plain text stays text; only http(s) addresses become links, without the
// punctuation that ends a sentence; invisible characters are marked.
func TestPlainParts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in string
		want     []TextPart
	}{
		{"plain", "nothing to see", []TextPart{{Text: "nothing to see"}}},
		{"empty", "", nil},
		{"link", "see https://example.org/a/b?x=1#y now", []TextPart{{Text: "see "}, {Text: "https://example.org/a/b?x=1#y", Href: "https://example.org/a/b?x=1#y"}, {Text: " now"}}},
		{"sentence end", "go to http://x.test/a.", []TextPart{{Text: "go to "}, {Text: "http://x.test/a", Href: "http://x.test/a"}, {Text: "."}}},
		{"closing bracket", "(see https://x.test/a)", []TextPart{{Text: "(see "}, {Text: "https://x.test/a", Href: "https://x.test/a"}, {Text: ")"}}},
		{"balanced bracket", "https://x.test/a_(b)", []TextPart{{Text: "https://x.test/a_(b)", Href: "https://x.test/a_(b)"}}},
		{"other schemes stay text", "javascript:alert(1) ftp://x.test data:text/html,x", []TextPart{{Text: "javascript:alert(1) ftp://x.test data:text/html,x"}}},
		{"no host", "https:// and http:///x", []TextPart{{Text: "https:// and http:///x"}}},
		{"quote ends the address", `http://x.test/"onmouseover="y`, []TextPart{{Text: "http://x.test/", Href: "http://x.test/"}, {Text: `"onmouseover="y`}}},
		{"crlf", "a\r\nb", []TextPart{{Text: "a\nb"}}},
		{"invisible", "a\u202eb", []TextPart{{Text: "a<U+202E>b"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, plainParts(tt.in))
		})
	}
}

// userText escapes the text and the address, and marks the link as untrusted.
func TestUserTextEscapes(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	require.NoError(t, userText(plainParts(`<b>x</b> & http://x.test/?a=1&b=2`)).Render(context.Background(), &b))
	assert.Equal(t, `&lt;b&gt;x&lt;/b&gt; &amp; <a href="http://x.test/?a=1&amp;b=2" rel="noopener nofollow ugc">http://x.test/?a=1&amp;b=2</a>`, b.String())
}

func TestParseOn(t *testing.T) {
	t.Parallel()
	assert.Equal(t, &lineRef{side: "new", line: 12, path: "a/b:c.go"}, parseOn("new:12:a/b:c.go"))
	assert.Equal(t, &lineRef{side: "old", line: 1, path: "x"}, parseOn("old:1:x"))
	for _, s := range []string{"", "new", "new:12", "new:12:", "new:0:x", "new:-1:x", "new:x:y", "up:1:x", "new::x"} {
		assert.Nil(t, parseOn(s), s)
	}
}

func TestShellWord(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "feat/search", shellWord("feat/search"))
	assert.Equal(t, `'a b'`, shellWord("a b"))
	assert.Equal(t, `'it'\''s'`, shellWord("it's"))
	assert.Equal(t, `'$(x)'`, shellWord("$(x)"))
}

// The notes of a split row: a context line counts once.
func TestSplitRowNotes(t *testing.T) {
	t.Parallel()
	a, b := CommentView{ID: "1"}, CommentView{ID: "2"}
	ctx := &DiffLine{Notes: []CommentView{a}}
	assert.Equal(t, []CommentView{a}, SplitRow{ctx, ctx}.Notes())
	del, add := &DiffLine{Notes: []CommentView{a}}, &DiffLine{Notes: []CommentView{b}}
	assert.Equal(t, []CommentView{a, b}, SplitRow{del, add}.Notes())
	assert.Equal(t, []CommentView{a}, SplitRow{del, nil}.Notes())
	assert.Equal(t, []CommentView{b}, SplitRow{nil, add}.Notes())
	assert.Nil(t, SplitRow{}.Notes())
}
