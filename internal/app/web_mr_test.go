// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// mrRepo is a public repository "proj" with README.md on main and a branch
// feat/search that is one commit ahead of it (search.go, three lines).
type mrRepo struct {
	*cliHarness
	wt, url string
}

func newMRRepo(t *testing.T, vis domain.Visibility) *mrRepo {
	t.Helper()
	c := newCLIHarness(t)
	_, err := c.forge.Repos.Create(c.ctx, domain.Root(), createParams(c.harness, "proj", vis))
	require.NoError(t, err)
	r := &mrRepo{cliHarness: c, wt: c.workTree("main", "README.md"), url: c.urlAuth(adminToken, "/proj.git")}
	c.mustGit(r.wt, "push", "-q", r.url, "main")
	c.mustGit(r.wt, "checkout", "-q", "-b", "feat/search")
	c.writeFile(r.wt, "search.go", []byte("package search\n\nfunc Find() {}\n"))
	c.mustGit(r.wt, "push", "-q", r.url, "feat/search")
	return r
}

func (r *mrRepo) headOf(branch string) string { return r.bareGit("proj", "rev-parse", branch) }

// form posts url-encoded values as the signed-in owner, from this site.
func (b browser) form(path, token string, values url.Values) page {
	b.h.t.Helper()
	return b.do("POST", path, post(token), values.Encode())
}

var reHeadField = regexp.MustCompile(`name="head_id" value="([0-9a-f]{40})"`)

// formHead reads the head the merge form of a page carries.
func formHead(t *testing.T, body string) string {
	t.Helper()
	m := reHeadField.FindStringSubmatch(body)
	require.Len(t, m, 2, "the page has a merge form")
	return m[1]
}

// AC7: the pages as an anonymous visitor sees them on a public repository:
// read-only, no forms, public cache headers; the same pages signed in carry
// the forms and the private headers.
func TestWebMergeRequestPagesRender(t *testing.T) {
	r := newMRRepo(t, domain.VisibilityPublic)
	b := browser{r.harness}
	token := r.signIn(t)

	// The compare page: anonymous reads, the owner gets the form.
	anon := b.get("/proj/compare/feat/search", nil)
	require.Equal(t, 200, anon.status, anon.body)
	assert.Equal(t, "no-cache", anon.hdr.Get("Cache-Control"))
	assert.Equal(t, []string{"Accept-Encoding"}, anon.hdr.Values("Vary"))
	assert.Contains(t, anon.body, "feat/search")
	assert.Contains(t, anon.body, `<span class="kn">package</span> search</td>`, "the diff of the single commit")
	assert.NotContains(t, anon.body, "<form class=\"cform create\"")
	assert.NotContains(t, anon.body, `<form class="cform create"`)
	assertNoInline(t, anon.body)
	owner := b.get("/proj/compare/feat/search", as(token))
	require.Equal(t, 200, owner.status)
	assert.Equal(t, "private, no-store", owner.hdr.Get("Cache-Control"))
	assert.Contains(t, owner.hdr.Values("Vary"), "Cookie")
	assert.Contains(t, owner.body, `<form class="cform create" method="post" action="/proj/merge-requests">`)
	assert.Contains(t, owner.body, `name="source" value="feat/search"`)
	assert.Contains(t, owner.body, `name="target" value="main"`)
	assert.Equal(t, 404, b.get("/proj/compare/nope", nil).status)
	assert.Equal(t, 404, b.get("/proj/compare/main/extra", nil).status)
	main := b.get("/proj/compare/main", nil)
	assert.Equal(t, 200, main.status)
	assert.Contains(t, main.body, "nothing to compare")

	// An empty list, with the way to start one.
	list := b.get("/proj/merge-requests", nil)
	require.Equal(t, 200, list.status)
	assert.Contains(t, list.body, "no open merge requests")
	assert.Contains(t, list.body, `href="/proj/compare/feat/search"`)
	assert.Contains(t, list.body, `data-key="m"`, "the section of the nav")
	assert.NotContains(t, strings.ToLower(list.body), "proposal")

	// Open one from the compare page.
	res := b.form("/proj/merge-requests", token, url.Values{
		"source": {"feat/search"}, "target": {"main"}, "title": {"Add <b>search</b>"},
		"description": {"## Summary\r\n\r\nFinds **things** with `Find()`.\r\n\r\n- See https://example.org/spec) and javascript:alert(1)\r\n- [x](javascript:alert(1)) <script>alert(1)</script>"},
	})
	require.Equal(t, http.StatusSeeOther, res.status, res.body)
	assert.Equal(t, "/proj/merge-requests/1", res.hdr.Get("Location"))
	assert.Equal(t, "no-store", res.hdr.Get("Cache-Control"))
	head := r.headOf("feat/search")

	// The same pair twice is refused with a page, and nothing is created.
	dup := b.form("/proj/merge-requests", token, url.Values{"source": {"feat/search"}, "target": {"main"}, "title": {"dup"}})
	assert.Equal(t, http.StatusConflict, dup.status)
	assert.Equal(t, "no-store", dup.hdr.Get("Cache-Control"))
	assert.Contains(t, dup.body, "already exists")
	bad := b.form("/proj/merge-requests", token, url.Values{"source": {"feat/search"}, "target": {"main"}, "title": {"bidi\u202etitle"}})
	assert.Equal(t, http.StatusBadRequest, bad.status)
	assert.NotContains(t, bad.body, "\u202e")

	// The compare page now points at it.
	assert.Contains(t, b.get("/proj/compare/feat/search", as(token)).body, `href="/proj/merge-requests/1"`)
	assert.NotContains(t, b.get("/proj/compare/feat/search", as(token)).body, `<form class="cform create"`)

	// The list.
	list = b.get("/proj/merge-requests", nil)
	assert.Contains(t, list.body, `<a href="/proj/merge-requests/1">Add &lt;b&gt;search&lt;/b&gt;</a>`)
	assert.Contains(t, list.body, "feat/search → main")
	assert.Equal(t, "no-cache", list.hdr.Get("Cache-Control"))
	assertNoInline(t, list.body)
	assert.Equal(t, "private, no-store", b.get("/proj/merge-requests", as(token)).hdr.Get("Cache-Control"))

	// The page, anonymous: read-only.
	mr := b.get("/proj/merge-requests/1", nil)
	require.Equal(t, 200, mr.status, mr.body)
	assert.Equal(t, "no-cache", mr.hdr.Get("Cache-Control"))
	assert.Equal(t, []string{"Accept-Encoding"}, mr.hdr.Values("Vary"))
	assert.Contains(t, mr.body, "<h1><span class=\"mute\">#1</span> Add &lt;b&gt;search&lt;/b&gt;</h1>")
	assert.NotContains(t, mr.body, "<b>search</b>")
	assert.Contains(t, mr.body, "feat/search")
	assert.Contains(t, mr.body, `<span class="kn">package</span> search</td>`)
	assert.Contains(t, mr.body, "no checks")
	assert.Contains(t, mr.body, "fast-forward, squash")
	// The description is Markdown, rendered as a README: no raw HTML, no
	// script URL; its heading ids keep apart from the ids of the page.
	assert.Contains(t, mr.body, `<article class="mr-desc prose" aria-label="description" hx-ignore>`)
	assert.Contains(t, mr.body, `<h2 id="mr-desc-summary">Summary</h2>`)
	assert.Contains(t, mr.body, `<strong>things</strong>`)
	assert.Contains(t, mr.body, `<code>Find()</code>`)
	assert.Contains(t, mr.body, `<a href="https://example.org/spec"`, "a bare address is linked")
	assert.NotContains(t, mr.body, `href="javascript:`)
	assert.NotContains(t, mr.body, `<script>alert(1)</script>`)
	assert.NotContains(t, mr.body, "<form", "an anonymous visitor gets no form")
	assert.NotContains(t, mr.body, "ermite mr merge", "nor the merge line")
	assert.NotContains(t, mr.body, `id="merge-menu"`)
	assert.Contains(t, mr.body, `href="/proj/commit/`+head+`"`, "the commits are in the rail")
	assert.NotContains(t, strings.ToLower(mr.body), "proposal")
	assertNoInline(t, mr.body)
	assert.Equal(t, 404, b.get("/proj/merge-requests/1/since", nil).status, "the read position is the owner's")
	assert.Equal(t, 404, b.get("/proj/merge-requests/9", nil).status)
	assert.Equal(t, 404, b.get("/proj/merge-requests/01", nil).status)
	assert.Equal(t, 404, b.get("/proj/merge-requests/x", nil).status)

	// Signed in: forms, the merge box with the head and the CLI lines.
	own := b.get("/proj/merge-requests/1", as(token))
	require.Equal(t, 200, own.status)
	assert.Equal(t, "private, no-store", own.hdr.Get("Cache-Control"))
	assert.Contains(t, own.hdr.Values("Vary"), "Cookie")
	assert.Equal(t, head, formHead(t, own.body))
	assert.Contains(t, own.body, `action="/proj/merge-requests/1/merge"`)
	assert.Contains(t, own.body, `id="merge-menu"`, "the merge is a menu of the bar above the diff")
	assert.Contains(t, own.body, `name="mode" value="fast-forward" checked`, "the first possible mode is the default")
	assert.Contains(t, own.body, `name="mode" value="squash"`)
	assert.Contains(t, own.body, `name="mode" value="merge"`, "every mode is allowed by default")
	assert.Contains(t, own.body, "ermite mr merge proj 1 --head "+head)
	assert.Contains(t, own.body, `name="delete_branch" value="1"> delete feat/search after the merge <kbd aria-hidden="true">D</kbd>`)
	assert.NotContains(t, own.body, `name="delete_branch" value="1" checked>`, "unchecked by default")
	assert.Contains(t, own.body, "ermite mr merge proj 1 --head "+head+" --mode squash")
	assert.Contains(t, own.body, "ermite mr merge proj 1 --head "+head+" --mode merge")
	assert.NotContains(t, own.body, "--squash")
	assert.Contains(t, own.body, `action="/proj/merge-requests/1/comments"`)
	assert.Contains(t, own.body, `action="/proj/merge-requests/1/close"`)
	assert.Contains(t, own.body, `href="/proj/merge-requests/1?on=new:1:search.go#f`, "line numbers link to their form")
	assertNoInline(t, own.body)

	// Reading is safe: GET marked nothing as read.
	assert.NotContains(t, own.body, "since your last read")
	show := r.cli(t, adminToken, "mr", "show", "proj", "1", "--json").ok(t)
	assert.Contains(t, show.stdout, `"last_read_id": ""`)
}

// A private repository is invisible to the anonymous visitor on every merge
// request page, and the owner sees it.
func TestWebMergeRequestPrivate(t *testing.T) {
	r := newMRRepo(t, domain.VisibilityPrivate)
	b := browser{r.harness}
	token := r.signIn(t)
	r.cli(t, adminToken, "mr", "create", "proj", "--source", "feat/search", "--title", "Secret plan").ok(t)

	missing := b.get("/nothere/merge-requests/1", nil)
	for _, p := range []string{
		"/proj/merge-requests", "/proj/merge-requests/1", "/proj/merge-requests/1/since", "/proj/compare/feat/search",
	} {
		got := b.get(p, nil)
		assert.Equal(t, 404, got.status, p)
		assert.NotContains(t, got.body, "Secret plan", p)
		assert.NotContains(t, got.body, "search.go", p)
	}
	assert.Equal(t, missing.body, b.get("/proj/merge-requests/1", nil).body, "same page as a missing repository")
	assert.Equal(t, 404, b.get("/-/events?topic=repo:proj&view=mrs", nil).status)

	for _, p := range []string{"/proj/merge-requests", "/proj/merge-requests/1", "/proj/merge-requests/1/since", "/proj/compare/feat/search"} {
		got := b.get(p, as(token))
		assert.Equal(t, 200, got.status, p)
		assert.Equal(t, "private, no-store", got.hdr.Get("Cache-Control"), p)
		assert.Contains(t, got.hdr.Values("Vary"), "Cookie", p)
	}
	assert.Contains(t, b.get("/proj/merge-requests", as(token)).body, "Secret plan")
}

// AC2: every write needs the session and an Origin of this site.
func TestWebMergeRequestWritesNeedSessionAndOrigin(t *testing.T) {
	r := newMRRepo(t, domain.VisibilityPublic)
	b := browser{r.harness}
	token := r.signIn(t)
	r.cli(t, adminToken, "mr", "create", "proj", "--source", "feat/search", "--title", "t").ok(t)
	head := r.headOf("feat/search")

	paths := map[string]string{
		"/proj/merge-requests":                      "source=feat/search&target=main&title=x",
		"/proj/merge-requests/1/comments":           "text=hello",
		"/proj/merge-requests/1/comments/1/resolve": "resolved=true",
		"/proj/merge-requests/1/merge":              "mode=fast-forward&head_id=" + head,
		"/proj/merge-requests/1/close":              "",
		"/proj/merge-requests/1/read":               "head_id=" + head,
		"/proj/merge-requests/1/ready":              "",
	}
	for path, body := range paths {
		noOrigin := b.do("POST", path, as(token), body)
		assert.Equal(t, 403, noOrigin.status, path+" without Origin")
		assert.Equal(t, "no-store", noOrigin.hdr.Get("Cache-Control"))
		foreign := b.do("POST", path, map[string]string{"Cookie": sessionCookie + "=" + token, "Origin": "https://evil.example"}, body)
		assert.Equal(t, 403, foreign.status, path+" foreign Origin")
		anon := b.do("POST", path, map[string]string{"Origin": publicOrigin}, body)
		assert.Equal(t, 403, anon.status, path+" without a session")
	}
	// Nothing happened.
	out := r.cli(t, "", "mr", "show", "proj", "1", "--json").ok(t).stdout
	assert.Contains(t, out, `"state": "open"`)
	assert.NotContains(t, out, "hello")
	assert.Equal(t, "", strings.TrimSpace(r.bareGit("proj", "rev-list", "main..feat/search", "--not", "feat/search")))

	// A GET on a write route is not a page.
	assert.Equal(t, 404, b.get("/proj/merge-requests/1/merge", as(token)).status)
	assert.Equal(t, 404, b.get("/proj/merge-requests/1/close", as(token)).status)
}

var reNoteID = regexp.MustCompile(`id="c(\d+)"`)

// The review of one request, in the browser's words: a note on a line, a
// general one, resolve, since the last read after a push, a stale head, the
// fast-forward merge.
func TestWebMergeRequestReviewAndFastForward(t *testing.T) {
	r := newMRRepo(t, domain.VisibilityPublic)
	b := browser{r.harness}
	token := r.signIn(t)
	r.cli(t, adminToken, "mr", "create", "proj", "--source", "feat/search", "--title", "Add search").ok(t)
	head1 := r.headOf("feat/search")
	base := "/proj/merge-requests/1"

	// The form of a line, then the note under that line.
	line := b.get(base+"?on=new:3:search.go", as(token))
	require.Equal(t, 200, line.status)
	assert.Contains(t, line.body, `<form class="cform"`)
	assert.Contains(t, line.body, `name="path" value="search.go"`)
	assert.Contains(t, line.body, `name="line" value="3"`)
	assert.Contains(t, line.body, `name="side" value="new"`)
	assert.Contains(t, line.body, `comment on search.go:3 (new)`, "one line: the title names it")
	assert.Contains(t, b.get(base+"?on=new:1-3:search.go", as(token)).body, `comment on search.go:1-3 (new)`, "a range: the title names all of it")
	assert.Equal(t, 0, strings.Count(b.get(base+"?on=new:99:search.go", as(token)).body, `name="line" value="99"`), "a line the diff lacks has no form")
	assert.Equal(t, 0, strings.Count(b.get(base+"?on=new:3:search.go", nil).body, `<form`), "nor for a visitor")

	res := b.form(base+"/comments", token, url.Values{"text": {"Why <i>empty</i>?\r\nSee https://example.org/a."}, "path": {"search.go"}, "line": {"3"}, "side": {"new"}})
	require.Equal(t, http.StatusSeeOther, res.status, res.body)
	assert.Equal(t, base+"#c1", res.hdr.Get("Location"))
	assert.Equal(t, "no-store", res.hdr.Get("Cache-Control"))
	res = b.form(base+"/comments", token, url.Values{"text": {"Looks fine overall."}})
	require.Equal(t, http.StatusSeeOther, res.status, res.body)
	assert.Equal(t, base+"#c2", res.hdr.Get("Location"))

	anon := b.get(base, nil)
	i := strings.Index(anon.body, `<tr class="fnote">`)
	require.GreaterOrEqual(t, i, 0, "a note row under the line")
	note := anon.body[i:]
	assert.Less(t, strings.Index(note, "Why &lt;i&gt;empty&lt;/i&gt;?"), strings.Index(note, "</tr>"), "the note sits in the row under its line")
	assert.Contains(t, anon.body, `<a href="https://example.org/a" rel="nofollow noopener noreferrer">https://example.org/a</a>.`)
	assert.NotContains(t, anon.body, "<i>empty</i>")
	assert.Contains(t, anon.body, "Looks fine overall.")
	assert.Equal(t, 2, len(reNoteID.FindAllString(anon.body, -1)))
	assert.NotContains(t, anon.body, "[resolve]", "a visitor cannot resolve")

	// A comment that breaks a rule is refused with the rule, nothing is lost.
	bad := b.form(base+"/comments", token, url.Values{"text": {"ok"}, "path": {"search.go"}, "line": {"99"}, "side": {"new"}})
	assert.Equal(t, http.StatusBadRequest, bad.status)
	assert.Equal(t, "no-store", bad.hdr.Get("Cache-Control"))
	assert.Contains(t, bad.body, "search.go")
	bad = b.form(base+"/comments", token, url.Values{"text": {"x"}, "path": {"search.go"}, "line": {"zz"}, "side": {"new"}})
	assert.Equal(t, http.StatusBadRequest, bad.status)
	bad = b.form(base+"/comments", token, url.Values{"text": {"x"}, "path": {"search.go"}, "line": {"3"}, "side": {"sideways"}})
	assert.Equal(t, http.StatusBadRequest, bad.status)
	bad = b.form(base+"/comments", token, url.Values{"text": {"evil\u202etext"}})
	assert.Equal(t, http.StatusBadRequest, bad.status)
	assert.NotContains(t, bad.body, "\u202e")
	assert.Equal(t, 404, b.form("/proj/merge-requests/9/comments", token, url.Values{"text": {"x"}}).status)

	// Resolve and unresolve.
	own := b.get(base, as(token))
	assert.Contains(t, own.body, `action="/proj/merge-requests/1/comments/1/resolve"`)
	res = b.form(base+"/comments/1/resolve", token, url.Values{"resolved": {"true"}})
	require.Equal(t, http.StatusSeeOther, res.status, res.body)
	assert.Equal(t, base+"#c1", res.hdr.Get("Location"))
	own = b.get(base, as(token))
	assert.Contains(t, own.body, `data-resolved="true"`)
	assert.Contains(t, own.body, ">Unresolve</button>")
	assert.Equal(t, http.StatusSeeOther, b.form(base+"/comments/1/resolve", token, url.Values{"resolved": {"false"}}).status)
	assert.Equal(t, http.StatusBadRequest, b.form(base+"/comments/1/resolve", token, url.Values{"resolved": {"maybe"}}).status)
	assert.Equal(t, 404, b.form(base+"/comments/42/resolve", token, url.Values{"resolved": {"true"}}).status)

	// Mark read, then push: the since page shows the new change only.
	assert.Contains(t, own.body, `action="/proj/merge-requests/1/read"`)
	res = b.form(base+"/read", token, url.Values{"head_id": {head1}})
	require.Equal(t, http.StatusSeeOther, res.status, res.body)
	assert.NotContains(t, b.get(base, as(token)).body, "data-read>Mark read", "the head is the one that was read")
	r.writeFile(r.wt, "extra.txt", []byte("extra\n"))
	r.mustGit(r.wt, "push", "-q", r.url, "feat/search")
	head2 := r.headOf("feat/search")

	all := b.get(base, as(token))
	assert.Contains(t, all.body, `href="/proj/merge-requests/1/since"`)
	assert.Contains(t, all.body, "since your last read")
	assert.Contains(t, all.body, "<tr data-new>", "the commit pushed since is marked")
	assert.Contains(t, all.body, "data-read>Mark read")
	assert.Contains(t, all.body, "outdated", "the notes were written on the earlier head")
	since := b.get(base+"/since", as(token))
	require.Equal(t, 200, since.status, since.body)
	assert.Equal(t, "private, no-store", since.hdr.Get("Cache-Control"))
	assert.Contains(t, since.body, "changes since "+head1[:7])
	assert.Contains(t, since.body, "extra</td>")
	assert.NotContains(t, since.body, `<span class="kn">package</span> search</td>`, "only what changed after the read head")
	assert.Contains(t, since.body, "empty", "the notes are listed below, their lines are not in this diff")
	assert.NotContains(t, since.body, "<i>", "raw HTML is dropped")
	assert.Contains(t, since.body, "search.go:3 (new)")
	assert.NotContains(t, since.body, `<form class="cform"`, "no comment form on this diff")

	// The merge form of the page the owner read carries that head: the push
	// that came after is not merged unseen.
	stale := b.form(base+"/merge", token, url.Values{"mode": {"fast-forward"}, "head_id": {head1}})
	assert.Equal(t, http.StatusConflict, stale.status)
	assert.Equal(t, "no-store", stale.hdr.Get("Cache-Control"))
	assert.Contains(t, stale.body, "branch moved")
	assert.NotEqual(t, head2, r.headOf("main"))
	assert.Equal(t, http.StatusBadRequest, b.form(base+"/merge", token, url.Values{"mode": {"rebase"}, "head_id": {head2}}).status)
	assert.Equal(t, http.StatusBadRequest, b.form(base+"/merge", token, url.Values{"mode": {"squash"}, "head_id": {"abc"}}).status)

	// Fast-forward with the head that was read.
	fresh := b.get(base, as(token))
	require.Equal(t, head2, formHead(t, fresh.body))
	res = b.form(base+"/merge", token, url.Values{"mode": {"fast-forward"}, "head_id": {formHead(t, fresh.body)}})
	require.Equal(t, http.StatusSeeOther, res.status, res.body)
	assert.Equal(t, base, res.hdr.Get("Location"))
	assert.Equal(t, head2, r.headOf("main"), "the target moved to the head")

	done := b.get(base, nil)
	assert.Contains(t, done.body, "merged")
	assert.Contains(t, done.body, "fast-forward")
	assert.Contains(t, done.body, `href="/proj/commit/`+head2+`"`)
	assert.NotContains(t, done.body, "<form")
	assert.NotContains(t, b.get(base, as(token)).body, `id="merge-menu"`, "nothing to merge any more")
	// Writes on a merged request are refused with a page.
	late := b.form(base+"/comments", token, url.Values{"text": {"too late"}})
	assert.Equal(t, http.StatusConflict, late.status)
	assert.Equal(t, http.StatusConflict, b.form(base+"/close", token, nil).status)
	list := b.get("/proj/merge-requests", nil)
	assert.Contains(t, list.body, "Merged and closed")
	assert.Regexp(t, `(?s)Merged and closed.*Add search`, list.body)
}

// Squash and close, and the order of the list: open first, then the decided.
func TestWebMergeRequestSquashCloseAndList(t *testing.T) {
	r := newMRRepo(t, domain.VisibilityPublic)
	b := browser{r.harness}
	token := r.signIn(t)
	head := r.headOf("feat/search")
	main0 := r.headOf("main")

	// Open from compare with a description; the merge form squashes.
	res := b.form("/proj/merge-requests", token, url.Values{
		"source": {"feat/search"}, "target": {"main"}, "title": {"Squash me"}, "description": {""},
	})
	require.Equal(t, http.StatusSeeOther, res.status, res.body)
	assert.Contains(t, b.get("/proj/merge-requests/1", as(token)).body, `name="delete_branch" value="1"`, "the box offers to delete the branch")
	res = b.form("/proj/merge-requests/1/merge", token, url.Values{"mode": {"squash"}, "head_id": {head}, "delete_branch": {"1"}})
	require.Equal(t, http.StatusSeeOther, res.status, res.body)
	r.forge.MergeRequests.Wait(r.ctx) // the deletion follows the redirect
	assert.Empty(t, r.bareGit("proj", "for-each-ref", "refs/heads/feat/search"), "the source branch is deleted")
	merged := r.headOf("main")
	assert.NotEqual(t, head, merged, "squash makes a new commit")
	assert.Equal(t, main0, r.bareGit("proj", "rev-parse", "main^"), "on top of the old target")
	assert.Contains(t, r.bareGit("proj", "cat-file", "-p", "main"), "gpgsig", "signed by the instance key")
	page := b.get("/proj/merge-requests/1", nil)
	assert.Contains(t, page.body, "squash")
	assert.Contains(t, page.body, `href="/proj/commit/`+merged+`"`)

	// A second request: closed.
	r.mustGit(r.wt, "checkout", "-q", "-b", "feat/other", "main")
	r.mustGit(r.wt, "pull", "-q", r.url, "main")
	r.writeFile(r.wt, "other.go", []byte("package other\n"))
	r.mustGit(r.wt, "push", "-q", r.url, "feat/other")
	r.mustGit(r.wt, "checkout", "-q", "-b", "feat/third", "feat/other")
	r.writeFile(r.wt, "third.go", []byte("package third\n"))
	r.mustGit(r.wt, "push", "-q", r.url, "feat/third")
	for _, title := range []struct{ src, title string }{{"feat/other", "Other change"}, {"feat/third", "Third change"}} {
		res = b.form("/proj/merge-requests", token, url.Values{"source": {title.src}, "target": {"main"}, "title": {title.title}})
		require.Equal(t, http.StatusSeeOther, res.status, res.body)
	}
	res = b.form("/proj/merge-requests/2/close", token, nil)
	require.Equal(t, http.StatusSeeOther, res.status, res.body)
	assert.Equal(t, "/proj/merge-requests/2", res.hdr.Get("Location"))
	assert.Equal(t, "no-store", res.hdr.Get("Cache-Control"))
	closed := b.get("/proj/merge-requests/2", nil)
	assert.Contains(t, closed.body, `data-state="closed"`)
	assert.NotContains(t, closed.body, "<form")
	assert.Equal(t, http.StatusConflict, b.form("/proj/merge-requests/2/close", token, nil).status, "closing twice says so")

	// Open one first, then merged and closed ones, newest first within each.
	list := b.get("/proj/merge-requests", nil).body
	third, squash, other := strings.Index(list, "Third change"), strings.Index(list, "Squash me"), strings.Index(list, "Other change")
	require.True(t, third >= 0 && squash >= 0 && other >= 0, list)
	assert.Less(t, third, squash, "the open request comes first")
	assert.Less(t, third, other)
	assert.Less(t, strings.Index(list, ">Open<"), strings.Index(list, "Merged and closed"))
	assert.Less(t, other, squash, "newest first among the decided")

	// A compare page of a branch that is behind says so.
	r.mustGit(r.wt, "checkout", "-q", "-b", "feat/stale", main0)
	r.writeFile(r.wt, "stale.go", []byte("package stale\n"))
	r.mustGit(r.wt, "push", "-q", r.url, "feat/stale")
	stale := b.get("/proj/compare/feat/stale", nil)
	assert.Equal(t, 200, stale.status)
	assert.Contains(t, stale.body, "has moved on")
}

// A "WIP:" request is a draft: the list and the page say so, the box offers
// "Ready for review" instead of the merge form and the terminal lines, the
// merge is refused; ready drops the prefix and the merge form comes back. The
// tab counts the open requests.
func TestWebMergeRequestDraft(t *testing.T) {
	r := newMRRepo(t, domain.VisibilityPublic)
	b := browser{r.harness}
	token := r.signIn(t)
	head := r.headOf("feat/search")
	r.cli(t, adminToken, "mr", "create", "proj", "--source", "feat/search", "--title", "WIP: search").ok(t)

	page := b.get("/proj/merge-requests/1", as(token)).body
	assert.Contains(t, page, `data-state="draft"`)
	assert.Contains(t, page, `action="/proj/merge-requests/1/ready"`)
	assert.NotContains(t, page, `action="/proj/merge-requests/1/merge"`)
	assert.NotContains(t, page, "ermite mr merge", "no terminal line for a draft")
	assert.Contains(t, page, `<span class="count">1<span class="sr"> open</span></span>`, "the tab counts the open requests")
	assert.Contains(t, b.get("/proj/merge-requests", nil).body, `data-state="draft"`)

	res := b.form("/proj/merge-requests/1/merge", token, url.Values{"mode": {"fast-forward"}, "head_id": {head}})
	assert.Equal(t, http.StatusConflict, res.status, "a draft is not merged")
	assert.Contains(t, res.body, `data-canonical="/proj/merge-requests/1"`, "the refused page names its own address")
	assert.Contains(t, res.body, `class="d-err"`, "the reason stays inline")
	assert.NotContains(t, b.get("/proj/merge-requests/1", as(token)).body, "data-canonical", "the plain page has no marker")

	res = b.form("/proj/merge-requests/1/ready", token, nil)
	require.Equal(t, http.StatusSeeOther, res.status, res.body)
	page = b.get("/proj/merge-requests/1", as(token)).body
	assert.Contains(t, page, "> search</h1>", "the prefix is gone")
	assert.Contains(t, page, `data-state="open"`)
	assert.Contains(t, page, `action="/proj/merge-requests/1/merge"`)
	assert.Contains(t, page, `<details class="d-or">`, "the terminal lines fold away")
	assert.Equal(t, http.StatusConflict, b.form("/proj/merge-requests/1/ready", token, nil).status, "not a draft any more")
}

// A request opened with a token shows the owner's name, not the label of the
// token, which names a device.
func TestWebMergeRequestAuthorIsTheOwner(t *testing.T) {
	r := newMRRepo(t, domain.VisibilityPublic)
	out, errOut, code := r.bin(t, adminToken, "token", "create", "laptop", "--preset", "cli")
	require.Equal(t, 0, code, errOut)
	r.cli(t, strings.TrimSpace(out), "mr", "create", "proj", "--source", "feat/search", "--title", "search").ok(t)

	b := browser{r.harness}
	for _, path := range []string{"/proj/merge-requests/1", "/proj/merge-requests"} {
		body := b.get(path, nil).body
		assert.NotContains(t, body, "laptop", path)
	}
	assert.Contains(t, b.get("/proj/merge-requests", nil).body, `<td class="c-who dim">owner</td>`)
}

// The box offers no deletion for the default branch; a fast-forward with the
// box checked deletes the source.
func TestWebMergeDeleteBranchBox(t *testing.T) {
	r := newMRRepo(t, domain.VisibilityPublic)
	b := browser{r.harness}
	token := r.signIn(t)
	// old is an orphan branch: main can be proposed to it, and main stays an
	// ancestor of feat/search so that it fast-forwards.
	r.mustGit(r.wt, "checkout", "-q", "--orphan", "old")
	r.writeFile(r.wt, "old.txt", []byte("old\n")) // commits
	r.mustGit(r.wt, "push", "-q", r.url, "old")

	for _, mr := range []url.Values{
		{"source": {"feat/search"}, "target": {"main"}, "title": {"Search"}},
		{"source": {"main"}, "target": {"old"}, "title": {"Back"}},
	} {
		require.Equal(t, http.StatusSeeOther, b.form("/proj/merge-requests", token, mr).status)
	}
	assert.NotContains(t, b.get("/proj/merge-requests/2", as(token)).body, `name="delete_branch"`, "never for the default branch")

	one := b.get("/proj/merge-requests/1", as(token))
	res := b.form("/proj/merge-requests/1/merge", token, url.Values{"mode": {"fast-forward"}, "head_id": {formHead(t, one.body)}, "delete_branch": {"1"}})
	require.Equal(t, http.StatusSeeOther, res.status, res.body)
	r.forge.MergeRequests.Wait(r.ctx) // the deletion follows the redirect
	assert.Empty(t, r.bareGit("proj", "for-each-ref", "refs/heads/feat/search"))
	done := b.get("/proj/merge-requests/1", nil)
	assert.Equal(t, 200, done.status, "the merged page reads without its branch")
	assert.NotContains(t, done.body, `href="/proj/tree/feat/search"`, "no link to a gone branch")
}

// The comment limit of the API is a message, not a crash.
func TestWebMergeRequestLimitIsFriendly(t *testing.T) {
	r := newMRRepo(t, domain.VisibilityPublic)
	b := browser{r.harness}
	token := r.signIn(t)
	r.cli(t, adminToken, "mr", "create", "proj", "--source", "feat/search", "--title", "t").ok(t)
	for i := 0; i < 1000; i++ {
		_, err := r.forge.MergeRequests.AddComment(r.ctx, domain.Root(), r.name("proj"), 1, ports.AddCommentParams{Text: "n"})
		require.NoError(t, err)
	}
	full := b.form("/proj/merge-requests/1/comments", token, url.Values{"text": {"one more"}})
	assert.Equal(t, http.StatusConflict, full.status)
	assert.Contains(t, full.body, "limit reached")
	assert.Contains(t, full.body, "1000 comments")
	assert.Equal(t, "no-store", full.hdr.Get("Cache-Control"))
}

// Text is escaped everywhere and invisible characters are shown as markers.
func TestWebMergeRequestEscapesText(t *testing.T) {
	r := newMRRepo(t, domain.VisibilityPublic)
	b := browser{r.harness}
	token := r.signIn(t)
	res := b.form("/proj/merge-requests", token, url.Values{
		"source": {"feat/search"}, "target": {"main"}, "title": {`"><script>alert(1)</script>`},
		"description": {"<img src=x onerror=alert(1)>\nhttp://x.test/\"onmouseover=\"alert(1)"},
	})
	require.Equal(t, http.StatusSeeOther, res.status, res.body)
	r.cli(t, adminToken, "mr", "comment", "proj", "1", "-m", "<script>x</script> & more").ok(t)
	for _, tok := range []string{"", token} {
		var h map[string]string
		if tok != "" {
			h = as(tok)
		}
		body := b.get("/proj/merge-requests/1", h).body
		assert.NotContains(t, body, "<script>alert")
		assert.NotContains(t, body, "<img src=x")
		assert.NotContains(t, body, "<script>x</script>")
		assert.Contains(t, html.UnescapeString(body), `"><script>alert(1)</script>`)
		// A note is safe Markdown: a line that opens with an HTML tag is an HTML
		// block, dropped whole.
		assert.Contains(t, body, `class="c-body prose"><!-- raw HTML omitted -->`)
		assertNoInline(t, body)
		assert.NotContains(t, body, `onmouseover="alert`, "an address stops at a quote")
	}
}

// The merge request pages learn of changes over the event stream: a fixed
// sentence and a link, never the text of a comment or a title.
func TestWebMergeRequestEvents(t *testing.T) {
	r := newMRRepo(t, domain.VisibilityPublic)
	list := r.sse("topic=repo:proj&view=mrs", "")
	r.cli(t, adminToken, "mr", "create", "proj", "--source", "feat/search", "--title", "Secret <title>").ok(t)
	ev := nextNamed(t, list, "mr-notice")
	assert.Contains(t, ev.data, `<div id="mr-notice" class="live-slot" role="status" data-slot>`)
	assert.Contains(t, ev.data, `href="/proj/merge-requests"`)
	assert.NotContains(t, ev.data, "Secret")

	one := r.sse("topic=repo:proj&view=mr&n=1&ref=feat/search", "")
	other := r.sse("topic=repo:proj&view=mr&n=2&ref=feat/other", "")
	r.cli(t, adminToken, "mr", "comment", "proj", "1", "-m", "private words").ok(t)
	ev = nextNamed(t, one, "mr-notice")
	assert.Contains(t, ev.data, "a comment was added")
	assert.Contains(t, ev.data, `href="/proj/merge-requests/1"`)
	assert.NotContains(t, ev.data, "private words")

	r.writeFile(r.wt, "more.txt", []byte("more\n"))
	r.mustGit(r.wt, "push", "-q", r.url, "feat/search")
	ev = nextNamed(t, one, "mr-notice")
	assert.Contains(t, ev.data, "the branch changed")

	r.cli(t, adminToken, "mr", "close", "proj", "1").ok(t)
	assert.Contains(t, nextNamed(t, one, "mr-notice").data, "was closed")

	// A page of another request hears nothing of this one.
	for drained := false; !drained; {
		select {
		case ev := <-other.events:
			if !ev.comment && ev.name != "" {
				t.Fatalf("unexpected event %+v", ev)
			}
		default:
			drained = true
		}
	}
	assert.Contains(t, r.get1("/proj/merge-requests/1"), `hx-sse:connect="/-/events?topic=repo%3Aproj&amp;view=mr&amp;n=1&amp;ref=feat%2Fsearch&amp;head=`+r.headOf("feat/search")+`&amp;target=main"`)
}

// nextNamed returns the next event that carries the fragment event: the
// events that change nothing on the page (an id alone) are skipped.
func nextNamed(t *testing.T, s *sseStream, name string) sseEvent {
	t.Helper()
	for range 10 {
		if ev := s.next(t, watchWait); ev.name == name {
			return ev
		}
	}
	t.Fatalf("no %s event", name)
	return sseEvent{}
}

func (r *mrRepo) get1(path string) string {
	r.t.Helper()
	_, _, body := r.get(path)
	return body
}

// A rewritten branch that dropped the commit the owner read says so, and shows
// the whole change.
func TestWebMergeRequestSinceAfterForcePush(t *testing.T) {
	r := newMRRepo(t, domain.VisibilityPublic)
	b := browser{r.harness}
	token := r.signIn(t)
	r.cli(t, adminToken, "mr", "create", "proj", "--source", "feat/search", "--title", "t").ok(t)
	base := "/proj/merge-requests/1"

	nothing := b.get(base+"/since", as(token))
	assert.Equal(t, 200, nothing.status)
	assert.Contains(t, nothing.body, "nothing was marked read yet")

	head1 := r.headOf("feat/search")
	require.Equal(t, http.StatusSeeOther, b.form(base+"/read", token, url.Values{"head_id": {head1}}).status)
	assert.Equal(t, http.StatusBadRequest, b.form(base+"/read", token, url.Values{"head_id": {strings.Repeat("0", 40)}}).status, "not a commit of the repository")
	r.mustGit(r.wt, "reset", "-q", "--hard", "main")
	r.writeFile(r.wt, "other.txt", []byte("o\n"))
	r.mustGit(r.wt, "push", "-q", "--force", r.url, "feat/search")
	// Push refreshes the pull refs asynchronously. Reconcile them synchronously
	// before GC, or the old pull head/merge can still retain the read commit.
	require.NoError(t, r.forge.CI.RescanPulls(r.ctx, r.name("proj")))
	require.NotContains(t, strings.Fields(r.bareGit("proj", "rev-list", "--all")), head1)
	r.bareGit("proj", "reflog", "expire", "--expire=now", "--all")
	r.bareGit("proj", "gc", "-q", "--prune=now")

	lost := b.get(base+"/since", as(token))
	require.Equal(t, 200, lost.status)
	assert.Contains(t, lost.body, "is gone, the branch was rewritten")
	assert.Contains(t, lost.body, "other.txt", "the whole change is shown")
}

// A request of many files: the page lists every file and holds the hunks of
// the first window only; the other files are placeholders that load their
// window from /files, which carries the base and the head of the diff. A
// comment on a far file comes with its window; a moved head answers 409; the
// page of a window (?from=, the links of a page without scripts) shows its
// hunks and its line forms.
func TestWebMergeRequestFileWindows(t *testing.T) {
	r := newMRRepo(t, domain.VisibilityPublic)
	b := browser{r.harness}
	token := r.signIn(t)
	const n = 60
	for i := range n {
		r.writeFile(r.wt, fmt.Sprintf("many/f%02d.txt", i), []byte(fmt.Sprintf("line %d\n", i)))
	}
	r.mustGit(r.wt, "push", "-q", r.url, "feat/search")
	head := r.headOf("feat/search")
	r.cli(t, adminToken, "mr", "create", "proj", "--source", "feat/search", "--title", "Many").ok(t)
	r.cli(t, adminToken, "mr", "comment", "proj", "1", "--path", "many/f55.txt", "--line", "1", "-m", "far note").ok(t)

	page := b.get("/proj/merge-requests/1", nil)
	require.Equal(t, 200, page.status)
	assert.Equal(t, n+1, strings.Count(page.body, `<li><a href="#f`), "every file is listed")
	assert.Equal(t, n+1-25, strings.Count(page.body, `class="dlazy`), "the files past the first window are placeholders")
	assert.NotContains(t, page.body, "far note", "the comment of a far file comes with its window")
	assert.Contains(t, page.body, `data-notes="c1"`, "its placeholder knows it")
	assert.Contains(t, page.body, `<span class="cn" title="1 open comment">1</span>`, "the list counts it")
	m := reWindowSrc.FindStringSubmatch(page.body)
	require.Len(t, m, 2, "a placeholder holds the address of its window")
	src := html.UnescapeString(m[1])
	assert.Contains(t, src, "head="+head)

	win := b.get(strings.Replace(src, "from=25", "from=50", 1), nil)
	require.Equal(t, 200, win.status, win.body)
	assert.Contains(t, win.body, "far note")
	assert.Contains(t, win.body, `line 55</td>`)
	assert.NotContains(t, win.body, "<html", "a window is a fragment")
	assert.NotContains(t, win.body, `class="dlazy`)
	// The window renders the layouts of the page: both when it switches them,
	// the one it shows otherwise.
	both := b.get(src+"&view=split&layouts=both", nil)
	assert.Contains(t, both.body, `class="dt uni"`)
	assert.Contains(t, both.body, `class="dt spl"`)
	one := b.get(src+"&view=split&layouts=one", nil)
	assert.NotContains(t, one.body, `class="dt uni"`)
	assert.Contains(t, one.body, `class="dt spl"`)
	assert.Equal(t, 404, b.get(strings.Replace(src, "from=25", "from=7", 1), nil).status, "not a window start")
	assert.Equal(t, 404, b.get(strings.Replace(src, "from=25", "from=100", 1), nil).status, "past the files")

	// The page of a window, without scripts: its hunks, the line forms of the
	// owner point back to it.
	own := b.get("/proj/merge-requests/1?from=50", as(token))
	require.Equal(t, 200, own.status)
	assert.Contains(t, own.body, "line 55</td>")
	assert.Contains(t, own.body, `href="/proj/merge-requests/1?from=50&amp;on=new:1:many%2Ff55.txt#f`)
	assert.NotContains(t, own.body, "line 0</td>", "the first window is a placeholder there")
	assert.Contains(t, own.body, `href="/proj/merge-requests/1?view=split&amp;from=50"`, "the layout links keep the window")
	assert.Equal(t, 404, b.get("/proj/merge-requests/1?from=3", nil).status)

	// A comment on a line of a far window: its form, its cancel link and the
	// answer return to that window.
	form := b.get("/proj/merge-requests/1?from=50&on=new:1:many%2Ff55.txt", as(token))
	require.Equal(t, 200, form.status)
	assert.Contains(t, form.body, `name="from" value="50"`)
	assert.Contains(t, form.body, `class="btn" href="/proj/merge-requests/1?from=50#f`)
	res := b.form("/proj/merge-requests/1/comments", token, url.Values{"text": {"near it"}, "path": {"many/f55.txt"}, "line": {"1"}, "side": {"new"}, "from": {"50"}})
	require.Equal(t, http.StatusSeeOther, res.status, res.body)
	assert.Equal(t, "/proj/merge-requests/1?from=50#c2", res.hdr.Get("Location"))
	assert.Contains(t, b.get("/proj/merge-requests/1?from=50", as(token)).body, `<input type="hidden" name="from" value="50">`, "its resolve form too")
	res = b.form("/proj/merge-requests/1/comments/2/resolve", token, url.Values{"resolved": {"true"}, "from": {"50"}})
	assert.Equal(t, "/proj/merge-requests/1?from=50#c2", res.hdr.Get("Location"))
	res = b.form("/proj/merge-requests/1/comments/2/resolve", token, url.Values{"resolved": {"false"}, "from": {"7"}})
	assert.Equal(t, "/proj/merge-requests/1#c2", res.hdr.Get("Location"), "a window that is none is dropped")

	// The head moved: the window says to reload, never a file of another diff.
	r.writeFile(r.wt, "later.txt", []byte("later\n"))
	r.mustGit(r.wt, "push", "-q", r.url, "feat/search")
	stale := b.get(src, nil)
	assert.Equal(t, http.StatusConflict, stale.status)
	assert.Contains(t, stale.body, "data-moved")
}

var reWindowSrc = regexp.MustCompile(`data-src="([^"]*from=25[^"]*)"`)

// advanceMain pushes a commit to main that the feature branch lacks. file is
// the file it adds (give search.go to conflict with the feature branch).
func (r *mrRepo) advanceMain(file, content string) {
	r.t.Helper()
	r.mustGit(r.wt, "checkout", "-q", "main")
	r.writeFile(r.wt, file, []byte(content))
	r.mustGit(r.wt, "push", "-q", r.url, "main")
	r.mustGit(r.wt, "checkout", "-q", "feat/search")
}

// mrPageWith polls the owner's page of !1 until it contains want.
func (r *mrRepo) mrPageWith(token, want string) string {
	r.t.Helper()
	var body string
	require.Eventually(r.t, func() bool {
		body = browser{r.harness}.get("/proj/merge-requests/1", as(token)).body
		return strings.Contains(body, want)
	}, 10*time.Second, 25*time.Millisecond, "the page never showed %q", want)
	return body
}

// The merge box shows the mergeability of the request: ready, then behind
// (fast-forward is disabled with its reason, the other modes are not), then
// in conflict (no mode is possible and the button says why).
func TestWebMergeRequestMergeabilityBadge(t *testing.T) {
	r := newMRRepo(t, domain.VisibilityPublic)
	token := r.signIn(t)
	r.cli(t, adminToken, "mr", "create", "proj", "--source", "feat/search", "--title", "Add search").ok(t)

	body := r.mrPageWith(token, "Ready to merge")
	assert.Contains(t, body, `<div id="mr-mergeability" class="live-slot" role="status" data-slot>`)
	assert.Contains(t, body, `data-state="up_to_date"`)
	assert.Contains(t, body, `name="mode" value="fast-forward" checked`)
	assert.NotContains(t, body, `value="fast-forward" checked disabled`)
	assert.Contains(t, body, `id="mr-signing"`, "squash and merge commits say who signs them")
	assert.Contains(t, body, "signed with ermite's own key (generated on first start)")
	assert.Contains(t, body, "<code>ermite signing-key show</code>")
	assert.Contains(t, body, "SHA256:", "the fingerprint of the signing key")
	assert.Contains(t, body, `:disabled="`, "the controls follow the badge")
	assertNoInline(t, body)

	// Main moves on: the branch is behind by one commit and merges cleanly.
	r.advanceMain("other.txt", "other\n")
	body = r.mrPageWith(token, "Behind main by 1 commit")
	assert.Contains(t, body, "merges without conflicts")
	assert.Contains(t, body, `data-ff-ok="false"`)
	assert.Contains(t, body, `data-merge-ok="true"`)
	assert.Regexp(t, `<input type="radio" name="mode" value="fast-forward"[^>]* disabled`, body)
	assert.Contains(t, body, `title="behind the target: rebase or use squash/merge"`)
	assert.Regexp(t, `name="mode" value="squash" checked`, body, "the first possible mode is checked")
	assert.NotRegexp(t, `name="mode" value="squash"[^>]* disabled`, body)

	// A conflicting change to main: every mode is disabled and the files are listed.
	r.mustGit(r.wt, "checkout", "-q", "main")
	r.writeFile(r.wt, "search.go", []byte("package other\n"))
	r.mustGit(r.wt, "push", "-q", r.url, "main")
	body = r.mrPageWith(token, "Conflicts with main")
	assert.Contains(t, body, "rebase needed")
	assert.Contains(t, body, `<ul class="mg-paths" aria-label="conflicting files"><li><code>search.go</code></li></ul>`)
	assert.Regexp(t, `<button type="submit" class="btn primary" data-default disabled title="conflicts with the target"`, body)
	assert.NotContains(t, body, `name="mode" value="squash" checked`)

	// The server refuses it too, and says so in the box of the page.
	res := browser{r.harness}.form("/proj/merge-requests/1/merge", token, url.Values{"mode": {"squash"}, "head_id": {formHead(t, body)}})
	assert.Equal(t, http.StatusConflict, res.status)
	assert.Contains(t, res.body, `<p class="d-err" role="alert">`)
	assert.Contains(t, res.body, "conflicts with main")
}

// A merge the server refuses (the target moved after the page was read) shows
// the page of the request again, 409, with the reason in its merge box.
func TestWebMergeRequestInlineMergeError(t *testing.T) {
	r := newMRRepo(t, domain.VisibilityPublic)
	b := browser{r.harness}
	token := r.signIn(t)
	r.cli(t, adminToken, "mr", "create", "proj", "--source", "feat/search", "--title", "Add search").ok(t)
	body := r.mrPageWith(token, "Ready to merge")
	head := formHead(t, body)

	r.advanceMain("other.txt", "other\n") // raced: the form below still says fast-forward
	res := b.form("/proj/merge-requests/1/merge", token, url.Values{"mode": {"fast-forward"}, "head_id": {head}})
	assert.Equal(t, http.StatusConflict, res.status)
	assert.Equal(t, "no-store", res.hdr.Get("Cache-Control"))
	assert.Contains(t, res.body, `id="merge-menu"`, "the page of the request, not a state page")
	assert.Contains(t, res.body, `<p class="d-err" role="alert">`)
	assert.Contains(t, res.body, "the target has moved")
	assert.Contains(t, res.body, `<details class="menu mmenu" id="merge-menu" data-state="open" open>`, "the box is open on the error")
	assert.Equal(t, head, r.headOf("feat/search"))
	assert.NotContains(t, b.get("/proj/merge-requests/1", as(token)).body, "d-err", "a reload shows no error")

	// A head that moved is refused the same way.
	r.writeFile(r.wt, "more.txt", []byte("more\n"))
	r.mustGit(r.wt, "push", "-q", r.url, "feat/search")
	res = b.form("/proj/merge-requests/1/merge", token, url.Values{"mode": {"squash"}, "head_id": {head}})
	assert.Equal(t, http.StatusConflict, res.status)
	assert.Contains(t, res.body, `<p class="d-err" role="alert">`)
	assert.Contains(t, res.body, "branch moved")

	// Other refusals keep the state page; a mode that does not exist is a 400.
	assert.Equal(t, http.StatusBadRequest, b.form("/proj/merge-requests/1/merge", token, url.Values{"mode": {"rebase"}, "head_id": {head}}).status)
}

// A merge commit through the page; with the branch deletion requested the
// merged page says the branch is being deleted, not that it is gone.
func TestWebMergeRequestMergeCommitAndDeletingNote(t *testing.T) {
	r := newMRRepo(t, domain.VisibilityPublic)
	b := browser{r.harness}
	token := r.signIn(t)
	r.cli(t, adminToken, "mr", "create", "proj", "--source", "feat/search", "--title", "Add search").ok(t)
	r.advanceMain("other.txt", "other\n")
	body := r.mrPageWith(token, "Behind main by 1 commit")
	head := formHead(t, body)
	main0 := r.headOf("main")

	res := b.form("/proj/merge-requests/1/merge", token, url.Values{"mode": {"merge"}, "head_id": {head}, "delete_branch": {"1"}})
	require.Equal(t, http.StatusSeeOther, res.status, res.body)
	assert.Equal(t, "/proj/merge-requests/1?deleting=1", res.hdr.Get("Location"))
	merged := r.headOf("main")
	assert.Equal(t, main0, r.bareGit("proj", "rev-parse", merged+"^1"))
	assert.Equal(t, head, r.bareGit("proj", "rev-parse", merged+"^2"))
	assert.Contains(t, r.bareGit("proj", "cat-file", "-p", merged), "gpgsig")
	page := b.get("/proj/merge-requests/1?deleting=1", as(token))
	assert.Contains(t, page.body, "merge commit")
	if strings.Contains(page.body, "is being deleted") {
		assert.Contains(t, page.body, "feat/search is being deleted")
	}
	assert.NotContains(t, page.body, "is deleted")
	r.forge.MergeRequests.Wait(r.ctx)
	assert.Empty(t, r.bareGit("proj", "for-each-ref", "refs/heads/feat/search"))
	assert.NotContains(t, b.get("/proj/merge-requests/1?deleting=1", as(token)).body, "is being deleted", "once gone, nothing is being deleted")
}

// The badge follows the request live: a push to the target turns the fragment
// of the page to "checking" or "behind", and the settled result follows.
func TestWebMergeRequestMergeabilityLive(t *testing.T) {
	r := newMRRepo(t, domain.VisibilityPublic)
	token := r.signIn(t)
	r.cli(t, adminToken, "mr", "create", "proj", "--source", "feat/search", "--title", "Add search").ok(t)
	r.mrPageWith(token, "Ready to merge")

	one := r.sseAs("topic=repo:proj&view=mr&n=1&ref=feat/search&target=main", "", as(token))
	anon := r.sse("topic=repo:proj&view=mr&n=1&ref=feat/search&target=main", "")
	r.advanceMain("other.txt", "other\n")
	deadline := time.After(watchWait)
	for done := false; !done; {
		ev := nextNamed(t, one, "mr-mergeability")
		select {
		case <-deadline:
			t.Fatal("the badge never showed the result")
		default:
		}
		if strings.Contains(ev.data, "Behind main by 1 commit") {
			assert.Contains(t, ev.data, `<div id="mr-mergeability" class="live-slot" role="status" data-slot>`)
			assert.Contains(t, ev.data, `data-ff-ok="false"`)
			assert.Contains(t, ev.data, `data-merge-ok="true"`)
			done = true
		} else {
			assert.Contains(t, ev.data, "Checking…", "before the result, the badge says it checks")
		}
	}
	// The box is the owner's: a visitor's stream carries no badge.
	for {
		select {
		case ev := <-anon.events:
			assert.NotEqual(t, "mr-mergeability", ev.name)
			continue
		default:
		}
		break
	}
}
