// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/bnema/zerowrap"

	pb "bnema.dev/ermite-api/gen/ermite"
)

// Live updates. A page that can change under the eyes of its reader opens
// GET /-/events (the Live component of the status bar). The handler follows
// EventService.Watch as the visitor of the request and turns each event into
// the HTML fragments the page shows, sent as Server-Sent Events:
//
//	GET /-/events?topic=home
//	GET /-/events?topic=repo:<name>&view=summary|refs
//	GET /-/events?topic=repo:<name>&view=commits&ref=<branch>&head=<sha>
//	GET /-/events?topic=repo:<name>&view=mrs
//	GET /-/events?topic=repo:<name>&view=mr&n=<number>&ref=<source branch>
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
//	gap             data: gap; events were lost, or what the page shows is gone or
//	                changed: the page fetches itself again (ermiteLive in ermite.js)
//
// The data of an event is one fragment, a top-level element with an id, one
// "data:" line per line of HTML; the page swaps the element that has that id.
// An event that changes nothing on the page is still sent as an id alone, so
// that a reconnection resumes after it.

// DefaultHeartbeat is how often an idle stream gets a comment line, so that
// proxies keep it open and a dead peer is noticed.
const DefaultHeartbeat = 25 * time.Second

const (
	// startGrace is how long the handler waits after the Watch call answered
	// for an early failure (too many watchers) before it commits to 200: the
	// Connect protocol reports it in the stream, after the headers.
	startGrace = 100 * time.Millisecond
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
		_, err := s.cfg.Repos.GetRepo(cctx, connect.NewRequest(&pb.GetRepoRequest{Repo: repo}))
		ccancel()
		if err != nil {
			s.denyEvents(ctx, w, err)
			return
		}
	}

	stream, err := s.cfg.Events.Watch(ctx, connect.NewRequest(&pb.WatchRequest{
		Topics: []string{topic}, ResumeAfter: r.Header.Get("Last-Event-ID"),
	}))
	if err != nil {
		s.denyEvents(ctx, w, err)
		return
	}
	// Wait for the answer of the API, which comes once the watch is registered
	// (or refused), then read the stream from a goroutine.
	_ = stream.ResponseHeader()
	msgs := make(chan recvd)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(msgs)
		for stream.Receive() {
			select {
			case msgs <- recvd{ev: stream.Msg().GetEvent()}:
			case <-ctx.Done():
				return
			}
		}
		if err := stream.Err(); err != nil {
			select {
			case msgs <- recvd{err: err}:
			case <-ctx.Done():
			}
		}
	}()
	defer func() {
		cancel()
		_ = stream.Close()
		<-done
	}()

	var first *recvd
	grace := time.NewTimer(startGrace)
	select {
	case m, open := <-msgs:
		grace.Stop()
		if !open {
			return // the server is stopping
		}
		first = &m
	case <-grace.C:
	case <-ctx.Done():
		grace.Stop()
		return
	}
	if first != nil && first.err != nil {
		s.denyEvents(ctx, w, first.err)
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

	st := &liveStream{s: s, repo: repo, view: q.Get("view"), ref: q.Get("ref"), head: q.Get("head"), n: q.Get("n")}
	handle := func(m recvd) bool {
		if m.err != nil {
			if ctx.Err() == nil && connect.CodeOf(m.err) != connect.CodeCanceled {
				log.Debug().Err(m.err).Str("topic", topic).Msg("event stream ended")
			}
			return false
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
		return out.send(m.ev.GetId(), frags) == nil
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
	ev  *pb.Event
	err error
}

// denyStream answers a request that gets no stream.
func denyStream(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, msg, status)
}

// denyEvents maps an error of the API, before the stream started, to a
// status. Not found and invalid are 404 whatever the reason: a private
// repository is a missing one.
func (s *Server) denyEvents(ctx context.Context, w http.ResponseWriter, err error) {
	switch code := connect.CodeOf(err); {
	case code == connect.CodeNotFound, code == connect.CodeInvalidArgument:
		denyStream(w, http.StatusNotFound, "not found")
	case code == connect.CodeResourceExhausted:
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		denyStream(w, http.StatusServiceUnavailable, "too many event streams, retry later")
	case code == connect.CodeUnauthenticated:
		denyStream(w, http.StatusUnauthorized, "unauthorized")
	case errors.Is(err, context.Canceled), code == connect.CodeCanceled:
		// The visitor is gone.
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
