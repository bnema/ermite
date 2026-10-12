// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

func TestFormatEvent(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "event: home-list\nid: b-1\ndata: <tr id=\"x\">\ndata:   <td>1</td>\ndata: </tr>\n\n",
		formatEvent("b-1", "home-list", "<tr id=\"x\">\n  <td>1</td>\n</tr>", true), "one data line per line")
	assert.Equal(t, "event: gap\ndata: gap\n\n", formatEvent("", "gap", "gap", true))
	assert.Equal(t, "id: b-2\n\n", formatEvent("b-2", "", "", false), "an id alone")
	assert.Equal(t, "event: e\ndata: a\ndata: b\ndata: c\n\n", formatEvent("", "e", "a\r\nb\rc", true), "no line break escapes the field")
	assert.Equal(t, "event: e\ndata: \n\n", formatEvent("", "e", "", true))
}

func TestParseTopic(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"home": "", "repo:x": "x", "repo:a.b-c": "a.b-c"} {
		repo, ok := parseTopic(in)
		assert.True(t, ok, in)
		assert.Equal(t, want, repo, in)
	}
	for _, in := range []string{"", "repo:", "other", "home,repo:x", "Repo:x", "repo"} {
		_, ok := parseTopic(in)
		assert.False(t, ok, in)
	}
}

func TestLiveURL(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "/-/events?topic=home", liveURL("home", ""))
	assert.Equal(t, "/-/events?topic=repo%3Ax&view=refs", liveURL("repo:x", "view=refs"))
}

func TestNewCommitsText(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "1 new commit — reload", newCommits("1", 1))
	assert.Equal(t, "3 new commits — reload", newCommits("3", 3))
	assert.Equal(t, "100+ new commits — reload", newCommits("100+", 2))
}

// watchProbe drives the generated EventService mock: what the watch does, and
// a record of the principal and the parameters of the watches it was asked for.
type watchProbe struct {
	err     error
	events  []ports.WatchEvent
	opened  atomic.Int32
	closed  atomic.Int32
	resume  atomic.Value // string: ResumeAfter of the last watch
	topics  atomic.Value // []string: Topics of the last watch
	client  atomic.Value // string: Client of the last watch
	who     atomic.Value // domain.Principal: caller of the last watch
	release chan struct{}
	push    chan ports.WatchEvent // events sent on demand, after the fixed ones
}

// service is the EventService the web watches: the mock, answering with the
// probe.
func (f *watchProbe) service(t testing.TB) *mocks.EventService {
	t.Helper()
	m := mocks.NewEventService(t)
	m.EXPECT().Watch(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(f.run).Maybe()
	return m
}

func (f *watchProbe) run(ctx context.Context, p domain.Principal, params ports.WatchParams, send func(ports.WatchEvent) error) error {
	f.opened.Add(1)
	defer f.closed.Add(1)
	f.resume.Store(params.ResumeAfter)
	f.topics.Store(params.Topics)
	f.client.Store(params.Client)
	f.who.Store(p)
	if f.err != nil {
		return f.err
	}
	if params.Ready != nil {
		params.Ready()
	}
	for _, ev := range f.events {
		if err := send(ev); err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-f.release:
			return nil
		case ev := <-f.push:
			if err := send(ev); err != nil {
				return err
			}
		}
	}
}

// eventsServer serves the web on a fake EventService, with a write timeout
// shorter than the heartbeat, like a server that sets one.
func eventsServer(t *testing.T, f *watchProbe, beat time.Duration, writeTimeout time.Duration) *httptest.Server {
	t.Helper()
	return eventsServerWith(t, f, beat, writeTimeout, nil)
}

// eventsServerWith is eventsServer with sessions, when sess is not nil.
func eventsServerWith(t *testing.T, f *watchProbe, beat time.Duration, writeTimeout time.Duration, sess ports.WebSessionService) *httptest.Server {
	t.Helper()
	srv := newTestServer(t, Config{
		Repos:     reposServing(t, publicRepo(t, "x")),
		Browse:    mocks.NewBrowseService(t),
		Events:    f.service(t),
		Heartbeat: beat, Sessions: sess, Limiter: limiterFor(sess),
	})
	ts := httptest.NewUnstartedServer(srv)
	ts.Config.WriteTimeout = writeTimeout
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

func open(t *testing.T, ts *httptest.Server, query string, hdr map[string]string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/-/events?"+query, nil)
	require.NoError(t, err)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { cancel(); _ = resp.Body.Close() })
	return resp, cancel
}

// An idle stream gets a comment every heartbeat, and the write timeout of the
// server does not end it.
func TestEventsHeartbeatSurvivesWriteTimeout(t *testing.T) {
	t.Parallel()
	f := &watchProbe{release: make(chan struct{})}
	ts := eventsServer(t, f, 40*time.Millisecond, 100*time.Millisecond)
	resp, cancel := open(t, ts, "topic=home", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	sc := bufio.NewScanner(resp.Body)
	beats := 0
	deadline := time.AfterFunc(5*time.Second, cancel)
	defer deadline.Stop()
	start := time.Now()
	for sc.Scan() && beats < 8 {
		if strings.HasPrefix(sc.Text(), ": heartbeat") {
			beats++
		}
	}
	assert.Equal(t, 8, beats)
	assert.Greater(t, time.Since(start), 250*time.Millisecond, "the stream outlived the 100 ms write timeout")
}

// The id of the event, its name and the lines of its data come through; the
// Last-Event-ID header is the resume point; a Gap becomes a gap event.
func TestEventsStreamsEventsAndResumes(t *testing.T) {
	t.Parallel()
	f := &watchProbe{release: make(chan struct{}), events: []ports.WatchEvent{
		{ID: "b-7", Event: domain.PushEvent{Repo: repoName(t, "x")}},
		{Gap: true},
	}}
	ts := eventsServer(t, f, time.Hour, 0)
	resp, _ := open(t, ts, "topic=repo:x&view=summary", map[string]string{"Last-Event-ID": "b-6"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	sc := bufio.NewScanner(resp.Body)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if sc.Text() == "data: gap" {
			break
		}
	}
	assert.Contains(t, lines, "id: b-7", "an event with nothing to show keeps its id")
	assert.Contains(t, lines, "event: gap")
	assert.Equal(t, "b-6", f.resume.Load())
	assert.Equal(t, []string{"repo:x"}, f.topics.Load())
	assert.Equal(t, domain.Anonymous(), f.who.Load(), "a visitor watches as anonymous")
	assert.NotEmpty(t, f.client.Load(), "the watch is attributed to the address of the visitor")
}

// Too many watchers is a 503 the browser can retry, not a stream; what the
// watch is refused for decides the status.
func TestEventsRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		err        error
		want       int
		retryAfter bool
	}{
		{"resource exhausted", fmt.Errorf("%w: too many watchers", domain.ErrResourceExhausted), http.StatusServiceUnavailable, true},
		{"not found", domain.ErrNotFound, http.StatusNotFound, false},
		{"invalid", domain.ErrInvalid, http.StatusNotFound, false},
		{"unauthenticated", domain.ErrUnauthenticated, http.StatusUnauthorized, false},
		{"internal", errors.New("boom"), http.StatusInternalServerError, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ts := eventsServer(t, &watchProbe{err: tt.err}, time.Hour, 0)
			resp, _ := open(t, ts, "topic=home", nil)
			assert.Equal(t, tt.want, resp.StatusCode)
			assert.Equal(t, tt.retryAfter, resp.Header.Get("Retry-After") != "")
			assert.NotContains(t, resp.Header.Get("Content-Type"), "event-stream")
			assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
		})
	}
}

// A panic in the watch runs outside the handler goroutine, where net/http
// would not recover it: it ends the stream with a 500 instead of the process.
func TestEventsWatchPanicIsAnError(t *testing.T) {
	t.Parallel()
	m := mocks.NewEventService(t)
	m.EXPECT().Watch(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(context.Context, domain.Principal, ports.WatchParams, func(ports.WatchEvent) error) error {
			panic("boom")
		})
	ts := httptest.NewServer(newTestServer(t, Config{
		Repos:  reposServing(t, publicRepo(t, "x")),
		Browse: mocks.NewBrowseService(t),
		Events: m,
	}))
	t.Cleanup(ts.Close)
	resp, _ := open(t, ts, "topic=home", nil)
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

// The topic of a repository the visitor cannot see is refused before any
// watch is opened: a private repository is a missing one.
func TestEventsPrivateRepositoryTopic(t *testing.T) {
	t.Parallel()
	f := &watchProbe{release: make(chan struct{})}
	srv := newTestServer(t, Config{
		Repos: reposServing(t, publicRepo(t, "pub")), Browse: mocks.NewBrowseService(t), Events: f.service(t),
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	for _, query := range []string{"topic=repo:secret", "topic=repo:Bad"} {
		resp, _ := open(t, ts, query, nil)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, query)
	}
	assert.Zero(t, f.opened.Load(), "no watch was opened for them")
}

// The owner's stream is the owner's: the watch is opened as the owner.
func TestEventsOwnerWatchesAsOwner(t *testing.T) {
	t.Parallel()
	f := &watchProbe{release: make(chan struct{})}
	sess, _ := newRevocable(t, "tok")
	ts := eventsServerWith(t, f, time.Hour, 0, sess)
	resp, _ := open(t, ts, "topic=home", map[string]string{"Cookie": CookieName + "=tok"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, domain.Owner(), f.who.Load())
}

// The handler returns, and the watch it opened ends, when the visitor goes
// away or the API ends the stream.
func TestEventsNoLeak(t *testing.T) {
	t.Parallel()
	f := &watchProbe{release: make(chan struct{})}
	ts := eventsServer(t, f, time.Hour, 0)

	resp, cancel := open(t, ts, "topic=home", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Eventually(t, func() bool { return f.opened.Load() == 1 }, 5*time.Second, time.Millisecond)
	cancel()
	require.Eventually(t, func() bool { return f.closed.Load() == 1 }, 5*time.Second, time.Millisecond, "the visitor left")

	resp, _ = open(t, ts, "topic=home", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Eventually(t, func() bool { return f.opened.Load() == 2 }, 5*time.Second, time.Millisecond)
	close(f.release) // the API ends the stream (shutdown)
	done := make(chan struct{})
	go func() { _, _ = bufio.NewReader(resp.Body).ReadString(0); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end with the API's")
	}
	require.Eventually(t, func() bool { return f.closed.Load() == 2 }, 5*time.Second, time.Millisecond)
}

func TestEventsOffWithoutEventService(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, Config{Repos: mocks.NewRepoService(t), Browse: mocks.NewBrowseService(t)})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/-/events?topic=home", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// A change of the repository (its default branch, say) makes the pages that
// show it load themselves again: the summary and the refs page, whose rows
// and marks depend on it. The commits page shows a branch, not the default.
func TestRepoUpdatedReloadsSummaryAndRefs(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "x")), Browse: mocks.NewBrowseService(t)})

	ev := ports.WatchEvent{Event: domain.RepoUpdated{Repo: repoName(t, "x")}}
	for view, wantGap := range map[string]bool{"summary": true, "refs": true, "commits": false} {
		ls := &liveStream{s: srv, repo: "x", view: view}
		got, err := ls.render(t.Context(), ev)
		require.NoError(t, err, view)
		if wantGap {
			assert.Equal(t, gap, got, view)
		} else {
			assert.Empty(t, got, view)
		}
	}
}

func limiterFor(sess ports.WebSessionService) ports.FailureLimiter {
	if sess == nil {
		return nil
	}
	return &countingLimiter{}
}

// newRevocable is a ports.WebSessionService whose one session ends when the test sets
// ended, like SignOutEverywhere does.
func newRevocable(t *testing.T, valid string) (*mocks.WebSessionService, *atomic.Bool) {
	t.Helper()
	var ended atomic.Bool
	return newVerifier(t, valid, func(tok string) (bool, error) { return tok == valid && !ended.Load(), nil }), &ended
}

// GO-002: the stream of the owner is checked before each event is sent, not
// only on the heartbeat: after the session ended, the next event is not sent
// and the stream ends.
func TestEventsOwnerStreamEndsBeforeNextEvent(t *testing.T) {
	t.Parallel()
	f := &watchProbe{release: make(chan struct{}), push: make(chan ports.WatchEvent)}
	sess, ended := newRevocable(t, "tok")
	ts := eventsServerWith(t, f, time.Hour, 0, sess) // no heartbeat in the test
	resp, cancel := open(t, ts, "topic=home", map[string]string{"Cookie": CookieName + "=tok"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Cache-Control"), "private")
	// Without the check the stream would stay open: bound the read.
	defer time.AfterFunc(5*time.Second, cancel).Stop()
	rd := bufio.NewReader(resp.Body)

	line := func() string {
		s, err := rd.ReadString('\n')
		if err != nil {
			return "EOF"
		}
		return strings.TrimRight(s, "\n")
	}
	// The stream is up; a first event goes through while the session lives.
	require.Equal(t, ": connected", line())
	line()
	f.push <- ports.WatchEvent{ID: "b-1", Gap: true}
	var seen []string
	for l := line(); l != "data: gap"; l = line() {
		seen = append(seen, l)
		require.NotEqual(t, "EOF", l)
	}
	assert.Contains(t, seen, "id: b-1")

	// Signed out everywhere: the next event is not sent, the stream ends.
	ended.Store(true)
	select {
	case f.push <- ports.WatchEvent{ID: "b-2", Gap: true}:
	case <-time.After(5 * time.Second):
		t.Fatal("the event was not delivered to the web")
	}
	rest, _ := io.ReadAll(rd)
	assert.NotContains(t, string(rest), "b-2", "nothing is sent after the session ended")
	require.Eventually(t, func() bool { return f.closed.Load() == 1 }, 5*time.Second, time.Millisecond, "the watch is closed")
}

// A visitor's stream does not depend on sessions: events keep flowing.
func TestEventsVisitorStreamIgnoresSessions(t *testing.T) {
	t.Parallel()
	f := &watchProbe{release: make(chan struct{}), push: make(chan ports.WatchEvent)}
	sess, ended := newRevocable(t, "tok")
	ended.Store(true)
	ts := eventsServerWith(t, f, time.Hour, 0, sess)
	resp, _ := open(t, ts, "topic=home", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	f.push <- ports.WatchEvent{ID: "b-1", Gap: true}
	sc := bufio.NewScanner(resp.Body)
	found := false
	for sc.Scan() {
		if sc.Text() == "id: b-1" {
			found = true
			break
		}
	}
	assert.True(t, found)
}

// GO-006: a stream opened with an invalid cookie is anonymous, and its headers
// say the answer must not be stored and varies on the cookie.
func TestEventsInvalidCookieStream(t *testing.T) {
	t.Parallel()
	f := &watchProbe{release: make(chan struct{})}
	ts := eventsServerWith(t, f, time.Hour, 0, newSessions(t, "tok"))
	resp, _ := open(t, ts, "topic=home", map[string]string{"Cookie": CookieName + "=bad"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "no-store, no-transform", resp.Header.Get("Cache-Control"))
	assert.Contains(t, resp.Header.Values("Vary"), "Cookie")
	assert.Contains(t, resp.Header.Get("Set-Cookie"), "Max-Age=0")
}
