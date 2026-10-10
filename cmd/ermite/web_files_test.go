// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/domain"
)

// Trojan Source: a right-to-left override and an isolate around code.
const webTrojan = "package main\n\nfunc main() {\n\taccess := \"user\u202e \u2066// admin only\u2069 \u2066\"\n\t_ = access\n}\n"

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, img))
	return b.Bytes()
}

// webFilesRepo is the repository of the tree, blob and raw tests: files of
// every kind the pages treat differently, a directory README, a branch and a
// tag with a slash.
func (h *harness) webFilesRepo(name string) {
	h.t.Helper()
	_, err := h.forge.Repos.Create(h.ctx, domain.Root(), app.CreateParams{
		Name: h.name(name), Description: "Files of every kind", Visibility: domain.VisibilityPublic,
	})
	require.NoError(h.t, err)
	wt := h.t.TempDir()
	h.mustGit(wt, "init", "-q", "--initial-branch=main")
	h.writeFile(wt, "README.md", []byte("# Files\n\nSee [the guide](docs/guide.md).\n"))
	h.writeFile(wt, "main.go", []byte("package main\n\nimport \"fmt\"\n\n// main prints.\nfunc main() {\n\tfmt.Println(\"hi <b>\", 42)\n}\n"))
	h.writeFile(wt, "docs/README.md", []byte("# Docs\n\nBack to [the guide](guide.md) and [root](../main.go).\n"))
	h.writeFile(wt, "docs/guide.md", []byte("# Guide\n\n## Steps\n\nRun it.\n\n```sh\nmake all\n```\n"))
	h.writeFile(wt, "src/lib/util.go", []byte("package lib\n"))
	h.writeFile(wt, "notes.txt", []byte("plain notes\nsecond line\n"))
	h.writeFile(wt, "big.txt", bytes.Repeat([]byte("0123456789abcdef\n"), 70000)) // 1.1 MiB
	h.writeFile(wt, "lines.txt", bytes.Repeat([]byte("x\n"), 20005))
	h.writeFile(wt, "data.bin", []byte("\x00\x01\x02binary\x00"))
	h.writeFile(wt, "pic.png", tinyPNG(h.t))
	h.writeFile(wt, "logo.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><script>alert(2)</script></svg>`))
	h.writeFile(wt, "page.html", []byte("<script>alert(3)</script>\n"))
	h.writeFile(wt, "evil.go", []byte(webTrojan))
	h.writeFile(wt, "empty.txt", nil)
	h.writeFile(wt, "fake.png", []byte("<html><script>alert(1)</script></html>\n"))
	h.writeFile(wt, "fake.jpg", []byte("GIF89a\x00\x00binary\x00"))
	h.mustGit(wt, "tag", "v1.0")
	h.mustGit(wt, "checkout", "-q", "-b", "feature/x")
	h.writeFile(wt, "x/y.txt", []byte("on the feature branch\n"))
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/"+name+".git"), "main", "feature/x", "--tags")
}

// AC1: the tree and blob pages render for a fixture repository; the HTML is
// compared after normalising times and ids.
func TestWebFilesGolden(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.webFilesRepo("files")

	for _, tt := range []struct {
		name, path string
		status     int
	}{
		{"tree-root", "/files/tree/main", 200},
		{"tree-subdir", "/files/tree/main/docs", 200},
		{"tree-nested", "/files/tree/main/src/lib", 200},
		{"tree-slash-ref", "/files/tree/feature/x/x", 200},
		{"blob-code", "/files/blob/main/main.go", 200},
		{"blob-markdown", "/files/blob/main/docs/guide.md", 200},
		{"blob-text", "/files/blob/main/notes.txt", 200},
		{"blob-too-large", "/files/blob/main/big.txt", 200},
		{"blob-binary", "/files/blob/main/data.bin", 200},
		{"blob-image", "/files/blob/main/pic.png", 200},
		{"blob-svg", "/files/blob/main/logo.svg", 200},
		{"blob-trojan", "/files/blob/main/evil.go", 200},
		{"blob-empty", "/files/blob/main/empty.txt", 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status, hdr, body := h.get(tt.path)
			require.Equal(t, tt.status, status)
			assert.Equal(t, "no-cache", hdr.Get("Cache-Control"))
			assert.Contains(t, hdr.Get("Content-Type"), "text/html")
			assertNoInline(t, body)
			golden(t, tt.name, normalise(body))
		})
	}
}

// Ref names with a slash are found by their longest prefix, and a commit id
// works where a ref does.
func TestWebFilesRefs(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.webFilesRepo("files")

	_, _, body := h.get("/files/tree/feature/x")
	assert.Contains(t, body, ">x/<", "the directory of the branch feature/x")
	status, _, body := h.get("/files/blob/feature/x/x/y.txt")
	require.Equal(t, 200, status)
	assert.Contains(t, body, "on the feature branch")
	status, _, body = h.get("/files/blob/v1.0/main.go")
	require.Equal(t, 200, status, "a tag")
	assert.Contains(t, body, "package")

	sha := h.bareGit("files", "rev-parse", "main")
	status, _, body = h.get("/files/blob/" + sha + "/notes.txt")
	require.Equal(t, 200, status, "a commit id")
	assert.Contains(t, body, "plain notes")
	assert.Contains(t, body, "detached")
	status, _, _ = h.get("/files/tree/" + sha[:7])
	assert.Equal(t, 200, status, "an abbreviated commit id")

	// Links of a directory README stay under the directory and the ref.
	_, _, body = h.get("/files/tree/main/docs")
	assert.Contains(t, body, `href="/files/blob/main/docs/guide.md"`)
	assert.Contains(t, body, `href="/files/blob/main/main.go"`)
}

// AC3: what does not exist is the 404 page, with no hint of why.
func TestWebFilesNotFound(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.webFilesRepo("files")
	h.seed("quiet", domain.VisibilityPublic) // empty repository

	for _, p := range []string{
		"/files/tree/nope", "/files/tree/main/no/such/dir", "/files/blob/main/nothing.txt", "/files/blob/nope/main.go",
		"/files/blob/main", "/files/blob/main/docs", "/files/tree/main/main.go", "/files/raw/main/nothing.txt",
		"/files/raw/main", "/files/raw/main/docs", "/files/blob/ffffffffffffffffffffffffffffffffffffffff/main.go",
		"/files/blob", "/files/raw", "/files/blob/main/../main.go",
		"/quiet/tree/main", "/quiet/tree", "/quiet/blob/main/x", "/never/blob/main/x", "/never/raw/main/x",
	} {
		status, hdr, body := h.get(p)
		assert.Equal(t, 404, status, p)
		assert.Equal(t, "no-cache", hdr.Get("Cache-Control"), p)
		assert.Contains(t, hdr.Get("Content-Type"), "text/html", p)
		assert.NotContains(t, body, "nothing.txt", p)
	}
	status, _, body := h.get("/files/blob/main/nothing.txt")
	require.Equal(t, 404, status)
	_, _, other := h.get("/files/tree/nope")
	assert.Equal(t, normalise(other), normalise(body), "one 404 page for everything")
}

// AC2: the tree, blob and raw pages of a private repository are the 404 of a
// missing one.
func TestWebFilesPrivate(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("secret-sauce", domain.VisibilityPrivate, "README.md", "a.txt")

	for _, kind := range []string{"tree", "blob", "raw"} {
		for _, p := range []string{"/secret-sauce/" + kind + "/main", "/secret-sauce/" + kind + "/main/a.txt"} {
			status, hdr, body := h.get(p)
			assert.Equal(t, 404, status, p)
			assert.NotContains(t, body, "secret-sauce", p)
			assert.NotContains(t, body, "content of", p)
			assert.NotContains(t, hdr.Get("Content-Type"), "text/plain", p)
			_, _, missing := h.get(strings.Replace(p, "secret-sauce", "never-existed", 1))
			assert.Equal(t, normalise(missing), normalise(body), "%s is the page of a missing repository", p)
		}
	}
}

func rawGet(t *testing.T, h *harness, path string) (int, http.Header, []byte) {
	t.Helper()
	status, hdr, body := h.get(path)
	return status, hdr, []byte(body)
}

// The raw route serves text as text/plain, the four image types as images,
// everything else as an attachment; never as HTML or SVG; and always with
// nosniff and a CSP that makes the file inert.
func TestWebRaw(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.webFilesRepo("files")
	sha := h.bareGit("files", "rev-parse", "main")

	for _, tt := range []struct {
		name, path, typ, disposition string
	}{
		{"text", "/files/raw/main/notes.txt", "text/plain; charset=utf-8", ""},
		{"code", "/files/raw/main/main.go", "text/plain; charset=utf-8", ""},
		{"markdown", "/files/raw/main/README.md", "text/plain; charset=utf-8", ""},
		{"html is text", "/files/raw/main/page.html", "text/plain; charset=utf-8", ""},
		{"empty", "/files/raw/main/empty.txt", "text/plain; charset=utf-8", ""},
		{"large text", "/files/raw/main/big.txt", "text/plain; charset=utf-8", ""},
		{"png", "/files/raw/main/pic.png", "image/png", ""},
		{"binary", "/files/raw/main/data.bin", "application/octet-stream", `attachment; filename=data.bin`},
		{"svg is text", "/files/raw/main/logo.svg", "text/plain; charset=utf-8", ""},
		{"slash ref", "/files/raw/feature/x/x/y.txt", "text/plain; charset=utf-8", ""},
		{"tag", "/files/raw/v1.0/main.go", "text/plain; charset=utf-8", ""},
		{"commit", "/files/raw/" + sha + "/notes.txt", "text/plain; charset=utf-8", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status, hdr, body := rawGet(t, h, tt.path)
			require.Equal(t, 200, status)
			assert.Equal(t, tt.typ, hdr.Get("Content-Type"))
			assert.Equal(t, tt.disposition, hdr.Get("Content-Disposition"))
			assert.Equal(t, "nosniff", hdr.Get("X-Content-Type-Options"))
			assert.Equal(t, "default-src 'none'; sandbox", hdr.Get("Content-Security-Policy"))
			assert.Equal(t, "DENY", hdr.Get("X-Frame-Options"))
			assert.Equal(t, strconv.Itoa(len(body)), hdr.Get("Content-Length"))
			// Even at a full commit id: the repository can turn private.
			assert.Equal(t, "no-cache", hdr.Get("Cache-Control"))
		})
	}

	// Content is the file, byte for byte.
	_, _, body := rawGet(t, h, "/files/raw/main/data.bin")
	assert.Equal(t, []byte("\x00\x01\x02binary\x00"), body)
	_, _, body = rawGet(t, h, "/files/raw/main/pic.png")
	assert.Equal(t, tinyPNG(t), body)
	_, _, body = rawGet(t, h, "/files/raw/main/big.txt")
	assert.Len(t, body, 17*70000)
	_, _, body = rawGet(t, h, "/files/raw/main/evil.go")
	assert.Equal(t, webTrojan, string(body), "raw is not altered")

	// A file named like an image is not one unless its bytes say so.
	_, hdr, _ := rawGet(t, h, "/files/raw/main/fake.png")
	assert.Equal(t, "text/plain; charset=utf-8", hdr.Get("Content-Type"), "the type comes from the bytes, not the name")
	_, hdr, _ = rawGet(t, h, "/files/raw/main/fake.jpg")
	assert.Equal(t, "image/gif", hdr.Get("Content-Type"))
	_, _, page := h.get("/files/blob/main/fake.png")
	assert.NotContains(t, page, "<script>alert", "shown as code, escaped")

	// HEAD and conditional requests.
	req, err := http.NewRequest(http.MethodGet, h.url("/files/raw/main/notes.txt"), nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	etag := resp.Header.Get("ETag")
	require.NotEmpty(t, etag)
	req.Header.Set("If-None-Match", etag)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNotModified, resp.StatusCode)
	assert.Equal(t, http.StatusOK, h.status(http.MethodHead, "/files/raw/main/notes.txt", nil))
}

var reLineID = regexp.MustCompile(`<span class="line" id="L(\d+)">`)

// AC1, AC6: the code view numbers its lines with anchors, the page has no
// inline script or style, and a file that attacks the page is shown inert.
func TestWebBlobCode(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.webFilesRepo("files")

	_, _, body := h.get("/files/blob/main/main.go")
	assert.Equal(t, 8, len(reLineID.FindAllString(body, -1)))
	assert.Contains(t, body, `<a class="ln" href="#L6">6</a>`)
	assert.Contains(t, body, `<span class="kn">package</span>`)
	assert.Contains(t, body, `hi &lt;b&gt;`, "source is escaped")
	assert.NotContains(t, body, "<b>")
	assertNoInline(t, body)
	assert.Regexp(t, `href="/files/raw/main/main.go"`, body)

	// Trojan Source: the controls are shown, not applied.
	_, _, body = h.get("/files/blob/main/evil.go")
	for _, r := range []string{"\u202e", "\u2066", "\u2069"} {
		assert.NotContains(t, body, r, "%+q is not output", r)
	}
	assert.Contains(t, body, `<span class="ctl" title="U+202E right-to-left override">&lt;U+202E&gt;</span>`)
	assert.Contains(t, body, "&lt;U+2066&gt;")
	assert.Contains(t, body, "&lt;U+2069&gt;")

	// The image and SVG: only the four raster types are images.
	_, _, body = h.get("/files/blob/main/pic.png")
	assert.Contains(t, body, `<img src="/files/raw/main/pic.png"`)
	_, _, body = h.get("/files/blob/main/logo.svg")
	assert.NotContains(t, body, "<img")
	assert.NotContains(t, body, "<svg", "the source is escaped, never markup")
	assert.NotContains(t, body, "<script>alert")
	assert.Contains(t, body, "&lt;svg")
	assertNoInline(t, body)

	// Too many lines: the first ones, with a notice.
	_, _, body = h.get("/files/blob/main/lines.txt")
	assert.Equal(t, 20000, len(reLineID.FindAllString(body, -1)))
	assert.Contains(t, body, "20005 lines: showing the first 20000")

	// Markdown: rendered, source under a fold, links under the directory.
	_, _, body = h.get("/files/blob/main/docs/guide.md")
	assert.Contains(t, body, `<details class="source">`)
	assert.Contains(t, body, "<h1 id=\"md-guide\">Guide</h1>")
	assert.Contains(t, body, `href="#md-steps"`, "outline")
}

// AC6 on the new pages: the CSP of the site, the security headers, nothing
// inline and nothing loaded from another origin.
func TestWebFilesContentSecurity(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.webFilesRepo("files")

	const csp = "default-src 'self'; script-src 'self' 'unsafe-eval'; frame-ancestors 'none'"
	for _, p := range []string{
		"/files/tree/main", "/files/tree/main/docs", "/files/blob/main/main.go", "/files/blob/main/docs/guide.md",
		"/files/blob/main/pic.png", "/files/blob/main/logo.svg", "/files/blob/main/evil.go", "/files/blob/main/big.txt",
		"/files/blob/main/data.bin", "/files/tree/nope",
	} {
		_, hdr, body := h.get(p)
		assert.Equal(t, csp, hdr.Get("Content-Security-Policy"), p)
		assert.Equal(t, "DENY", hdr.Get("X-Frame-Options"), p)
		assert.Equal(t, "nosniff", hdr.Get("X-Content-Type-Options"), p)
		assertNoInline(t, body)
		assert.NotRegexp(t, `(?i)\b(?:src|action)="(?:https?:)?//`, body, p)
		assert.False(t, reImport.MatchString(body), p)
		for _, m := range regexp.MustCompile(`<(?:script|link)\b[^>]*\b(?:src|href)="([^"]*)"`).FindAllStringSubmatch(body, -1) {
			assert.True(t, strings.HasPrefix(m[1], "/-/static/"), "%s loads %s", p, m[1])
		}
	}

	// The theme of the code view is a static file with no colour of its own.
	_, _, page := h.get("/files/blob/main/main.go")
	css := regexp.MustCompile(`href="(/-/static/css/[^"]+)"`).FindStringSubmatch(page)
	require.Len(t, css, 2)
	_, _, sheet := h.get(css[1])
	assert.Contains(t, sheet, ".chroma .kn { color: var(--syn-keyword)")
	assert.NotRegexp(t, `\.chroma[^{]*\{[^}]*#[0-9a-fA-F]{3,6}`, sheet)
}
