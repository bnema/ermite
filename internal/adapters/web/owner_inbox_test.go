// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

// attentionItems are one item of each kind, in the order Attention gives.
func attentionItems(t *testing.T) []ports.AttentionItem {
	t.Helper()
	now := time.Now()
	pending := testPendingIssue(t)
	rec := depsRec(t)
	batch := *rec.Base()
	batch.State, batch.Error = domain.DepsFailed, "go mod tidy: <b>boom</b>\nsecond line"
	deps := ports.InboxDeps{Rec: rec, Batch: batch, Run: depsRun(3, domain.CICompleted)}
	title, err := domain.ParseMRTitle("add <search>")
	require.NoError(t, err)
	head, err := domain.ParseObjectID(fmt.Sprintf("%040x", 9))
	require.NoError(t, err)
	mr := domain.MergeRequest{Repo: repoName(t, "beta"), Number: 4, Title: title, State: domain.MRStateOpen, HeadID: head}
	checks := []domain.CICheck{{Repo: repoName(t, "beta"), Sha: head.String(), RunID: 2, Status: domain.CICompleted, Conclusion: domain.CISuccess}}
	unreleased := ports.UnreleasedRepo{Repo: repoName(t, "gamma"), Branch: "main", Distance: domain.TagDistance{Commits: 12, Tag: domain.TagInfo{Name: "v1.2.0", At: now.Add(-40 * 24 * time.Hour)}}}
	return []ports.AttentionItem{
		{Kind: ports.AttentionModeration, Repo: pending.Repo, Since: pending.CreatedAt, Pending: &pending},
		{Kind: ports.AttentionDepsFailed, Repo: rec.Repo, Since: now.Add(-time.Hour), Deps: &deps},
		{Kind: ports.AttentionReady, Repo: mr.Repo, Since: now.Add(-2 * time.Hour), MergeRequest: &mr, Checks: checks},
		{Kind: ports.AttentionRelease, Repo: unreleased.Repo, Since: unreleased.Distance.Tag.At, Unreleased: &unreleased},
	}
}

// attentionServer serves the inbox with an InboxService that answers only
// what each test expects.
func attentionServer(t *testing.T) (*Server, *mocks.InboxService) {
	t.Helper()
	svc := mocks.NewInboxService(t)
	return newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "alpha")), Browse: homeBrowse(t), Inbox: svc,
		Moderation: mocks.NewIssueModerationService(t), Sessions: newSessions(t, "tok"), Limiter: &countingLimiter{},
	}), svc
}

func TestInboxListsEveryKind(t *testing.T) {
	t.Parallel()
	items := attentionItems(t)
	srv, svc := attentionServer(t)
	svc.EXPECT().Attention(mock.Anything, mock.Anything).Return(items, nil).Once()
	svc.EXPECT().Counts(mock.Anything, mock.Anything).Return(ports.OwnerCounts{Attention: 4}, nil).Once()

	rec := inboxGet(srv, inboxPath, true)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	body := rec.Body.String()
	sec := body[strings.Index(body, `<section id="attention"`):]
	sec = sec[:strings.Index(sec, "</section>")]
	pid := items[0].Pending.ID.String()

	// The order of Attention, the first row under the cursor.
	ids := []string{"att-moderation-alpha-" + pid, "att-deps-failed-alpha-base", "att-ready-beta-4", "att-release-due-gamma-release"}
	last := -1
	for _, id := range ids {
		at := strings.Index(sec, `id="`+id)
		require.GreaterOrEqual(t, at, 0, id)
		assert.Greater(t, at, last, "order of %s", id)
		last = at
	}
	assert.Contains(t, sec, `<li id="att-moderation-alpha-`+pid+`" class="att" data-kind="moderation" data-cursor>`)
	assert.Equal(t, 1, strings.Count(sec, "data-cursor"))

	// Moderation: plain text, no link, the actions.
	mod := liOf(t, sec, "att-moderation-alpha-"+pid, "<li ")
	assert.Contains(t, mod, `<span class="att-kind att-moderation">Moderation</span>`)
	assert.Contains(t, mod, `<span class="att-title">&lt;script&gt;alert(1)&lt;/script&gt; crash</span>`)
	assert.NotContains(t, mod, "<script>")
	assert.NotContains(t, mod, "<img")
	assert.Contains(t, mod, "Eve &lt;i&gt;")
	assert.Contains(t, mod, `class="att-preview">**not bold**`)
	for _, op := range []string{"approve", "reject", "ban"} {
		assert.Contains(t, mod, `action="/-/moderation/alpha/`+pid+`/`+op+`"`)
	}

	// Deps failed: the batch to /-/deps, the first line of the error, the log.
	deps := liOf(t, sec, "att-deps-failed-alpha-base", "<li ")
	assert.Contains(t, deps, ">Deps failed</span>")
	assert.Regexp(t, `<a class="att-title" href="/-/deps#deps-[^"]+">Patch &amp; minor</a>`, deps)
	assert.Contains(t, deps, "go mod tidy: &lt;b&gt;boom&lt;/b&gt;</span>")
	assert.NotContains(t, deps, "second line")
	assert.Contains(t, deps, `">View log</a>`)

	// Ready: the merge request, its checks, Open.
	ready := liOf(t, sec, "att-ready-beta-4", "<li ")
	assert.Contains(t, ready, `<a class="att-title" href="/beta/merge-requests/4">add &lt;search&gt;</a>`)
	assert.Contains(t, ready, `class="ck-sum"`)
	assert.Contains(t, ready, `<a class="btn" href="/beta/merge-requests/4">Open</a>`)

	// Release due: the commits since the tag, its age, Commits.
	rel := liOf(t, sec, "att-release-due-gamma-release", "<li ")
	assert.Contains(t, rel, `<a class="att-title" href="/gamma/commits/main">12 commits since v1.2.0</a>`)
	assert.Contains(t, rel, "tagged 40d ago")
	assert.Contains(t, rel, `<a class="btn" href="/gamma/commits/main">Commits</a>`)
}

func TestInboxEmpty(t *testing.T) {
	t.Parallel()
	srv, svc := attentionServer(t)
	svc.EXPECT().Attention(mock.Anything, mock.Anything).Return(nil, nil).Once()
	svc.EXPECT().Counts(mock.Anything, mock.Anything).Return(ports.OwnerCounts{}, nil).Once()
	body := inboxGet(srv, inboxPath, true).Body.String()
	assert.Contains(t, body, `<section id="attention" aria-label="Awaiting you"><p class="lede dim">nothing awaits you.</p></section>`)
	assert.NotContains(t, body, `class="attention"`)
}

func TestInboxAttentionIsMissingForEveryoneElse(t *testing.T) {
	t.Parallel()
	srv, _ := attentionServer(t) // never asked
	missing := inboxGet(srv, "/-/nothing-here", false)
	anon := inboxGet(srv, inboxPath, false)
	assert.Equal(t, 404, anon.Code)
	assert.Equal(t, missing.Body.String(), anon.Body.String())
	assert.NotContains(t, anon.Body.String(), "owner-nav")
}

func TestInboxLiveViewRendersTheAttention(t *testing.T) {
	t.Parallel()
	srv, svc := attentionServer(t)
	svc.EXPECT().Attention(mock.Anything, mock.Anything).Return(attentionItems(t), nil).Once()
	svc.EXPECT().Counts(mock.Anything, mock.Anything).Return(ports.OwnerCounts{Attention: 4}, nil).Once()
	ls := &liveStream{s: srv, forOwner: true, view: "inbox"}
	frags, err := ls.render(withOwner(t.Context()), ports.WatchEvent{ID: "b-1", Event: domain.DependencyUpdatesChanged{Repo: repoName(t, "alpha")}})
	require.NoError(t, err)
	require.Len(t, frags, 1)
	assert.Contains(t, frags[0].html, `<section id="attention"`)
	assert.Contains(t, frags[0].html, `id="att-ready-beta-4"`)
	assert.Contains(t, frags[0].html, `id="owner-nav"`)
}
