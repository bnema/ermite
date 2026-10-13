// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// liveStream turns the events of Watch into the fragments one page shows.
type liveStream struct {
	s    *Server
	repo string // "" on the home page
	view string // "summary", "refs" or "commits"
	ref  string // commits: the branch listed
	head string // commits: the commit listed first
	n    string // mr: the number of the merge request shown; run: the run id
	job  string // job: the id of the job shown
}

// renderFragment renders a component to the data of an event.
func renderFragment(ctx context.Context, event string, c templ.Component) (fragment, error) {
	var b bytes.Buffer
	if err := c.Render(ctx, &b); err != nil {
		return fragment{}, err
	}
	return fragment{event: event, html: b.String()}, nil
}

var gap = []fragment{{event: "gap", html: "gap"}}

// liveEvent reports whether the event of a watch is one the pages follow: a
// gap, or one of the events of the forge. The use case may send others, which
// no page shows and no stream carries, not even their id.
func liveEvent(we ports.WatchEvent) bool {
	if we.Gap {
		return true
	}
	switch we.Event.(type) {
	case domain.RepoCreated, domain.RepoUpdated, domain.RepoDeleted, domain.PushEvent,
		domain.MergeRequestOpened, domain.MergeRequestMerged, domain.MergeRequestClosed, domain.CommentAdded,
		domain.RunUpdated, domain.JobUpdated:
		return true
	}
	return false
}

// render returns the fragments for an event, none when it changes nothing on
// the page.
func (l *liveStream) render(ctx context.Context, we ports.WatchEvent) ([]fragment, error) {
	if we.Gap {
		return gap, nil
	}
	if we.Event == nil {
		return nil, nil
	}
	if l.repo == "" {
		return l.home(ctx, we)
	}
	return l.repoPage(ctx, we)
}

// repoByName reads a repository as the caller of the request. A name that is
// not valid is a repository that is not there.
func (l *liveStream) repoByName(ctx context.Context, rawName string) (ports.RepoView, error) {
	name, err := domain.ParseRepoName(rawName)
	if err != nil {
		return ports.RepoView{}, domain.ErrNotFound
	}
	return l.s.repoView(ctx, name)
}

// home upserts the row of the repository an event is about. The row is read
// again from the use case as the principal of the stream, so a repository
// that was deleted or that the visitor can no longer see (public to private)
// removes the row, and the stream never carries more than the list shows.
func (l *liveStream) home(ctx context.Context, we ports.WatchEvent) ([]fragment, error) {
	var name string
	fresh := false
	switch ev := we.Event.(type) {
	case domain.RepoDeleted:
		return l.gone(ctx, ev.Repo.String())
	case domain.RepoCreated:
		name = ev.Repo.String()
	case domain.RepoUpdated:
		name = ev.Repo.String()
	case domain.PushEvent:
		name, fresh = ev.Repo.String(), true
	default:
		return nil, nil
	}
	view, err := l.repoByName(ctx, name)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return l.gone(ctx, name)
	case err != nil:
		return nil, err
	}
	row := homeRow(view, l.s.cfg.Now(), false)
	row.Fresh = fresh
	f, err := renderFragment(ctx, "home-list", HomeRowView(row))
	return []fragment{f}, err
}

func (l *liveStream) gone(ctx context.Context, name string) ([]fragment, error) {
	f, err := renderFragment(ctx, "home-list", RepoGone("repo-"+name))
	return []fragment{f}, err
}

// repoPage handles the events of the topic of one repository.
func (l *liveStream) repoPage(ctx context.Context, we ports.WatchEvent) ([]fragment, error) {
	switch ev := we.Event.(type) {
	case domain.RunUpdated:
		return l.runEvent(ctx, ev)
	case domain.JobUpdated:
		return l.jobEvent(ctx, ev)
	case domain.RepoDeleted, domain.RepoCreated:
		// Gone, or a new repository under the same name: whatever the page
		// shows is not true any more.
		return gap, nil
	case domain.RepoUpdated:
		_, err := l.repoByName(ctx, l.repo)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			return gap, nil // no longer visible
		case err != nil:
			return nil, err
		case l.view == "summary", l.view == "refs":
			return gap, nil // the description, the default branch
		}
		return nil, nil
	case domain.MergeRequestOpened:
		return l.mrNotice(ctx, ev.MergeRequest.Number, "a merge request was opened — reload")
	case domain.MergeRequestMerged:
		return l.mrNotice(ctx, ev.MergeRequest.Number, "this merge request was merged — reload")
	case domain.MergeRequestClosed:
		return l.mrNotice(ctx, ev.MergeRequest.Number, "this merge request was closed — reload")
	case domain.CommentAdded:
		return l.mrNotice(ctx, ev.Number, "a comment was added — reload")
	case domain.PushEvent:
		switch l.view {
		case "mr":
			return l.mrPushed(ctx, ev)
		case "mrs":
			return nil, nil
		}
		switch l.view {
		case "refs":
			return l.refs(ctx, we.ID)
		case "commits":
			return l.banner(ctx, ev)
		}
		return l.lastPush(ctx)
	}
	return nil, nil
}

// lastPush is the "last push" fact of the summary.
func (l *liveStream) lastPush(ctx context.Context) ([]fragment, error) {
	view, err := l.repoByName(ctx, l.repo)
	if err != nil {
		return nil, err
	}
	f, err := renderFragment(ctx, "repo-last-push", FactValue(lastPushFact(view, l.s.cfg.Now())))
	return []fragment{f}, err
}

// refs is the table of the refs page and the facts of its rail. The fragments
// of one event are built once for all the watchers of the repository that
// see the same things (see fanout); eventID names the event.
func (l *liveStream) refs(ctx context.Context, eventID string) ([]fragment, error) {
	key := fanKey{repo: l.repo, event: eventID, owner: SignedIn(ctx)}
	return l.s.refsFan.do(ctx, key, func() ([]fragment, error) {
		d, err := l.s.loadRepo(ctx, l.repo)
		if err != nil {
			return nil, err
		}
		p := l.s.refsPage(ctx, d)
		body, err := renderFragment(ctx, "refs-list", RefsBody(p))
		if err != nil {
			return nil, err
		}
		kv, err := renderFragment(ctx, "refs-list", RefsKV(p))
		return []fragment{body, kv}, err
	})
}

const (
	// fanTTL is how long the fragments of an event are kept for the watchers
	// that are told of it a little later than the first.
	fanTTL = 2 * time.Second
	// fanMax bounds the events kept; past it an event is built for each
	// watcher, as without sharing.
	fanMax = 64
)

// fanKey says which watchers may share the fragments of an event: those of
// the same repository, for the same event, and of the same class. The class
// is anonymous or owner (a session is the owner's alone, ADR 0018): what is
// built as the owner can show what an anonymous visitor may not see, so the
// two never share an entry.
type fanKey struct {
	repo, event string
	owner       bool
}

type fanEntry struct {
	done  chan struct{} // closed once frags or err is set
	frags []fragment
	err   error
	at    time.Time // when it was built; zero while it is
}

// fanout builds the fragments of an event once for the many watchers that
// receive it: a push to a repository that 256 pages watch would otherwise
// load the refs and the commits they designate 256 times. The zero value is
// ready. A failure is never kept: the next watcher builds it again.
type fanout struct {
	mu sync.Mutex
	m  map[fanKey]*fanEntry
}

// do returns the fragments of key, built by build when nobody has or is
// building them. build runs with the context of the watcher that calls it,
// which is of the class of the key; a watcher whose builder failed (its
// request was cancelled, say) does not fail the others, they build again.
func (f *fanout) do(ctx context.Context, key fanKey, build func() ([]fragment, error)) ([]fragment, error) {
	if key.event == "" {
		return build() // no event to share by
	}
	for range 3 {
		f.mu.Lock()
		now := time.Now()
		for k, e := range f.m {
			if !e.at.IsZero() && now.Sub(e.at) > fanTTL {
				delete(f.m, k)
			}
		}
		e, shared := f.m[key]
		if !shared {
			if len(f.m) >= fanMax {
				f.mu.Unlock()
				return build()
			}
			if f.m == nil {
				f.m = make(map[fanKey]*fanEntry)
			}
			e = &fanEntry{done: make(chan struct{})}
			f.m[key] = e
		}
		f.mu.Unlock()

		if !shared {
			return f.lead(key, e, build)
		}
		select {
		case <-e.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if e.err == nil {
			return e.frags, nil
		}
	}
	return build()
}

// lead builds the entry that the caller registered.
func (f *fanout) lead(key fanKey, e *fanEntry, build func() ([]fragment, error)) (frags []fragment, err error) {
	err = errors.New("web: fragments were not built") // a panic of build
	defer func() {
		f.mu.Lock()
		e.frags, e.err, e.at = frags, err, time.Now()
		if err != nil && f.m[key] == e {
			delete(f.m, key)
		}
		f.mu.Unlock()
		close(e.done)
	}()
	return build()
}

// banner tells the commits page how many commits its branch got since the
// head it lists.
//
//hexcheck:ignore suspicious-business-logic-in-adapter counts the commits of a page for the banner text; display only
func (l *liveStream) banner(ctx context.Context, p domain.PushEvent) ([]fragment, error) {
	if l.ref == "" || !isFullID(l.head) {
		return nil, nil
	}
	tip, touched := "", false
	for _, u := range p.Updates {
		if u.Ref == "refs/heads/"+l.ref {
			tip, touched = u.New, true
			if u.IsDelete() {
				tip = ""
			}
		}
	}
	switch {
	case !touched:
		return nil, nil
	case tip == "":
		return gap, nil // the branch was deleted
	}
	text := ""
	if tip != l.head {
		name, err := domain.ParseRepoName(l.repo)
		if err != nil {
			return nil, err
		}
		res, err := l.s.commitList(ctx, name, tip, "", pageSize, "")
		if err != nil {
			return nil, err
		}
		// The count is approximate for a history with merges: the log is
		// ordered by date, not as "head..tip", so commits that are not new may
		// be counted before the head is met. And one page of the log being
		// all that is read, "100+" cannot be told from a rewritten branch
		// whose old head is further back: the use cases have no "is ancestor".
		n, found := 0, false
		for _, c := range res.Commits {
			if c.ID.String() == l.head {
				found = true
				break
			}
			n++
		}
		switch {
		case found:
			text = newCommits(strconv.Itoa(n), n)
		case res.NextPageToken != "":
			text = newCommits(strconv.Itoa(n)+"+", 2)
		default:
			text = "the branch was rewritten — reload"
		}
	}
	c := LiveSlot("commits-head")
	if text != "" {
		c = CommitsBanner(text, repoHref(l.repo, "commits", l.ref))
	}
	f, err := renderFragment(ctx, "commits-head", c)
	return []fragment{f}, err
}

// newCommits is the text of the banner: "1 new commit — reload".
func newCommits(count string, n int) string {
	if n == 1 {
		return count + " new commit — reload"
	}
	return count + " new commits — reload"
}

// isFullID reports whether s is a full commit id.
func isFullID(s string) bool { return len(s) == 40 && isCommitID(s) }

// mrNotice tells a merge request page that something happened to the request
// it shows, or the list that its content changed. Only a fixed sentence is sent:
// the text of a comment or a title never leaves through the stream.
func (l *liveStream) mrNotice(ctx context.Context, number int64, text string) ([]fragment, error) {
	switch l.view {
	case "mrs":
		text = "merge requests changed — reload"
		return l.notice(ctx, text, mrBase(l.repo))
	case "mr":
		if strconv.FormatInt(number, 10) != l.n {
			return nil, nil
		}
		return l.notice(ctx, text, mrHref(l.repo, number))
	}
	return nil, nil
}

// mrPushed announces a push to the source branch of the merge request shown.
func (l *liveStream) mrPushed(ctx context.Context, p domain.PushEvent) ([]fragment, error) {
	if l.ref == "" {
		return nil, nil
	}
	for _, u := range p.Updates {
		if u.Ref == "refs/heads/"+l.ref {
			n, err := strconv.ParseInt(l.n, 10, 64)
			if err != nil {
				return nil, nil
			}
			return l.notice(ctx, "the branch changed — reload", mrHref(l.repo, n))
		}
	}
	return nil, nil
}

func (l *liveStream) notice(ctx context.Context, text, href string) ([]fragment, error) {
	f, err := renderFragment(ctx, "mr-notice", MRNotice(text, href))
	return []fragment{f}, err
}

// runEvent updates the pages of CI that a RunUpdated changes: the row of the
// list of runs, or the body of the run page shown. Only a state is sent: the
// events carry no log and no text of the workflow.
func (l *liveStream) runEvent(ctx context.Context, ev domain.RunUpdated) ([]fragment, error) {
	if l.s.cfg.Runs == nil {
		return nil, nil
	}
	switch l.view {
	case "actions":
		f, err := renderFragment(ctx, "ci-run", RunListRow(l.s.runRow(l.repo, ev.Run, l.s.cfg.Now())))
		if err != nil {
			return nil, err
		}
		if ev.Run.Status == domain.CIQueued {
			f.html = strings.Replace(f.html, "<tr id=", "<tr data-new id=", 1)
		}
		return []fragment{f}, nil
	case "run":
		if strconv.FormatInt(ev.Run.ID, 10) != l.n {
			return nil, nil
		}
		return l.runBody(ctx)
	}
	return nil, nil
}

// jobEvent updates the body of the run page and the steps of the job page.
func (l *liveStream) jobEvent(ctx context.Context, ev domain.JobUpdated) ([]fragment, error) {
	if l.s.cfg.Runs == nil {
		return nil, nil
	}
	switch l.view {
	case "run":
		if strconv.FormatInt(ev.Job.RunID, 10) != l.n {
			return nil, nil
		}
		return l.runBody(ctx)
	case "job":
		if ev.Job.ID != l.job {
			return nil, nil
		}
		return l.jobState(ctx)
	}
	return nil, nil
}

// runBody renders again the jobs of the run page.
func (l *liveStream) runBody(ctx context.Context) ([]fragment, error) {
	id, err := strconv.ParseInt(l.n, 10, 64)
	if err != nil {
		return nil, nil
	}
	d, err := l.s.loadRepo(ctx, l.repo)
	if err != nil {
		return nil, err
	}
	p, err := l.s.runPage(ctx, d, id, 0)
	if err != nil {
		return nil, err
	}
	body, err := renderFragment(ctx, "ci-run", runBodyFragment(p))
	if err != nil {
		return nil, err
	}
	// The buttons depend on the state: the page fetches itself again when the
	// run completed, so that the owner gets the right ones.
	if !p.Active {
		return []fragment{body, {event: "gap", html: "gap"}}, nil
	}
	return []fragment{body}, nil
}

// jobState tells the job page that its state or its steps changed: the page
// reloads the part of the log after the last line it holds (ermite.js).
func (l *liveStream) jobState(ctx context.Context) ([]fragment, error) {
	d, err := l.s.loadRepo(ctx, l.repo)
	if err != nil {
		return nil, err
	}
	view, err := l.s.cfg.Runs.GetJob(ctx, principal(ctx), d.repo.Name, l.job, 0)
	if err != nil {
		return nil, err
	}
	now := l.s.cfg.Now()
	row := l.s.jobRow(d.name(), view, now)
	f, err := renderFragment(ctx, "ci-job", JobHead(row, view.Lines))
	if err != nil {
		return nil, err
	}
	steps, err := renderFragment(ctx, "ci-job", JobSteps(stepViews(view, now)))
	if err != nil {
		return nil, err
	}
	run, err := l.s.cfg.Runs.GetRun(ctx, principal(ctx), d.repo.Name, view.RunID, 0)
	if err != nil {
		return nil, err
	}
	anns, more, err := l.s.jobAnnotations(ctx, d, view.RunID, run.Run.Sha, view, 0)
	if err != nil {
		return nil, err
	}
	notes, err := renderFragment(ctx, "ci-job", JobAnnotations(anns, more))
	if err != nil {
		return nil, err
	}
	return []fragment{f, steps, notes}, nil
}
