// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

// hunkOf is a modified file with one hunk of the lines given as "+text",
// "-text" and " text".
func hunkOf(name string, lines ...string) domain.FileDiff {
	var ls []domain.Line
	o, n := 0, 0
	for _, s := range lines {
		switch s[0] {
		case '+':
			n++
			ls = append(ls, line(domain.LineAddition, 0, n, s[1:]))
		case '-':
			o++
			ls = append(ls, line(domain.LineDeletion, o, 0, s[1:]))
		default:
			o++
			n++
			ls = append(ls, line(domain.LineContext, o, n, s[1:]))
		}
	}
	return domain.FileDiff{
		Status: domain.FileModified, OldPath: name, NewPath: name,
		Hunks: []domain.Hunk{{OldStart: 1, OldLines: o, NewStart: 1, NewLines: n, Lines: ls}},
	}
}

// rendered is the HTML of the code cell of every line of the first hunk.
func rendered(t *testing.T, f DiffFile) []string {
	t.Helper()
	var out []string
	for _, l := range f.Hunks[0].Lines {
		var b strings.Builder
		require.NoError(t, codeLine(l).Render(context.Background(), &b))
		out = append(out, b.String())
	}
	return out
}

// uncachedFiles is the files of a diff coloured with no cache: every call
// lexes.
func uncachedFiles(ctx context.Context, files []domain.FileDiff) []DiffFile {
	return (*hlCache)(nil).files(ctx, files)
}

func highlighted(t *testing.T, fd domain.FileDiff) DiffFile {
	t.Helper()
	return uncachedFiles(context.Background(), []domain.FileDiff{fd})[0]
}

// A block comment that spans lines is a comment on each of them: the hunk is
// lexed as a sequence, not line by line.
func TestHighlightMultiLineComment(t *testing.T) {
	t.Parallel()
	f := highlighted(t, hunkOf("a.go",
		" package a",
		"+/* first",
		"+ second <b> */",
		"+var x = 1",
	))
	got := rendered(t, f)
	assert.Equal(t, `<span class="kn">package</span> a`, got[0])
	assert.Equal(t, `<span class="cm">/* first</span>`, got[1])
	assert.Equal(t, `<span class="cm"> second &lt;b&gt; */</span>`, got[2])
	assert.Contains(t, got[3], `<span class="kd">var</span>`)
}

// Each side is lexed on its own: a deleted line has the colours it had in
// the old file, whatever the added lines around it open.
func TestHighlightSidesApart(t *testing.T) {
	t.Parallel()
	f := highlighted(t, hunkOf("a.go",
		"-x := 1 // old",
		"-y := 2",
		"+/* new",
		"+y := 2 */",
	))
	got := rendered(t, f)
	assert.NotContains(t, got[1], `class="cm"`, "the old side has no comment open")
	assert.Equal(t, `<span class="cm">/* new</span>`, got[2])
	assert.Equal(t, `<span class="cm">y := 2 */</span>`, got[3])
	assert.Contains(t, got[0], `<span class="c1">// old</span>`)
}

// Whatever is coloured, the text of the line is untouched and escaped,
// invisible characters are markers, and the CR of a CRLF line is dropped.
func TestHighlightKeepsText(t *testing.T) {
	t.Parallel()
	texts := []string{
		`s := "<script>alert(1)</script>" // & "quoted"`,
		"if x { // \u202eadmin \u200b",
		"\tfunc() {}\r",
		"",
		`x := '<' + "é世"`,
	}
	var in []string
	for _, s := range texts {
		in = append(in, "+"+s)
	}
	f := highlighted(t, hunkOf("a.go", in...))
	plain := diffFiles([]domain.FileDiff{hunkOf("a.go", in...)})[0]
	for i, l := range f.Hunks[0].Lines {
		var got, want strings.Builder
		for _, s := range l.Segs {
			got.WriteString(s.Text)
		}
		for _, s := range plain.Hunks[0].Lines[i].Segs {
			want.WriteString(s.Text)
		}
		assert.Equal(t, want.String(), got.String(), "line %d", i)
	}
	for i, h := range rendered(t, f) {
		assert.NotContains(t, h, "<script>", "line %d", i)
		assert.NotContains(t, h, "\u202e")
		assert.NotContains(t, h, "\u200b")
		assert.NotContains(t, h, "style=")
	}
	assert.Contains(t, rendered(t, f)[1], `&lt;U+202E&gt;`)
	assert.Contains(t, rendered(t, f)[0], `&lt;script&gt;`)
}

// Only the classes of the style sheet are ever written, and the sets agree.
func TestStyledClassesMatchCSS(t *testing.T) {
	t.Parallel()
	css, err := os.ReadFile("static/css/56-chroma.css")
	require.NoError(t, err)
	got := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*\.chroma \.([a-z0-9]+) \{`).FindAllStringSubmatch(string(css), -1) {
		if m[1] != "line" && m[1] != "ctl" {
			got[m[1]] = true
		}
	}
	assert.Equal(t, got, styledClasses)
}

// No language, a binary or a truncated file, a file over a limit, a line too
// long, a lone CR: the text stays as the plain diff has it.
func TestHighlightSkips(t *testing.T) {
	t.Parallel()
	plainOf := func(fd domain.FileDiff) []DiffFile { return diffFiles([]domain.FileDiff{fd}) }
	big := make([]string, 0, 6000)
	for range 6000 {
		big = append(big, "+var longer_name_of_a_variable = 12345 // and a comment")
	}
	longLine := "+x := \"" + strings.Repeat("a", maxDiffHLLine) + "\""
	binary := hunkOf("a.go", "+package a")
	binary.Binary = true
	truncated := hunkOf("a.go", "+package a")
	truncated.Truncated = true
	for name, fd := range map[string]domain.FileDiff{
		"no extension":   hunkOf("LICENSE.unknownext", "+package a", "+func f() {}"),
		"plain text":     hunkOf("notes.txt", "+package a"),
		"binary":         binary,
		"truncated":      truncated,
		"over the limit": hunkOf("big.go", big...),
		"long line":      hunkOf("a.go", "+package a", longLine),
		"lone cr":        hunkOf("a.go", "+package a", "+x := 1\r// y"),
	} {
		got := uncachedFiles(context.Background(), []domain.FileDiff{fd})
		assert.Equal(t, plainOf(fd), got, name)
	}
}

// The files past the text budget of the request are plain. Small files and a
// small budget: only the byte accounting decides, never the time. Not
// parallel: it changes maxDiffHLTotalBytes.
func TestHighlightRequestBudget(t *testing.T) {
	saved := maxDiffHLTotalBytes
	t.Cleanup(func() { maxDiffHLTotalBytes = saved })
	line := "+var v = 1"
	one := diffHLBytes(hunkOf("a.go", line))
	maxDiffHLTotalBytes = 2 * one // two files fit, the third does not
	var files []domain.FileDiff
	for i := range 3 {
		files = append(files, hunkOf(fmt.Sprintf("f%d.go", i), line))
	}
	out := uncachedFiles(context.Background(), files)
	assert.Contains(t, rendered(t, out[0])[0], `class="kd"`)
	assert.Contains(t, rendered(t, out[1])[0], `class="kd"`)
	assert.NotContains(t, rendered(t, out[2])[0], `class=`, "over the budget of the request")
}

// What a cached entry weighs is what it holds: a file of plain lines keeps
// no room reserved for runs it does not have.
func TestHighlightDataSize(t *testing.T) {
	t.Parallel()
	var lines []domain.Line
	for i := range 2000 {
		lines = append(lines, domain.Line{Kind: domain.LineAddition, NewNumber: i + 1})
	}
	d, ok := computeFile(context.Background(), diffLexer("a.go"), []domain.Hunk{{Lines: lines}})
	require.True(t, ok)
	assert.LessOrEqual(t, cap(d.runs), len(d.runs)+len(d.runs)/4+16, "the reserve is given back")
	assert.Equal(t, 64+4*cap(d.lineEnd)+4*cap(d.runs), d.size())
	assert.Less(t, d.size(), 10_000, "about 4 bytes a line, not 20")
}

// With every lexing slot taken the diff is shown plain at once, not queued.
func TestHighlightNoSlot(t *testing.T) {
	for range cap(lexSlots) {
		lexSlots <- struct{}{}
	}
	defer func() {
		for range cap(lexSlots) {
			<-lexSlots
		}
	}()
	f := highlighted(t, hunkOf("a.go", "+package a"))
	assert.Equal(t, "package a", rendered(t, f)[0])
}

// A cancelled request lexes nothing.
func TestHighlightCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fd := hunkOf("a.go", "+package a")
	got := uncachedFiles(ctx, []domain.FileDiff{fd})
	assert.Equal(t, diffFiles([]domain.FileDiff{fd}), got)
}

// The lines /context loads are coloured with the file lexed from its start:
// a comment that opens above them colours them.
func TestHighlightContext(t *testing.T) {
	t.Parallel()
	lines := []string{"package a", "/* open", "still open", "closed */", "var x = 1", "func f() {}"}
	segs := (*hlCache)(nil).highlightContext(context.Background(), "", "a.go", lines, 2, 5)
	require.Len(t, segs, 3)
	assert.Equal(t, []Seg{{Text: "still open", Class: "cm"}}, segs[0])
	assert.Equal(t, []Seg{{Text: "closed */", Class: "cm"}}, segs[1])
	assert.Equal(t, "var", segs[2][0].Text)
	assert.Equal(t, "kd", segs[2][0].Class)

	assert.Nil(t, (*hlCache)(nil).highlightContext(context.Background(), "", "a.txt", lines, 2, 5), "no language")
	assert.Nil(t, (*hlCache)(nil).highlightContext(context.Background(), "", "a.go", lines, 2, 99), "past the end")
	assert.Nil(t, (*hlCache)(nil).highlightContext(context.Background(), "", "a.go", lines, 3, 3), "nothing to show")
}

// goSource is n lines of Go with comments, strings and keywords.
func goSource(n int) []string {
	var out []string
	for i := 0; len(out) < n; i++ {
		out = append(out,
			fmt.Sprintf("func handler%d(w http.ResponseWriter, r *http.Request) error {", i),
			fmt.Sprintf("\t// serve the page number %d", i),
			fmt.Sprintf("\tname := r.URL.Query().Get(%q)", fmt.Sprint("key", i)),
			"\t/* a block",
			"\t   comment */",
			"\tif name == \"\" {",
			"\t\treturn fmt.Errorf(\"missing %s: %w\", name, errNotFound)",
			"\t}",
			"\treturn nil",
			"}",
		)
	}
	return out[:n]
}

// largeDiff is n files of 1,000 lines of Go with 250 lines changed in place
// and 250 added, 1,500 lines a file (the limit of the diffs is 2,000 lines a
// file and 20,000 in all, domain.DefaultDiffLimits); a file is about 36 KB.
func largeDiff(n int) []domain.FileDiff {
	var files []domain.FileDiff
	for i := range n {
		src := goSource(1000)
		var ls []string
		for j, s := range src {
			switch j % 4 {
			case 0:
				ls = append(ls, "-"+s, "+"+s+" // changed")
			case 1:
				ls = append(ls, "+"+s)
			default:
				ls = append(ls, " "+s)
			}
		}
		f := hunkOf(fmt.Sprintf("pkg/f%d.go", i), ls...)
		f.OldID, f.NewID = fakeBlob(i, 0), fakeBlob(i, 1)
		files = append(files, f)
	}
	return files
}

// blobID is a made-up blob id, a different one for each n and side.
func fakeBlob(n, side int) domain.ObjectID {
	id, err := domain.ParseObjectID(fmt.Sprintf("%038x%02x", n, side))
	if err != nil {
		panic(err)
	}
	return id
}

// BenchmarkDiffHighlight measures the cost of the colours on a large diff:
// the conversion alone, the conversion with the colours, and the colours with
// the page rendering of every line; "capped" is a diff twice the size the
// request budget lexes, which shows the bound.
func BenchmarkDiffHighlight(b *testing.B) {
	files := largeDiff(6)
	ctx := context.Background()
	b.Run("plain", func(b *testing.B) {
		for b.Loop() {
			diffFiles(files)
		}
	})
	b.Run("highlighted", func(b *testing.B) {
		for b.Loop() {
			uncachedFiles(ctx, files)
		}
	})
	b.Run("highlighted+render", func(b *testing.B) {
		var w strings.Builder
		for b.Loop() {
			w.Reset()
			for _, f := range uncachedFiles(ctx, files) {
				for _, l := range f.Hunks[0].Lines {
					_ = codeLine(l).Render(ctx, &w)
				}
			}
		}
	})
	b.Run("cached", func(b *testing.B) {
		c := newHLCache(64 << 20)
		c.files(ctx, files) // the first visitor lexes
		for b.Loop() {
			c.files(ctx, files)
		}
	})
	b.Run("cached+render", func(b *testing.B) {
		c := newHLCache(64 << 20)
		c.files(ctx, files)
		var w strings.Builder
		for b.Loop() {
			w.Reset()
			for _, f := range c.files(ctx, files) {
				for _, l := range f.Hunks[0].Lines {
					_ = codeLine(l).Render(ctx, &w)
				}
			}
		}
	})
	b.Run("capped", func(b *testing.B) {
		over := largeDiff(14)
		for b.Loop() {
			uncachedFiles(ctx, over)
		}
	})
	b.Run("plain+render", func(b *testing.B) {
		var w strings.Builder
		for b.Loop() {
			w.Reset()
			for _, f := range diffFiles(files) {
				for _, l := range f.Hunks[0].Lines {
					_ = codeLine(l).Render(ctx, &w)
				}
			}
		}
	})
}
