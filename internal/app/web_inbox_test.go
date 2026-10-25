// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The owner's inbox end to end: two repositories with open merge requests and
// issues, the filter, the home button, the owner stream, and a 404 for
// everyone who is not the signed-in owner.
func TestWebInboxEndToEnd(t *testing.T) {
	r := newMRRepo(t, domain.VisibilityPublic)
	b := browser{r.harness}
	token := r.signIn(t)

	_, err := r.forge.Repos.Create(r.ctx, domain.Root(), createParams(r.harness, "other", domain.VisibilityPrivate))
	require.NoError(t, err)
	otherURL := r.urlAuth(adminToken, "/other.git")
	r.mustGit(r.wt, "push", "-q", otherURL, "main")
	r.mustGit(r.wt, "push", "-q", otherURL, "feat/search")

	owner := domain.IssuesOwner
	for _, n := range []string{"proj", "other"} {
		_, err := r.forge.Repos.Update(r.ctx, domain.Root(), r.name(n), ports.UpdateRepoParams{Issues: &owner})
		require.NoError(t, err)
	}

	// Nothing is open yet.
	empty := b.get("/-/inbox", as(token))
	require.Equal(t, 200, empty.status, empty.body)
	assert.Contains(t, empty.body, "no open merge requests.")
	assert.Contains(t, empty.body, "no open issues.")

	// A queued run is visible and its run-level events refresh the owner stream.
	draft, err := r.forge.store.CI().CreateRun(r.ctx, r.name("proj"), ".github/workflows/build.yml", func(id, number int64) ports.CIRunDraft {
		return ports.CIRunDraft{Run: domain.CIRun{Repo: r.name("proj"), ID: id, Number: number, Attempt: 1, WorkflowName: "Inbox build", Status: domain.CIQueued, Ref: "refs/heads/main", Event: "push", CreatedAt: time.Now()}}
	})
	require.NoError(t, err)
	runs := b.get("/-/inbox", as(token))
	assert.Contains(t, runs.body, "Inbox build")
	assert.Contains(t, runs.body, "<strong>1</strong> running")
	assert.Contains(t, runs.body, `data-inbox href="/-/inbox"`)

	// The owner stream sees both repositories.
	stream := ownerStream(t, r.harness, token)

	r.forge.Bus.Publish(r.ctx, domain.RunUpdated{Run: draft.Run, At: time.Now(), Visibility: domain.VisibilityPublic, CreatedAt: time.Now()})
	var runFragments strings.Builder
	for range inboxFragments {
		runFragments.WriteString(nextFragment(t, stream).data)
	}
	assert.Contains(t, runFragments.String(), "Inbox build")
	// Completion removes the row and updates the total in the same live fragments.
	draft.Run.Status, draft.Run.Conclusion = domain.CICompleted, domain.CISuccess
	_, err = r.forge.store.CI().Apply(r.ctx, ports.CIWrite{Runs: []domain.CIRun{draft.Run}})
	require.NoError(t, err)
	r.forge.Bus.Publish(r.ctx, domain.RunUpdated{Run: draft.Run, At: time.Now(), Visibility: domain.VisibilityPublic, CreatedAt: time.Now()})
	runFragments.Reset()
	for range inboxFragments {
		runFragments.WriteString(nextFragment(t, stream).data)
	}
	assert.NotContains(t, runFragments.String(), "Inbox build")
	assert.Contains(t, runFragments.String(), "no runs in progress.")
	assert.Contains(t, runFragments.String(), "<strong>0</strong> running")

	root := domain.Root()
	_, err = r.forge.MergeRequests.Create(r.ctx, root, r.name("proj"), ports.CreateMergeRequestParams{Title: "add search", Source: "feat/search", Target: "main"})
	require.NoError(t, err)
	ev := nextFragment(t, stream)
	assert.Equal(t, "inbox", ev.name)
	assert.Contains(t, ev.data, `id="inbox-counts"`)
	for range inboxFragments - 1 { // the other fragments of this event
		nextFragment(t, stream)
	}
	_, err = r.forge.Issues.Create(r.ctx, root, r.name("other"), ports.CreateIssueParams{Title: "search is slow"})
	require.NoError(t, err)
	var seen strings.Builder
	for range inboxFragments { // the fragments of this event
		seen.WriteString(nextFragment(t, stream).data)
	}
	assert.Contains(t, seen.String(), "search is slow", "an event of the second repository")
	_, err = r.forge.MergeRequests.Create(r.ctx, root, r.name("other"), ports.CreateMergeRequestParams{Title: "other work", Source: "feat/search", Target: "main"})
	require.NoError(t, err)
	_, err = r.forge.Issues.Create(r.ctx, root, r.name("proj"), ports.CreateIssueParams{Title: "proj bug"})
	require.NoError(t, err)

	all := b.get("/-/inbox", as(token))
	require.Equal(t, 200, all.status, all.body)
	assert.Equal(t, "private, no-store", all.hdr.Get("Cache-Control"))
	assert.Contains(t, all.body, "<strong>2</strong> merge requests · <strong>2</strong> issues")
	for _, want := range []string{"add search", "other work", "search is slow", "proj bug", `href="/other/issues/`, `href="/proj/merge-requests/`} {
		assert.Contains(t, all.body, want)
	}

	only := b.get("/-/inbox?repo=other", as(token))
	require.Equal(t, 200, only.status)
	assert.Contains(t, only.body, "<strong>2</strong> merge requests · <strong>2</strong> issues", "the counters ignore the filter")
	assert.Contains(t, only.body, "other work")
	assert.Contains(t, only.body, "search is slow")
	assert.NotContains(t, only.body, "add search")
	assert.NotContains(t, only.body, "proj bug")

	// Issues off: that repository's issues leave the inbox, its merge requests stay.
	off := domain.IssuesOff
	_, err = r.forge.Repos.Update(r.ctx, root, r.name("other"), ports.UpdateRepoParams{Issues: &off})
	require.NoError(t, err)
	hidden := b.get("/-/inbox", as(token))
	assert.Contains(t, hidden.body, "<strong>2</strong> merge requests · <strong>1</strong> issues")
	assert.NotContains(t, hidden.body, "search is slow")

	// The home button, for the owner only.
	assert.Contains(t, b.get("/", as(token)).body, `href="/-/inbox"`)
	anonHome := b.get("/", nil)
	assert.NotContains(t, anonHome.body, "/-/inbox")
	assert.NotContains(t, anonHome.body, "Inbox")

	// Anyone else: the 404 of an unknown page, and no stream.
	missing := b.get("/-/no-such-page", nil)
	anon := b.get("/-/inbox", nil)
	assert.Equal(t, 404, anon.status)
	assert.Equal(t, missing.body, anon.body)
	assert.Equal(t, 404, b.get("/-/inbox", map[string]string{"Authorization": "Bearer " + adminToken}).status, "a token is not a session")
	unknown := b.get("/-/events?topic=nope&view=inbox", nil)
	ownerAnon := b.get("/-/events?topic=owner&view=inbox", nil)
	assert.Equal(t, unknown.status, ownerAnon.status, "the answer to an unknown topic")
	assert.Equal(t, strings.ReplaceAll(unknown.body, "nope", "owner"), ownerAnon.body)
	assert.Equal(t, unknown.status, b.get("/-/events?topic=owner&view=inbox", map[string]string{"Authorization": "Bearer " + adminToken}).status)
}

// inboxFragments is the number of fragments one inbox event sends.
const inboxFragments = 8

// nextFragment returns the next event that carries a fragment. Events the
// inbox does not show (a mergeability check) still send their id alone, so a
// resumed stream starts after them; they are skipped.
func nextFragment(t *testing.T, s *sseStream) sseEvent {
	t.Helper()
	for {
		if ev := s.next(t, watchWait); ev.name != "" {
			return ev
		}
	}
}

// ownerStream opens the owner topic with the session cookie.
func ownerStream(t *testing.T, h *harness, token string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(h.ctx)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url("/-/events?topic=owner&view=inbox"), nil)
	require.NoError(t, err)
	req.Header.Set("Cookie", sessionCookie+"="+token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	s := &sseStream{resp: resp, events: make(chan sseEvent, 256), cancel: cancel}
	go func() {
		defer close(s.events)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		var cur sseEvent
		var data []string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if cur.id != "" || cur.name != "" || len(data) > 0 {
					cur.data = strings.Join(data, "\n")
					s.events <- cur
				}
				cur, data = sseEvent{}, nil
			case strings.HasPrefix(line, ":"):
				cur.comment = true
			case strings.HasPrefix(line, "event: "):
				cur.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				cur.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			}
		}
	}()
	return s
}
