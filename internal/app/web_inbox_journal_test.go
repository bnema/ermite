// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The activity journal end to end: real pushes and tags through the hooks, a
// merge request opened and merged, and the inbox that shows them with the
// commits since the last tag.
func TestWebInboxJournalEndToEnd(t *testing.T) {
	r := newMRRepo(t, domain.VisibilityPublic) // main and feat/search pushed
	b := browser{r.harness}
	token := r.signIn(t)
	ctx := r.ctx

	// A tag, then two commits on main after it, then a push to another branch.
	r.mustGit(r.wt, "checkout", "-q", "main")
	r.mustGit(r.wt, "tag", "-a", "-m", "first release", "v0.1.0")
	r.mustGit(r.wt, "push", "-q", r.url, "v0.1.0")
	r.writeFile(r.wt, "a.txt", []byte("a\n"))
	r.writeFile(r.wt, "b.txt", []byte("b\n"))
	r.mustGit(r.wt, "push", "-q", r.url, "main")
	r.mustGit(r.wt, "checkout", "-q", "feat/search")
	r.writeFile(r.wt, "c.txt", []byte("c\n"))
	r.mustGit(r.wt, "push", "-q", r.url, "feat/search")

	stream := ownerStream(t, r.harness, token, "activity")
	mr, err := r.forge.MergeRequests.Create(ctx, domain.Root(), r.name("proj"), ports.CreateMergeRequestParams{Title: "add search", Source: "feat/search", Target: "main"})
	require.NoError(t, err)
	var live strings.Builder
	for range inboxFragments {
		live.WriteString(nextFragment(t, stream).data)
	}
	assert.Contains(t, live.String(), `<tr data-kind="mr_opened">`, "the journal is refreshed live")

	_, err = r.forge.MergeRequests.Merge(ctx, domain.Owner(), r.name("proj"), mr.Number, ports.MergeParams{Mode: domain.MergeModeMerge, HeadID: mustOID(t, r.headOf("feat/search"))})
	require.NoError(t, err)
	r.forge.MergeRequests.Wait(ctx)

	// A tag deleted, and a lightweight one on the commit before a and b.
	r.mustGit(r.wt, "push", "-q", r.url, ":refs/tags/v0.1.0")
	r.mustGit(r.wt, "fetch", "-q", r.url, "main")
	r.mustGit(r.wt, "tag", "v0.0.1", "FETCH_HEAD~3")
	r.mustGit(r.wt, "push", "-q", r.url, "v0.0.1")

	// The merge was recorded before Merge returned; the hooks of the pushes
	// ran before git push returned.
	page := b.get("/-/inbox?tab=activity", as(token)).body
	require.Contains(t, page, `data-kind="mr_merged"`)
	sec := page[strings.Index(page, `id="inbox-journal"`):]

	for _, want := range []string{
		`<tr data-kind="tag_created">`, `<span class="j-ref">v0.1.0</span>`,
		`<tr data-kind="tag_deleted">`,
		`<tr data-kind="push">`, `<span class="mute num">· 2 commits</span>`, "add b.txt",
		`<tr data-kind="mr_opened">`, `<tr data-kind="mr_merged">`, `href="/proj/merge-requests/1"><span class="j-ref">!1</span> add search</a>`,
	} {
		assert.Contains(t, sec, want)
	}
	assert.Equal(t, 2, strings.Count(sec, `data-kind="push"`), "the first push of main and a+b; neither feat/search nor the merge")
	assert.Less(t, strings.Index(sec, `data-kind="mr_merged"`), strings.Index(sec, `data-kind="mr_opened"`), "newest first")

	// v0.0.1 is behind the merge commit, b, a and the two commits of
	// feat/search.
	assert.Contains(t, sec, `<a class="u-repo" href="/proj">proj</a>`)
	assert.Contains(t, sec, `href="/proj/commits/main">5 commits</a>`)
	assert.Contains(t, sec, `<a class="u-tag" href="/proj/tree/v0.0.1">v0.0.1</a>`)

	// The activity tab keeps the range and includes every repository.
	other := b.get("/-/inbox?tab=activity&range=7d", as(token))
	require.Equal(t, 200, other.status)
	assert.Contains(t, other.body, `data-kind="mr_merged"`)
	assert.Contains(t, other.body, `aria-current="page">7 days</a>`)

	assert.Equal(t, 404, b.get("/-/inbox?range=7d", nil).status)
}

func mustOID(t *testing.T, s string) domain.ObjectID {
	t.Helper()
	id, err := domain.ParseObjectID(strings.TrimSpace(s))
	require.NoError(t, err)
	return id
}
