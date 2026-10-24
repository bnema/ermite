// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
)

// The schedule event (CI-3, D5). A scheduler goroutine looks, once per minute of
// its clock, at the "cron" entries of the workflows of every repository's default
// branch and starts the ones that match that minute (UTC; "timezone" is ignored).
//
//   - The run is on the latest commit of the default branch. Its actor and its
//     trust are those of whoever pushed that commit (ci_tips.go), as on GitHub:
//     the default branch is not necessarily the owner's work.
//   - A minute is looked at once, when the clock first reaches it. Ticks missed
//     while ermite was down or busy are not made up: a pass that is still going
//     when the next minute starts is dropped, as GitHub drops late runs.
//   - The schedules of a repository are read once and cached with the commit
//     they were read at. A push (of any principal, a job token's included, which
//     starts no run but may change the file) marks the repository, and the
//     cache is dropped when its default branch moved. A repository that no
//     longer exists is forgotten at the next pass.
//   - The work is bounded: at most ScheduleBudget repositories are looked at per
//     tick, so a pass over many repositories spreads over several ticks (and
//     never starts a run late for a minute that is over). A pass dropped
//     before its end leaves a cursor: the next pass starts after the last
//     repository handled, so that no repository is starved.
//
// The cron entries of one workflow that match the same minute start one run of
// it (the first in file order); the event payload is that entry's text.

const (
	// DefaultCIScheduleTick is how often the scheduler looks at its clock.
	DefaultCIScheduleTick = time.Second
	// DefaultCIScheduleBudget is the repositories the scheduler handles at most
	// per tick.
	DefaultCIScheduleBudget = 64
)

// scheduledCron is one cron entry of a workflow.
type scheduledCron struct {
	spec  string
	sched domain.CronSchedule
}

// scheduledWorkflow is a workflow with at least one valid cron entry.
type scheduledWorkflow struct {
	path  string
	yaml  []byte
	crons []scheduledCron
}

// scheduleEntry is what was read from one repository at tip.
type scheduleEntry struct {
	tip   domain.ObjectID
	files []scheduledWorkflow
}

// schedulePass is the look at one minute over the repositories.
type schedulePass struct {
	minute time.Time
	repos  []domain.RepoName
	next   int
}

type scheduler struct {
	mu    sync.Mutex
	dirty map[domain.RepoName]struct{}
	cache map[domain.RepoName]*scheduleEntry
	// passed is the minute of the last pass that went through every repository.
	passed time.Time

	// The fields below belong to the scheduler goroutine.
	last time.Time // the latest minute a pass was started for
	pass *schedulePass
	// cursor is the last repository a pass handled when it was dropped before
	// the end; the next pass starts after it, so that repositories beyond the
	// budget of a tick are not starved by the ones that come first. Zero when
	// the last pass went through every repository.
	cursor domain.RepoName
}

func newScheduler(boot time.Time) *scheduler {
	return &scheduler{
		dirty: map[domain.RepoName]struct{}{}, cache: map[domain.RepoName]*scheduleEntry{},
		// The minute ermite starts in belongs to the process before it.
		last: boot.UTC().Truncate(time.Minute),
	}
}

func (s *scheduler) get(repo domain.RepoName) *scheduleEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cache[repo]
}

func (s *scheduler) put(repo domain.RepoName, e *scheduleEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[repo] = e
}

func (s *scheduler) drop(repo domain.RepoName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, repo)
}

// markDirty remembers that the default branch of a repository may have moved.
func (s *scheduler) markDirty(repo domain.RepoName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, cached := s.cache[repo]; cached {
		s.dirty[repo] = struct{}{}
	}
}

func (s *scheduler) forgetDirty(repo domain.RepoName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.dirty, repo)
}

// takeDirty removes and returns at most n marked repositories.
func (s *scheduler) takeDirty(n int) []domain.RepoName {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.RepoName
	for r := range s.dirty {
		if len(out) == n {
			break
		}
		out = append(out, r)
		delete(s.dirty, r)
	}
	return out
}

// prune forgets the repositories that are not in keep.
func (s *scheduler) prune(keep []domain.RepoName) {
	live := make(map[domain.RepoName]struct{}, len(keep))
	for _, r := range keep {
		live[r] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for r := range s.cache {
		if _, ok := live[r]; !ok {
			delete(s.cache, r)
		}
	}
	for r := range s.dirty {
		if _, ok := live[r]; !ok {
			delete(s.dirty, r)
		}
	}
}

// CachedSchedules is the number of scheduled workflows the scheduler holds for a
// repository, -1 when it holds nothing for it. For tests and diagnostics.
func (c *CI) CachedSchedules(repo domain.RepoName) int {
	if e := c.sched.get(repo); e != nil {
		return len(e.files)
	}
	return -1
}

// SchedulePassed is the minute of the last pass of the scheduler that went
// through every repository (zero before the first). For tests and diagnostics.
func (c *CI) SchedulePassed() time.Time {
	c.sched.mu.Lock()
	defer c.sched.mu.Unlock()
	return c.sched.passed
}

// markSchedulesDirty tells the scheduler that these updates may have moved the
// default branch of the repository.
func (c *CI) markSchedulesDirty(repo domain.RepoName, updates []domain.RefUpdate) {
	for _, u := range updates {
		if strings.HasPrefix(u.Ref, "refs/heads/") {
			c.sched.markDirty(repo)
			return
		}
	}
}

// ScheduleNow does one tick of the scheduler on the calling goroutine, with the
// clock of the configuration. It is for tests (with a ScheduleTick too long to
// fire) and operators' tooling; the server never needs to call it. It must not
// run at the same time as the scheduler goroutine.
func (c *CI) ScheduleNow(ctx context.Context) {
	c.guardSchedule(func(context.Context) { c.scheduleTick(ctx) })
}

func (c *CI) scheduleLoop() {
	defer c.wg.Done()
	t := time.NewTicker(c.cfg.ScheduleTick)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
		}
		c.guardSchedule(c.scheduleTick)
	}
}

func (c *CI) guardSchedule(fn func(context.Context)) {
	log := zerowrap.FromCtx(c.ctx)
	defer func() {
		if r := recover(); r != nil {
			log.Error().Interface("panic", r).Bytes("stack", debug.Stack()).Msg("ci scheduler panicked")
		}
	}()
	fn(c.ctx)
}

// scheduleTick does one tick of work: it checks the repositories whose default
// branch may have moved, starts a pass when the clock reached a new minute, and
// carries on the pass within the budget.
func (c *CI) scheduleTick(ctx context.Context) {
	s := c.sched
	budget := c.cfg.ScheduleBudget
	for _, repo := range s.takeDirty(budget) {
		budget--
		if s.get(repo) != nil && !c.scheduleCurrent(ctx, repo) {
			s.drop(repo)
		}
	}
	if minute := c.now().Truncate(time.Minute); minute.After(s.last) {
		// A pass still going belongs to a minute that is over: it is dropped, and
		// so is every minute between.
		s.last = minute
		s.pass = c.startPass(ctx, minute)
	}
	p := s.pass
	for p != nil && p.next < len(p.repos) && budget > 0 && ctx.Err() == nil {
		repo := p.repos[p.next]
		p.next++
		s.cursor = repo
		budget--
		if c.cfg.ScheduleVisit != nil {
			c.cfg.ScheduleVisit(repo)
		}
		if err := c.scheduleRepo(ctx, repo, p.minute); err != nil && ctx.Err() == nil {
			ctxLog(ctx).Error().Err(err).Str("repo", repo.String()).Msg("ci schedule failed")
		}
	}
	if p != nil && p.next >= len(p.repos) {
		s.mu.Lock()
		s.passed = p.minute
		s.mu.Unlock()
		s.pass, s.cursor = nil, domain.RepoName{}
	}
}

// startPass lists the repositories to look at for a minute and forgets the
// ones that are gone. Without a runner CI is off and nothing is looked at.
func (c *CI) startPass(ctx context.Context, minute time.Time) *schedulePass {
	p := &schedulePass{minute: minute}
	repos, err := c.cfg.Meta.List(ctx)
	if err != nil {
		ctxLog(ctx).Error().Err(err).Msg("ci schedule: list the repositories")
		return p
	}
	names := make([]domain.RepoName, 0, len(repos))
	for _, r := range repos {
		if r.Archived {
			continue
		}
		names = append(names, r.Name)
	}
	c.sched.prune(names)
	if runners, err := c.cfg.Store.ListRunners(ctx); err != nil || len(runners) == 0 {
		return p
	}
	// Start after the repository the dropped pass stopped at (the list is in
	// name order), and wrap around.
	start := 0
	if !c.sched.cursor.IsZero() {
		for start < len(names) && names[start].String() <= c.sched.cursor.String() {
			start++
		}
	}
	p.repos = append(slices.Clone(names[start:]), names[:start]...)
	return p
}

// scheduleCurrent reports whether the cached schedules of a repository were
// read at the tip of its default branch as it is now.
func (c *CI) scheduleCurrent(ctx context.Context, repo domain.RepoName) bool {
	e := c.sched.get(repo)
	if e == nil {
		return false
	}
	tip, ok := c.defaultTip(ctx, repo)
	return ok && tip == e.tip
}

// defaultTip is the latest commit of the default branch.
func (c *CI) defaultTip(ctx context.Context, repo domain.RepoName) (domain.ObjectID, bool) {
	def, exists, err := c.cfg.Repos.DefaultBranch(ctx, repo)
	if err != nil || !exists {
		return domain.ObjectID{}, false
	}
	tip, err := c.cfg.Git.BranchHead(ctx, repo, def)
	if err != nil {
		return domain.ObjectID{}, false
	}
	return tip, true
}

// loadSchedules reads the cron entries of the workflows at the tip of the
// default branch. A workflow that does not parse is skipped (the push of the
// file reported it).
func (c *CI) loadSchedules(ctx context.Context, repo domain.RepoName) (*scheduleEntry, error) {
	tip, ok := c.defaultTip(ctx, repo)
	if !ok {
		return &scheduleEntry{}, nil // no repository, no default branch, nothing to schedule
	}
	e := &scheduleEntry{tip: tip}
	// Every workflow, disabled or not: the cache is of the commit, not of the
	// state, which is read when a schedule fires (fireSchedule). A load that
	// raced with a disable or an enable would otherwise keep the wrong set of
	// workflows for as long as the tip does not move.
	files, err := c.listWorkflowFiles(ctx, repo, tip)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if f.problem != "" {
			continue
		}
		wf, perrs := parseWorkflow(f.path, f.content)
		if len(perrs) > 0 || wf.On == nil {
			continue
		}
		ev := wf.On.Event(eventSchedule)
		if ev == nil {
			continue
		}
		sw := scheduledWorkflow{path: f.path, yaml: f.content}
		for _, sc := range ev.Schedule {
			cron, err := domain.ParseCron(sc.Cron)
			if err != nil {
				continue
			}
			loc, err := domain.ParseCronTimezone(sc.Timezone)
			if err != nil {
				continue // the push of the file reported it (parseWorkflow)
			}
			sw.crons = append(sw.crons, scheduledCron{spec: sc.Cron, sched: domain.CronSchedule{Cron: cron, Loc: loc}})
		}
		if len(sw.crons) > 0 {
			e.files = append(e.files, sw)
		}
	}
	return e, nil
}

// dueFor returns, for each workflow of the entry that has an entry matching the
// minute, the text of the first one that does.
func dueFor(e *scheduleEntry, minute time.Time) (idx []int, specs []string) {
	for i, f := range e.files {
		for _, sc := range f.crons {
			if sc.sched.Due(minute) {
				idx, specs = append(idx, i), append(specs, sc.spec)
				break
			}
		}
	}
	return idx, specs
}

// scheduleRepo starts the scheduled workflows of a repository that are due at
// the minute.
func (c *CI) scheduleRepo(ctx context.Context, repo domain.RepoName, minute time.Time) error {
	// The cache is of the commit the run will be on: it is read again when the
	// default branch moved (a push marked it, or the default branch was changed).
	tip, ok := c.defaultTip(ctx, repo)
	if !ok {
		c.sched.drop(repo)
		return nil
	}
	e := c.sched.get(repo)
	if e == nil || e.tip != tip {
		var err error
		if e, err = c.loadSchedules(ctx, repo); err != nil {
			return fmt.Errorf("read the schedules: %w", err)
		}
		if c.cfg.ScheduleLoaded != nil {
			c.cfg.ScheduleLoaded(repo)
		}
		c.sched.put(repo, e)
	}
	idx, specs := dueFor(e, minute)
	if len(idx) == 0 {
		return nil
	}
	meta, err := c.cfg.Meta.Get(ctx, repo)
	if err != nil {
		c.sched.drop(repo)
		return nil // deleted meanwhile
	}
	if meta.Archived {
		c.sched.drop(repo)
		return nil
	}
	def, _, err := c.cfg.Repos.DefaultBranch(ctx, repo)
	if err != nil {
		return nil
	}
	commits, _, err := c.cfg.Git.ListRange(ctx, repo, domain.ObjectID{}, e.tip, 1)
	if err != nil || len(commits) == 0 {
		return fmt.Errorf("read the head commit of %s: %w", def, err)
	}
	var errs []error
	for n, i := range idx {
		errs = append(errs, c.fireSchedule(ctx, meta, def, e, commits[0], e.files[i], specs[n]))
	}
	return errors.Join(errs...)
}

// fireSchedule starts the run of one scheduled workflow.
func (c *CI) fireSchedule(ctx context.Context, repo domain.Repo, def string, e *scheduleEntry, head domain.Commit, sw scheduledWorkflow, spec string) error {
	if repo.Archived {
		return nil
	}
	// The cache holds the disabled workflows too; the state is read now (and the
	// store checks it again when the run is stored).
	if off, err := c.workflowDisabled(ctx, repo.Name, sw.path); err != nil || off {
		return err
	}
	wf, perrs := parseWorkflow(sw.path, sw.yaml)
	if len(perrs) > 0 {
		return nil
	}
	facts := pushFacts{
		update: domain.RefUpdate{Ref: "refs/heads/" + def, Old: domain.ZeroOID, New: e.tip.String()}, commit: e.tip, head: head,
		defaultBranch: def, custom: &customEvent{name: eventSchedule, cron: spec},
	}
	// Whoever moved the default branch to this commit, with their trust
	// (ci_tips.go); the owner's name and no trust when that is not known.
	actor, pusher, trusted := c.scheduleActor(ctx, repo.Name, def, e.tip)
	t := ciTrigger{repo: repo.Name, actor: actor, pusher: pusher, trusted: trusted, at: c.now()}
	if problem := c.matrixProblem(ctx, repo, t, facts, sw.path, wf); problem.text != "" {
		name := sw.path
		if problem.named {
			name = workflowName(wf, sw.path)
		}
		return c.startInvalid(ctx, repo, t, facts, sw.path, name, problem.text)
	}
	_, err := c.launchRun(ctx, repo, t, facts, sw.path, string(sw.yaml), wf)
	if errors.Is(err, domain.ErrWorkflowDisabled) {
		return nil
	}
	return err
}
