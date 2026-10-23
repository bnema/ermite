// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/httpserver"
	"bnema.dev/ermite/internal/config"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// robotsRule is one Allow or Disallow line.
type robotsRule struct {
	allow   bool
	pattern string
	re      *regexp.Regexp
}

// parseRobots reads the rules of a robots.txt with a single "User-agent: *"
// group, as the default has.
func parseRobots(t *testing.T, body string) []robotsRule {
	t.Helper()
	var rules []robotsRule
	for _, line := range strings.Split(body, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		var allow bool
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "allow":
			allow = true
		case "disallow":
		default:
			continue
		}
		// RFC 9309 §2.2.2: "*" is any sequence of characters, a final "$"
		// anchors the end; everything else is literal.
		anchored := strings.HasSuffix(value, "$")
		expr := "^" + strings.ReplaceAll(regexp.QuoteMeta(strings.TrimSuffix(value, "$")), `\*`, ".*")
		if anchored {
			expr += "$"
		}
		rules = append(rules, robotsRule{allow: allow, pattern: value, re: regexp.MustCompile(expr)})
	}
	require.NotEmpty(t, rules)
	return rules
}

// robotsAllowed applies the rules as RFC 9309 §2.2.2 does: the longest
// matching pattern wins, Allow wins a tie, no match allows. target is the path
// and query.
func robotsAllowed(rules []robotsRule, target string) bool {
	best, allowed := -1, true
	for _, r := range rules {
		if !r.re.MatchString(target) {
			continue
		}
		n := len(r.pattern)
		if n > best || (n == best && r.allow) {
			best, allowed = n, r.allow
		}
	}
	return allowed
}

// The default robots.txt keeps the summaries, issues, merge requests and
// releases open and blocks the endless views. Each blocked view is also
// requested on a real repository, so that a renamed route fails the test
// instead of leaving a rule that guards nothing.
func TestDefaultRobotsTxtRules(t *testing.T) {
	rules := parseRobots(t, httpserver.DefaultRobotsTxt)

	for _, path := range []string{
		"/", "/r", "/r/", "/r/issues/1", "/r/merge-requests/1",
		"/r/merge-requests?page=2", "/r/releases", "/r/releases/tag/v1",
		"/r/releases/tag/tree-v1", "/-/static/x.css",
	} {
		assert.True(t, robotsAllowed(rules, path), "%s should be allowed", path)
	}

	h := newHarness(t, harnessOpts{})
	h.publicRepo("r", "a")
	sha := h.bareGit("r", "rev-parse", "main")
	wt := h.workTree("feat", "b")
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/r.git"), "feat")
	mr, err := h.forge.MergeRequests.Create(h.ctx, domain.Root(), h.name("r"), ports.CreateMergeRequestParams{Title: "feat", Source: "feat", Target: "main"})
	require.NoError(t, err)
	require.EqualValues(t, 1, mr.Number)
	rel, err := h.forge.Releases.CreateRelease(h.ctx, domain.Root(), h.name("r"), ports.CreateReleaseParams{Tag: "v1", TargetCommitish: "main", Title: "one"})
	require.NoError(t, err)
	_, err = h.forge.Releases.UploadAsset(h.ctx, domain.Root(), h.name("r"), ports.UploadAssetParams{ReleaseID: rel.ID, Name: "a", Size: 1}, strings.NewReader("x"))
	require.NoError(t, err)

	for _, tc := range []struct {
		path string
		// live is a request of the same route that must not be a 404 on r;
		// empty when the route needs state the harness does not make (a CI
		// run).
		live string
	}{
		{"/r/blob/main/a", "/r/blob/main/a"},
		{"/r/raw/main/a", "/r/raw/main/a"},
		{"/r/tree", "/r/tree"},
		{"/r/tree/main", "/r/tree/main"},
		{"/r/commit/abc", "/r/commit/" + sha},
		{"/r/commits", "/r/commits"},
		{"/r/commits?page=x", "/r/commits/main"},
		{"/r/compare/a...b", "/r/compare/feat"},
		{"/r/refs", "/r/refs"},
		{"/r/actions", "/r/actions"},
		{"/r/actions/runs/1", ""},
		{"/r/merge-requests/1?view=split", "/r/merge-requests/1?view=split"},
		{"/r/merge-requests/1/files", ""},
		{"/r/releases/download/v1/a", "/r/releases/download/v1/a"},
		{"/r/info/refs", "/r/info/refs?service=git-upload-pack"},
		{"/r/git-upload-pack", ""},
		{"/-/events", "/-/events"},
	} {
		assert.False(t, robotsAllowed(rules, tc.path), "%s should be blocked", tc.path)
		if tc.live != "" {
			assert.NotEqual(t, http.StatusNotFound, h.status("GET", tc.live, nil), "%s is a route", tc.live)
		}
	}
}

// The full router serves /robots.txt ahead of the web, which is mounted on
// "/" and would answer its 404 page.
func TestRobotsTxtRoute(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	status, _, body := h.get("/robots.txt")
	assert.Equal(t, 200, status)
	assert.Equal(t, httpserver.DefaultRobotsTxt, body)
}

// ERMITE_ROBOTS_TXT_FILE reaches the server.
func TestRobotsTxtFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "robots.txt")
	custom := "User-agent: *\nDisallow: /\n"
	require.NoError(t, os.WriteFile(path, []byte(custom), 0o600))
	h := newHarness(t, harnessOpts{env: map[string]string{config.EnvRobotsTxtFile: path}})

	status, _, body := h.get("/robots.txt")
	assert.Equal(t, 200, status)
	assert.Equal(t, custom, body)
}
