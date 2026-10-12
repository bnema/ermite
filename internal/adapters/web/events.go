// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/clientlimit"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Live updates. A page that can change under the eyes of its reader opens
// GET /-/events (the Live component of the hint badge). The handler follows
// EventService.Watch as the principal of the request and turns each event into
// the HTML fragments the page shows, sent as Server-Sent Events:
//
//	GET /-/events?topic=home
//	GET /-/events?topic=repo:<name>&view=summary|refs
//	GET /-/events?topic=repo:<name>&view=commits&ref=<branch>&head=<sha>
//	GET /-/events?topic=repo:<name>&view=mrs
//	GET /-/events?topic=repo:<name>&view=mr&n=<number>&ref=<source branch>
//	GET /-/events?topic=repo:<name>&view=actions
//	GET /-/events?topic=repo:<name>&view=run&n=<run id>
//	GET /-/events?topic=repo:<name>&view=job&job=<job id>
//
// topic is the Watch topic. view says which fragments of a repository the
// page shows, so that the server renders only those (summary when absent).
// On the commits page, ref is the branch it lists and head the commit it
// lists first: the banner counts the commits between that head and the new
// tip, so it stays right however many pushes came in and however often the
// stream reconnected. A Last-Event-ID header resumes the stream
// (resume_after).
//
// Events, each with the id of the Watch event it comes from:
//
//	home-list       data: <tr id="repo-<name>">…</tr>, or <tr id="repo-<name>" data-gone></tr>
//	                for a repository that is deleted or no longer visible
//	repo-last-push  data: <dd id="last-push-<name>">…</dd>
//	refs-list       data: <tbody id="ref-rows">…</tbody>, and again with
//	                <dl id="refs-kv">…</dl>, the facts of the rail
//	commits-head    data: <div id="commits-head">N new commits — reload</div>
//	mr-notice       data: <div id="mr-notice">…</div>, on the merge request pages:
//	                something changed, with the link that reloads the page
//	ci-run          data: <tr id="run-<id>">…</tr> (the list of runs) or
//	                <div id="run-body">…</div> (one run)
//	ci-job          data: <h2 id="job-head">…</h2> and <ol id="job-steps">…</ol>; the log
//	                itself is never sent in the stream, the page reads the lines after
//	                the last it holds from /raw?from=
//	gap             data: gap; events were lost, or what the page shows is gone or
//	                changed: the page fetches itself again (ermiteLive in ermite.js)
//
// The data of an event is one fragment, a top-level element with an id, one
// "data:" line per line of HTML; the page swaps the element that has that id
// (ermiteLive.swap). The slots of the banners (commits-head, mr-notice) are
// status regions that stay in the page: only their children are replaced, so
// that a screen reader announces the banner.
// An event that changes nothing on the page is still sent as an id alone, so
// that a reconnection resumes after it.

// DefaultHeartbeat is how often an idle stream gets a comment line, so that
// proxies keep it open and a dead peer is noticed.
const DefaultHeartbeat = 25 * time.Second

const (
	// writeWindow bounds one write to the visitor, whatever the write timeout
	// of the server is.
	writeWindow = 30 * time.Second
	// retryAfter is the delay asked from a visitor refused for lack of room.
	retryAfter = 10
)

// parseTopic reads "home" or "repo:<name>"; repo is "" for home.
func parseTopic(topic string) (repo string, ok bool) {
	if topic == "home" {
		return "", true
	}
	if name, found := strings.CutPrefix(topic, "repo:"); found && name != "" {
		return name, true
	}
	return "", false
}

// events is GET /-/events.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Events == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	topic := q.Get("topic")
	repo, ok := parseTopic(topic)
	if !ok {
		denyStream(w, http.StatusBadRequest, "topic must be home or repo:<name>")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	log := zerowrap.FromCtx(ctx)

	if repo != "" {
		// A repository the visitor cannot see is a missing one (AC2).
		cctx, ccancel := context.WithTimeout(ctx, requestTimeout)
		err := s.canSee(cctx, repo)
		ccancel()
		if err != nil {
			s.denyEvents(ctx, w, err)
			return
		}
	}

	// The watch runs in a goroutine and hands each event to this one through
	// an unbuffered channel: a visitor that reads slowly holds the watch in
	// its send, falls behind the buffer of the use case, and gets a gap.
	msgs := make(chan recvd)
	started := make(chan struct{})
	var startOnce sync.Once
	start := func() { startOnce.Do(func() { close(started) }) }
	finished := make(chan struct{})
	params := ports.WatchParams{
		Topics: []string{topic}, ResumeAfter: r.Header.Get("Last-Event-ID"),
		// The visitor is the client of the watch, for the limit per client.
		Client: clientlimit.Key(clientlimit.ClientAddr(r, s.cfg.TrustedProxies)),
		Ready:  start,
	}
	go func() {
		defer close(finished)
		defer close(msgs)
		// net/http only recovers the handler goroutine: a panic in the watch
		// would end the process. It ends this stream as an error instead.
		defer func() {
			if v := recover(); v != nil {
				log.Error().Interface("panic", v).Msg("event watch panicked")
				select {
				case msgs <- recvd{err: fmt.Errorf("watch panicked: %v", v)}:
				case <-ctx.Done():
				}
			}
		}()
		err := s.cfg.Events.Watch(ctx, principal(ctx), params, func(we ports.WatchEvent) error {
			start()
			select {
			case msgs <- recvd{ev: we}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if err != nil && ctx.Err() == nil {
			select {
			case msgs <- recvd{err: err}:
			case <-ctx.Done():
			}
		}
	}()
	defer func() {
		cancel()
		<-finished
	}()

	// The watch is registered (or refused, which ends it before): only then
	// does the answer commit to 200.
	var first *recvd
	select {
	case <-started:
	case m, open := <-msgs:
		if !open {
			return // the server is stopping
		}
		if m.err != nil {
			s.denyEvents(ctx, w, m.err)
			return
		}
		first = &m
	case <-ctx.Done():
		return
	}

	rc := http.NewResponseController(w)
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	cacheHeaders(ctx, hdr, "no-cache, no-transform", sessionCache+", no-transform")
	if hdr.Get("Cache-Control") == noStore { // an invalid cookie was cleared
		hdr.Set("Cache-Control", noStore+", no-transform")
	}
	hdr.Set("X-Accel-Buffering", "no")
	_ = rc.SetWriteDeadline(time.Now().Add(writeWindow))
	w.WriteHeader(http.StatusOK)
	out := &sseWriter{w: w, rc: rc}
	if err := out.raw(": connected\n\n"); err != nil {
		return
	}

	st := &liveStream{s: s, repo: repo, view: q.Get("view"), ref: q.Get("ref"), head: q.Get("head"), n: q.Get("n"), job: q.Get("job")}
	handle := func(m recvd) bool {
		if m.err != nil {
			if ctx.Err() == nil && !errors.Is(m.err, context.Canceled) {
				log.Debug().Err(m.err).Str("topic", topic).Msg("event stream ended")
			}
			return false
		}
		if !liveEvent(m.ev) {
			return true // nothing to show, not even an id to resume after
		}
		// An owner's stream ends when the session does, before it sends
		// anything more: an event can carry what only the owner may see, and
		// waiting for the next heartbeat would leave a window.
		if SignedIn(ctx) && !s.sessionStillValid(ctx, r) {
			return false
		}
		rctx, rcancel := context.WithTimeout(ctx, requestTimeout)
		frags, err := st.render(rctx, m.ev)
		rcancel()
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			// What the page shows can no longer be told from the events:
			// have it fetch itself again.
			log.Warn().Err(err).Str("topic", topic).Msg("render live fragment")
			frags = gap
		}
		return out.send(m.ev.ID, frags) == nil
	}
	if first != nil && !handle(*first) {
		return
	}

	beat := s.cfg.Heartbeat
	if beat <= 0 {
		beat = DefaultHeartbeat
	}
	tick := time.NewTicker(beat)
	defer tick.Stop()
	for {
		select {
		case m, open := <-msgs:
			if !open || !handle(m) {
				return
			}
		case <-tick.C:
			// A stream opened by the owner ends when the session does
			// (sign out everywhere, expiry): the page reconnects as it can.
			if SignedIn(ctx) && !s.sessionStillValid(ctx, r) {
				return
			}
			if out.raw(": heartbeat\n\n") != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// recvd is a message of the Watch stream, or how it failed.
type recvd struct {
	ev  ports.WatchEvent
	err error
}

// canSee checks that the caller of the request may see the repository named in
// a topic: one it may not is as missing as one that does not exist.
func (s *Server) canSee(ctx context.Context, rawName string) error {
	name, err := domain.ParseRepoName(rawName)
	if err != nil {
		return err
	}
	_, err = s.cfg.Repos.Get(ctx, principal(ctx), name)
	return err
}

// denyStream answers a request that gets no stream.
func denyStream(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, msg, status)
}

// denyEvents maps an error of a use case, before the stream started, to a
// status. Not found and invalid are 404 whatever the reason: a private
// repository is a missing one.
func (s *Server) denyEvents(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, context.Canceled):
		// The visitor is gone.
	case errors.Is(err, domain.ErrNotFound), domain.IsInvalid(err):
		denyStream(w, http.StatusNotFound, "not found")
	case errors.Is(err, domain.ErrResourceExhausted):
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		denyStream(w, http.StatusServiceUnavailable, "too many event streams, retry later")
	case errors.Is(err, domain.ErrUnauthenticated):
		denyStream(w, http.StatusUnauthorized, "unauthorized")
	default:
		log := zerowrap.FromCtx(ctx)
		log.Error().Err(err).Msg("open event stream")
		denyStream(w, http.StatusInternalServerError, "internal error")
	}
}

// fragment is one SSE event to send: the name and the HTML of the fragment.
type fragment struct{ event, html string }

// sseWriter writes events to a visitor, flushing each, under a deadline of
// its own so that the write timeout of the server cannot end the stream.
type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (o *sseWriter) raw(s string) error {
	_ = o.rc.SetWriteDeadline(time.Now().Add(writeWindow))
	if _, err := io.WriteString(o.w, s); err != nil {
		return err
	}
	return o.rc.Flush()
}

// send writes the fragments of one Watch event under its id. An event that
// has no fragment still carries its id, so that a reconnection resumes after
// it.
func (o *sseWriter) send(id string, frags []fragment) error {
	if len(frags) == 0 {
		if id == "" {
			return nil
		}
		return o.raw(formatEvent(id, "", "", false))
	}
	for _, f := range frags {
		if err := o.raw(formatEvent(id, f.event, f.html, true)); err != nil {
			return err
		}
	}
	return nil
}

// formatEvent is one SSE event. Each line of data is a "data:" field, and a
// browser joins them back with newlines.
func formatEvent(id, event, data string, hasData bool) string {
	var b strings.Builder
	if event != "" {
		b.WriteString("event: " + event + "\n")
	}
	if id != "" {
		b.WriteString("id: " + id + "\n")
	}
	if hasData {
		data = strings.ReplaceAll(strings.ReplaceAll(data, "\r\n", "\n"), "\r", "\n")
		for line := range strings.SplitSeq(data, "\n") {
			b.WriteString("data: " + line + "\n")
		}
	}
	b.WriteString("\n")
	return b.String()
}

// liveURL is the stream a page follows: the topic and, for a repository
// page, what it shows (see the comment on events).
func liveURL(topic, query string) string {
	u := "/-/events?topic=" + url.QueryEscape(topic)
	if query != "" {
		u += "&" + query
	}
	return u
}
