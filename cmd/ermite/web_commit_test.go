// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
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
