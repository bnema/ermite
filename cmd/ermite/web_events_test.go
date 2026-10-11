// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"context"
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/domain"
)

// sseEvent is one event read from /-/events.
type sseEvent struct {
	id, name, data string
	comment        bool
}

// sseStream is a client of /-/events.
type sseStream struct {
	resp   *http.Response
	events chan sseEvent
	cancel context.CancelFunc
}

// sse opens /-/events?<query> like the browser does and reads it in the
// background. It returns once the server answered, which it does when the
// watch is registered: what happens after is not missed.
func (h *harness) sse(query, lastID string) *sseStream {
	h.t.Helper()
	ctx, cancel := context.WithCancel(h.ctx)
	h.t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url("/-/events?"+query), nil)
	require.NoError(h.t, err)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	t := h.t
	h.t.Cleanup(func() { _ = resp.Body.Close() })
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
				s.events <- sseEvent{comment: true, data: strings.TrimSpace(line[1:])}
			default:
				k, v, _ := strings.Cut(line, ":")
				v = strings.TrimPrefix(v, " ")
				switch k {
				case "id":
					cur.id = v
				case "event":
					cur.name = v
				case "data":
					data = append(data, v)
				}
			}
		}
	}()
	return s
}

// next returns the next event (not a comment) or fails after d.
func (s *sseStream) next(t *testing.T, d time.Duration) sseEvent {
	t.Helper()
	timeout := time.After(d)
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				t.Fatal("the stream ended")
			}
			if ev.comment {
				continue
			}
			return ev
		case <-timeout:
			t.Fatalf("no event within %v", d)
		}
	}
}

// AC5: a push shows up as the row of the repository on the home stream
// within a second; creating, hiding and deleting repositories updates or
// removes the row.
func TestWebEventsHome(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("pub", domain.VisibilityPublic)
	s := h.sse("topic=home", "")
	assert.Equal(t, "text/event-stream", s.resp.Header.Get("Content-Type"))
	assert.Contains(t, s.resp.Header.Get("Cache-Control"), "no-cache")

	wt := h.workTree("main", "a.txt")
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/pub.git"), "main")
	start := time.Now()
	ev := s.next(t, time.Second)
	assert.Less(t, time.Since(start), 2*time.Second)
	assert.Equal(t, "home-list", ev.name)
	assert.NotEmpty(t, ev.id)
	assert.Contains(t, ev.data, `<tr id="repo-pub"`)
	assert.Contains(t, ev.data, `new push`)
	assert.Contains(t, ev.data, `href="/pub"`)

	// A new repository is a row; so is a repository created by a push.
	_, err := h.forge.Repos.Create(h.ctx, domain.Root(), app.CreateParams{Name: h.name("fresh"), Description: "a <b>fresh</b> one", Visibility: domain.VisibilityPublic})
	require.NoError(t, err)
	ev = s.next(t, watchWait)
	assert.Equal(t, "home-list", ev.name)
	assert.Contains(t, ev.data, `<tr id="repo-fresh"`)
	assert.Contains(t, ev.data, "a &lt;b&gt;fresh&lt;/b&gt; one", "text is escaped")
	assert.NotContains(t, ev.data, "new push")

	// A repository that turns private is removed from the anonymous list.
	priv := domain.VisibilityPrivate
	_, err = h.forge.Repos.Update(h.ctx, domain.Root(), h.name("fresh"), app.UpdateParams{Visibility: &priv})
	require.NoError(t, err)
	ev = s.next(t, watchWait)
	assert.Equal(t, "home-list", ev.name)
	assert.Contains(t, ev.data, `<tr id="repo-fresh" data-gone`)
	assert.NotContains(t, ev.data, "fresh one")

	// And a deleted one.
	require.NoError(t, h.forge.Repos.Delete(h.ctx, domain.Root(), h.name("pub")))
	ev = s.next(t, watchWait)
	assert.Contains(t, ev.data, `<tr id="repo-pub" data-gone`)
}

// AC5: the stream of a repository carries its last push; the refs page its
// refs; the commits page the banner with the number of new commits.
func TestWebEventsRepoFragments(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("x", domain.VisibilityPublic)
	wt := h.workTree("main", "a.txt")
	push := func() { h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/x.git"), "main") }
	push()

	summary := h.sse("topic=repo:x&view=summary", "")
	refs := h.sse("topic=repo:x&view=refs", "")

	_, _, page := h.get("/x/commits/main")
	require.Contains(t, page, `<div id="commits-head" class="live-slot" role="status" data-slot>`, "the slot of the banner, a status region that stays in the page")
	m := regexp.MustCompile(`hx-sse:connect="([^"]+)"`).FindStringSubmatch(page)
	require.Len(t, m, 2, "the commits page is live")
	url := html.UnescapeString(m[1])
	require.True(t, strings.HasPrefix(url, "/-/events?"), url)
	commits := h.sse(strings.TrimPrefix(url, "/-/events?"), "")

	h.writeFile(wt, "b.txt", []byte("b\n"))
	h.writeFile(wt, "c.txt", []byte("c\n"))
	start := time.Now()
	push()

	ev := summary.next(t, time.Second)
	assert.Less(t, time.Since(start), 2*time.Second)
	assert.Equal(t, "repo-last-push", ev.name)
	assert.Contains(t, ev.data, `<dd id="last-push-x">`)
	assert.Contains(t, ev.data, "now")

	ev = refs.next(t, watchWait)
	assert.Equal(t, "refs-list", ev.name)
	assert.Contains(t, ev.data, `<tbody id="ref-rows" data-rows>`)
	assert.Contains(t, ev.data, `>main</a>`)
	ev = refs.next(t, watchWait)
	assert.Equal(t, "refs-list", ev.name)
	assert.Contains(t, ev.data, `<dl class="kv" id="refs-kv">`)

	ev = commits.next(t, watchWait)
	assert.Equal(t, "commits-head", ev.name)
	assert.Contains(t, ev.data, `id="commits-head"`)
	assert.Contains(t, ev.data, "2 new commits — reload")
	assert.Contains(t, ev.data, `href="/x/commits/main"`)

	// One more commit: the count is since the head the page lists.
	h.writeFile(wt, "d.txt", []byte("d\n"))
	push()
	assert.Contains(t, commits.next(t, watchWait).data, "3 new commits — reload")

	// A push to another branch is not news for this page.
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/x.git"), "main:refs/heads/side")
	idOnly := commits.next(t, watchWait)
	assert.NotEmpty(t, idOnly.id, "the event is acknowledged, so that a reconnection resumes after it")
	assert.Empty(t, idOnly.name)
	assert.Empty(t, idOnly.data)
	h.writeFile(wt, "e.txt", []byte("e\n"))
	push()
	assert.Contains(t, commits.next(t, watchWait).data, "4 new commits — reload")

	// Pages that do not list the head of a branch do not announce anything.
	_, _, detached := h.get("/x/commits/main/a.txt")
	assert.NotContains(t, detached, `id="commits-head"`)
	assert.NotContains(t, detached, "hx-sse:connect")
	_, _, old := h.get("/x/commits/main?page=nope")
	assert.NotContains(t, old, "hx-sse:connect")
}

// AC5: Last-Event-ID resumes after that event; an id nobody issued is a gap,
// which tells the page to load itself again.
func TestWebEventsResumeAndGap(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("pub", domain.VisibilityPublic)
	s := h.sse("topic=home", "")
	wt := h.workTree("main", "a.txt")
	var ids []string
	for i := range 3 {
		if i > 0 {
			h.writeFile(wt, "f"+string(rune('0'+i))+".txt", []byte("x\n"))
		}
		h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/pub.git"), "main")
		ids = append(ids, s.next(t, watchWait).id)
	}
	s.cancel()

	r := h.sse("topic=home", ids[0])
	for _, want := range ids[1:] {
		ev := r.next(t, watchWait)
		assert.Equal(t, want, ev.id)
		assert.Equal(t, "home-list", ev.name)
	}
	h.writeFile(wt, "live.txt", []byte("x\n"))
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/pub.git"), "main")
	live := r.next(t, watchWait)
	assert.NotContains(t, ids, live.id)

	g := h.sse("topic=home", "not-an-id")
	gap := g.next(t, watchWait)
	assert.Equal(t, "gap", gap.name)
	assert.Empty(t, gap.id)
	assert.Equal(t, "home-list", g.next(t, watchWait).name, "the oldest buffered event follows")
}

// A small ring: the id that fell out of it is a gap.
func TestWebEventsGapAfterOverflow(t *testing.T) {
	h := newHarness(t, harnessOpts{eventBuffer: 3})
	h.seed("pub", domain.VisibilityPublic)
	s := h.sse("topic=home", "")
	wt := h.workTree("main", "a.txt")
	var ids []string
	for i := range 6 {
		if i > 0 {
			h.writeFile(wt, "f"+string(rune('0'+i))+".txt", []byte("x\n"))
		}
		h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/pub.git"), "main")
		ids = append(ids, s.next(t, watchWait).id)
	}
	r := h.sse("topic=home", ids[1])
	assert.Equal(t, "gap", r.next(t, watchWait).name)
	assert.Equal(t, ids[3], r.next(t, watchWait).id)
}

// AC2, AC5: nothing of a private repository reaches the anonymous stream,
// and its topic is a missing one.
func TestWebEventsPrivate(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("pub", domain.VisibilityPublic)
	h.seed("priv", domain.VisibilityPrivate)
	s := h.sse("topic=home", "")

	missing := h.status("GET", "/-/events?topic=repo:nothing", nil)
	assert.Equal(t, http.StatusNotFound, missing)
	assert.Equal(t, missing, h.status("GET", "/-/events?topic=repo:priv", nil), "private is indistinguishable from missing")

	wt := h.workTree("main", "a.txt")
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/priv.git"), "main")
	desc := "secret change"
	_, err := h.forge.Repos.Update(h.ctx, domain.Root(), h.name("priv"), app.UpdateParams{Description: &desc})
	require.NoError(t, err)
	// Made public later, it does not reveal what happened while it was
	// private; the update that makes it public is shown.
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/pub.git"), "main")
	ev := s.next(t, watchWait)
	assert.Contains(t, ev.data, `id="repo-pub"`, "the first event is the public push: %s", ev.data)
	h.sentinelSSE(t, s, "sentinel")
	_, _, body := h.get("/")
	assert.Contains(t, body, "repo-pub")
	assert.NotContains(t, body, "repo-priv")
}

// sentinelSSE proves that nothing came since the last event: it creates a
// public repository and requires its row to be the next event.
func (h *harness) sentinelSSE(t *testing.T, s *sseStream, name string) {
	t.Helper()
	_, err := h.forge.Repos.Create(h.ctx, domain.Root(), app.CreateParams{Name: h.name(name), Visibility: domain.VisibilityPublic})
	require.NoError(t, err)
	ev := s.next(t, watchWait)
	assert.Contains(t, ev.data, `id="repo-`+name+`"`, "an event came before the sentinel: %s", ev.data)
}

// A topic is home or repo:<name>; anything else is refused before a stream
// starts.
func TestWebEventsTopics(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("pub", domain.VisibilityPublic)
	for path, want := range map[string]int{
		"/-/events":                         http.StatusBadRequest,
		"/-/events?topic=":                  http.StatusBadRequest,
		"/-/events?topic=other":             http.StatusBadRequest,
		"/-/events?topic=repo:":             http.StatusBadRequest,
		"/-/events?topic=repo:Bad%20Name":   http.StatusNotFound,
		"/-/events?topic=repo:..":           http.StatusNotFound,
		"/-/events?topic=repo:nothing":      http.StatusNotFound,
		"/-/events?topic=repo:pub.git":      http.StatusNotFound,
		"/-/events?topic=home,repo:pub":     http.StatusBadRequest,
		"/-/events?topic=repo:pub&view=zzz": http.StatusOK,
	} {
		if want == http.StatusOK {
			s := h.sse(strings.TrimPrefix(path, "/-/events?"), "")
			s.cancel()
			continue
		}
		assert.Equal(t, want, h.status("GET", path, nil), path)
	}
	// POST is not allowed: a browser POST without Origin and session is a 403.
	assert.Equal(t, http.StatusForbidden, h.status("POST", "/-/events?topic=home", nil))
}

// The pages connect to the stream of their topic.
func TestWebPagesAreLive(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.webRepo("demo")
	for path, want := range map[string]string{
		"/":          `hx-sse:connect="/-/events?topic=home"`,
		"/demo":      `hx-sse:connect="/-/events?topic=repo%3Ademo&amp;view=summary"`,
		"/demo/refs": `hx-sse:connect="/-/events?topic=repo%3Ademo&amp;view=refs"`,
	} {
		_, _, body := h.get(path)
		assert.Contains(t, body, want, path)
	}
	_, _, nf := h.get("/nothing")
	assert.NotContains(t, nf, "hx-sse:connect")
}

// The stream ends cleanly when the server stops its event log (the start of
// the shutdown): the client sees the end of the body, and the handler is gone
// (the test server cannot close with a handler left).
func TestWebEventsEndOnShutdown(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("pub", domain.VisibilityPublic)
	home := h.sse("topic=home", "")
	repo := h.sse("topic=repo:pub", "")

	h.forge.EventLog.Close(context.WithoutCancel(h.ctx))
	for _, s := range []*sseStream{home, repo} {
		select {
		case _, ok := <-s.events:
			for ok {
				_, ok = <-s.events
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the stream did not end")
		}
	}
	require.Eventually(t, func() bool { return h.forge.EventLog.Watchers() == 0 }, 5*time.Second, 5*time.Millisecond)
}

// The web counts each visitor against the API's limit of streams per client:
// one address cannot take every watch slot of the forge.
func TestWebEventsLimitPerVisitor(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	for range app.MaxWatchersPerClient {
		h.sse("topic=home", "")
	}
	req, err := http.NewRequestWithContext(h.ctx, http.MethodGet, h.url("/-/events?topic=home"), nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("Retry-After"))
	assert.Equal(t, app.MaxWatchersPerClient, h.forge.EventLog.Watchers())
}

// A visitor that goes away frees its watch.
func TestWebEventsClientDisconnect(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("pub", domain.VisibilityPublic)
	require.Equal(t, 0, h.forge.EventLog.Watchers())
	s := h.sse("topic=home", "")
	require.Eventually(t, func() bool { return h.forge.EventLog.Watchers() == 1 }, 5*time.Second, 5*time.Millisecond)
	s.cancel()
	require.Eventually(t, func() bool { return h.forge.EventLog.Watchers() == 0 }, 5*time.Second, 5*time.Millisecond)
}
