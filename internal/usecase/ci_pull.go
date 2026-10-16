// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-actions/workflow"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The pull_request event (CI-3, D5). A merge request is what GitHub calls a pull
// request: ermite keeps refs/pull/<n>/head at the head of its source branch and,
// while the head merges cleanly into the target, refs/pull/<n>/merge at a test
// merge commit (parents: target tip, then head; message "Merge <head> into
// <base>"). A pull_request run is for that merge commit: github.ref is the merge
// ref, github.sha the merge commit, the workflow is read from it, and its checks
// attach to the head (domain.CIRun.CheckSha).
//
// Two workers, apart from the one that plans pushes (so a push never waits for
// them):
//
//   - the event worker handles the activities one at a time, in order: a merge
//     request opened (OnPullRequest), its source branch pushed (a synchronize),
//     closed or merged. An event carries the commits it is for, fixed when it
//     happened, and the actor and trust of whoever caused it: it runs for exactly
//     those commits, with that trust, even when the branches have moved on by the
//     time it is handled (GitHub runs each synchronize). A run is trusted only
//     if the head and the base of its event were pushed by trusted principals
//     too (pullTipsTrusted, pullBaseTrusted). The merge commit of a run is computed from the head and
//     base of its event, never from the branches as they are now.
//   - the refresh worker keeps refs/pull/<n>/{head,merge} at the latest branches
//     when a source or a target branch moves; the refreshes of one merge request
//     coalesce, and the work is bounded.
//
// Whatever publishes a ref or creates a run does it through the PullGate, under
// the merge lock of the repository and only while the request is open, so a close
// or a merge that completes meanwhile discards it: a closed request gets no
// refs and no run but its "closed" one, which uses the last merge commit that was
// published, or the head, and is never computed again.
//
// A head that does not merge cleanly makes no run for that update, as on GitHub;
// the merge ref then keeps the last commit that merged, as GitHub's does.
//
// The pull_request_target event (CI-4, D4) has the same activities, payload
// and filters, but runs the workflows of the default branch's tip, as taken
// when the activity happened, conflict or not: github.ref is the default
// branch, github.sha that tip. Its checks attach to the head too. It is
// trusted when the cause of the activity is trusted and the default tip was
// pushed by a trusted principal: the head and the base do not matter, as none
// of their files run unless a workflow checks them out (GitHub's "pwn
// request"). It is stricter than GitHub, which gives the secrets to any pull
// request of the repository: an agent's activity runs it without secrets.

const (
	// DefaultCIPullRefreshes bounds the merge requests (and the pushes) waiting
	// for the refresh worker. Past it the repository is marked dirty instead and
	// the worker rescans its open merge requests when it has capacity, so that
	// nothing stays stale for good.
	DefaultCIPullRefreshes = 4096
	// DefaultCIPullSnapshot bounds the merge requests one push takes the snapshot
	// of (the open requests whose source branch it pushed). Past it the rest is
	// skipped, an error is logged, and the repository is rescanned for the refs.
	DefaultCIPullSnapshot = 256
	// Stages of the work of the merge requests, for the test hook
	// CIConfig.PullStage.
	pullStageDiscover = "discover"
	pullStageRescan   = "rescan"
	pullStageEvent    = "event"
	pullStageComputed = "computed"
	pullStageRefresh  = "refresh"
)

// errPullClosed means a merge request was closed or merged before the work for
// it could be published.
var errPullClosed = errors.New("merge request is not open")

// pullEvent is one activity of a merge request waiting to be handled.
type pullEvent struct {
	repo   domain.RepoName
	number int64
	action string
	// head and base are the commits the activity is for; before is the head
	// before a synchronize, zero otherwise.
	head, base, before domain.ObjectID
	actor              string
	pusher             domain.CIPusher
	trusted            bool
	// actorTrusted is the trust of the cause alone (the pusher of a
	// synchronize, whoever opened or closed): trusted also requires the head
	// and the base to be, pull_request_target the default tip.
	actorTrusted bool
	// target is the default branch's tip when the activity happened: what its
	// pull_request_target runs execute.
	target defaultTip
	// merge is the last merge commit the trust of a closing was checked
	// against, zero when there was none.
	merge domain.ObjectID
	// run is false when the cause is a job token: no run starts.
	run bool
	log zerowrap.Logger
}

func (e pullEvent) trigger(at time.Time) ciTrigger {
	return ciTrigger{repo: e.repo, actor: e.actor, pusher: e.pusher, trusted: e.trusted, at: at, log: e.log}
}

// defaultTip is the tip of the default branch at one moment (CI-4, D4).
type defaultTip struct {
	branch string
	commit domain.ObjectID
	// trusted: a trusted principal set the branch to exactly that commit.
	trusted bool
}

// defaultTipOf takes the tip of the default branch of a repository now, and
// whether a trusted principal pushed it. The zero value when the repository
// has no default branch (no pull_request_target run), and not trusted on any
// error or without a record (refTrusted).
func (c *CI) defaultTipOf(ctx context.Context, repo domain.RepoName) defaultTip {
	def, exists, err := c.cfg.Repos.DefaultBranch(ctx, repo)
	if err != nil || !exists {
		return defaultTip{}
	}
	tip, err := c.cfg.Git.BranchHead(ctx, repo, def)
	if err != nil {
		return defaultTip{}
	}
	return defaultTip{branch: def, commit: tip, trusted: c.refTrusted(ctx, repo, pinnedRef{ref: "refs/heads/" + def, commit: tip, object: tip})}
}

type pullKey struct {
	repo   domain.RepoName
	number int64
}

// pullQueue is what the two workers read.
//
// Overload policy. The activities (events) are never coalesced and never
// dropped by the cap of the refreshes: they go on their own queue, which behaves
// as the queue of the push triggers (CI.queue) does when it is full: the item is
// dropped and an error is logged, so a push is never slowed or failed by CI. Only
// the refresh of refs/pull/<n>/{head,merge}, which is idempotent and recomputed
// from the branches as they are, is coalesced (one waiting refresh per merge
// request) and bounded (max); past the bound the repository is marked dirty and
// its open merge requests are rescanned when the worker has capacity.
type pullQueue struct {
	events chan pullEvent

	mu      sync.Mutex
	max     int // bound of the waiting refreshes
	pending map[pullKey]bool
	order   []pullKey
	// dirty are the repositories whose merge requests may have stale refs: the
	// bound is the number of repositories.
	dirty      map[domain.RepoName]bool
	dirtyOrder []domain.RepoName
	// attempts remember the last state tried for each merge request, so that a
	// rescan does not try again what it already tried (a conflict).
	attempts *pullAttempts
	wake     chan struct{}
	gate     ports.PullGate // set by SetPullGate
}

func newPullQueue(size, max, attempts int) *pullQueue {
	return &pullQueue{
		events: make(chan pullEvent, size), max: max, pending: map[pullKey]bool{},
		dirty: map[domain.RepoName]bool{}, attempts: newPullAttempts(attempts), wake: make(chan struct{}, 1),
	}
}

// SetPullGate gives the CI the gate of the merge requests (see PullGate). Call
// it before merge requests can be opened.
func (c *CI) SetPullGate(g ports.PullGate) {
	c.pull.mu.Lock()
	c.pull.gate = g
	c.pull.mu.Unlock()
}

func (c *CI) pullEnabled() bool { return c.cfg.MRs != nil && c.cfg.Pull != nil }

func (c *CI) stage(stage string) {
	if c.cfg.PullStage != nil {
		c.cfg.PullStage(stage)
	}
}

// OnPullRequest queues an activity of a merge request.
func (c *CI) OnPullRequest(ctx context.Context, repo domain.RepoName, ev ports.PullRequestEvent, p domain.Principal) {
	if !c.pullEnabled() {
		return
	}
	e := pullEvent{
		repo: repo, number: ev.Number, action: ev.Action, head: ev.Head, base: ev.Base,
		actor: authorOf(p), pusher: domain.PusherOf(p), run: !p.ViaJobToken(), log: zerowrap.FromCtx(ctx),
		actorTrusted: domain.TrustedActor(p), target: c.defaultTipOf(ctx, repo),
	}
	if e.actorTrusted {
		e.trusted, e.merge = c.pullTipsTrusted(ctx, repo, ev)
	}
	c.queuePullEvent(e)
}

// pullTipsTrusted reports whether the code the runs of an activity that is not
// a push execute was pushed by trusted principals (CI-4, D1), and, for a
// closing, the last merge commit it checked (zero when there is none). The
// workflow of a pull_request run comes from its merge commit, so from the head
// and the base: whoever opens or closes the request did not necessarily write
// either, and without this the owner opening the request of an agent's branch
// (or into an agent's branch) would run the agent's workflow with the owner's
// secrets. Every check is positive: an error, an unknown commit or a missing
// record is not trusted.
//
// The records of the source and target branches (domain.CIRefTip) must name
// exactly the head and the base of the event; they are read here, when the
// activity happens, as the commits of the event are fixed: a push of either
// branch before the event is handled changes nothing (a push of the source has
// its own event and trust). A tip with no record (pushed before the records
// existed: the next trusted push restores it), or a record for another commit
// (the branch was force-pushed, moved or deleted) is not trusted. The base of
// an opening is ev.Base, the target tip of that moment. A closing runs the last
// merge commit published (which no longer changes once the request is closed:
// the event worker checks it is still the one checked here), whose parents are
// its head and base; without one it runs its head alone.
//
// A synchronize carries the trust of the push that set the source to its head
// (onBranchesMoved), and its base is checked there. The closing a merge makes
// is checked by mergedTrusted.
func (c *CI) pullTipsTrusted(ctx context.Context, repo domain.RepoName, ev ports.PullRequestEvent) (bool, domain.ObjectID) {
	mr, err := c.cfg.MRs.Get(ctx, repo, ev.Number)
	if err != nil {
		pullUntrusted(ctx, repo, ev, err, "the merge request cannot be read")
		return false, domain.ObjectID{}
	}
	base := ev.Base
	var merge domain.ObjectID
	if ev.Action == domain.PullClosed {
		var parents []domain.ObjectID
		if merge, parents, err = c.lastMerge(ctx, repo, mr.Number); err != nil {
			pullUntrusted(ctx, repo, ev, err, "the last merge commit cannot be read")
			return false, merge
		}
		base = domain.ObjectID{} // no merge commit: the head runs alone
		if !merge.IsZero() {
			if parents[1] != ev.Head {
				pullUntrusted(ctx, repo, ev, nil, "the last merge commit is of another head")
				return false, merge
			}
			base = parents[0]
		}
	}
	if ev.Merged {
		return c.mergedTrusted(ctx, repo, ev, mr, base), merge
	}
	if !c.refTrusted(ctx, repo, pinnedRef{ref: "refs/heads/" + mr.Source, commit: ev.Head, object: ev.Head}) {
		pullUntrusted(ctx, repo, ev, nil, "the head was not pushed by a trusted principal")
		return false, merge
	}
	if ev.Action == domain.PullClosed && merge.IsZero() {
		return true, merge
	}
	return c.pullBaseTrusted(ctx, repo, ev.Number, ev.Action, mr.Target, base), merge
}

// pullUntrusted logs why the runs of an activity are not trusted.
func pullUntrusted(ctx context.Context, repo domain.RepoName, ev ports.PullRequestEvent, err error, why string) {
	ctxLog(ctx).Info().Err(err).Str("repo", repo.String()).Int64("number", ev.Number).Str("action", ev.Action).
		Str("head", ev.Head.String()).Msg("ci pull request runs are not trusted: " + why)
}

// lastMerge returns the last merge commit published for a request and its two
// parents (base, head); zero and no error when there is none.
func (c *CI) lastMerge(ctx context.Context, repo domain.RepoName, number int64) (domain.ObjectID, []domain.ObjectID, error) {
	merge, err := c.cfg.Pull.PullRef(ctx, repo, domain.PullMergeRef(number))
	if errors.Is(err, domain.ErrNotFound) {
		return domain.ObjectID{}, nil, nil
	}
	if err != nil {
		return merge, nil, err
	}
	cs, _, err := c.cfg.Git.ListRange(ctx, repo, domain.ObjectID{}, merge, 1)
	if err != nil {
		return merge, nil, err
	}
	if len(cs) != 1 || len(cs[0].Parents) != 2 {
		return merge, nil, fmt.Errorf("%s is not a merge commit", merge)
	}
	return merge, cs[0].Parents, nil
}

// mergedTrusted reports whether the closing a merge made runs code the merger
// took into the target: the request is merged, the target was set to its merge
// result by a trusted principal (the merge records the merger), the head the
// run executes is the one merged (the parents of the last merge commit were
// checked against it), and the base it executes, if any, is in the merge
// result's history. The records of the source branch are gone by then.
func (c *CI) mergedTrusted(ctx context.Context, repo domain.RepoName, ev ports.PullRequestEvent, mr domain.MergeRequest, base domain.ObjectID) bool {
	switch {
	case ev.Action != domain.PullClosed || mr.State != domain.MRStateMerged || mr.MergedCommitID.IsZero():
		pullUntrusted(ctx, repo, ev, nil, "the request is not merged")
		return false
	case ev.Head.IsZero() || ev.Head != mr.HeadID:
		pullUntrusted(ctx, repo, ev, nil, "the head is not the merged one")
		return false
	case !c.refTrusted(ctx, repo, pinnedRef{ref: "refs/heads/" + mr.Target, commit: mr.MergedCommitID, object: mr.MergedCommitID}):
		pullUntrusted(ctx, repo, ev, nil, "the merge result was not set by a trusted principal")
		return false
	}
	if base.IsZero() {
		return true
	}
	if mb, err := c.cfg.Git.MergeBase(ctx, repo, base, mr.MergedCommitID); err != nil || mb != base {
		pullUntrusted(ctx, repo, ev, err, "the base is not in the merge result")
		return false
	}
	return true
}

// pullBaseTrusted reports whether the target branch was set to exactly base by
// a trusted principal: the merge commit of a run carries the target's files
// too, its workflow included.
func (c *CI) pullBaseTrusted(ctx context.Context, repo domain.RepoName, number int64, action, target string, base domain.ObjectID) bool {
	if c.refTrusted(ctx, repo, pinnedRef{ref: "refs/heads/" + target, commit: base, object: base}) {
		return true
	}
	ctxLog(ctx).Info().Str("repo", repo.String()).Int64("number", number).Str("action", action).Str("base", base.String()).
		Msg("ci pull request runs are not trusted: the base was not pushed by a trusted principal")
	return false
}

// onBranchesMoved is called for every push and merge, after its push
// workflows are queued (so a push run never waits for this). It takes the
// snapshot of the push for the merge requests it touches, here and now: the open
// merge requests are listed (a scan), and for those whose source branch was
// pushed a synchronize event is queued carrying the pushed head, the target tip
// as it is at this moment (read once per target for the push), and the actor and
// trust of the push. The event runs for exactly that snapshot, however the
// branches move before it is handled: a later push to the target, possibly by
// someone less trusted, changes nothing about what a trusted run executes.
// The refresh of the refs of every merge request touched (those that have a
// pushed branch as source or as target, from the two indexes) is queued for the
// refresh worker, which is the only fully asynchronous part. A push that touches
// no merge request queues nothing and marks nothing; the whole-repository rescan
// is for overflow, truncation and startup.
func (c *CI) onBranchesMoved(ctx context.Context, repo domain.RepoName, updates []domain.RefUpdate, actor string, pusher domain.CIPusher, trusted, run bool) {
	if !c.pullEnabled() {
		return
	}
	moved := map[string]domain.RefUpdate{}
	for _, u := range updates {
		if b, ok := strings.CutPrefix(u.Ref, "refs/heads/"); ok && !u.IsDelete() {
			moved[b] = u
		}
	}
	if len(moved) == 0 {
		return
	}
	c.stage(pullStageDiscover)
	log := zerowrap.FromCtx(ctx)
	branches := make([]string, 0, len(moved))
	for b := range moved {
		branches = append(branches, b)
	}

	// The requests whose branch is the target of a push (a target move): their
	// refs are queued for a refresh. Only these: an unrelated push queues nothing
	// and marks nothing. The lookup is bounded like the snapshot; past the bound
	// or on an error the repository is marked dirty, and its rescan finds the
	// stale ones.
	targets, tgtTruncated, err := c.cfg.MRs.OpenByTarget(ctx, repo, branches, c.cfg.PullSnapshot)
	switch {
	case err != nil:
		log.Error().Err(err).Str("repo", repo.String()).Msg("ci: look up the merge requests targeting a pushed branch: the repository is rescanned")
		c.markPullDirty(repo)
	case tgtTruncated:
		log.Error().Str("repo", repo.String()).Int("bound", c.cfg.PullSnapshot).
			Msg("ci pull request refresh of a push is cut short: more than the bound of merge requests target the pushed branches; the repository is rescanned")
		c.markPullDirty(repo)
	}
	for _, n := range targets {
		c.queuePullRefresh(pullKey{repo, n})
	}

	// The requests whose source branch was pushed come from the index of the open
	// merge requests by source: a scan of the keys of those branches, bounded and
	// cancellable, never of every merge request the repository ever had. Past the
	// bound or on cancellation the rest is skipped and logged (the overload policy
	// of the event queue: those events are lost), and the repository is marked
	// dirty, so that the refs of the skipped requests are repaired by the rescan.
	numbers, truncated, err := c.cfg.MRs.OpenBySource(ctx, repo, branches, c.cfg.PullSnapshot)
	if err != nil {
		log.Error().Err(err).Str("repo", repo.String()).Msg("ci: look up the merge requests of a push: its pull_request runs are not started; the repository is rescanned")
		c.markPullDirty(repo)
		if len(numbers) == 0 {
			return
		}
	} else if truncated {
		log.Error().Str("repo", repo.String()).Int("bound", c.cfg.PullSnapshot).
			Msg("ci pull request snapshot of a push is cut short: more than the bound of merge requests have a pushed branch as source; the pull_request runs of the others are not started and the repository is rescanned")
		c.markPullDirty(repo)
	}
	tips := map[string]domain.ObjectID{}
	baseTrusted := map[string]bool{}
	var target *defaultTip // the default tip of the snapshot, read once
	for _, n := range numbers {
		mr, err := c.cfg.MRs.Get(ctx, repo, n)
		if err != nil || !mr.IsOpen() {
			continue // closed or gone since the index was read
		}
		u, srcMoved := moved[mr.Source]
		if !srcMoved {
			continue
		}
		if run {
			if head, herr := domain.ParseObjectID(u.New); herr == nil {
				base, seen := tips[mr.Target]
				if !seen {
					base, _ = c.cfg.Git.BranchHead(ctx, repo, mr.Target)
					tips[mr.Target] = base
				}
				before, _ := domain.ParseObjectID(u.Old) // zero for a new branch
				// The pusher's trust covers the head; the base of the snapshot
				// must have been pushed by a trusted principal too.
				bt, checked := baseTrusted[mr.Target]
				if trusted && !checked {
					bt = c.pullBaseTrusted(ctx, repo, mr.Number, domain.PullSynchronize, mr.Target, base)
					baseTrusted[mr.Target] = bt
				}
				evTrusted := trusted && bt
				if target == nil {
					t := c.defaultTipOf(ctx, repo)
					target = &t
				}
				c.queuePullEvent(pullEvent{
					repo: repo, number: mr.Number, action: domain.PullSynchronize, head: head, base: base, before: before,
					actor: actor, pusher: pusher, trusted: evTrusted, actorTrusted: trusted, target: *target, run: true, log: log,
				})
			}
		}
		c.queuePullRefresh(pullKey{repo, mr.Number})
	}
}

// markPullDirty marks a repository for a rescan of its merge requests.
func (c *CI) markPullDirty(repo domain.RepoName) {
	c.pull.mu.Lock()
	c.pull.markDirty(repo)
	c.pull.mu.Unlock()
	c.wakePull()
}

// markDirty records that the merge requests of a repository may be stale. The
// caller holds mu.
func (q *pullQueue) markDirty(repo domain.RepoName) {
	if !q.dirty[repo] {
		q.dirty[repo] = true
		q.dirtyOrder = append(q.dirtyOrder, repo)
	}
}

func (c *CI) wakePull() {
	select {
	case c.pull.wake <- struct{}{}:
	default:
	}
}

// queuePullEvent queues an activity. A full queue drops it and logs an error,
// exactly as the queue of the push triggers does (CI.enqueue): CI never slows
// or fails a push. The refresh cap does not apply to events.
func (c *CI) queuePullEvent(e pullEvent) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return
	}
	select {
	case c.pull.events <- e:
	default:
		e.log.Error().Str("repo", e.repo.String()).Int64("number", e.number).Str("action", e.action).Msg("ci pull request queue is full: this activity of a merge request is not run")
	}
}

// queuePullRefresh asks for the refs of a merge request to be brought up to
// date; asking again while it waits changes nothing. When the queue is full the
// repository is marked dirty instead: the worker rescans its open merge requests
// once it has capacity.
func (c *CI) queuePullRefresh(k pullKey) {
	q := c.pull
	q.mu.Lock()
	switch {
	case q.pending[k]:
	case len(q.pending) < q.max:
		q.pending[k] = true
		q.order = append(q.order, k)
	default:
		q.markDirty(k.repo)
	}
	q.mu.Unlock()
	c.wakePull()
}

// nextPullWork takes what the refresh worker does next: a refresh, or a dirty
// repository to rescan, in that order.
func (c *CI) nextPullWork() (key *pullKey, dirty *domain.RepoName) {
	q := c.pull
	q.mu.Lock()
	defer q.mu.Unlock()
	switch {
	case len(q.order) > 0:
		k := q.order[0]
		q.order = q.order[1:]
		delete(q.pending, k)
		return &k, nil
	case len(q.dirtyOrder) > 0:
		r := q.dirtyOrder[0]
		q.dirtyOrder = q.dirtyOrder[1:]
		delete(q.dirty, r)
		return nil, &r
	}
	return nil, nil
}

// RescanPulls rescans the merge requests of a repository now, on the calling
// goroutine, as the refresh worker does for a repository it marked dirty. It is
// for tests and operators' tooling; the server never needs to call it.
func (c *CI) RescanPulls(ctx context.Context, repo domain.RepoName) error {
	return c.rescanPulls(ctx, repo)
}

// rescanPulls checks the open merge requests of a repository against their refs
// and refreshes, one after the other and here (not through the bounded queue, so
// a rescan never marks its repository dirty again), each whose
// refs/pull/<n>/head is not the tip of its source branch or whose merge ref is
// not a merge of the two current tips. A merge request whose current state was
// already tried (a head that conflicts has no merge ref, so it never looks
// current) is skipped until one of its branches moves. It repairs what a
// saturated queue, a failed discovery or a restart left behind.
func (c *CI) rescanPulls(ctx context.Context, repo domain.RepoName) error {
	c.stage(pullStageRescan)
	mrs, err := c.cfg.MRs.ListOpen(ctx, repo)
	if err != nil {
		return fmt.Errorf("list the open merge requests: %w", err)
	}
	for _, mr := range mrs {
		if ctx.Err() != nil {
			return nil
		}
		head, herr := c.cfg.Git.BranchHead(ctx, repo, mr.Source)
		base, berr := c.cfg.Git.BranchHead(ctx, repo, mr.Target)
		if herr != nil || berr != nil {
			continue // a branch is gone: nothing to publish
		}
		if c.pull.attempts.tried(pullKey{repo, mr.Number}, base, head) || c.pullRefsCurrent(ctx, mr, head, base) {
			continue
		}
		if err := c.refreshPull(ctx, pullKey{repo, mr.Number}); err != nil && !errors.Is(err, errPullClosed) {
			ctxLog(ctx).Error().Err(err).Str("repo", repo.String()).Int64("number", mr.Number).Msg("ci: refresh the refs of a merge request in a rescan")
		}
	}
	return nil
}

// pullRefsCurrent reports whether the head ref is head and the merge ref, if
// there is one, merges base and head. A head that conflicts has no merge ref and
// counts as current once the head ref is right: the next push or move tries again.
func (c *CI) pullRefsCurrent(ctx context.Context, mr domain.MergeRequest, head, base domain.ObjectID) bool {
	if cur, err := c.cfg.Pull.PullRef(ctx, mr.Repo, domain.PullHeadRef(mr.Number)); err != nil || cur != head {
		return false
	}
	merge, err := c.cfg.Pull.PullRef(ctx, mr.Repo, domain.PullMergeRef(mr.Number))
	if err != nil {
		return false // none: a refresh decides whether the merge is clean
	}
	cs, _, err := c.cfg.Git.ListRange(ctx, mr.Repo, domain.ObjectID{}, merge, 1)
	return err == nil && len(cs) == 1 && len(cs[0].Parents) == 2 && cs[0].Parents[0] == base && cs[0].Parents[1] == head
}

// rescanAllPulls marks dirty every repository that has open merge requests, at
// startup: the refs may be stale after a restart or a crash.
func (c *CI) rescanAllPulls(ctx context.Context) {
	repos, err := c.cfg.Meta.List(ctx)
	if err != nil {
		ctxLog(ctx).Error().Err(err).Msg("ci: list the repositories to rescan their merge requests")
		return
	}
	q := c.pull
	q.mu.Lock()
	for _, r := range repos {
		q.markDirty(r.Name)
	}
	q.mu.Unlock()
	c.wakePull()
}

// pullLoop is the event worker.
func (c *CI) pullLoop() {
	defer c.wg.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case e := <-c.pull.events:
			c.guardPull(e.log, e.repo, func(ctx context.Context) error { return c.handlePullEvent(ctx, e) })
		}
	}
}

// refreshLoop is the refresh worker: it refreshes the refs of the merge
// requests a push touched, and rescans the repositories that fell behind.
func (c *CI) refreshLoop() {
	defer c.wg.Done()
	c.rescanAllPulls(c.ctx)
	for {
		key, dirty := c.nextPullWork()
		if c.ctx.Err() != nil {
			return
		}
		switch {
		case key != nil:
			c.guardPull(zerowrap.FromCtx(c.ctx), key.repo, func(ctx context.Context) error { return c.refreshPull(ctx, *key) })
		case dirty != nil:
			c.guardPull(zerowrap.FromCtx(c.ctx), *dirty, func(ctx context.Context) error { return c.rescanPulls(ctx, *dirty) })
		default:
			select {
			case <-c.ctx.Done():
				return
			case <-c.pull.wake:
			}
		}
	}
}

// guardPull runs fn; a panic or an error is logged and never escapes.
func (c *CI) guardPull(log zerowrap.Logger, repo domain.RepoName, fn func(context.Context) error) {
	ctx := zerowrap.WithCtx(c.ctx, log)
	defer func() {
		if r := recover(); r != nil {
			log.Error().Interface("panic", r).Bytes("stack", debug.Stack()).Str("repo", repo.String()).Msg("ci pull request work panicked")
		}
	}()
	if err := fn(ctx); err != nil && ctx.Err() == nil && !errors.Is(err, errPullClosed) {
		log.Error().Err(err).Str("repo", repo.String()).Msg("ci pull request work failed")
	}
}

// whileOpen runs fn under the merge lock while the request is open; errPullClosed
// when it is not. Without a gate (tests of the CI alone) it checks and runs.
func (c *CI) whileOpen(ctx context.Context, repo domain.RepoName, number int64, fn func() error) error {
	c.pull.mu.Lock()
	gate := c.pull.gate
	c.pull.mu.Unlock()
	var err error
	if gate != nil {
		err = gate.WhileOpen(ctx, repo, number, fn)
	} else if mr, gerr := c.cfg.MRs.Get(ctx, repo, number); gerr != nil {
		err = gerr
	} else if !mr.IsOpen() {
		err = domain.ErrPreconditionFailed
	} else {
		err = fn()
	}
	if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrNotFound) {
		return errPullClosed
	}
	return err
}

// createRun stores a run. The run of an open merge request is stored under the
// gate, so that one that is closed meanwhile gets none (errPullClosed).
func (c *CI) createRun(ctx context.Context, f pushFacts, repo domain.RepoName, wfPath string, build func(id, number int64) ports.CIRunDraft) (ports.CIRunDraft, error) {
	if f.pull == nil || f.pull.action == domain.PullClosed {
		return c.cfg.Store.CreateRun(ctx, repo, wfPath, build)
	}
	var d ports.CIRunDraft
	err := c.whileOpen(ctx, repo, f.pull.mr.Number, func() error {
		var err error
		d, err = c.cfg.Store.CreateRun(ctx, repo, wfPath, build)
		return err
	})
	return d, err
}

// pullFacts is what a pull_request run is for.
type pullFacts struct {
	mr     domain.MergeRequest
	action string
	// head and base are the commits of the event; the merge commit of the run
	// has them as parents.
	head, base domain.ObjectID
	// before is the head before a synchronize.
	before domain.ObjectID
	merged bool
	// merge is the test merge commit of head and base, zero when there is
	// none (a conflict, or a closed request that never merged cleanly).
	merge domain.ObjectID
	// target is set for a pull_request_target run.
	target bool
}

// pullState is the merge of a head into a base.
type pullState struct {
	head, base domain.ObjectID
	// merge is the test merge commit of head and base when clean; for a closed
	// request, the last one published, zero if none.
	merge domain.ObjectID
	clean bool
	// otherHead is set for a closed request whose last merge commit is of
	// another head than its event's: the provenance of the event's head says
	// nothing of the code the run executes.
	otherHead bool
}

func (c *CI) handlePullEvent(ctx context.Context, e pullEvent) error {
	c.stage(pullStageEvent)
	mr, err := c.cfg.MRs.Get(ctx, e.repo, e.number)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	switch e.action {
	case domain.PullOpened, domain.PullSynchronize:
		if !mr.IsOpen() {
			return nil // closed meanwhile: the closed event follows
		}
		st, err := c.mergeOf(ctx, mr, e.head, e.base)
		if err != nil {
			return err
		}
		c.stage(pullStageComputed)
		if err := c.publishPull(ctx, mr, st); err != nil || !e.run {
			return err
		}
		var err1 error
		if st.clean { // a conflict makes no pull_request run, as on GitHub
			err1 = c.startPullRuns(ctx, e, mr, st, false)
		}
		return errors.Join(err1, c.startPullRuns(ctx, e, mr, st, true))
	case domain.PullClosed:
		if mr.IsOpen() || !e.run {
			return nil
		}
		st, ok, err := c.closedPull(ctx, e, mr)
		if err != nil || !ok {
			return err
		}
		pe := e
		if st.otherHead || st.merge != e.merge {
			pe.trusted = false // not the code the trust was checked for
		}
		return errors.Join(c.startPullRuns(ctx, pe, mr, st, false), c.startPullRuns(ctx, e, mr, st, true))
	}
	return nil
}

// refreshPull brings the refs of a merge request to its branches as they are.
func (c *CI) refreshPull(ctx context.Context, k pullKey) error {
	c.stage(pullStageRefresh)
	mr, err := c.cfg.MRs.Get(ctx, k.repo, k.number)
	if errors.Is(err, domain.ErrNotFound) || (err == nil && !mr.IsOpen()) {
		return nil
	}
	if err != nil {
		return err
	}
	head, err := c.cfg.Git.BranchHead(ctx, k.repo, mr.Source)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	base, err := c.cfg.Git.BranchHead(ctx, k.repo, mr.Target)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	st, err := c.mergeOf(ctx, mr, head, base)
	if err != nil {
		return err
	}
	return c.publishPull(ctx, mr, st)
}

// mergeOf is the test merge of head into base. It computes the commit only; a
// merge ref that is already that merge is reused.
func (c *CI) mergeOf(ctx context.Context, mr domain.MergeRequest, head, base domain.ObjectID) (pullState, error) {
	st := pullState{head: head, base: base}
	if head.IsZero() || base.IsZero() {
		return st, nil
	}
	if old, err := c.cfg.Pull.PullRef(ctx, mr.Repo, domain.PullMergeRef(mr.Number)); err == nil {
		cs, _, err := c.cfg.Git.ListRange(ctx, mr.Repo, domain.ObjectID{}, old, 1)
		if err == nil && len(cs) == 1 && len(cs[0].Parents) == 2 && cs[0].Parents[0] == base && cs[0].Parents[1] == head {
			st.merge, st.clean = old, true
			return st, nil
		}
	} else if !errors.Is(err, domain.ErrNotFound) {
		return st, err
	}
	tree, clean, err := c.cfg.Pull.TestMerge(ctx, mr.Repo, base, head)
	k := pullKey{mr.Repo, mr.Number}
	if err != nil {
		return st, err
	}
	if !clean {
		c.pull.attempts.conflicted(k, base, head) // a rescan does not try these tips again
		return st, nil
	}
	c.pull.attempts.merged(k)
	committer := c.cfg.Committer
	if committer.Name == "" {
		committer = domain.Identity{Name: "ermite", Email: "ermite@localhost"}
	}
	id, err := c.cfg.Git.WriteCommit(ctx, mr.Repo, domain.TestMergeCommit(tree, base, head, committer, c.now()))
	if err != nil {
		return st, err
	}
	st.merge, st.clean = id, true
	return st, nil
}

// publishPull points refs/pull/<n>/head and /merge at st, under the gate. A ref
// moves only to the latest: the head ref when st.head is still the head of the
// source branch, the merge ref when the target tip is st.base too, so that an
// older event never moves a ref back. errPullClosed when the request is closed.
func (c *CI) publishPull(ctx context.Context, mr domain.MergeRequest, st pullState) error {
	if st.head.IsZero() {
		return nil
	}
	changed := false
	err := c.whileOpen(ctx, mr.Repo, mr.Number, func() error {
		curHead, err := c.cfg.Git.BranchHead(ctx, mr.Repo, mr.Source)
		if err != nil || curHead != st.head {
			return ignoreNotFound(err)
		}
		if old, err := c.cfg.Pull.PullRef(ctx, mr.Repo, domain.PullHeadRef(mr.Number)); err != nil || old != st.head {
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return err
			}
			if err := c.cfg.Pull.SetPullRef(ctx, mr.Repo, domain.PullHeadRef(mr.Number), st.head); err != nil {
				return err
			}
			changed = true
		}
		if !st.clean {
			return nil
		}
		curBase, err := c.cfg.Git.BranchHead(ctx, mr.Repo, mr.Target)
		if err != nil || curBase != st.base {
			return ignoreNotFound(err)
		}
		if old, err := c.cfg.Pull.PullRef(ctx, mr.Repo, domain.PullMergeRef(mr.Number)); err != nil || old != st.merge {
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return err
			}
			if err := c.cfg.Pull.SetPullRef(ctx, mr.Repo, domain.PullMergeRef(mr.Number), st.merge); err != nil {
				return err
			}
			changed = true
		}
		return nil
	})
	if changed && c.cfg.Fresh != nil {
		c.cfg.Fresh.Refresh(ctx, mr.Repo)
	}
	return err
}

func ignoreNotFound(err error) error {
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	return err
}

// closedPull is the state a closed or merged merge request runs with: the last
// test merge commit published, or the head when there is none. Nothing is
// computed again. ok is false when the request has neither (its branch never
// existed).
func (c *CI) closedPull(ctx context.Context, e pullEvent, mr domain.MergeRequest) (st pullState, ok bool, err error) {
	if st.merge, err = c.cfg.Pull.PullRef(ctx, mr.Repo, domain.PullMergeRef(mr.Number)); err != nil && !errors.Is(err, domain.ErrNotFound) {
		return st, false, err
	}
	st.head = e.head
	if st.head.IsZero() {
		st.head = mr.HeadID
	}
	if st.head.IsZero() {
		if st.head, err = c.cfg.Pull.PullRef(ctx, mr.Repo, domain.PullHeadRef(mr.Number)); err != nil && !errors.Is(err, domain.ErrNotFound) {
			return st, false, err
		}
	}
	st.base = e.base
	if !st.merge.IsZero() {
		if cs, _, err := c.cfg.Git.ListRange(ctx, mr.Repo, domain.ObjectID{}, st.merge, 1); err == nil && len(cs) == 1 && len(cs[0].Parents) == 2 {
			st.base = cs[0].Parents[0]
			if st.head.IsZero() {
				st.head = cs[0].Parents[1]
			}
			st.otherHead = cs[0].Parents[1] != e.head
		}
	}
	if st.base.IsZero() {
		st.base = mr.BaseID
	}
	st.clean = true
	return st, !st.head.IsZero(), nil
}

// startPullRuns starts the pull_request workflows of the merge request that
// match the activity and the files it changes, or with target its
// pull_request_target workflows: those of the default tip of the event.
func (c *CI) startPullRuns(ctx context.Context, e pullEvent, mr domain.MergeRequest, st pullState, target bool) error {
	if target && (e.target.commit.IsZero() || st.head.IsZero()) {
		return nil // no default branch, or no head to run for
	}
	runners, err := c.cfg.Store.ListRunners(ctx)
	if err != nil {
		return fmt.Errorf("list the runners: %w", err)
	}
	if len(runners) == 0 {
		return nil
	}
	repo, err := c.cfg.Meta.Get(ctx, e.repo)
	if err != nil {
		return nil // deleted meanwhile
	}
	t := e.trigger(c.now())
	ref, sha := domain.PullMergeRef(mr.Number), st.merge
	switch {
	case target:
		// The code is the default tip's: the cause and that tip decide.
		ref, sha = "refs/heads/"+e.target.branch, e.target.commit
		t.trusted = e.actorTrusted && e.target.trusted
		if e.actorTrusted && !e.target.trusted {
			ctxLog(ctx).Info().Str("repo", e.repo.String()).Int64("number", mr.Number).Str("action", e.action).Str("tip", sha.String()).
				Msg("ci pull_request_target runs are not trusted: the default branch tip was not pushed by a trusted principal")
		}
	case sha.IsZero():
		ref, sha = domain.PullHeadRef(mr.Number), st.head
	}
	pf := &pullFacts{
		mr: mr, action: e.action, head: st.head, base: st.base, before: e.before, merged: mr.State == domain.MRStateMerged,
		merge: st.merge, target: target,
	}
	f := pushFacts{
		update: domain.RefUpdate{Ref: ref, Old: domain.ZeroOID, New: sha.String()}, commit: sha, pull: pf,
	}
	if def, _, err := c.cfg.Repos.DefaultBranch(ctx, e.repo); err == nil {
		f.defaultBranch = def
	}
	head, _, err := c.cfg.Git.ListRange(ctx, e.repo, domain.ObjectID{}, st.head, 1)
	if err != nil || len(head) == 0 {
		return fmt.Errorf("read the head commit of !%d: %w", mr.Number, err)
	}
	f.head = head[0]
	// The files of the merge request are those between the merge base of the
	// target and the head, and the head: what the review shows.
	f.filesTruncated = true // unavailable unless read below: a paths filter then passes
	if !st.base.IsZero() {
		if mb, err := c.cfg.Git.MergeBase(ctx, e.repo, st.base, st.head); err == nil {
			if paths, truncated, err := c.cfg.Diff.ChangedPaths(ctx, e.repo, mb, st.head, workflow.MaxFilesEvaluated); err == nil {
				f.files, f.filesTruncated = paths, truncated
			}
		}
	}
	files, err := c.workflowFiles(ctx, e.repo, sha)
	if err != nil || len(files) == 0 {
		return err
	}
	if !target && workflow.SkipCI(f.head.Message) {
		return nil // GitHub's skip directives are for push and pull_request only
	}
	var errs []error
	for _, file := range files {
		if file.problem != "" {
			continue // reported by the push of the branch
		}
		wf, perrs := workflow.Parse(file.path, file.content)
		if len(perrs) > 0 || !matchPullRequest(wf, f.eventName(), e.action, mr.Target, f.files, f.filesTruncated) {
			continue
		}
		if problem := c.matrixProblem(ctx, repo, t, f, file.path, wf); problem.text != "" {
			name := file.path
			if problem.named {
				name = workflowName(wf, file.path)
			}
			errs = append(errs, c.startInvalid(ctx, repo, t, f, file.path, name, problem.text))
			continue
		}
		errs = append(errs, c.startRun(ctx, repo, t, f, file.path, string(file.content), wf))
	}
	return errors.Join(errs...)
}

// matchPullRequest reports whether the workflow runs for the activity of a merge
// request into base: the event (pull_request or pull_request_target) is
// listed, the activity is one of its types (opened, synchronize and reopened
// when it lists none), and the branches and paths filters, which apply to the
// base branch and the changed files, let it through. The filters are those of
// the push event (ermite-actions), run on the event under another name.
func matchPullRequest(wf *workflow.Workflow, event, action, base string, files []string, truncated bool) bool {
	if wf == nil || wf.On == nil {
		return false
	}
	ev := wf.On.Event(event)
	if ev == nil {
		return false
	}
	types := ev.Types
	if types == nil {
		types = domain.DefaultPullRequestTypes
	}
	if !slices.Contains(types, action) {
		return false
	}
	as := *ev
	as.Name = "push"
	probe := &workflow.Workflow{On: &workflow.On{Events: []*workflow.Event{&as}}}
	return probe.MatchPush(workflow.PushInfo{Ref: "refs/heads/" + base, ChangedFiles: files, FilesTruncated: truncated})
}

// --- the github context ---

// pullGithubJSON builds the github context of a pull_request run: github.ref is
// the merge ref, github.sha the merge commit (the head for a closed request
// that never merged), and the event is shaped like GitHub's pull_request
// webhook payload with the fields ermite can fill. A pull_request_target run
// has the same payload under its own event name, on the default branch.
func (c *CI) pullGithubJSON(repo domain.Repo, run domain.CIRun, f pushFacts) string {
	p := f.pull
	mr := p.mr
	u := c.urls(repo)
	id := c.identity(run.Actor)
	owner := c.identity("owner")
	repoObj := c.repoObj(repo, u, f.defaultBranch, run.CreatedAt)
	num := strconv.FormatInt(mr.Number, 10)
	prAPI := u.api + "/repos/" + u.full + "/pulls/" + num
	html := u.repo + "/merge-requests/" + num
	side := func(branch string, sha domain.ObjectID) map[string]any {
		return map[string]any{
			"label": u.owner + ":" + branch, "ref": branch, "sha": sha.String(),
			"user": u.userObj(owner, false), "repo": repoObj,
		}
	}
	tm := func(t time.Time) any {
		if t.IsZero() {
			return nil
		}
		return t.UTC().Format(time.RFC3339)
	}
	var body any
	if s := mr.Description.String(); s != "" {
		body = s
	}
	state, closedAt, mergedAt := "open", any(nil), any(nil)
	mergeSha := any(nil)
	if !mr.IsOpen() {
		state, closedAt = "closed", tm(mr.UpdatedAt)
	}
	switch {
	case p.merged:
		mergedAt = tm(mr.UpdatedAt)
		mergeSha = mr.MergedCommitID.String()
	case !p.merge.IsZero() && p.merge != p.head:
		mergeSha = p.merge.String() // the test merge commit
	}
	prID := stableID("pull", u.full+"#"+num)
	pr := map[string]any{
		"url": prAPI, "id": prID, "node_id": nodeID("011", "PullRequest", prID), "html_url": html,
		"diff_url": html + ".diff", "patch_url": html + ".patch", "issue_url": u.api + "/repos/" + u.full + "/issues/" + num,
		"number": mr.Number, "state": state, "locked": false, "title": mr.Title.String(), "user": u.userObj(c.identity(mr.Author), false),
		"body": body, "created_at": tm(mr.CreatedAt), "updated_at": tm(mr.UpdatedAt), "closed_at": closedAt, "merged_at": mergedAt,
		"merge_commit_sha": mergeSha, "assignee": nil, "assignees": []any{}, "requested_reviewers": []any{},
		"requested_teams": []any{}, "labels": []any{}, "milestone": nil, "draft": false,
		"head": side(mr.Source, p.head), "base": side(mr.Target, p.base), "author_association": "OWNER",
		"auto_merge": nil, "active_lock_reason": nil, "merged": p.merged,
	}
	event := map[string]any{
		"action": p.action, "number": mr.Number, "pull_request": pr, "repository": repoObj, "sender": u.userObj(id, false),
	}
	if p.action == domain.PullSynchronize {
		before := p.before.String()
		if p.before.IsZero() {
			before = domain.ZeroOID
		}
		event["before"], event["after"] = before, p.head.String()
	}
	refName := strings.TrimPrefix(run.Ref, domain.PullRefPrefix)
	if p.target {
		refName = strings.TrimPrefix(run.Ref, "refs/heads/")
	}
	return c.githubContext(repo, run, ghEvent{
		name: f.eventName(), refName: refName, refType: "branch",
		headRef: mr.Source, baseRef: mr.Target, payload: event,
	})
}
