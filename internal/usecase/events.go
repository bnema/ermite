// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// DefaultEventBuffer is the number of events the Events use case remembers.
const DefaultEventBuffer = 1024

// Limits of the watchers.
const (
	// MaxWatchTopics is the most topics one watch may follow.
	MaxWatchTopics = 64
	// MaxWatchers is the most watches in progress at once.
	MaxWatchers = 256
	// MaxWatchersPerClient is the most watches in progress for one client
	// address (see ports.WatchParams.Client).
	MaxWatchersPerClient = 16
	// DefaultRevalidateEvery is how long a watcher with a stored token goes
	// without asking the token store whether the token still exists.
	DefaultRevalidateEvery = 5 * time.Second
)

// Topics a watcher may follow.
const (
	// TopicHome follows every repository the caller may see.
	TopicHome = "home"
	// TopicOwner follows the merge requests and issues of every repository,
	// for the owner only (the inbox). Anyone else is told it is an unknown
	// topic.
	TopicOwner = "owner"
	// topicRepoPrefix starts the topic of one repository: "repo:<name>".
	topicRepoPrefix = "repo:"
)

// bufferedEvent is an event with its sequence number in the buffer, and what
// was true of its repository when it was buffered. Access to an event is
// decided from that, and from the repository as it is when the event is
// delivered: a repository made public later does not reveal what happened
// while it was private, and a repository deleted and created again under the
// same name does not inherit the events of the old one.
type bufferedEvent struct {
	seq uint64
	ev  domain.Event

	// known is false when the repository did not exist any more when the
	// event was buffered: nobody is told about it.
	known bool
	// vis is the visibility of the repository at that time.
	vis domain.Visibility
	// created is the creation time of that repository.
	created time.Time
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
	auth  ports.Revalidator
	clock ports.Clock
	boot  string
	size  int
	every time.Duration // see EventsConfig.RevalidateEvery

	stop context.CancelFunc
	done chan struct{}

	mu      sync.Mutex
	ring    []bufferedEvent // capacity size, oldest first
	nextSeq uint64          // sequence number of the next event, from 1
	changed chan struct{}   // closed and replaced on every append and on close
	closed  bool
	watches int
	clients map[string]int // watches in progress by client
}

var _ ports.EventService = (*Events)(nil)

// EventsConfig configures Events.
type EventsConfig struct {
	// Size is the number of events remembered; DefaultEventBuffer when not
	// positive.
	Size int
	// Auth confirms that the token of a watcher still exists. Required.
	Auth ports.Revalidator
	// RevalidateEvery is how long the answer of Auth is kept for a watcher;
	// DefaultRevalidateEvery when zero, and negative to ask for every event.
	RevalidateEvery time.Duration
}

// NewEvents subscribes to the bus and starts remembering its events. ctx
// carries the logger; its cancellation is not followed, call Close.
func NewEvents(ctx context.Context, repos *Repos, bus ports.EventBus, clock ports.Clock, cfg EventsConfig) *Events {
	size := cfg.Size
	if size <= 0 {
		size = DefaultEventBuffer
	}
	every := cfg.RevalidateEvery
	if every == 0 {
		every = DefaultRevalidateEvery
	}
	run, stop := context.WithCancel(context.WithoutCancel(ctx))
	e := &Events{
		repos:   repos,
		auth:    cfg.Auth,
		clock:   clock,
		every:   every,
		clients: make(map[string]int),
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
			e.append(ctx, ev)
		case <-ctx.Done():
			return
		}
	}
}

// snapshot records the visibility and the generation of the repository an
// event is about. Every event of the repository carries them.
func (e *Events) snapshot(ctx context.Context, ev domain.Event) bufferedEvent {
	switch ev := ev.(type) {
	case domain.RepoCreated:
		return bufferedEvent{known: true, vis: ev.Visibility, created: ev.At}
	case domain.RepoUpdated:
		return bufferedEvent{known: true, vis: ev.Visibility, created: ev.CreatedAt}
	case domain.RepoDeleted:
		return bufferedEvent{known: true, vis: ev.Visibility, created: ev.CreatedAt}
	case domain.PushEvent:
		if ev.CreatedAt.IsZero() {
			return bufferedEvent{} // repository unknown at publish time: nobody is told
		}
		return bufferedEvent{known: true, vis: ev.Visibility, created: ev.CreatedAt}
	case domain.MergeRequestOpened:
		return repoSnapshot(ev.Visibility, ev.CreatedAt)
	case domain.MergeRequestMerged:
		return repoSnapshot(ev.Visibility, ev.CreatedAt)
	case domain.MergeRequestClosed:
		return repoSnapshot(ev.Visibility, ev.CreatedAt)
	case domain.MergeRequestReady:
		return repoSnapshot(ev.Visibility, ev.CreatedAt)
	case domain.CommentAdded:
		return repoSnapshot(ev.Visibility, ev.CreatedAt)
	case domain.IssueOpened:
		return repoSnapshot(ev.Visibility, ev.CreatedAt)
	case domain.IssueClosed:
		return repoSnapshot(ev.Visibility, ev.CreatedAt)
	case domain.IssueReopened:
		return repoSnapshot(ev.Visibility, ev.CreatedAt)
	case domain.IssueCommented:
		return repoSnapshot(ev.Visibility, ev.CreatedAt)
	case domain.IssueSubmitted, domain.IssuePendingRemoved:
		return bufferedEvent{known: true} // owner topic only: no repository snapshot
	case domain.CommentEdited:
		return repoSnapshot(ev.Visibility, ev.CreatedAt)
	case domain.CommentDeleted:
		return repoSnapshot(ev.Visibility, ev.CreatedAt)
	case domain.CommentResolved:
		return repoSnapshot(ev.Visibility, ev.CreatedAt)
	case domain.MergeRequestMergeability:
		return repoSnapshot(ev.Visibility, ev.CreatedAt)
	case domain.RunUpdated:
		return repoSnapshot(ev.Visibility, ev.CreatedAt)
	case domain.JobUpdated:
		return repoSnapshot(ev.Visibility, ev.CreatedAt)
	}
	return bufferedEvent{known: true}
}

// repoSnapshot is the snapshot of an event that carries the visibility and the
// creation time of its repository; a zero creation time means the repository
// was unknown when it was published, and nobody is told.
func repoSnapshot(vis domain.Visibility, created time.Time) bufferedEvent {
	if created.IsZero() {
		return bufferedEvent{}
	}
	return bufferedEvent{known: true, vis: vis, created: created}
}

func (e *Events) append(ctx context.Context, ev domain.Event) {
	be := e.snapshot(ctx, ev) // outside the lock: it reads the store
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.ring) == e.size {
		copy(e.ring, e.ring[1:])
		e.ring = e.ring[:e.size-1]
	}
	be.seq, be.ev = e.nextSeq, ev
	e.ring = append(e.ring, be)
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

// Close ends every watch cleanly, stops feeding and waits for the feeder,
// until ctx ends. It is safe to call more than once.
func (e *Events) Close(ctx context.Context) {
	e.stop()
	select {
	case <-e.done:
	case <-ctx.Done():
	}
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

// watch is the state of one Watch call.
type watch struct {
	p     domain.Principal
	home  bool
	owner bool // the owner topic: merge request, issue, repo update and deletion events of every repository
	repos map[domain.RepoName]bool

	checked time.Time // when the token store last confirmed the principal
}

func (w *watch) wants(name domain.RepoName) bool { return w.home || w.repos[name] }

// wantsEvent is wants for an event: the owner topic also follows the events
// about merge requests and issues of every repository.
func (w *watch) wantsEvent(name domain.RepoName, ev domain.Event) bool {
	return w.wants(name) || (w.owner && (isItemEvent(ev) || isRepoLifecycleEvent(ev)))
}

// parseTopics validates the topics of a watch. A repository topic needs a
// repository the principal may read (else domain.ErrNotFound); an unknown
// topic, or more than MaxWatchTopics, is domain.ErrInvalid.
func (e *Events) parseTopics(ctx context.Context, p domain.Principal, topics []string) (*watch, error) {
	if len(topics) > MaxWatchTopics {
		return nil, fmt.Errorf("%w: %d topics, at most %d", domain.ErrInvalid, len(topics), MaxWatchTopics)
	}
	w := &watch{p: p, repos: make(map[domain.RepoName]bool), checked: time.Now()}
	if len(topics) == 0 {
		w.home = true
	}
	for _, t := range topics {
		if t == TopicHome {
			w.home = true
			continue
		}
		if t == TopicOwner && p.HasOwnerRights() {
			w.owner = true
			continue
		}
		raw, ok := strings.CutPrefix(t, topicRepoPrefix)
		if !ok {
			// Includes "owner" for anyone but the owner: the same error as
			// any unknown topic.
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
// A principal with a stored token is checked again for every event, and every
// few seconds when idle: when the token expired or was revoked the watch ends
// with domain.ErrUnauthenticated and nothing more is sent. More watches than
// MaxWatchers, or than MaxWatchersPerClient for one Client, give
// domain.ErrResourceExhausted.
//
// Without ResumeAfter only new events are sent. With it, the buffered events
// after that id are sent first, then new ones. When the id is no longer
// buffered, or was not issued by this run, a gap comes first and the oldest
// buffered event follows. A watcher that falls more than the buffer behind
// gets a gap too, and carries on with the oldest buffered event.
func (e *Events) Watch(ctx context.Context, p domain.Principal, params ports.WatchParams, send func(ports.WatchEvent) error) error {
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
	if e.watches >= MaxWatchers || (params.Client != "" && e.clients[params.Client] >= MaxWatchersPerClient) {
		e.mu.Unlock()
		return fmt.Errorf("%w: too many watchers", domain.ErrResourceExhausted)
	}
	e.watches++
	if params.Client != "" {
		e.clients[params.Client]++
	}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.watches--
		if params.Client != "" {
			if e.clients[params.Client]--; e.clients[params.Client] == 0 {
				delete(e.clients, params.Client)
			}
		}
		e.mu.Unlock()
	}()

	if params.Ready != nil {
		params.Ready()
	}
	log := zerowrap.FromCtx(ctx)
	log.Debug().Strs("topics", params.Topics).Bool("resume", params.ResumeAfter != "").Bool("gap", gap).Msg("watch started")
	if gap {
		if err := send(ports.WatchEvent{Gap: true}); err != nil {
			return err
		}
	}
	for {
		if err := e.stillAllowed(ctx, w); err != nil {
			return err
		}
		batch, changed, oldest, lost, closed := e.since(next)
		if lost {
			if err := send(ports.WatchEvent{Gap: true}); err != nil {
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
		e.wait(ctx, w, changed)
		if ctx.Err() != nil {
			return nil
		}
	}
}

// wait blocks until more events arrive or ctx ends. A watcher with a stored
// token also wakes up every few seconds, so that a revocation ends the watch
// of a quiet forge too (the caller checks the token on the next turn).
func (e *Events) wait(ctx context.Context, w *watch, changed <-chan struct{}) {
	var idle <-chan time.Time
	if w.p.TokenID() != "" {
		t := time.NewTimer(e.idleCheck())
		defer t.Stop()
		idle = t.C
	}
	select {
	case <-changed:
	case <-idle:
	case <-ctx.Done():
	}
}

// idleCheck is how long a watcher with a stored token waits for news before
// checking its token anyway.
func (e *Events) idleCheck() time.Duration {
	if e.every <= 0 {
		return DefaultRevalidateEvery
	}
	return min(e.every, DefaultRevalidateEvery)
}

// stillAllowed checks that the holder of a stored token may still watch: the
// token has not expired and, at most every few seconds, still exists. Root and
// anonymous callers have nothing to lose. The owner topic needs nothing more:
// a stored token with owner rights loses them only by ceasing to exist, which
// is checked here, and the owner's web session, which this use case cannot see,
// is checked by the web adapter before it sends anything.
func (e *Events) stillAllowed(ctx context.Context, w *watch) error {
	if w.p.TokenID() == "" {
		return nil
	}
	if exp := w.p.ExpiresAt(); !exp.IsZero() && !e.clock.Now().Before(exp) {
		return domain.ErrUnauthenticated
	}
	if e.every > 0 && time.Since(w.checked) < e.every {
		return nil
	}
	if err := e.auth.Revalidate(ctx, w.p); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	w.checked = time.Now()
	return nil
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
func (e *Events) deliver(ctx context.Context, w *watch, be bufferedEvent, send func(ports.WatchEvent) error) error {
	if err := e.stillAllowed(ctx, w); err != nil {
		return err
	}
	out := ports.WatchEvent{ID: e.id(be.seq), Event: be.ev}
	switch ev := be.ev.(type) {
	case domain.EventGap:
		// The feeder lost events: tell every watcher.
		return send(ports.WatchEvent{Gap: true})
	case domain.RepoDeleted:
		// The repository is gone: use the visibility it had.
		if !w.wantsEvent(ev.Repo, ev) || !canReadAs(w.p, ev.Repo, ev.Visibility) {
			return nil
		}
		return send(out)
	case domain.IssueSubmitted:
		return e.deliverOwnerOnly(ctx, w, ev.Repo, out, send)
	case domain.IssuePendingRemoved:
		return e.deliverOwnerOnly(ctx, w, ev.Repo, out, send)
	case domain.RepoCreated:
		return e.deliverRepo(ctx, w, ev.Repo, true, be, out, send)
	case domain.RepoUpdated:
		return e.deliverRepo(ctx, w, ev.Repo, true, be, out, send)
	case domain.PushEvent:
		return e.deliverRepo(ctx, w, ev.Repo, false, be, out, send)
	case domain.MergeRequestOpened:
		return e.deliverRepo(ctx, w, ev.MergeRequest.Repo, false, be, out, send)
	case domain.MergeRequestMerged:
		return e.deliverRepo(ctx, w, ev.MergeRequest.Repo, false, be, out, send)
	case domain.MergeRequestClosed:
		return e.deliverRepo(ctx, w, ev.MergeRequest.Repo, false, be, out, send)
	case domain.MergeRequestReady:
		return e.deliverRepo(ctx, w, ev.MergeRequest.Repo, false, be, out, send)
	case domain.CommentAdded:
		return e.deliverRepo(ctx, w, ev.Repo, false, be, out, send)
	case domain.IssueOpened:
		return e.deliverRepo(ctx, w, ev.Issue.Repo, false, be, out, send)
	case domain.IssueClosed:
		return e.deliverRepo(ctx, w, ev.Issue.Repo, false, be, out, send)
	case domain.IssueReopened:
		return e.deliverRepo(ctx, w, ev.Issue.Repo, false, be, out, send)
	case domain.IssueCommented:
		return e.deliverRepo(ctx, w, ev.Repo, false, be, out, send)
	case domain.CommentEdited:
		return e.deliverRepo(ctx, w, ev.Repo, false, be, out, send)
	case domain.CommentDeleted:
		return e.deliverRepo(ctx, w, ev.Repo, false, be, out, send)
	case domain.CommentResolved:
		return e.deliverRepo(ctx, w, ev.Repo, false, be, out, send)
	case domain.MergeRequestMergeability:
		return e.deliverRepo(ctx, w, ev.Repo, false, be, out, send)
	case domain.RunUpdated:
		// Home shows the CI state of each repository.
		return e.deliverRepo(ctx, w, ev.Run.Repo, false, be, out, send)
	case domain.JobUpdated:
		// Home gets a job when it ends only: a failed job turns the state to
		// failing before its run ends. Its progress stays on the topic of its
		// repository.
		if !w.repos[ev.Job.Repo] && ev.Job.Phase != domain.CIPhaseEnded {
			return nil
		}
		return e.deliverRepo(ctx, w, ev.Job.Repo, false, be, out, send)
	}
	return nil // an event this version does not stream
}

// deliverOwnerOnly sends an owner-topic event: never home, never a repository
// topic, not even to an owner who watches the repository. No content travels.
func (e *Events) deliverOwnerOnly(ctx context.Context, w *watch, name domain.RepoName, out ports.WatchEvent, send func(ports.WatchEvent) error) error {
	if !w.owner {
		return nil
	}
	repo, err := lookupRepo(ctx, e.repos.meta, name)
	switch {
	case errors.Is(err, domain.ErrNotFound), ctx.Err() != nil:
		return nil
	case err != nil:
		return fmt.Errorf("check access to %q: %w", name, err)
	}
	if repo.Settings.Issues.OrDefault() == domain.IssuesOff {
		return nil
	}
	return send(out)
}

// deliverRepo sends an event about a repository if the watcher could read the
// repository when the event happened and can read it now, and the repository
// is still the one the event is about. A repository that no longer exists is
// skipped: its RepoDeleted follows or came before.
//
// The exception is a repository that became private: the watchers who could
// read it before are told once, with its name and its new visibility only,
// so that what they show can be removed.
func (e *Events) deliverRepo(ctx context.Context, w *watch, name domain.RepoName, withRepo bool, be bufferedEvent, out ports.WatchEvent, send func(ports.WatchEvent) error) error {
	if !w.wantsEvent(name, be.ev) || !be.known {
		return nil
	}
	repo, err := lookupRepo(ctx, e.repos.meta, name)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return nil
	case ctx.Err() != nil:
		return nil
	case err != nil:
		return fmt.Errorf("check access to %q: %w", name, err)
	}
	if repo.CreatedAt.After(be.created) {
		return nil // the event is about a repository of this name that was deleted
	}
	if !canReadAs(w.p, name, be.vis) {
		if up, ok := be.ev.(domain.RepoUpdated); ok && canReadAs(w.p, name, up.Previous) {
			out.Repo = &ports.RepoView{Repo: domain.Repo{Name: name, Visibility: up.Visibility}}
			return send(out)
		}
		return nil
	}
	if !canRead(w.p, repo) {
		return nil
	}
	if isIssueEvent(be.ev) && repo.Settings.Issues.OrDefault() == domain.IssuesOff {
		return nil // issues were turned off since: a replay must not show them
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

// isIssueEvent reports whether ev is about an issue.
func isIssueEvent(ev domain.Event) bool {
	switch ev.(type) {
	case domain.IssueOpened, domain.IssueClosed, domain.IssueReopened, domain.IssueCommented:
		return true
	}
	return false
}

// isRepoLifecycleEvent reports whether ev is a repository update or deletion:
// the owner topic follows them, for they change what the inbox lists.
func isRepoLifecycleEvent(ev domain.Event) bool {
	switch ev.(type) {
	case domain.RepoUpdated, domain.RepoDeleted:
		return true
	}
	return false
}

// isItemEvent reports whether ev is about a merge request or an issue.
func isItemEvent(ev domain.Event) bool {
	switch ev.(type) {
	case domain.MergeRequestOpened, domain.MergeRequestMerged, domain.MergeRequestClosed,
		domain.MergeRequestReady, domain.MergeRequestMergeability, domain.CommentAdded,
		domain.CommentEdited, domain.CommentDeleted, domain.CommentResolved:
		return true
	}
	return isIssueEvent(ev)
}
