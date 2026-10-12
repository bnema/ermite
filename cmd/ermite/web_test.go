// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files of the web tests")

// webFixtureReadme exercises the Markdown policy and the outline.
const webFixtureReadme = "# Demo\n\nA small *demo* with a [guide](docs/guide.md) and [the site](https://example.org/x).\n\n" +
	"## Install\n\n```sh\ngo install example.org/demo@latest\n```\n\n## Usage\n\n| a | b |\n|---|--:|\n| 1 | 2 |\n"

// webRepo is the repository of the golden pages: public, with a README, two
// branches (one with a slash), two tags and a description.
func (h *harness) webRepo(name string) {
	h.t.Helper()
	_, err := h.forge.Repos.Create(h.ctx, domain.Root(), ports.CreateRepoParams{
		Name: h.name(name), Description: "A demo repository", Visibility: domain.VisibilityPublic,
	})
	require.NoError(h.t, err)
	wt := h.t.TempDir()
	h.mustGit(wt, "init", "-q", "--initial-branch=main")
	commit := func(date, msg string, files map[string]string) {
		for p, c := range files {
			require.NoError(h.t, os.MkdirAll(filepath.Dir(filepath.Join(wt, p)), 0o755))
			require.NoError(h.t, os.WriteFile(filepath.Join(wt, p), []byte(c), 0o644))
			h.mustGit(wt, "add", p)
		}
		env := []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
		out, err := h.gitEnv(env, wt, "commit", "-q", "-m", msg)
		require.NoError(h.t, err, out)
	}
	commit("2026-01-01T10:00:00Z", "first commit", map[string]string{"README.md": webFixtureReadme, "docs/guide.md": "# Guide\n"})
	h.mustGit(wt, "tag", "v0.1.0")
	commit("2026-01-02T10:00:00Z", "add main.go", map[string]string{"main.go": "package main\n"})
	h.mustGit(wt, "tag", "-a", "-m", "release", "v0.2.0")
	h.mustGit(wt, "checkout", "-q", "-b", "feature/x")
	commit("2026-01-03T10:00:00Z", "work on x", map[string]string{"x.txt": "x\n"})
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/"+name+".git"), "main", "feature/x", "--tags")
}

// get does a GET and returns the status, the headers and the body.
func (h *harness) get(path string) (int, http.Header, string) {
	h.t.Helper()
	resp, err := http.Get(h.url(path))
	require.NoError(h.t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(h.t, err)
	return resp.StatusCode, resp.Header, string(b)
}

var (
	reSHA40   = regexp.MustCompile(`\b[0-9a-f]{40}\b`)
	reSHA7    = regexp.MustCompile(`\b[0-9a-f]{7}\b`)
	reAsset   = regexp.MustCompile(`(/-/static/[a-z]+/[a-z.-]+?)\.[0-9a-f]{12}(\.\w+)`)
	reAgo     = regexp.MustCompile(`(\bnow\b|\d+(m|h|d|mo|y) ago)`)
	reStamp   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}(:\d{2}Z| UTC)?`)
	reReqID   = regexp.MustCompile(`request id: [0-9a-f]{8}`)
	reSpaces  = regexp.MustCompile(`[ \t]+\n|\n[ \t]*\n+`)
	reMarkers = regexp.MustCompile(`>\s*<`)
)

// normalise makes a page comparable between runs: it hides what depends on
// the clock and on the commit ids, which hold the commit times.
func normalise(s string) string {
	s = reAsset.ReplaceAllString(s, "$1.HASH$2")
	s = reSHA40.ReplaceAllString(s, "SHA40")
	s = reSHA7.ReplaceAllString(s, "SHA7")
	s = reAgo.ReplaceAllString(s, "AGO")
	s = reStamp.ReplaceAllString(s, "STAMP")
	s = reReqID.ReplaceAllString(s, "request id: ID")
	s = reSpaces.ReplaceAllString(s, "\n")
	s = reMarkers.ReplaceAllString(s, ">\n<")
	return strings.TrimSpace(s) + "\n"
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "web", name+".html")
	if *updateGolden {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "run: go test ./cmd/ermite -run TestWebGolden -update")
	assert.Equal(t, string(want), got, name)
}

// AC1: the pages render for a fixture repository with the structure of the
// mockup; the HTML is compared after normalising times and ids.
func TestWebGolden(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.webRepo("demo")
	h.seed("quiet", domain.VisibilityPublic) // empty repository

	for _, tt := range []struct {
		name, path string
		status     int
	}{
		{"home", "/", 200},
		{"summary", "/demo", 200},
		{"refs", "/demo/refs", 200},
		{"empty", "/quiet", 200},
		{"not-found", "/nope", 404},
		{"not-found-page", "/demo/nowhere/main", 404},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status, hdr, body := h.get(tt.path)
			require.Equal(t, tt.status, status)
			assert.Equal(t, "no-cache", hdr.Get("Cache-Control"))
			assert.Contains(t, hdr.Get("Content-Type"), "text/html")
			golden(t, tt.name, normalise(body))
		})
	}
}

// AC2: a private repository is invisible on the web: not in the home list,
// not in the ":" targets, and its URLs give the same 404 page as a missing
// repository.
func TestWebPrivateRepoIsInvisible(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("open", domain.VisibilityPublic, "README.md")
	h.seed("secret-sauce", domain.VisibilityPrivate, "README.md")

	status, _, home := h.get("/")
	require.Equal(t, 200, status)
	assert.Contains(t, home, "open")
	assert.NotContains(t, home, "secret-sauce")

	_, _, missingBody := h.get("/never-existed")
	for _, p := range []string{"/secret-sauce", "/secret-sauce/", "/secret-sauce/refs", "/secret-sauce/tree/main", "/secret-sauce.git"} {
		status, hdr, body := h.get(p)
		assert.Equal(t, 404, status, p)
		assert.NotContains(t, body, "secret-sauce", p)
		assert.Equal(t, "no-cache", hdr.Get("Cache-Control"), p)
		_, _, missing := h.get(strings.Replace(p, "secret-sauce", "never-existed", 1))
		assert.Equal(t, normalise(missing), normalise(missingBody), "%s is the page of a missing repository", p)
	}
	// The open repository's pages do not list it either.
	_, _, summary := h.get("/open")
	assert.NotContains(t, summary, "secret-sauce")
}

// AC4, end to end: a README that attacks the page is neutralised.
func TestWebReadmeIsNeutralised(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	_, err := h.forge.Repos.Create(h.ctx, domain.Root(), ports.CreateRepoParams{Name: h.name("evil"), Visibility: domain.VisibilityPublic})
	require.NoError(t, err)
	wt := h.t.TempDir()
	h.mustGit(wt, "init", "-q", "--initial-branch=main")
	h.writeFile(wt, "README.md", []byte("# <script>alert(1)</script>Evil\n\n"+
		"<img src=x onerror=alert(1)>\n\n[click](javascript:alert(1))\n\n![p](https://evil.example/p.png)\n\n"+
		"<svg onload=alert(1)></svg>\n\n[ok](docs/a.md)\n"))
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/evil.git"), "main")

	status, _, body := h.get("/evil")
	require.Equal(t, 200, status)
	for _, bad := range []string{"alert(1)</script", "onerror", "javascript:", "evil.example", "<svg", "<img"} {
		assert.NotContains(t, body, bad)
	}
	assert.Contains(t, body, `href="/evil/blob/main/docs/a.md"`)
	assertNoInline(t, body)
}

var (
	reInlineScript = regexp.MustCompile(`(?is)<script\b[^>]*>`)
	reStyleAttr    = regexp.MustCompile(`(?i)\sstyle\s*=`)
	reStyleTag     = regexp.MustCompile(`(?i)<style\b`)
	reImport       = regexp.MustCompile(`(?i)@import|url\(`)
	reCSSURL       = regexp.MustCompile(`url\(\s*([^)]*?)\s*\)`)
)

// assertNoInline checks the page has no inline script, style element or
// style attribute.
func assertNoInline(t *testing.T, body string) {
	t.Helper()
	for _, m := range reInlineScript.FindAllString(body, -1) {
		assert.Contains(t, m, "src=", "inline script: %s", m)
	}
	assert.False(t, reStyleAttr.MatchString(body), "style attribute")
	assert.False(t, reStyleTag.MatchString(body), "style element")
}

// AC6: the CSP forbids inline scripts and styles, nothing is loaded from
// another origin, no page has an inline script or a style attribute, and
// every page carries the security headers.
func TestWebContentSecurity(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.webRepo("demo")
	h.seed("quiet", domain.VisibilityPublic)

	const csp = "default-src 'self'; script-src 'self' 'unsafe-eval'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'"
	for _, p := range []string{"/", "/demo", "/demo/refs", "/quiet", "/nope", "/demo/commits/main"} {
		status, hdr, body := h.get(p)
		assert.Contains(t, []int{200, 404}, status, p)
		assert.Equal(t, csp, hdr.Get("Content-Security-Policy"), p)
		assert.NotContains(t, hdr.Get("Content-Security-Policy"), "unsafe-inline")
		assert.Equal(t, "DENY", hdr.Get("X-Frame-Options"), p)
		assert.Equal(t, "nosniff", hdr.Get("X-Content-Type-Options"), p)
		assert.Equal(t, "same-origin", hdr.Get("Referrer-Policy"), p)
		assert.Equal(t, "no-cache", hdr.Get("Cache-Control"), p)
		assertNoInline(t, body)

		// Everything the page loads is a path on this origin.
		for _, m := range regexp.MustCompile(`<(?:script|link)\b[^>]*\b(?:src|href)="([^"]*)"`).FindAllStringSubmatch(body, -1) {
			assert.True(t, strings.HasPrefix(m[1], "/-/static/"), "%s loads %s", p, m[1])
		}
		assert.NotRegexp(t, `(?i)\b(?:src|action)="(?:https?:)?//`, body, p)
		assert.False(t, reImport.MatchString(body), p)

		// Every static asset the page uses is served, immutable, from here.
		for _, m := range regexp.MustCompile(`"(/-/static/[^"]+)"`).FindAllStringSubmatch(body, -1) {
			st, ah, ab := h.get(m[1])
			assert.Equal(t, 200, st, m[1])
			assert.Equal(t, "public, max-age=31536000, immutable", ah.Get("Cache-Control"), m[1])
			assert.Equal(t, "nosniff", ah.Get("X-Content-Type-Options"), m[1])
			assert.NotEmpty(t, ab)
			if strings.HasSuffix(m[1], ".css") {
				// The sheet loads its fonts and icons from here, and nothing else.
				assert.NotContains(t, ab, "@import", m[1])
				for _, u := range reCSSURL.FindAllStringSubmatch(ab, -1) {
					assert.True(t, strings.HasPrefix(u[1], "/-/static/"), "%s loads %s", m[1], u[1])
					st, _, _ := h.get(u[1])
					assert.Equal(t, 200, st, u[1])
				}
			}
		}
	}

	// The footer links to the source (AGPL §13).
	_, hdr, body := h.get("/")
	assert.Contains(t, body, `<a href="http://example.test/ermite"`)

	// Pages name their speculation rules (prefetch on hover), a static file.
	rules := strings.Trim(hdr.Get("Speculation-Rules"), `"`)
	require.True(t, strings.HasPrefix(rules, "/-/static/rules/rules."), rules)
	st, rh, rb := h.get(rules)
	assert.Equal(t, 200, st)
	assert.Equal(t, "application/speculationrules+json", rh.Get("Content-Type"))
	assert.Contains(t, rb, `"prefetch"`)

	// Unknown, stale or traversing asset names are 404s.
	for _, p := range []string{"/-/static/css/ermite.000000000000.css", "/-/static/css/ermite.css", "/-/static/", "/-/static/../x", "/-/static/js/"} {
		st, _, _ := h.get(p)
		assert.Equal(t, 404, st, p)
	}
}

// The summary rail names the newest tag; with more tags than it dates, it
// gives their count instead of an empty block.
func TestWebSummaryLatestTag(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.webRepo("demo")
	_, _, body := h.get("/demo")
	assert.Contains(t, body, `<a class="tag-name" href="/demo/tree/v0.2.0">`)
	assert.Contains(t, body, `<p class="tag-sub dim">add main.go</p>`)

	h.seed("many", domain.VisibilityPublic, "a.txt")
	wt := t.TempDir()
	h.mustGit(wt, "clone", "-q", h.urlAuth(adminToken, "/many.git"), ".")
	for i := range 21 {
		h.mustGit(wt, "tag", "t"+strconv.Itoa(i))
	}
	h.mustGit(wt, "push", "-q", "--tags", "origin")
	_, _, body = h.get("/many")
	assert.NotContains(t, body, `class="latest-tag"`)
	assert.Contains(t, body, `<p class="dim">21 tags</p>`)
}
