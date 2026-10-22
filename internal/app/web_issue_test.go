// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"net/url"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

var reIssueLocation = regexp.MustCompile(`^/proj/issues/(\d+)$`)

// The issue pages end to end: the owner opens, comments on, closes and
// reopens an issue through the forms; an anonymous visitor reads them on a
// public repository and cannot write; with issues off every page is a 404.
func TestWebIssuePagesEndToEnd(t *testing.T) {
	r := newMRRepo(t, domain.VisibilityPublic)
	b := browser{r.harness}
	token := r.signIn(t)

	// Off by default: no page, no tab, for the owner as well.
	assert.Equal(t, 404, b.get("/proj/issues", as(token)).status)
	assert.Equal(t, 404, b.get("/proj/issues/new", as(token)).status)
	assert.NotContains(t, b.get("/proj", as(token)).body, `data-key="g i"`)

	mode := domain.IssuesOwner
	_, err := r.forge.Repos.Update(r.ctx, domain.Root(), r.name("proj"), ports.UpdateRepoParams{Issues: &mode})
	require.NoError(t, err)

	home := b.get("/proj", nil)
	require.Equal(t, 200, home.status)
	assert.Contains(t, home.body, `href="/proj/issues"`, "the Issues tab")

	empty := b.get("/proj/issues", nil)
	require.Equal(t, 200, empty.status, empty.body)
	assert.Contains(t, empty.body, "no open issues")
	assert.NotContains(t, empty.body, "New issue")

	// The form is the owner's.
	assert.Equal(t, 404, b.get("/proj/issues/new", nil).status)
	form := b.get("/proj/issues/new", as(token))
	require.Equal(t, 200, form.status)
	assert.Contains(t, form.body, `action="/proj/issues"`)

	// A POST without the session is refused.
	anon := b.do("POST", "/proj/issues", map[string]string{"Origin": publicOrigin}, url.Values{"title": {"x"}}.Encode())
	assert.Equal(t, 403, anon.status)

	// Create.
	created := b.form("/proj/issues", token, url.Values{"title": {"search is slow"}, "body": {"## Steps\n\nrun **find**"}})
	require.Equal(t, 303, created.status, created.body)
	assert.Equal(t, "no-store", created.hdr.Get("Cache-Control"))
	m := reIssueLocation.FindStringSubmatch(created.hdr.Get("Location"))
	require.Len(t, m, 2, created.hdr.Get("Location"))
	path := "/proj/issues/" + m[1]

	owner := b.get(path, as(token))
	require.Equal(t, 200, owner.status, owner.body)
	assert.Equal(t, "private, no-store", owner.hdr.Get("Cache-Control"))
	assert.Contains(t, owner.body, "search is slow")
	assert.Contains(t, owner.body, "<strong>find</strong>")
	assert.Contains(t, owner.body, `action="`+path+`/comments"`)
	assert.Contains(t, owner.body, `action="`+path+`/close"`)

	// An invalid title is a state page, nothing is created.
	bad := b.form("/proj/issues", token, url.Values{"title": {""}})
	assert.Equal(t, 400, bad.status)

	// Comment.
	com := b.form(path+"/comments", token, url.Values{"text": {"reproduced\r\non main"}})
	require.Equal(t, 303, com.status, com.body)
	assert.Equal(t, path+"#c1", com.hdr.Get("Location"))
	assert.Equal(t, 400, b.form(path+"/comments", token, url.Values{"text": {"  "}}).status, "a blank comment")

	// Anonymous reads a public repository: no forms, public cache headers.
	pub := b.get(path, nil)
	require.Equal(t, 200, pub.status)
	assert.Equal(t, "no-cache", pub.hdr.Get("Cache-Control"))
	assert.Contains(t, pub.body, "reproduced")
	assert.NotContains(t, pub.body, `class="cform"`)
	assert.NotContains(t, pub.body, path+"/close")
	list := b.get("/proj/issues", nil)
	assert.Contains(t, list.body, `href="`+path+`"`)
	assert.Contains(t, list.body, "search is slow")
	assert.Equal(t, 403, b.do("POST", path+"/close", map[string]string{"Origin": publicOrigin}, "").status)

	// Close, then reopen.
	closed := b.form(path+"/close", token, nil)
	require.Equal(t, 303, closed.status, closed.body)
	assert.Equal(t, path, closed.hdr.Get("Location"))
	assert.Contains(t, b.get("/proj/issues?state=closed", nil).body, `href="`+path+`"`)
	assert.NotContains(t, b.get("/proj/issues", nil).body, `href="`+path+`"`)
	again := b.get(path, as(token))
	assert.Contains(t, again.body, `action="`+path+`/reopen"`)
	assert.Equal(t, 409, b.form(path+"/close", token, nil).status, "closing a closed issue")

	reopened := b.form(path+"/reopen", token, nil)
	require.Equal(t, 303, reopened.status, reopened.body)
	assert.Contains(t, b.get("/proj/issues", nil).body, `href="`+path+`"`)

	// The number is shared with the merge requests: #n goes to the right page.
	mr, err := r.forge.MergeRequests.Create(r.ctx, domain.Root(), r.name("proj"), ports.CreateMergeRequestParams{
		Title: "add search", Source: "feat/search", Target: "main",
	})
	require.NoError(t, err)
	to := b.get("/proj/issues/"+itoa64(mr.Number), nil)
	assert.Equal(t, 303, to.status)
	assert.Equal(t, "/proj/merge-requests/"+itoa64(mr.Number), to.hdr.Get("Location"))
	back := b.get("/proj/merge-requests/"+m[1], nil)
	assert.Equal(t, 303, back.status)
	assert.Equal(t, path, back.hdr.Get("Location"))
	assert.Equal(t, 404, b.get("/proj/issues/999", nil).status)
	assert.Equal(t, 404, b.get("/proj/merge-requests/999", nil).status)

	// Private repository: invisible to an anonymous visitor.
	priv := domain.VisibilityPrivate
	_, err = r.forge.Repos.Update(r.ctx, domain.Root(), r.name("proj"), ports.UpdateRepoParams{Visibility: &priv})
	require.NoError(t, err)
	assert.Equal(t, 404, b.get("/proj/issues", nil).status)
	assert.Equal(t, 404, b.get(path, nil).status)
	assert.Equal(t, 200, b.get(path, as(token)).status)

	// Off again: 404 for everyone.
	off := domain.IssuesOff
	_, err = r.forge.Repos.Update(r.ctx, domain.Root(), r.name("proj"), ports.UpdateRepoParams{Issues: &off})
	require.NoError(t, err)
	assert.Equal(t, 404, b.get(path, as(token)).status)
	assert.Equal(t, 404, b.get("/proj/issues", as(token)).status)
	assert.Equal(t, 404, b.form(path+"/comments", token, url.Values{"text": {"x"}}).status)
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
