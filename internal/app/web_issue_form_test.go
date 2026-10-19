// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"crypto/sha256"
	"encoding/base64"
	"html"
	"math/bits"
	"net/url"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

var (
	reFormAttr  = func(name string) *regexp.Regexp { return regexp.MustCompile(`data-` + name + `="([^"]*)"`) }
	reFormField = func(name string) *regexp.Regexp {
		return regexp.MustCompile(`<input type="hidden" name="` + name + `" value="([^"]*)"`)
	}
	rePendingActions = regexp.MustCompile(`action="(/-/inbox/pending/[^"]+/(approve|reject|ban))"`)
)

// testPoW is the difficulty the tests lower the proof of work to.
const testPoW = 8

// solvePoW finds the smallest counter n with SHA-256(nonce || ":" || n) having
// bits leading zero bits, as the browser does.
func solvePoW(t *testing.T, nonce string, difficulty int) string {
	t.Helper()
	raw, err := url.PathUnescape(nonce)
	require.NoError(t, err)
	b, err := decodeB64URL(raw)
	require.NoError(t, err)
	for n := 0; n < 1<<26; n++ {
		sum := sha256.Sum256(append(append(append([]byte{}, b...), ':'), strconv.Itoa(n)...))
		z := 0
		for _, c := range sum {
			if c == 0 {
				z += 8
				continue
			}
			z += bits.LeadingZeros8(c)
			break
		}
		if z >= difficulty {
			return strconv.Itoa(n)
		}
	}
	t.Fatal("no solution")
	return ""
}

// formValues reads the challenge out of the HTML of the public form and
// solves it.
func formValues(t *testing.T, body string) url.Values {
	t.Helper()
	get := func(re *regexp.Regexp) string {
		m := re.FindStringSubmatch(body)
		require.Len(t, m, 2, "the form carries %s", re)
		return html.UnescapeString(m[1])
	}
	diff, err := strconv.Atoi(get(reFormAttr("difficulty")))
	require.NoError(t, err)
	require.Equal(t, testPoW, diff, "the test difficulty")
	return url.Values{
		"token": {get(reFormField("token"))}, "form_token": {get(reFormField("form_token"))},
		"solution": {solvePoW(t, get(reFormAttr("nonce")), diff)},
	}
}

// The public issue form end to end: a visitor solves the proof of work and
// submits, the owner sees the issue awaiting moderation in the inbox and
// approves it, and a rejection with a ban shuts the same address out.
func TestWebPublicIssueFormEndToEnd(t *testing.T) {
	h := newHarnessWith(t, harnessOpts{}, func(o *options) { o.issuePoW = testPoW })
	cli := &cliHarness{harness: h, url: h.srv.URL}
	_, err := h.forge.Repos.Create(h.ctx, domain.Root(), createParams(h, "proj", domain.VisibilityPublic))
	require.NoError(t, err)
	b := browser{h}
	token := cli.signIn(t)

	pub := domain.IssuesPublic
	_, err = h.forge.Repos.Update(h.ctx, domain.Root(), h.name("proj"), ports.UpdateRepoParams{Issues: &pub})
	require.NoError(t, err)
	site := map[string]string{"Origin": publicOrigin}

	// The visitor reads the list and finds the button.
	list := b.get("/proj/issues", nil)
	require.Equal(t, 200, list.status, list.body)
	assert.Contains(t, list.body, `href="/proj/issues/new"`)

	// The form, with a challenge, never stored.
	form := b.get("/proj/issues/new", nil)
	require.Equal(t, 200, form.status, form.body)
	assert.Equal(t, "no-store", form.hdr.Get("Cache-Control"))
	assert.Contains(t, form.body, "<noscript>")
	assert.Contains(t, form.body, `name="website"`)
	v := formValues(t, form.body)
	v.Set("title", "Search is <b>slow</b>")
	v.Set("body", "see [docs](https://example.org/d) and [x](javascript:alert(1))\r\n**bold**")
	v.Set("author", "Alex")

	// A bad cross-origin POST is refused before anything is checked.
	assert.Equal(t, 403, b.do("POST", "/proj/issues/new", nil, v.Encode()).status)
	assert.Equal(t, 403, b.do("POST", "/proj/issues/new", map[string]string{"Origin": "https://evil.example"}, v.Encode()).status)

	// The submission passes.
	sent := b.do("POST", "/proj/issues/new", site, v.Encode())
	require.Equal(t, 303, sent.status, sent.body)
	assert.Equal(t, "/proj/issues/submitted", sent.hdr.Get("Location"))
	assert.Equal(t, "no-store", sent.hdr.Get("Cache-Control"))
	thanks := b.get("/proj/issues/submitted", nil)
	assert.Equal(t, 200, thanks.status)
	assert.Contains(t, thanks.body, "Thanks, your issue is waiting for moderation.")

	// The same challenge cannot be used twice.
	replay := b.do("POST", "/proj/issues/new", site, v.Encode())
	assert.Equal(t, 400, replay.status)
	assert.Contains(t, replay.body, "could not be verified")

	// Nothing is public yet.
	assert.NotContains(t, b.get("/proj/issues", nil).body, "slow")

	// A text error shows the form again with a new challenge and the text.
	form2 := b.get("/proj/issues/new", nil)
	v2 := formValues(t, form2.body)
	v2.Set("title", "")
	v2.Set("body", "kept text")
	again := b.do("POST", "/proj/issues/new", site, v2.Encode())
	require.Equal(t, 422, again.status, again.body)
	assert.Contains(t, again.body, "kept text")
	assert.Contains(t, again.body, "The title is empty.")
	v3 := formValues(t, again.body)
	assert.NotEqual(t, v2.Get("token"), v3.Get("token"))

	// The owner finds it awaiting moderation, as plain text.
	inbox := b.get("/-/inbox", as(token))
	require.Equal(t, 200, inbox.status, inbox.body)
	assert.Contains(t, inbox.body, "<strong>1</strong> awaiting moderation")
	assert.Contains(t, inbox.body, "Search is &lt;b&gt;slow&lt;/b&gt;")
	assert.Contains(t, inbox.body, "**bold**")
	assert.NotContains(t, inbox.body, "<strong>bold")
	acts := rePendingActions.FindAllStringSubmatch(inbox.body, -1)
	require.Len(t, acts, 3)
	actions := map[string]string{}
	for _, a := range acts {
		actions[a[2]] = a[1]
	}

	// An anonymous POST to a moderation path is refused.
	assert.Equal(t, 403, b.do("POST", actions["approve"], site, "").status)

	// The owner shares the numbers with merge requests and issues: approve.
	ok := b.form(actions["approve"], token, nil)
	require.Equal(t, 303, ok.status, ok.body)
	assert.Equal(t, "/-/inbox", ok.hdr.Get("Location"))
	assert.Equal(t, 404, b.form(actions["approve"], token, nil).status, "approved once")
	shown := b.get("/proj/issues/1", nil)
	require.Equal(t, 200, shown.status, shown.body)
	assert.Contains(t, shown.body, "Search is &lt;b&gt;slow&lt;/b&gt;")
	assert.Contains(t, shown.body, `href="https://example.org/d"`)
	assert.Contains(t, shown.body, `rel="nofollow noopener noreferrer ugc"`)
	assert.NotContains(t, shown.body, "javascript:")
	assert.Contains(t, shown.body, "Alex")
	assert.Contains(t, b.get("/proj/issues", nil).body, `href="/proj/issues/1"`)
	empty := b.get("/-/inbox", as(token))
	assert.Contains(t, empty.body, "<strong>0</strong> awaiting moderation")

	// Reject and ban: the same address is then refused with the generic page.
	form4 := b.get("/proj/issues/new", nil)
	v4 := formValues(t, form4.body)
	v4.Set("title", "spam")
	require.Equal(t, 303, b.do("POST", "/proj/issues/new", site, v4.Encode()).status)
	inbox2 := b.get("/-/inbox", as(token))
	var ban string
	for _, a := range rePendingActions.FindAllStringSubmatch(inbox2.body, -1) {
		if a[2] == "ban" {
			ban = a[1]
		}
	}
	require.NotEmpty(t, ban)
	require.Equal(t, 303, b.form(ban, token, nil).status)

	form5 := b.get("/proj/issues/new", nil)
	require.Equal(t, 200, form5.status, "the form is still served; the ban bites at submission")
	v5 := formValues(t, form5.body)
	v5.Set("title", "more spam")
	banned := b.do("POST", "/proj/issues/new", site, v5.Encode())
	assert.Equal(t, 400, banned.status)
	assert.Contains(t, banned.body, "The submission could not be verified. Reload the page and try again.")
	assert.NotContains(t, banned.body, "ban")
	assert.Contains(t, b.get("/-/inbox", as(token)).body, "<strong>0</strong> awaiting moderation")
}

// A repository that is not public with public issues has no form.
func TestWebPublicIssueFormIsNotThere(t *testing.T) {
	h := newHarnessWith(t, harnessOpts{}, func(o *options) { o.issuePoW = testPoW })
	_, err := h.forge.Repos.Create(h.ctx, domain.Root(), createParams(h, "proj", domain.VisibilityPublic))
	require.NoError(t, err)
	b := browser{h}
	site := map[string]string{"Origin": publicOrigin}
	body := url.Values{"title": {"x"}}.Encode()

	missing := b.get("/nope/issues/new", nil)
	for name, mode := range map[string]domain.IssuesMode{"off": domain.IssuesOff, "owner": domain.IssuesOwner} {
		_, err := h.forge.Repos.Update(h.ctx, domain.Root(), h.name("proj"), ports.UpdateRepoParams{Issues: &mode})
		require.NoError(t, err)
		got := b.get("/proj/issues/new", nil)
		assert.Equal(t, 404, got.status, name)
		assert.Equal(t, missing.body, got.body, name+": the same page as a missing repository")
		assert.Equal(t, 404, b.do("POST", "/proj/issues/new", site, body).status, name)
	}

	pub := domain.IssuesPublic
	_, err = h.forge.Repos.Update(h.ctx, domain.Root(), h.name("proj"), ports.UpdateRepoParams{Issues: &pub})
	require.NoError(t, err)
	priv := domain.VisibilityPrivate
	_, err = h.forge.Repos.Update(h.ctx, domain.Root(), h.name("proj"), ports.UpdateRepoParams{Visibility: &priv})
	require.NoError(t, err)
	assert.Equal(t, 404, b.get("/proj/issues/new", nil).status, "private")
	assert.Equal(t, 404, b.do("POST", "/proj/issues/new", site, body).status, "private")
}

func decodeB64URL(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
