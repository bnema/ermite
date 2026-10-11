// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
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

// AC1, AC2, AC7: ?rows answers the entries of a directory as table rows only,
// for the Files view to insert under the directory; a private repository is
// the 404 of a missing one.
func TestWebTreeRows(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.webFilesRepo("files")
	h.seed("secret-sauce", domain.VisibilityPrivate, "dir/a.txt")

	// The page links its directories to their rows.
	_, _, page := h.get("/files/tree/main")
	assert.Contains(t, page, `href="/files/tree/main/docs" data-expand="/files/tree/main/docs?rows" aria-expanded="false"`)
	assert.Contains(t, page, `<script src="/-/static/js/files.`)
	assert.NotContains(t, page, "main.go</span>", "the palette lists no file")

	status, hdr, body := h.get("/files/tree/main/docs?rows")
	require.Equal(t, 200, status)
	assert.Equal(t, "no-cache", hdr.Get("Cache-Control"))
	assert.Contains(t, hdr.Get("Content-Type"), "text/html")
	assert.NotContains(t, body, "<html")
	assert.NotContains(t, body, "<table")
	assert.Equal(t, 2, strings.Count(body, "<tr"), "one row per entry")
	assert.Contains(t, body, `<a href="/files/blob/main/docs/README.md">README.md</a>`)
	assert.Contains(t, body, `<a href="/files/blob/main/docs/guide.md">guide.md</a>`)
	assert.NotContains(t, body, "data-cursor", "the cursor stays on the page")
	assertNoInline(t, body)

	// A subdirectory row can be opened in turn.
	_, _, body = h.get("/files/tree/main/src?rows")
	assert.Contains(t, body, `href="/files/tree/main/src/lib" data-expand="/files/tree/main/src/lib?rows" aria-expanded="false"`)
	assert.Contains(t, body, "▸")
	_, _, body = h.get("/files/tree/main/src/lib?rows")
	assert.Contains(t, body, `href="/files/blob/main/src/lib/util.go"`)
	assert.NotContains(t, body, "data-expand")

	// Not found, and private as anonymous: the 404 page, nothing of the rows.
	for _, p := range []string{"/files/tree/main/nope?rows", "/secret-sauce/tree/main/dir?rows", "/secret-sauce/tree/main?rows"} {
		status, _, body = h.get(p)
		assert.Equal(t, 404, status, p)
		assert.NotContains(t, body, "a.txt", p)
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

// AC4: GET /<repo>/paths/<sha> lists the file paths of a commit as gzipped
// JSON, behind the access rule on every request, even when the list is
// already cached; a visitor revalidates (no-cache + ETag), the owner's copy is
// private.
func TestWebPaths(t *testing.T) {
	c := newCLIHarness(t)
	h := c.harness
	h.webFilesRepo("files")
	sha := h.bareGit("files", "rev-parse", "main")

	var want []string
	for _, p := range strings.Split(h.bareGit("files", "ls-tree", "-r", "-z", "--name-only", "main"), "\x00") {
		if p != "" {
			want = append(want, p)
		}
	}
	require.NotEmpty(t, want)
	b := browser{h}
	path := "/files/paths/" + sha
	type list struct {
		Paths     []string `json:"paths"`
		Truncated bool     `json:"truncated"`
	}
	decodeList := func(t *testing.T, body string) list {
		t.Helper()
		var l list
		require.NoError(t, json.Unmarshal([]byte(body), &l))
		return l
	}
	gunzip := func(t *testing.T, body string) string {
		t.Helper()
		zr, err := gzip.NewReader(strings.NewReader(body))
		require.NoError(t, err)
		out, err := io.ReadAll(zr)
		require.NoError(t, err)
		return string(out)
	}

	// Anonymous with gzip: the cached bytes as they are. The client of the
	// harness would unzip by itself, so the encoding is asked for explicitly
	// through a transport that does not.
	raw := func(hdr map[string]string) page {
		req, err := http.NewRequest(http.MethodGet, h.url(path), nil)
		require.NoError(t, err)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		out, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return page{status: resp.StatusCode, hdr: resp.Header, body: string(out)}
	}
	gz := raw(map[string]string{"Accept-Encoding": "gzip"})
	require.Equal(t, 200, gz.status)
	assert.Equal(t, "gzip", gz.hdr.Get("Content-Encoding"))
	assert.Equal(t, "application/json", gz.hdr.Get("Content-Type"))
	assert.Equal(t, "nosniff", gz.hdr.Get("X-Content-Type-Options"))
	assert.Equal(t, "no-cache", gz.hdr.Get("Cache-Control"))
	assert.Equal(t, "Accept-Encoding", gz.hdr.Get("Vary"))
	etag := gz.hdr.Get("ETag")
	assert.Equal(t, `"paths-`+sha+`-gz"`, etag)
	got := decodeList(t, gunzip(t, gz.body))
	assert.Equal(t, want, got.Paths, "git ls-tree -r order")
	assert.False(t, got.Truncated)

	// Without gzip: plain JSON, the same list.
	plain := raw(nil)
	require.Equal(t, 200, plain.status)
	assert.Empty(t, plain.hdr.Get("Content-Encoding"))
	assert.Equal(t, want, decodeList(t, plain.body).Paths)
	assert.Equal(t, `"paths-`+sha+`"`, plain.hdr.Get("ETag"), "another variant, another validator")

	// Revalidation: 304, still through the access rule.
	nm := raw(map[string]string{"If-None-Match": etag, "Accept-Encoding": "gzip"})
	assert.Equal(t, http.StatusNotModified, nm.status)
	assert.Empty(t, nm.body)
	assert.Equal(t, "no-cache", nm.hdr.Get("Cache-Control"))
	nm = raw(map[string]string{"If-None-Match": plain.hdr.Get("ETag")})
	assert.Equal(t, http.StatusNotModified, nm.status, "identity validator, identity body")
	assert.Equal(t, 200, raw(map[string]string{"If-None-Match": etag}).status, "the gzip validator does not match the plain body")
	assert.Equal(t, 200, raw(map[string]string{"If-None-Match": `"paths-other"`}).status)

	// Only a full commit id: abbreviations, refs, upper case and unknown ids.
	for _, p := range []string{
		"/files/paths/" + sha[:7], "/files/paths/" + sha[:39], "/files/paths/main", "/files/paths/" + strings.ToUpper(sha),
		"/files/paths/" + strings.Repeat("0", 40), "/files/paths/", "/never/paths/" + sha,
	} {
		assert.Equal(t, 404, b.get(p, nil).status, p)
	}

	// Another repository does not serve a commit it does not have, though
	// the list of that commit is cached.
	h.seed("other", domain.VisibilityPublic, "x.txt")
	assert.Equal(t, 404, b.get("/other/paths/"+sha, nil).status)

	// The owner: private, never stored; the list is the same.
	token := c.signIn(t)
	own := b.get(path, as(token))
	require.Equal(t, 200, own.status)
	assert.Equal(t, "private, no-store", own.hdr.Get("Cache-Control"))
	assert.Contains(t, own.hdr.Values("Vary"), "Cookie")
	assert.Equal(t, want, decodeList(t, own.body).Paths)

	// The repository turns private: the list is cached on the server, and
	// anonymous gets the 404 of a missing repository, as the owner still
	// reads it.
	priv := domain.VisibilityPrivate
	_, err := h.forge.Repos.Update(h.ctx, domain.Root(), h.name("files"), app.UpdateParams{Visibility: &priv})
	require.NoError(t, err)
	for _, hdr := range []map[string]string{nil, {"Accept-Encoding": "gzip"}, {"If-None-Match": etag}} {
		p := b.get(path, hdr)
		assert.Equal(t, 404, p.status)
		assert.NotContains(t, p.body, "README.md")
		assert.NotContains(t, p.hdr.Get("Content-Type"), "json")
	}
	assert.Equal(t, 200, b.get(path, as(token)).status)
}

// AC3, AC4: the Files page carries the finder and the data it needs, on the
// commit the page shows, however the URL spelled it; the other pages carry
// the paths of their commit and no finder.
func TestWebFinderPage(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.webFilesRepo("files")
	sha := h.bareGit("files", "rev-parse", "main")
	attr := func(body, name string) string {
		m := regexp.MustCompile(`<body[^>]*\b` + name + `="([^"]*)"`).FindStringSubmatch(body)
		if m == nil {
			return "<none>"
		}
		return m[1]
	}

	for _, tt := range []struct{ name, path, paths, blob string }{
		{"branch", "/files/tree/main", "/files/paths/" + sha, "/files/blob/main"},
		{"subdirectory", "/files/tree/main/docs", "/files/paths/" + sha, "/files/blob/main"},
		{"slash ref", "/files/tree/feature/x", "/files/paths/" + h.bareGit("files", "rev-parse", "feature/x"), "/files/blob/feature/x"},
		{"tag", "/files/tree/v1.0", "/files/paths/" + sha, "/files/blob/v1.0"},
		{"full id", "/files/tree/" + sha, "/files/paths/" + sha, "/files/blob/" + sha},
		{"abbreviated id", "/files/tree/" + sha[:7], "/files/paths/" + sha, "/files/blob/" + sha[:7]},
		{"blob page", "/files/blob/main/main.go", "/files/paths/" + sha, "/files/blob/main"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status, _, body := h.get(tt.path)
			require.Equal(t, 200, status)
			assert.Equal(t, tt.paths, attr(body, "data-paths"))
			assert.Equal(t, tt.blob, attr(body, "data-blob"))
			if strings.Contains(tt.path, "/tree/") {
				assert.Contains(t, body, `id="finder-q"`)
				assert.Contains(t, body, `id="finder-list"`)
				assert.Contains(t, body, `placeholder="Find a file…"`)
			} else {
				assert.NotContains(t, body, `id="finder-q"`)
			}
		})
	}
	// No commit to search: no data.
	_, _, body := h.get("/nothing-here")
	assert.Equal(t, "<none>", attr(body, "data-paths"))
	_, _, body = h.get("/files/refs")
	assert.Equal(t, "/files/paths/"+sha, attr(body, "data-paths"))
}
