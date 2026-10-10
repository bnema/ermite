// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
)

func TestFormatEvent(t *testing.T) {
	assert.Equal(t, "event: home-list\nid: b-1\ndata: <tr id=\"x\">\ndata:   <td>1</td>\ndata: </tr>\n\n",
		formatEvent("b-1", "home-list", "<tr id=\"x\">\n  <td>1</td>\n</tr>", true), "one data line per line")
	assert.Equal(t, "event: gap\ndata: gap\n\n", formatEvent("", "gap", "gap", true))
	assert.Equal(t, "id: b-2\n\n", formatEvent("b-2", "", "", false), "an id alone")
	assert.Equal(t, "event: e\ndata: a\ndata: b\ndata: c\n\n", formatEvent("", "e", "a\r\nb\rc", true), "no line break escapes the field")
	assert.Equal(t, "event: e\ndata: \n\n", formatEvent("", "e", "", true))
}

func TestParseTopic(t *testing.T) {
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
	assert.Equal(t, "/-/events?topic=home", liveURL("home", ""))
	assert.Equal(t, "/-/events?topic=repo%3Ax&view=refs", liveURL("repo:x", "view=refs"))
}

func TestNewCommitsText(t *testing.T) {
	assert.Equal(t, "1 new commit — reload", newCommits("1", 1))
	assert.Equal(t, "3 new commits — reload", newCommits("3", 3))
	assert.Equal(t, "100+ new commits — reload", newCommits("100+", 2))
}

// fakeEvents is an EventService whose behaviour a test sets.
type fakeEvents struct {
	ermiteconnect.UnimplementedEventServiceHandler
	err     error
	events  []*pb.Event
	opened  atomic.Int32
	closed  atomic.Int32
	resume  atomic.Value
	release chan struct{}
	push    chan *pb.Event // events sent on demand, after the fixed ones
}

func (f *fakeEvents) Watch(ctx context.Context, req *connect.Request[pb.WatchRequest], st *connect.ServerStream[pb.WatchResponse]) error {
	f.opened.Add(1)
	defer f.closed.Add(1)
	f.resume.Store(req.Msg.GetResumeAfter())
	if f.err != nil {
		return f.err
	}
	_ = st.Send(nil)
	for _, ev := range f.events {
		if err := st.Send(&pb.WatchResponse{Event: ev}); err != nil {
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
			if err := st.Send(&pb.WatchResponse{Event: ev}); err != nil {
				return err
			}
		}
	}
}

// repoOK answers every GetRepo with a public repository.
type repoOK struct {
	ermiteconnect.UnimplementedRepoServiceHandler
}

func (repoOK) GetRepo(_ context.Context, req *connect.Request[pb.GetRepoRequest]) (*connect.Response[pb.GetRepoResponse], error) {
	return connect.NewResponse(&pb.GetRepoResponse{Repo: &pb.Repo{Name: req.Msg.GetRepo(), DefaultBranch: "main"}}), nil
}

// eventsServer serves the web on a fake API, with a write timeout shorter
// than the heartbeat, like a server that sets one.
func eventsServer(t *testing.T, f *fakeEvents, beat time.Duration, writeTimeout time.Duration) *httptest.Server {
	t.Helper()
	return eventsServerWith(t, f, beat, writeTimeout, nil)
}

// eventsServerWith is eventsServer with sessions, when sess is not nil.
func eventsServerWith(t *testing.T, f *fakeEvents, beat time.Duration, writeTimeout time.Duration, sess SessionAuth) *httptest.Server {
	t.Helper()
	api := http.NewServeMux()
	api.Handle(ermiteconnect.NewEventServiceHandler(f))
	api.Handle(ermiteconnect.NewRepoServiceHandler(repoOK{}))
	backend := httptest.NewServer(api)
	t.Cleanup(backend.Close)

	srv, err := New(Config{
		Repos:     ermiteconnect.NewRepoServiceClient(backend.Client(), backend.URL),
		Git:       ermiteconnect.NewGitServiceClient(backend.Client(), backend.URL),
		Events:    ermiteconnect.NewEventServiceClient(backend.Client(), backend.URL),
		PublicURL: "https://forge.example", SourceURL: "https://forge.example/ermite", Version: "v1",
		Heartbeat: beat, Sessions: sess, Limiter: limiterFor(sess),
	})
	require.NoError(t, err)
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
	f := &fakeEvents{release: make(chan struct{})}
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
	f := &fakeEvents{release: make(chan struct{}), events: []*pb.Event{
		{Id: "b-7", Kind: &pb.Event_Pushed{Pushed: &pb.Pushed{Repo: "x"}}},
		{Kind: &pb.Event_Gap{Gap: &pb.Gap{}}},
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
}

// Too many watchers is a 503 the browser can retry, not a stream.
func TestEventsResourceExhausted(t *testing.T) {
	f := &fakeEvents{err: connect.NewError(connect.CodeResourceExhausted, errors.New("too many watchers"))}
	ts := eventsServer(t, f, time.Hour, 0)
	resp, _ := open(t, ts, "topic=home", nil)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("Retry-After"))
	assert.NotContains(t, resp.Header.Get("Content-Type"), "event-stream")

	f.err = connect.NewError(connect.CodeNotFound, errors.New("x"))
	resp, _ = open(t, ts, "topic=home", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	f.err = errors.New("boom")
	resp, _ = open(t, ts, "topic=home", nil)
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

// The handler returns, and the watch it opened ends, when the visitor goes
// away or the API ends the stream.
func TestEventsNoLeak(t *testing.T) {
	f := &fakeEvents{release: make(chan struct{})}
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

func TestEventsOffWithoutClient(t *testing.T) {
	srv, err := New(Config{
		Repos:     ermiteconnect.NewRepoServiceClient(http.DefaultClient, "http://x.invalid"),
		Git:       ermiteconnect.NewGitServiceClient(http.DefaultClient, "http://x.invalid"),
		PublicURL: "https://forge.example",
	})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/-/events?topic=home", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// A change of the repository (its default branch, say) makes the pages that
// show it load themselves again: the summary and the refs page, whose rows
// and marks depend on it. The commits page shows a branch, not the default.
func TestRepoUpdatedReloadsSummaryAndRefs(t *testing.T) {
	api := http.NewServeMux()
	api.Handle(ermiteconnect.NewRepoServiceHandler(repoOK{}))
	backend := httptest.NewServer(api)
	t.Cleanup(backend.Close)
	srv, err := New(Config{
		Repos:     ermiteconnect.NewRepoServiceClient(backend.Client(), backend.URL),
		Git:       ermiteconnect.NewGitServiceClient(backend.Client(), backend.URL),
		PublicURL: "https://forge.example",
	})
	require.NoError(t, err)

	ev := &pb.Event{Kind: &pb.Event_RepoUpdated{RepoUpdated: &pb.RepoUpdated{Repo: &pb.Repo{Name: "x"}}}}
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

func limiterFor(sess SessionAuth) FailureLimiter {
	if sess == nil {
		return nil
	}
	return &countingLimiter{}
}

// revocable is a SessionAuth whose one session ends when the test says so, like
// SignOutEverywhere does.
type revocable struct {
	fakeSessions
	ended atomic.Bool
}

func (r *revocable) Verify(ctx context.Context, tok string) (context.Context, bool, error) {
	return ctx, tok == r.valid && !r.ended.Load(), nil
}

// GO-002: the stream of the owner is checked before each event is sent, not
// only on the heartbeat: after the session ended, the next event is not sent
// and the stream ends.
func TestEventsOwnerStreamEndsBeforeNextEvent(t *testing.T) {
	f := &fakeEvents{release: make(chan struct{}), push: make(chan *pb.Event)}
	sess := &revocable{fakeSessions: fakeSessions{valid: "tok"}}
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
	f.push <- &pb.Event{Id: "b-1", Kind: &pb.Event_Gap{Gap: &pb.Gap{}}}
	var seen []string
	for l := line(); l != "data: gap"; l = line() {
		seen = append(seen, l)
		require.NotEqual(t, "EOF", l)
	}
	assert.Contains(t, seen, "id: b-1")

	// Signed out everywhere: the next event is not sent, the stream ends.
	sess.ended.Store(true)
	select {
	case f.push <- &pb.Event{Id: "b-2", Kind: &pb.Event_Gap{Gap: &pb.Gap{}}}:
	case <-time.After(5 * time.Second):
		t.Fatal("the event was not delivered to the web")
	}
	rest, _ := io.ReadAll(rd)
	assert.NotContains(t, string(rest), "b-2", "nothing is sent after the session ended")
	require.Eventually(t, func() bool { return f.closed.Load() == 1 }, 5*time.Second, time.Millisecond, "the watch is closed")
}

// A visitor's stream does not depend on sessions: events keep flowing.
func TestEventsVisitorStreamIgnoresSessions(t *testing.T) {
	f := &fakeEvents{release: make(chan struct{}), push: make(chan *pb.Event)}
	sess := &revocable{fakeSessions: fakeSessions{valid: "tok"}}
	sess.ended.Store(true)
	ts := eventsServerWith(t, f, time.Hour, 0, sess)
	resp, _ := open(t, ts, "topic=home", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	f.push <- &pb.Event{Id: "b-1", Kind: &pb.Event_Gap{Gap: &pb.Gap{}}}
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
	f := &fakeEvents{release: make(chan struct{})}
	ts := eventsServerWith(t, f, time.Hour, 0, &fakeSessions{valid: "tok"})
	resp, _ := open(t, ts, "topic=home", map[string]string{"Cookie": CookieName + "=bad"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "no-store, no-transform", resp.Header.Get("Cache-Control"))
	assert.Contains(t, resp.Header.Values("Vary"), "Cookie")
	assert.Contains(t, resp.Header.Get("Set-Cookie"), "Max-Age=0")
}
