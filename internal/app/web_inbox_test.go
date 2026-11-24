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
// issues, the tabs, the home button, the owner stream, and a 404 for
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
	empty := b.get("/-/work", as(token))
	require.Equal(t, 200, empty.status, empty.body)
	assert.Contains(t, empty.body, "no open merge requests.")
	assert.Contains(t, empty.body, "no open issues.")

	// A queued run is on the activity page; its run-level events refresh the
	// activity stream.
	draft, err := r.forge.store.CI().CreateRun(r.ctx, r.name("proj"), ".github/workflows/build.yml", func(id, number int64) ports.CIRunDraft {
		return ports.CIRunDraft{Run: domain.CIRun{Repo: r.name("proj"), ID: id, Number: number, Attempt: 1, WorkflowName: "Inbox build", Status: domain.CIQueued, Ref: "refs/heads/main", Event: "push", CreatedAt: time.Now()}}
	})
	require.NoError(t, err)
	runs := b.get("/-/activity", as(token))
	assert.Contains(t, runs.body, "Inbox build")
	assert.Contains(t, runs.body, `id="activity-runs"`)
	assert.Contains(t, runs.body, `data-inbox href="/-/inbox"`)

	stream := ownerStream(t, r.harness, token, "activity")
	r.forge.Bus.Publish(r.ctx, domain.RunUpdated{Run: draft.Run, At: time.Now(), Visibility: domain.VisibilityPublic, CreatedAt: time.Now()})
	queued := fragmentWith(t, stream, `id="activity-runs"`)
	assert.Contains(t, queued, "Inbox build")
	assert.Contains(t, queued, `id="owner-nav"`, "a queued run can change the counts")
	// Completion removes the row in the same live fragment.
	draft.Run.Status, draft.Run.Conclusion = domain.CICompleted, domain.CISuccess
	_, err = r.forge.store.CI().Apply(r.ctx, ports.CIWrite{Runs: []domain.CIRun{draft.Run}})
	require.NoError(t, err)
	r.forge.Bus.Publish(r.ctx, domain.RunUpdated{Run: draft.Run, At: time.Now(), Visibility: domain.VisibilityPublic, CreatedAt: time.Now()})
	completed := fragmentWith(t, stream, "no runs in progress.")
	assert.NotContains(t, completed, "Inbox build")

	// The work stream sees the merge requests and issues of both repositories.
	stream = ownerStream(t, r.harness, token, "work")
	root := domain.Root()
	_, err = r.forge.MergeRequests.Create(r.ctx, root, r.name("proj"), ports.CreateMergeRequestParams{Title: "add search", Source: "feat/search", Target: "main"})
	require.NoError(t, err)
	opened := fragmentWith(t, stream, "add search")
	assert.Contains(t, opened, `id="work-mrs"`)
	assert.Contains(t, opened, `id="work-issues"`)
	assert.Contains(t, opened, `id="owner-nav"`)
	_, err = r.forge.Issues.Create(r.ctx, root, r.name("other"), ports.CreateIssueParams{Title: "search is slow"})
	require.NoError(t, err)
	assert.Contains(t, fragmentWith(t, stream, "search is slow"), `id="work-issues"`, "an event of the second repository")
	_, err = r.forge.MergeRequests.Create(r.ctx, root, r.name("other"), ports.CreateMergeRequestParams{Title: "other work", Source: "feat/search", Target: "main"})
	require.NoError(t, err)
	_, err = r.forge.Issues.Create(r.ctx, root, r.name("proj"), ports.CreateIssueParams{Title: "proj bug"})
	require.NoError(t, err)

	all := b.get("/-/work", as(token))
	require.Equal(t, 200, all.status, all.body)
	assert.Equal(t, "private, no-store", all.hdr.Get("Cache-Control"))
	assert.Contains(t, all.body, `aria-label="Work (4)" aria-current="page"`)
	for _, want := range []string{"add search", "other work", "search is slow", "proj bug", `href="/other/issues/`, `href="/proj/merge-requests/`} {
		assert.Contains(t, all.body, want)
	}

	// Issues off: that repository's issues leave the work, its merge requests stay.
	off := domain.IssuesOff
	_, err = r.forge.Repos.Update(r.ctx, root, r.name("other"), ports.UpdateRepoParams{Issues: &off})
	require.NoError(t, err)
	hidden := b.get("/-/work", as(token))
	assert.Contains(t, hidden.body, `aria-label="Work (3)" aria-current="page"`)
	assert.NotContains(t, hidden.body, "search is slow")

	// The owner navigation, for the owner only.
	assert.Contains(t, b.get("/", as(token)).body, `id="owner-nav"`)
	anonHome := b.get("/", nil)
	assert.NotContains(t, anonHome.body, "/-/inbox")
	assert.NotContains(t, anonHome.body, "owner-nav")

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

// fragmentWith returns the data of the next owner event that contains want.
// Events the view does not show still send their id alone, and an event can
// change only the navigation; they are skipped.
func fragmentWith(t *testing.T, s *sseStream, want string) string {
	t.Helper()
	for {
		ev := s.next(t, watchWait)
		if ev.name == "" {
			continue
		}
		require.Equal(t, "owner", ev.name)
		if strings.Contains(ev.data, want) {
			return ev.data
		}
	}
}

// ownerStream opens the owner topic of view with the session cookie.
func ownerStream(t *testing.T, h *harness, token, view string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(h.ctx)
	t.Cleanup(cancel)
	path := "/-/events?topic=owner&view=" + view
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url(path), nil)
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

// A push of the default branch reaches the owner topic as its journal signal:
// the inbox stream refreshes its section and the owner navigation.
func TestWebInboxStreamFollowsAPush(t *testing.T) {
	r := newMRRepo(t, domain.VisibilityPublic)
	token := r.signIn(t)
	stream := ownerStream(t, r.harness, token, "inbox")
	r.mustGit(r.wt, "checkout", "-q", "main")
	r.writeFile(r.wt, "push.txt", []byte("push\n"))
	r.mustGit(r.wt, "push", "-q", r.url, "main")
	got := fragmentWith(t, stream, `id="owner-nav"`)
	assert.Contains(t, got, `id="attention"`)
}
