// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

// AC1: the commit pages render for the fixtures with the structure of the
// mockup (golden HTML, normalised).
func TestWebCommitGolden(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	f := h.diffRepo("diffs")

	for _, tt := range []struct{ name, path string }{
		{"commit-modify", "/diffs/commit/" + f.modify},
		{"commit-modify-split", "/diffs/commit/" + f.modify + "?view=split"},
		{"commit-root", "/diffs/commit/" + f.root},
		{"commit-rename", "/diffs/commit/" + f.rename},
		{"commit-binary", "/diffs/commit/" + f.binary},
		{"commit-merge", "/diffs/commit/" + f.merge},
		{"commit-trojan", "/diffs/commit/" + f.trojan},
		{"commit-big", "/diffs/commit/" + f.big},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status, hdr, body := h.get(tt.path)
			require.Equal(t, 200, status)
			assert.Equal(t, "no-cache", hdr.Get("Cache-Control"))
			assertNoInline(t, body)
			golden(t, tt.name, normalise(body))
		})
	}
}

// The behaviours of the commit pages, asserted on their own.
func TestWebCommit(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	f := h.diffRepo("diffs")
	get := func(path string, status int) string {
		t.Helper()
		st, _, body := h.get(path)
		require.Equal(t, status, st, path)
		return body
	}

	t.Run("commit", func(t *testing.T) {
		body := get("/diffs/commit/"+f.modify, 200)
		assert.Contains(t, body, "<h1>modify a file and make a script executable</h1>")
		assert.Contains(t, body, "The body has a second paragraph.")
		assert.Contains(t, body, "And &lt;b&gt;html&lt;/b&gt; &amp;", "the message is text")
		assert.NotContains(t, body, "<b>html</b>")
		assert.Contains(t, body, "mode 100644 → 100755")
		assert.Contains(t, body, `<td class="m">+</td>`, "added lines keep their marker")
		assert.Contains(t, body, `<td class="m">-</td>`, "deleted lines keep their marker")
		assert.Contains(t, body, `data-diff="unified"`)
		assert.Contains(t, body, `hx-live="this.dataset.diff = sessionStorage`, "the browser remembers the layout when the URL does not choose")
		assert.Contains(t, body, `<table class="dt uni"`)
		assert.Contains(t, body, `<table class="dt spl"`, "both layouts are sent, the toggle needs no request")
		assert.Contains(t, body, `href="/diffs/commit/`+f.modify+`?view=split"`, "the toggle works without scripts")
		// The abbreviated id opens the same commit.
		assert.Equal(t, normalise(body), normalise(get("/diffs/commit/"+f.modify[:12], 200)))
	})

	t.Run("split without scripts", func(t *testing.T) {
		body := get("/diffs/commit/"+f.modify+"?view=split", 200)
		assert.Contains(t, body, `data-diff="split"`)
		assert.NotContains(t, body, `hx-live="this.dataset.diff`, "the URL decides, not the browser memory")
		assert.Contains(t, body, `class="ln del"`)
		assert.Contains(t, body, `class="ln add"`)
	})

	t.Run("root commit", func(t *testing.T) {
		body := get("/diffs/commit/"+f.root, 200)
		assert.Contains(t, body, "none (root commit)")
		assert.NotContains(t, body, `class="m del"`)
	})

	t.Run("rename delete binary", func(t *testing.T) {
		body := get("/diffs/commit/"+f.rename, 200)
		assert.Contains(t, body, "old.txt → docs-old.txt")
		assert.Contains(t, body, "renamed, 100% similar")
		assert.Contains(t, body, "gone.txt")
		bin := get("/diffs/commit/"+f.binary, 200)
		assert.Contains(t, bin, "binary file: no text diff")
		assert.NotContains(t, bin, `<table class="dt`)
	})

	t.Run("merge", func(t *testing.T) {
		body := get("/diffs/commit/"+f.merge, 200)
		assert.Contains(t, body, "merge of")
		assert.Contains(t, body, "diff against the first parent")
		assert.Equal(t, 2, strings.Count(body, `href="/diffs/commit/`)-strings.Count(body, `href="/diffs/commit/`+f.merge), "two parent links")
	})

	t.Run("trojan source", func(t *testing.T) {
		body := get("/diffs/commit/"+f.trojan, 200)
		for _, bad := range []string{"\u202e", "\u2066", "\u2069", "\u200b", "\u202c"} {
			assert.NotContains(t, body, bad, "no invisible character reaches the page: %U", []rune(bad))
		}
		for _, mark := range []string{"&lt;U+202E&gt;", "&lt;U+2066&gt;", "&lt;U+2069&gt;", "&lt;U+200B&gt;", "&lt;U+202C&gt;"} {
			assert.Contains(t, body, mark)
		}
		assert.Contains(t, body, "holds invisible characters")
	})

	t.Run("truncated", func(t *testing.T) {
		body := get("/diffs/commit/"+f.big, 200)
		assert.Contains(t, body, "diff too large to show (2600 lines changed): collapsed")
		assert.Contains(t, body, "this diff is incomplete")
		assert.NotRegexp(t, `<details class="dfile" id="f1"[^>]* open`, body, "a truncated file is collapsed")
		assert.NotContains(t, body, "line 2000", "no lines of a truncated file")
	})

	t.Run("not found", func(t *testing.T) {
		_, _, missing := h.get("/diffs/commit/0123456789abcdef0123456789abcdef01234567")
		for _, p := range []string{
			"/diffs/commit/0123456789abcdef0123456789abcdef01234567", // no such commit
			"/diffs/commit/main", "/diffs/commit/abc", "/diffs/commit/ABCDEF0", "/diffs/commit/v1.0.0", // not a commit id
			"/nope/commit/" + f.root, "/diffs/commit/" + f.root + "/x",
		} {
			st, _, body := h.get(p)
			assert.Equal(t, 404, st, p)
			assert.NotContains(t, body, "<h1>root", p)
		}
		st, _, body := h.get("/diffs/commit/0123456789abcdef0123456789abcdef01234567")
		assert.Equal(t, 404, st)
		assert.Equal(t, normalise(missing), normalise(body))
	})
}

// Over 4,000 lines only one layout is sent, the one the URL asks for.
func TestWebCommitLargeDiffView(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("large", domain.VisibilityPublic)
	wt := h.workTree("main", "README")
	var lines strings.Builder
	for i := range 1500 {
		fmt.Fprintf(&lines, "line %d\n", i)
	}
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(wt, name), []byte(lines.String()), 0o644))
	}
	h.mustGit(wt, "add", ".")
	h.mustGit(wt, "commit", "-q", "-m", "three big files")
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/large.git"), "main")
	sha := strings.TrimSpace(h.mustGit(wt, "rev-parse", "HEAD"))

	_, _, unified := h.get("/large/commit/" + sha)
	assert.True(t, strings.Contains(unified, `<table class="dt uni"`), "unified by default")
	assert.False(t, strings.Contains(unified, `<table class="dt spl"`), "one layout only")

	_, _, split := h.get("/large/commit/" + sha + "?view=split")
	assert.True(t, strings.Contains(split, `<table class="dt spl"`), "the split layout is sent")
	assert.False(t, strings.Contains(split, `<table class="dt uni"`), "and only it")
	assert.True(t, strings.Contains(split, `<div id="diffs" data-view="split">`))
}

// AC2: the commit pages of a private repository are the 404 page of a
// repository that does not exist.
func TestWebCommitPrivate(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	wt := h.workTree("main", "secret.txt")
	h.createRepo("hidden-one", domain.VisibilityPrivate)
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/hidden-one.git"), "main")
	sha := strings.TrimSpace(h.mustGit(wt, "rev-parse", "HEAD"))

	_, _, missing := h.get("/never-existed")
	for _, p := range []string{
		"/hidden-one/commit/" + sha, "/hidden-one/commit/" + sha + "?view=split",
	} {
		st, hdr, body := h.get(p)
		assert.Equal(t, 404, st, p)
		assert.Equal(t, "no-cache", hdr.Get("Cache-Control"), p)
		assert.NotContains(t, body, "hidden-one", p)
		assert.NotContains(t, body, "secret.txt", p)
		assert.Equal(t, normalise(missing), normalise(body), p)
	}
}

// The hidden lines of a diff: the page has a gap before a hunk and after the
// last one, and /context serves their rows for both layouts, numbered on both
// sides; a private repository's are the 404 page.
func TestWebDiffContext(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	wt := t.TempDir()
	h.mustGit(wt, "init", "-q", "--initial-branch=main")
	var src strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&src, "line %d <b>\n", i)
	}
	h.put(wt, "f.txt", []byte(src.String()), 0o644)
	h.commitAt(wt, "2026-02-01T10:00:00Z", "forty lines")
	h.put(wt, "f.txt", []byte(strings.Replace(src.String(), "line 20 <b>", "line twenty", 1)), 0o644)
	sha := h.commitAt(wt, "2026-02-02T10:00:00Z", "change line 20")
	h.createRepo("ctx", domain.VisibilityPublic)
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/ctx.git"), "main")

	_, _, page := h.get("/ctx/commit/" + sha)
	gap := "/ctx/context/" + sha + "?key=g0&amp;n=16&amp;new=1&amp;old=1&amp;path=f.txt"
	assert.Contains(t, page, gap, "lines 1 to 16 before the hunk (17 to 23)")
	assert.Contains(t, page, "16 hidden lines")
	assert.Contains(t, page, "/ctx/context/"+sha+"?key=tail&amp;n=0&amp;new=24&amp;old=24&amp;path=f.txt")

	st, _, body := h.get("/ctx/context/" + sha + "?key=g0&n=16&new=1&old=1&path=f.txt")
	require.Equal(t, 200, st)
	assert.Equal(t, 3, strings.Count(body, "line 16 &lt;b&gt;"), "unified once, split on both sides, escaped")
	assert.NotContains(t, body, "line 17")
	assert.NotContains(t, body, `class="gap"`, "nothing left")

	st, _, body = h.get("/ctx/context/" + sha + "?key=tail&n=0&new=24&old=24&path=f.txt")
	require.Equal(t, 200, st)
	assert.Contains(t, body, "line 40 &lt;b&gt;")
	assert.NotContains(t, body, "line 23 ")

	for _, bad := range []string{
		"/ctx/context/" + sha + "?n=1&new=1&old=1",                // no path
		"/ctx/context/" + sha + "?n=1&new=0&old=1&path=f.txt",     // line 0
		"/ctx/context/" + sha + "?n=-1&new=1&old=1&path=f.txt",    // negative
		"/ctx/context/" + sha + "?n=1&new=99&old=99&path=f.txt",   // past the end
		"/ctx/context/" + sha + "?n=1&new=1&old=1&path=nope.txt",  // no such file
		"/ctx/context/" + sha[:7] + "?n=1&new=1&old=1&path=f.txt", // not a full id
		"/ctx/context/main?n=1&new=1&old=1&path=f.txt",            // a ref
	} {
		st, _, _ := h.get(bad)
		assert.Equal(t, 404, st, bad)
	}

	// Lines as Git numbers them: a lone CR is not a line break; the last line
	// of a file without a final newline keeps its marker.
	var cr strings.Builder
	cr.WriteString("a\rb\n")
	for i := 2; i <= 30; i++ {
		fmt.Fprintf(&cr, "row %d\n", i)
	}
	h.put(wt, "cr.txt", []byte(cr.String()+"last"), 0o644)
	h.commitAt(wt, "2026-02-03T10:00:00Z", "cr")
	h.put(wt, "cr.txt", []byte(strings.Replace(cr.String(), "row 15\n", "row fifteen\n", 1)+"last"), 0o644)
	crSHA := h.commitAt(wt, "2026-02-04T10:00:00Z", "change row 15")
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/ctx.git"), "main")
	_, _, crPage := h.get("/ctx/commit/" + crSHA)
	assert.Contains(t, crPage, "11 hidden lines", "lines 1 to 11 before the hunk at 12")
	st, _, body = h.get("/ctx/context/" + crSHA + "?n=11&new=1&old=1&path=cr.txt")
	require.Equal(t, 200, st)
	assert.Contains(t, body, "row 11")
	assert.NotContains(t, body, "row 12", "a lone CR does not shift the numbers")
	st, _, body = h.get("/ctx/context/" + crSHA + "?n=0&new=19&old=19&path=cr.txt")
	require.Equal(t, 200, st)
	assert.Contains(t, body, "last<span class=\"eol\"", "no newline at the end of the file")

	// A private repository: the 404 page, nothing of the file.
	h.createRepo("ctx-hidden", domain.VisibilityPrivate)
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/ctx-hidden.git"), "main")
	st, _, body = h.get("/ctx-hidden/context/" + sha + "?n=0&new=1&old=1&path=f.txt")
	assert.Equal(t, 404, st)
	assert.NotContains(t, body, "line 1 ")
}
