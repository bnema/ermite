// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// diffFixture is the repository of the commit pages: one commit for each
// kind of change a diff shows.
type diffFixture struct {
	root, modify, rename, binary, merge, trojan, big string
}

// createRepo creates a public repository for the pages.
func (h *harness) createRepo(name string, vis domain.Visibility) {
	h.t.Helper()
	_, err := h.forge.Repos.Create(h.ctx, domain.Root(), ports.CreateRepoParams{Name: h.name(name), Description: "Diffs", Visibility: vis})
	require.NoError(h.t, err)
}

// commitAt commits what is staged at a fixed date and returns the id.
func (h *harness) commitAt(wt, date, msg string) string {
	h.t.Helper()
	env := []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
	out, err := h.gitEnv(env, wt, "commit", "-q", "--allow-empty", "-m", msg)
	require.NoError(h.t, err, out)
	return strings.TrimSpace(h.mustGit(wt, "rev-parse", "HEAD"))
}

func (h *harness) put(wt, name string, data []byte, mode os.FileMode) {
	h.t.Helper()
	full := filepath.Join(wt, name)
	require.NoError(h.t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(h.t, os.WriteFile(full, data, mode))
	require.NoError(h.t, os.Chmod(full, mode))
	h.mustGit(wt, "add", name)
}

// diffRepo builds the fixture and pushes it as a public repository.
func (h *harness) diffRepo(name string) diffFixture {
	h.t.Helper()
	wt := h.t.TempDir()
	h.mustGit(wt, "init", "-q", "--initial-branch=main")
	var f diffFixture

	h.put(wt, "a.txt", []byte("one\ntwo\nthree\nfour\nfive\n"), 0o644)
	h.put(wt, "old.txt", []byte("keep me\nkeep me too\nand me\n"), 0o644)
	h.put(wt, "run.sh", []byte("#!/bin/sh\necho hi\n"), 0o644)
	h.put(wt, "gone.txt", []byte("bye\n"), 0o644)
	h.put(wt, "logo.bin", []byte{0x89, 'P', 'N', 'G', 0, 1, 2, 3}, 0o644)
	f.root = h.commitAt(wt, "2026-02-01T10:00:00Z", "root: the first files")

	h.put(wt, "a.txt", []byte("one\n2\nthree\nfour\nfive\nsix\n"), 0o644)
	h.put(wt, "run.sh", []byte("#!/bin/sh\necho hi\n"), 0o755)
	f.modify = h.commitAt(wt, "2026-02-02T10:00:00Z", "modify a file and make a script executable\n\nThe body has a second paragraph.\n\nAnd <b>html</b> & \"quotes\".")

	h.mustGit(wt, "mv", "old.txt", "docs-old.txt")
	h.mustGit(wt, "rm", "-q", "gone.txt")
	f.rename = h.commitAt(wt, "2026-02-03T10:00:00Z", "rename a file and delete another")

	h.put(wt, "logo.bin", []byte{0x89, 'P', 'N', 'G', 0, 9, 9, 9, 9}, 0o644)
	f.binary = h.commitAt(wt, "2026-02-04T10:00:00Z", "change a binary file")

	// A side branch from the root, merged with a merge commit.
	h.mustGit(wt, "checkout", "-q", "-b", "side", f.root)
	h.put(wt, "side.txt", []byte("from the side\n"), 0o644)
	h.commitAt(wt, "2026-02-05T10:00:00Z", "work on the side")
	h.mustGit(wt, "checkout", "-q", "main")
	env := []string{"GIT_AUTHOR_DATE=2026-02-06T10:00:00Z", "GIT_COMMITTER_DATE=2026-02-06T10:00:00Z"}
	out, err := h.gitEnv(env, wt, "merge", "-q", "--no-ff", "-m", "merge the side branch", "side")
	require.NoError(h.t, err, out)
	f.merge = strings.TrimSpace(h.mustGit(wt, "rev-parse", "HEAD"))

	h.put(wt, "evil.go", []byte("package evil\n\nfunc admin(user string) bool {\n\tif user == \"user\u202e \u2066// check\u2069 \u2066\" {\n\t\treturn true // z\u200bw\n\t}\n\treturn false\n}\n"), 0o644)
	f.trojan = h.commitAt(wt, "2026-02-07T10:00:00Z", "add evil.go\n\nlooks \u202efine\u202c")

	var big strings.Builder
	for i := range 2600 {
		fmt.Fprintf(&big, "line %d\n", i)
	}
	h.put(wt, "big.txt", []byte(big.String()), 0o644)
	f.big = h.commitAt(wt, "2026-02-08T10:00:00Z", "add a very large file")

	h.mustGit(wt, "tag", "v1.0.0", f.modify)
	h.createRepo(name, domain.VisibilityPublic)
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/"+name+".git"), "main", "side", "--tags")
	return f
}

// longRepo is a repository with more commits than a page lists: 52 on main,
// one of which adds a.txt and one changes it.
func (h *harness) longRepo(name string) {
	h.t.Helper()
	wt := h.t.TempDir()
	h.mustGit(wt, "init", "-q", "--initial-branch=main")
	base := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	for i := range 52 {
		switch i {
		case 3:
			h.put(wt, "a.txt", []byte("first\n"), 0o644)
		case 30:
			h.put(wt, "a.txt", []byte("second\n"), 0o644)
		default:
			h.put(wt, "counter.txt", []byte(fmt.Sprintf("%d\n", i)), 0o644)
		}
		h.commitAt(wt, base.Add(time.Duration(i)*12*time.Hour).Format(time.RFC3339), fmt.Sprintf("commit number %d", i))
	}
	h.createRepo(name, domain.VisibilityPublic)
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/"+name+".git"), "main")
}

var reOlder = regexp.MustCompile(`<a class="btn" href="([^"]+)" rel="next">`)

// unescape undoes the HTML escaping of an attribute value.
func unescape(s string) string { return strings.ReplaceAll(s, "&amp;", "&") }

// AC1: the history pages render for the fixtures with the structure of the
// mockup (golden HTML, normalised).
func TestWebCommitsGolden(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.diffRepo("diffs")
	h.longRepo("long")

	// The second page is reached by the link of the first, as a visitor would.
	_, _, first := h.get("/long/commits/main")
	m := reOlder.FindStringSubmatch(first)
	require.Len(t, m, 2, "the first page links to the older commits")
	second := unescape(m[1])
	assert.True(t, strings.HasPrefix(second, "/long/commits/main?page="), second)

	for _, tt := range []struct{ name, path string }{
		{"commits-root", "/long/commits/main"},
		{"commits-page2", second},
		{"commits-path", "/long/commits/main/a.txt"},
		{"commits-small", "/diffs/commits"},
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

// The behaviours of the history pages, asserted on their own.
func TestWebCommits(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	f := h.diffRepo("diffs")
	h.longRepo("long")
	get := func(path string, status int) string {
		t.Helper()
		st, _, body := h.get(path)
		require.Equal(t, status, st, path)
		return body
	}

	t.Run("history", func(t *testing.T) {
		first := get("/long/commits/main", 200)
		assert.Equal(t, 50, strings.Count(first, `class="c-sha mute"`), "a page lists 50 commits")
		assert.Contains(t, first, "commit number 51")
		assert.NotContains(t, first, "commit number 1<", "the oldest are on the next page")
		assert.Contains(t, first, `<span class="btn" aria-disabled="true">← Newest</span>`)
		second := unescape(reOlder.FindStringSubmatch(first)[1])
		page2 := get(second, 200)
		assert.Equal(t, 2, strings.Count(page2, `class="c-sha mute"`))
		assert.Contains(t, page2, `href="/long/commits/main">← Newest</a>`)
		assert.Contains(t, page2, `<span class="btn" aria-disabled="true">Older →</span>`)
		get("/long/commits/main?page=garbage", 404)
	})

	t.Run("path filter", func(t *testing.T) {
		body := get("/long/commits/main/a.txt", 200)
		assert.Equal(t, 2, strings.Count(body, `class="c-sha mute"`))
		assert.Contains(t, body, "history of <code>a.txt</code> at main")
		assert.Contains(t, body, `href="/long/blob/main/a.txt"`, "the way to the file")
		get("/long/commits/main/never/existed.txt", 404)
	})

	t.Run("refs", func(t *testing.T) {
		// A tag, a branch, a commit id, and a ref with a slash.
		assert.Contains(t, get("/diffs/commits/v1.0.0", 200), "modify a file")
		assert.Contains(t, get("/diffs/commits/side", 200), "work on the side")
		assert.Contains(t, get("/diffs/commits/"+f.modify[:10], 200), "modify a file")
		assert.Contains(t, get("/diffs/commits/main", 200), `<span class="pill tag"><span class="sr">tag </span>v1.0.0</span>`)
		assert.Contains(t, get("/diffs/commits/main", 200), `<span class="pill">merge</span>`)
		get("/diffs/commits/nope", 404)
		get("/diffs/commits/mai", 404)
		get("/nope/commits/main", 404)
	})
}

// AC2: the history pages of a private repository are the 404 page of a
// repository that does not exist.
func TestWebCommitsPrivate(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	wt := h.workTree("main", "secret.txt")
	h.createRepo("hidden-one", domain.VisibilityPrivate)
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/hidden-one.git"), "main")

	_, _, missing := h.get("/never-existed")
	for _, p := range []string{"/hidden-one/commits/main", "/hidden-one/commits", "/hidden-one/commits/main/secret.txt"} {
		st, hdr, body := h.get(p)
		assert.Equal(t, 404, st, p)
		assert.Equal(t, "no-cache", hdr.Get("Cache-Control"), p)
		assert.NotContains(t, body, "hidden-one", p)
		assert.NotContains(t, body, "secret.txt", p)
		assert.Equal(t, normalise(missing), normalise(body), p)
	}
}
