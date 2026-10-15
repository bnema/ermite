// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Defaults of the CI engine.
const (
	// DefaultCIPollTimeout is how long FetchJob holds a call: the contract's 50 s.
	DefaultCIPollTimeout = 50 * time.Second
	// DefaultCITick is how often the watchdog looks at the jobs in flight.
	DefaultCITick = time.Second
	// DefaultCIKeepalive is how often WatchJob sends a keepalive.
	DefaultCIKeepalive = 30 * time.Second
	// DefaultCITriggerQueue is the size of the queue of pushes to plan.
	DefaultCITriggerQueue = 256
	// DefaultCIRemoteConcurrency is the git ls-remote processes ResolveActions
	// runs at once, whatever the number of jobs that resolve.
	DefaultCIRemoteConcurrency = 4
	// DefaultCIResolveTimeout bounds one ResolveActions call.
	DefaultCIResolveTimeout = 60 * time.Second
	// DefaultCIConcurrencyPruneBatch is the stale marks of a concurrency group a
	// transaction removes at most.
	DefaultCIConcurrencyPruneBatch = 256
	// MaxCIChecks is the most checks ListChecks returns.
	MaxCIChecks = 1000
	// MaxCILogStreams and MaxCILogStreamsPerClient bound the concurrent log
	// streams (GetJobLog).
	MaxCILogStreams          = 64
	MaxCILogStreamsPerClient = 8
	// maxCIWorkflowBytes is the largest workflow file (the parser's limit).
	maxCIWorkflowBytes = 1 << 20
	// maxCIWorkflowFiles bounds the workflow files read per push.
	maxCIWorkflowFiles = 256
)

// CIConfig holds what the CI use cases need. Store, Logs, Summaries, Meta, Repos, Reader,
// Git, Diff, Bus, Clock, Owner and PublicURL are required (the config supplies
// the defaults of Owner and Retention).
type CIConfig struct {
	Store ports.CIStore
	Logs  ports.CILogStore
	// Summaries keeps the job summaries, deleted with the logs of their run.
	Summaries ports.CISummaryStore
	Meta      ports.RepoMetaStore
	Repos     ports.GitRepos
	Reader    ports.RepoReader
	Git       ports.MergeGit
	Diff      ports.CIGit
	Bus       ports.EventBus
	Clock     ports.Clock
	// Auth re-validates the token of a log stream; nil skips it.
	Auth ports.Revalidator
	// Remote lists the refs of github.com repositories for ResolveActions; nil
	// answers every github.com action with unavailable.
	Remote ports.GitRemote
	// Archive serves the tarballs of the REST subset.
	Archive ports.GitArchive
	// Tokens finds the stored token of the pusher of a run, to ask what it may
	// read when an action of another repository is resolved for the run. Nil
	// reads nothing for a token (fail closed).
	Tokens ports.TokenStore
	// Settings resolves the variables and secrets of a job (CI-3). Nil gives
	// jobs none, and an empty "vars" context. Leave it unset rather than
	// assigning a typed nil pointer.
	Settings ports.CIJobSettings
	// RemoteConcurrency bounds the git ls-remote processes at once (default
	// DefaultCIRemoteConcurrency); ResolveTimeout bounds one ResolveActions call
	// (default DefaultCIResolveTimeout).
	RemoteConcurrency int
	ResolveTimeout    time.Duration
	// Owner is the owner login of the github context.
	Owner string
	// PublicURL is the base URL of the instance (github.server_url).
	PublicURL string
	// Retention is how long runs, jobs, checks and logs are kept.
	Retention time.Duration

	// The durations below default to the contract's; tests shorten them.
	PollTimeout       time.Duration
	Tick              time.Duration
	Keepalive         time.Duration
	AckTimeout        time.Duration
	HeartbeatDeadline time.Duration
	CancelGrace       time.Duration
	TimeoutSlack      time.Duration
	QueueTimeout      time.Duration
	TriggerQueue      int
	// ConcurrencyRetries bounds the retries of a delivery of the concurrency
	// groups that failed (a cancel, the planning of a promoted entry): 0 is the
	// default (8), a negative number none, which the tests use to leave the
	// recovery to the reconciliation.
	ConcurrencyRetries int
	// ConcurrencyPruneBatch bounds the stale marks of a concurrency group one
	// transaction removes (default 256, far below the size limit of a kv batch);
	// the others go with the next decisions and the reconciliation.
	ConcurrencyPruneBatch int
}

func (c CIConfig) withDefaults() CIConfig {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&c.PollTimeout, DefaultCIPollTimeout)
	def(&c.Tick, DefaultCITick)
	def(&c.Keepalive, DefaultCIKeepalive)
	def(&c.AckTimeout, domain.CIAckTimeout)
	def(&c.HeartbeatDeadline, domain.CIHeartbeatDeadline)
	def(&c.CancelGrace, domain.CICancelGrace)
	def(&c.TimeoutSlack, domain.CITimeoutSlack)
	def(&c.QueueTimeout, domain.CIQueueTimeout)
	def(&c.Retention, domain.CIRetentionDays*24*time.Hour)
	if c.RemoteConcurrency <= 0 {
		c.RemoteConcurrency = DefaultCIRemoteConcurrency
	}
	def(&c.ResolveTimeout, DefaultCIResolveTimeout)
	if c.TriggerQueue <= 0 {
		c.TriggerQueue = DefaultCITriggerQueue
	}
	if c.ConcurrencyPruneBatch <= 0 {
		c.ConcurrencyPruneBatch = DefaultCIConcurrencyPruneBatch
	}
	return c
}

// CI is the CI use cases: the workflow trigger, the planner, the assignment of
// jobs to runners, the runner protocol and the owner side (ADR 0025).
//
// Every change of a run goes through its lock (withRun): the records of a run
// are read, changed and written again by one goroutine at a time, so that two
// calls (a runner's update and an owner's cancel) never lose one another's
// write. Assignment takes the assignment lock before the lock of the run.
type CI struct {
	cfg   CIConfig
	boot  time.Time
	queue chan ciTrigger

	stop context.CancelFunc
	ctx  context.Context
	wg   sync.WaitGroup

	remoteSlots chan struct{} // slots of the git ls-remote processes
	locks       sync.Map      // run id -> *sync.Mutex
	assign      sync.Mutex
	jobs        notifier // woken when a job changes: key is the job reference
	pending     notifier // woken when a job may be ready or a runner was removed
	flows       *wfCache

	mu         sync.Mutex
	heartbeats map[string]time.Time // job reference -> last heartbeat
	seen       map[string]time.Time // runner id -> last seen
	polls      map[string]pollRecord
	// archiveTokens are the secrets of the tokens given for the archives of
	// ermite actions, per job reference then "<repository>@<sha>", so that a job
	// that resolves the same commit twice gets one token (memory only: a
	// restart issues new ones).
	archiveTokens map[string]map[string]string
	streams       int
	perClient     map[string]int
	closed        bool
}

var (
	_ ports.RunnerService   = (*CI)(nil)
	_ ports.RunService      = (*CI)(nil)
	_ ports.WorkflowTrigger = (*CI)(nil)
	_ ports.CIChecksReader  = (*CI)(nil)
)

// pollRecord remembers the assignment a poll made, for the retry of a lost
// response (FetchJobRequest.poll_id). The token is kept in memory only.
type pollRecord struct {
	pollID  string
	ref     string
	token   string
	ghToken string
	hash    domain.TokenHash
}

// NewCI builds the CI use cases. ctx carries the logger; its cancellation is
// not followed: call Start to run the workers and Close to stop them.
func NewCI(ctx context.Context, cfg CIConfig) *CI {
	if cfg.Owner == "" {
		panic("usecase: CIConfig.Owner is required")
	}
	cfg = cfg.withDefaults()
	run, stop := context.WithCancel(context.WithoutCancel(ctx))
	return &CI{
		cfg: cfg, boot: cfg.Clock.Now(), queue: make(chan ciTrigger, cfg.TriggerQueue),
		ctx: run, stop: stop, flows: newWfCache(64), remoteSlots: make(chan struct{}, cfg.RemoteConcurrency),
		heartbeats: map[string]time.Time{}, seen: map[string]time.Time{}, polls: map[string]pollRecord{},
		archiveTokens: map[string]map[string]string{},
		perClient:     map[string]int{},
	}
}

// Start runs the trigger worker and the watchdog, and plans again the runs
// that were open when the server stopped.
func (c *CI) Start() {
	c.wg.Add(2)
	go c.triggerLoop()
	go c.watchdog()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.resume()
	}()
}

// Close stops the workers and waits for them.
func (c *CI) Close(_ context.Context) {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.stop()
	c.wg.Wait()
}

func (c *CI) now() time.Time { return c.cfg.Clock.Now().UTC() }

// --- identifiers and secrets ---

func newToken(prefix string) string {
	var raw [32]byte
	_, _ = rand.Read(raw[:])
	return prefix + base64.RawURLEncoding.EncodeToString(raw[:])
}

func newRunnerID() string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

func jobRef(j domain.CIJob) string { return fmt.Sprintf("%d/%d/%s", j.RunID, j.RunAttempt, j.ID) }

// --- locks and notifications ---

// notifier wakes the goroutines waiting on a key. A channel exists only while
// somebody waits on it.
type notifier struct {
	mu sync.Mutex
	ch map[string]chan struct{}
}

// wait returns a channel closed by the next notify of key.
func (n *notifier) wait(key string) <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ch == nil {
		n.ch = map[string]chan struct{}{}
	}
	c, ok := n.ch[key]
	if !ok {
		c = make(chan struct{})
		n.ch[key] = c
	}
	return c
}

func (n *notifier) notify(key string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if c, ok := n.ch[key]; ok {
		close(c)
		delete(n.ch, key)
	}
}

const queueKey = "q"

func (c *CI) wakeQueue() { c.pending.notify(queueKey) }

func (c *CI) runLock(runID int64) *sync.Mutex {
	m, _ := c.locks.LoadOrStore(runID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// --- run state ---

// runState is a run being changed under its lock: the run, the jobs of one of
// its attempts, and what must be written. flush writes everything in one batch
// and publishes the events.
type runState struct {
	c       *CI
	run     domain.CIRun
	attempt int32
	jobs    []domain.CIJob
	src     *domain.CIRunSource

	runDirty bool
	dirty    map[string]bool
	// chk marks jobs whose check must be written even when the job itself did
	// not change (a rerun moves every check to the new attempt).
	checks map[string]bool
	// tokens are the job repository tokens to create, revoke the jobs whose
	// tokens are deleted, with the next flush.
	tokens []domain.CIRepoToken
	revoke []ports.CIJobRef
	// resolutions are the action resolutions to keep with the next flush.
	resolutions []domain.CIActionResolution
	// annotations replace the annotations of steps, clear drops all those of
	// executions of jobs, with the next flush.
	annotations []ports.CIStepAnnotations
	clear       []ports.CILogRef
}

// withRun locks the run, loads it with the jobs of attempt (0 for the latest)
// and calls fn. The run is found by id alone through its source.
func (c *CI) withRun(ctx context.Context, runID int64, attempt int32, fn func(rs *runState) error) error {
	lock := c.runLock(runID)
	lock.Lock()
	defer lock.Unlock()
	rs, err := c.loadRun(ctx, runID, attempt)
	if err != nil {
		return err
	}
	return fn(rs)
}

func (c *CI) loadRun(ctx context.Context, runID int64, attempt int32) (*runState, error) {
	src, err := c.cfg.Store.GetRunSource(ctx, runID)
	if err != nil {
		return nil, err
	}
	run, err := c.cfg.Store.GetRun(ctx, src.Repo, runID)
	if err != nil {
		return nil, err
	}
	if attempt <= 0 {
		attempt = run.Attempt
	}
	jobs, err := c.cfg.Store.ListJobs(ctx, runID, attempt)
	if err != nil {
		return nil, err
	}
	return &runState{c: c, run: run, attempt: attempt, jobs: jobs, src: &src, dirty: map[string]bool{}, checks: map[string]bool{}}, nil
}

// latest reports whether the state holds the latest attempt of the run.
func (rs *runState) latest() bool { return rs.attempt == rs.run.Attempt }

func (rs *runState) find(id string) *domain.CIJob {
	for i := range rs.jobs {
		if rs.jobs[i].ID == id {
			return &rs.jobs[i]
		}
	}
	return nil
}

func (rs *runState) touch(j *domain.CIJob) { rs.dirty[j.ID] = true }

// checkOf builds the check of a job.
func (rs *runState) checkOf(j domain.CIJob) domain.CICheck {
	return domain.CICheck{
		ID: j.ID, Repo: rs.run.Repo, Sha: rs.run.Sha, Name: j.Name, Status: j.Status(), Conclusion: j.Conclusion,
		RunID: rs.run.ID, JobID: j.ID, Attempt: rs.run.Attempt, WorkflowName: rs.run.WorkflowName,
		CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, CompletedAt: j.CompletedAt,
	}
}

// flush recomputes the status of the run, writes the changed jobs, their
// checks and the run in one batch, and publishes RunUpdated and JobUpdated.
func (rs *runState) flush(ctx context.Context) error {
	c := rs.c
	now := c.now()
	var w ports.CIWrite
	var changedJobs []int
	for i := range rs.jobs {
		if rs.dirty[rs.jobs[i].ID] {
			w.Jobs = append(w.Jobs, rs.jobs[i])
			changedJobs = append(changedJobs, i)
		}
	}
	if rs.latest() {
		for i := range rs.jobs {
			if rs.dirty[rs.jobs[i].ID] || rs.checks[rs.jobs[i].ID] {
				w.Checks = append(w.Checks, rs.checkOf(rs.jobs[i]))
			}
		}
		if next := rs.recompute(now); next != rs.run {
			rs.run, rs.runDirty = next, true
		}
	}
	if rs.runDirty {
		w.Runs = []domain.CIRun{rs.run}
	}
	w.RepoTokens, w.RevokeJobs, w.Resolutions = rs.tokens, rs.revoke, rs.resolutions
	w.Annotations, w.ClearAnnotations = rs.annotations, rs.clear
	if len(w.Jobs) == 0 && len(w.Runs) == 0 && len(w.Checks) == 0 && len(w.RepoTokens) == 0 && len(w.RevokeJobs) == 0 &&
		len(w.Resolutions) == 0 && len(w.Annotations) == 0 && len(w.ClearAnnotations) == 0 {
		return nil
	}
	v, err := c.cfg.Store.Apply(ctx, w)
	if err != nil {
		return fmt.Errorf("write run %d: %w", rs.run.ID, err)
	}
	for _, i := range changedJobs {
		rs.jobs[i].Version = v
	}
	if rs.runDirty {
		rs.run.Version = v
	}
	changedRun := rs.runDirty
	rs.releaseEnded(ctx, changedJobs, changedRun && rs.run.Status == domain.CICompleted)
	rs.dirty, rs.checks, rs.runDirty = map[string]bool{}, map[string]bool{}, false
	rs.tokens, rs.revoke, rs.resolutions = nil, nil, nil
	rs.annotations, rs.clear = nil, nil

	repo, err := c.cfg.Meta.Get(ctx, rs.run.Repo)
	if err != nil {
		for _, i := range changedJobs {
			c.jobs.notify(jobRef(rs.jobs[i]))
		}
		return nil // the repository is gone: nobody to tell
	}
	for _, i := range changedJobs {
		j := rs.jobs[i]
		lines, _ := c.cfg.Logs.Count(ctx, ports.CILogRef{RunID: j.RunID, JobID: j.ID, Attempt: j.Attempt})
		c.cfg.Bus.Publish(ctx, domain.JobUpdated{Job: j, Lines: lines, At: now, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt})
		c.jobs.notify(jobRef(j))
		if j.Phase == domain.CIPhaseReady {
			c.wakeQueue()
		}
	}
	if changedRun {
		c.cfg.Bus.Publish(ctx, domain.RunUpdated{Run: rs.run, At: now, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt})
	}
	return nil
}

// recompute returns the run with the status, the conclusion and the
// timestamps its jobs imply.
func (rs *runState) recompute(now time.Time) domain.CIRun {
	run := rs.run
	if run.Status == domain.CICompleted && len(rs.jobs) == 0 {
		return run // an invalid workflow file: failed from the start
	}
	allEnded := len(rs.jobs) > 0
	started := time.Time{}
	var failed, cancelled bool
	for _, j := range rs.jobs {
		if !j.Ended() {
			allEnded = false
		}
		// A job kept from an earlier attempt did not start in this one.
		if j.Attempt == rs.attempt && !j.StartedAt.IsZero() && (started.IsZero() || j.StartedAt.Before(started)) {
			started = j.StartedAt
		}
		switch {
		case j.Conclusion.IsFailure() && j.ContinueOnError:
			// continue-on-error: the job failed, the run does not.
		case j.Conclusion.IsFailure():
			failed = true
		case j.Conclusion == domain.CICancelled:
			cancelled = true
		}
	}
	if run.StartedAt.IsZero() && !started.IsZero() {
		run.StartedAt = started
	}
	switch {
	case allEnded:
		run.Status, run.CompletedAt = domain.CICompleted, now
		switch {
		case run.CancelRequested:
			run.Conclusion = domain.CICancelled
		case failed:
			run.Conclusion = domain.CIFailure
		case cancelled:
			run.Conclusion = domain.CICancelled
		default:
			run.Conclusion = domain.CISuccess
		}
		if rs.run.Status == domain.CICompleted {
			run.CompletedAt = rs.run.CompletedAt
		}
	case !run.StartedAt.IsZero():
		run.Status, run.Conclusion, run.CompletedAt = domain.CIInProgress, domain.CINoConclusion, time.Time{}
	default:
		run.Status, run.Conclusion, run.CompletedAt = domain.CIQueued, domain.CINoConclusion, time.Time{}
	}
	return run
}

// --- access ---

// readableRepo looks the repository up and applies the read rule.
func (c *CI) readableRepo(ctx context.Context, p domain.Principal, name domain.RepoName) (domain.Repo, error) {
	repo, err := lookupRepo(ctx, c.cfg.Meta, name)
	if err != nil {
		return domain.Repo{}, err
	}
	if !canRead(p, repo) {
		return domain.Repo{}, domain.ErrNotFound
	}
	return repo, nil
}

// writableRepo is the access rule of cancelling and re-running: a principal
// whose token holds ci:write on the repository sees the repository for this
// action even without repo:read, as for merge requests.
func (c *CI) writableRepo(ctx context.Context, p domain.Principal, name domain.RepoName) (domain.Repo, error) {
	repo, err := lookupRepo(ctx, c.cfg.Meta, name)
	if err != nil {
		return domain.Repo{}, err
	}
	if !canRead(p, repo) && !p.Allows(domain.ScopeCIWrite, name) {
		return domain.Repo{}, domain.ErrNotFound
	}
	if err := scopeAccess(p, repo, domain.ScopeCIWrite); err != nil {
		return domain.Repo{}, err
	}
	return repo, nil
}

// runWriteAccess is the branch rule of the write actions on a run, cancel and
// re-run, which come after the ci:write check. A run is tied to the ref that
// made it, and re-running it executes the workflow of that ref again while
// cancelling it stops the checks of that ref. So, like a push, a merge or a
// branch delete, these actions fall under the branch prefixes of a limited
// token (ADR 0027): it may act only on the runs of branches under its prefixes,
// and never on a run of a tag or of any other ref. Without prefixes, every run.
//
// Reading runs, jobs, logs and checks is not a write: it needs only the access
// to read the repository, as for the code, whatever the prefixes. The caller
// has seen the run by then, so the refusal is forbidden, not not found.
func runWriteAccess(p domain.Principal, run domain.CIRun) error {
	if !p.AllowsRef(run.Ref) {
		return fmt.Errorf("%w: this token may not write %s", domain.ErrForbidden, run.Ref)
	}
	return nil
}

// canManageRunners is the access rule of runners: runner:register on a token
// that is not restricted to repositories (a runner serves every repository),
// or root. A token limited to branch prefixes never manages runners, whatever
// scopes it holds: a runner runs the workflows of every branch of every
// repository, with their code and secrets, so registering or removing one
// reaches far beyond the prefixes (a replaced runner could also be a way to
// take over the jobs of main). Like repository administration and owner
// rights, it is out of reach of a limited token.
func canManageRunners(p domain.Principal) error {
	switch {
	case p.IsAnonymous():
		return domain.ErrUnauthenticated
	case !p.HasScope(domain.ScopeRunnerRegister):
		return fmt.Errorf("%w: this needs the %s scope", domain.ErrForbidden, domain.ScopeRunnerRegister)
	case len(p.Repos()) > 0:
		return fmt.Errorf("%w: a runner serves every repository: the token must not be restricted to some", domain.ErrForbidden)
	case p.IsBranchLimited():
		return fmt.Errorf("%w: a token limited to branch prefixes may not manage runners", domain.ErrForbidden)
	}
	return nil
}

// --- small helpers ---

func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// cutText cuts s to n bytes at a character boundary.
func cutText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// ctxLog returns the logger of the context, addressable so that the level
// methods can be chained on it.
func ctxLog(ctx context.Context) *zerowrap.Logger {
	l := zerowrap.FromCtx(ctx)
	return &l
}

// logRun returns the logger of the context with the fields of the run.
func logRun(ctx context.Context, run domain.CIRun) zerowrap.Logger {
	return zerowrap.FromCtx(ctx).WithFields(map[string]any{"repo": run.Repo.String(), "run_id": run.ID})
}

func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	return strings.Trim(s, "0123456789abcdef") == ""
}
