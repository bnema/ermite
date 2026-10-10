// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// DefaultEventBuffer is the number of events the Events use case remembers.
const DefaultEventBuffer = 1024

// Topics a watcher may follow.
const (
	// TopicHome follows every repository the caller may see.
	TopicHome = "home"
	// topicRepoPrefix starts the topic of one repository: "repo:<name>".
	topicRepoPrefix = "repo:"
)

// bufferedEvent is an event with its sequence number in the buffer.
type bufferedEvent struct {
	seq uint64
	ev  domain.Event
}

// Events keeps the last events of the bus in a ring buffer, numbers them, and
// lets callers watch them: live, or replayed from the id of the last event
// they saw. Events of a private repository only reach callers that may read
// it, checked again for every event.
//
// Event ids are opaque to clients: "<boot>-<seq>", where boot identifies this
// run of the server, so that an id from before a restart is unknown rather
// than taken for a newer event.
//
// The goroutine that feeds the buffer uses the logger of the context given to
// NewEvents and lives until Close.
type Events struct {
	repos *Repos
	boot  string
	size  int

	stop context.CancelFunc
	done chan struct{}

	mu      sync.Mutex
	ring    []bufferedEvent // capacity size, oldest first
	nextSeq uint64          // sequence number of the next event, from 1
	changed chan struct{}   // closed and replaced on every append and on close
	closed  bool
	watches int
}

// NewEvents subscribes to the bus and starts remembering its events, at most
// size of them (DefaultEventBuffer when not positive). ctx carries the
// logger; its cancellation is not followed, call Close.
func NewEvents(ctx context.Context, repos *Repos, bus ports.EventBus, size int, clock ports.Clock) *Events {
	if size <= 0 {
		size = DefaultEventBuffer
	}
	run, stop := context.WithCancel(context.WithoutCancel(ctx))
	e := &Events{
		repos:   repos,
		boot:    strconv.FormatInt(clock.Now().UnixMilli(), 36),
		size:    size,
		stop:    stop,
		done:    make(chan struct{}),
		nextSeq: 1,
		changed: make(chan struct{}),
	}
	sub := bus.Subscribe(run) // before returning: nothing published after is missed
	go e.feed(run, sub)
	return e
}

// feed appends the events of the bus until it stops.
func (e *Events) feed(ctx context.Context, sub <-chan domain.Event) {
	defer close(e.done)
	defer e.shutdown()
	for {
		select {
		case ev, ok := <-sub:
			if !ok {
				return
			}
			e.append(ev)
		case <-ctx.Done():
			return
		}
	}
}

func (e *Events) append(ev domain.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.ring) == e.size {
		copy(e.ring, e.ring[1:])
		e.ring = e.ring[:e.size-1]
	}
	e.ring = append(e.ring, bufferedEvent{seq: e.nextSeq, ev: ev})
	e.nextSeq++
	e.wake()
}

// wake releases the watchers waiting for news. The caller holds mu.
func (e *Events) wake() {
	close(e.changed)
	e.changed = make(chan struct{})
}

func (e *Events) shutdown() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.closed {
		e.closed = true
		e.wake()
	}
}

// Close ends every watch cleanly, stops feeding and waits for the feeder.
// It is safe to call more than once.
func (e *Events) Close(context.Context) {
	e.stop()
	<-e.done
}

// Watchers returns the number of watches in progress.
func (e *Events) Watchers() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.watches
}

func (e *Events) id(seq uint64) string { return e.boot + "-" + strconv.FormatUint(seq, 10) }

// parseID returns the sequence number of an id of this run.
func (e *Events) parseID(id string) (uint64, bool) {
	boot, seq, ok := strings.Cut(id, "-")
	if !ok || boot != e.boot {
		return 0, false
	}
	n, err := strconv.ParseUint(seq, 10, 64)
	return n, err == nil && n > 0
}

// WatchParams select the events of a watch.
type WatchParams struct {
	// Topics are TopicHome or "repo:<name>"; empty means TopicHome.
	Topics []string
	// ResumeAfter is the id of the last event the caller saw; empty to
	// receive only new events.
	ResumeAfter string
	// Ready, when not nil, is called once the topics are valid and the watch
	// is registered: events published from then on are not missed. Nothing is
	// sent before it returns.
	Ready func()
}

// WatchEvent is one item of a watch.
type WatchEvent struct {
	// ID identifies the event, for resuming. Empty on a gap.
	ID string
	// Gap tells that events were lost: the caller must reload what it shows.
	// Event is nil then.
	Gap bool
	// Event is a domain.RepoCreated, RepoUpdated, RepoDeleted or PushEvent.
	Event domain.Event
	// Repo is the state of the repository at delivery, for RepoCreated and
	// RepoUpdated.
	Repo *RepoView
}

// watch is the state of one Watch call.
type watch struct {
	p     domain.Principal
	home  bool
	repos map[domain.RepoName]bool
}

func (w *watch) wants(name domain.RepoName) bool { return w.home || w.repos[name] }

// parseTopics validates the topics of a watch. A repository topic needs a
// repository the principal may read (else domain.ErrNotFound); an unknown
// topic is domain.ErrInvalid.
func (e *Events) parseTopics(ctx context.Context, p domain.Principal, topics []string) (*watch, error) {
	w := &watch{p: p, repos: make(map[domain.RepoName]bool)}
	if len(topics) == 0 {
		w.home = true
	}
	for _, t := range topics {
		if t == TopicHome {
			w.home = true
			continue
		}
		raw, ok := strings.CutPrefix(t, topicRepoPrefix)
		if !ok {
			return nil, fmt.Errorf("%w: unknown topic %q (want %q or \"repo:<name>\")", domain.ErrInvalid, t, TopicHome)
		}
		name, err := domain.ParseRepoName(raw)
		if err != nil {
			return nil, err
		}
		if _, err := e.repos.Get(ctx, p, name); err != nil {
			return nil, err
		}
		w.repos[name] = true
	}
	return w, nil
}

// Watch calls send for the events the principal may see until ctx ends, the
// buffer closes (both return nil) or send fails (its error is returned).
//
// Without ResumeAfter only new events are sent. With it, the buffered events
// after that id are sent first, then new ones. When the id is no longer
// buffered, or was not issued by this run, a gap comes first and the oldest
// buffered event follows. A watcher that falls more than the buffer behind
// gets a gap too, and carries on with the oldest buffered event.
func (e *Events) Watch(ctx context.Context, p domain.Principal, params WatchParams, send func(WatchEvent) error) error {
	w, err := e.parseTopics(ctx, p, params.Topics)
	if err != nil {
		return err
	}

	e.mu.Lock()
	next := e.nextSeq // only new events
	gap := false
	if params.ResumeAfter != "" {
		seq, ok := e.parseID(params.ResumeAfter)
		oldest := e.nextSeq - uint64(len(e.ring)) // first buffered sequence, nextSeq when empty
		switch {
		case ok && seq >= oldest-1 && seq < e.nextSeq:
			next = seq + 1
		default:
			next, gap = oldest, true
		}
	}
	e.watches++
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.watches--
		e.mu.Unlock()
	}()

	if params.Ready != nil {
		params.Ready()
	}
	log := zerowrap.FromCtx(ctx)
	log.Debug().Strs("topics", params.Topics).Bool("resume", params.ResumeAfter != "").Bool("gap", gap).Msg("watch started")
	if gap {
		if err := send(WatchEvent{Gap: true}); err != nil {
			return err
		}
	}
	for {
		batch, changed, oldest, lost, closed := e.since(next)
		if lost {
			if err := send(WatchEvent{Gap: true}); err != nil {
				return err
			}
			next = oldest
			continue
		}
		for _, be := range batch {
			if err := e.deliver(ctx, w, be, send); err != nil {
				return err
			}
			next = be.seq + 1
		}
		if len(batch) > 0 {
			continue
		}
		if closed {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil
		}
	}
}

// since returns the buffered events from sequence next on, and the channel
// that closes when more arrive. lost is true when next fell out of the buffer;
// the caller then restarts from oldest, the first buffered sequence. The
// returned slice is a copy.
func (e *Events) since(next uint64) (batch []bufferedEvent, changed <-chan struct{}, oldest uint64, lost, closed bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	oldest = e.nextSeq - uint64(len(e.ring))
	if next < oldest {
		return nil, e.changed, oldest, true, e.closed
	}
	batch = append(batch, e.ring[next-oldest:]...)
	return batch, e.changed, oldest, false, e.closed
}

// deliver sends the event if the watcher follows its topic and may see it.
func (e *Events) deliver(ctx context.Context, w *watch, be bufferedEvent, send func(WatchEvent) error) error {
	out := WatchEvent{ID: e.id(be.seq), Event: be.ev}
	switch ev := be.ev.(type) {
	case domain.EventGap:
		// The feeder lost events: tell every watcher.
		return send(WatchEvent{Gap: true})
	case domain.RepoDeleted:
		// The repository is gone: use the visibility it had.
		if !w.wants(ev.Repo) || !canReadAs(w.p, ev.Repo, ev.Visibility) {
			return nil
		}
		return send(out)
	case domain.RepoCreated:
		return e.deliverRepo(ctx, w, ev.Repo, true, out, send)
	case domain.RepoUpdated:
		return e.deliverRepo(ctx, w, ev.Repo, true, out, send)
	case domain.PushEvent:
		return e.deliverRepo(ctx, w, ev.Repo, false, out, send)
	}
	return nil // an event this version does not stream
}

// deliverRepo re-checks the access to the repository, with its present
// visibility, and sends the event. A repository that no longer exists is
// skipped: its RepoDeleted follows or came before.
func (e *Events) deliverRepo(ctx context.Context, w *watch, name domain.RepoName, withRepo bool, out WatchEvent, send func(WatchEvent) error) error {
	if !w.wants(name) {
		return nil
	}
	repo, err := e.repos.Get(ctx, w.p, name)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return nil
	case ctx.Err() != nil:
		return nil
	case err != nil:
		return fmt.Errorf("check access to %q: %w", name, err)
	}
	if withRepo {
		view, err := e.repos.View(ctx, repo)
		if err != nil {
			return err
		}
		out.Repo = &view
	}
	return send(out)
}
