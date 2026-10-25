// SPDX-License-Identifier: AGPL-3.0-or-later

package ports

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"bnema.dev/ermite/internal/domain"
)

// CIRunDraft is what CreateRun stores: a run with its source, its jobs and
// their checks, built once the store has chosen the ids.
type CIRunDraft struct {
	Run    domain.CIRun
	Source domain.CIRunSource
	Jobs   []domain.CIJob
	Checks []domain.CICheck
}

// CIWrite is a set of records written in one atomic batch. A run or a job with
// Version 0 must not exist yet; any other must be unchanged since it was read.
// Checks are derived from the jobs and upserted without a precondition.
// Apply returns domain.ErrConflict otherwise and domain.ErrNotFound when the
// repository no longer exists.
//
// RepoTokens are job repository tokens created with the batch (they must not
// exist yet), RevokeJobs the jobs whose repository tokens are deleted by it,
// and Resolutions the action resolutions of a run attempt to keep. They are
// written under the lock of the run that owns them.
//
// Annotations replace, for each (execution of a job, step) they name, all the
// annotations stored for it (none deletes them); ClearAnnotations deletes all
// the annotations of the executions of jobs they name, steps included. Both
// are written with the batch, so a call is applied whole or not at all.
type CIWrite struct {
	Runs             []domain.CIRun
	Jobs             []domain.CIJob
	Checks           []domain.CICheck
	RepoTokens       []domain.CIRepoToken
	RevokeJobs       []CIJobRef
	Resolutions      []domain.CIActionResolution
	Annotations      []CIStepAnnotations
	ClearAnnotations []CILogRef
}

// CIConcurrencyGroup is a stored concurrency group: its repository, its key (the
// group, canonical: ConcurrencyKey), its record and its marks.
type CIConcurrencyGroup struct {
	Repo  domain.RepoName
	Group string
	State domain.CIConcurrencyGroupState
}

// CIStepAnnotations are all the annotations of one step of one execution of a
// job, in the order the step produced them.
type CIStepAnnotations struct {
	Repo domain.RepoName
	Exec CILogRef
	Step int32
	// Items are the annotations; none removes those of the step.
	Items []domain.CIAnnotation
}

// CIJobRef names one execution of a job.
type CIJobRef struct {
	RunID      int64
	RunAttempt int32
	JobID      string
}

// CIRepoTokenStore reads the job repository tokens, for the authentication of
// a request. They are written by CIStore.Apply. *kvstore.CI implements it.
type CIRepoTokenStore interface {
	// FindRepoTokenByHash returns the token whose secret has that hash, or
	// domain.ErrNotFound.
	FindRepoTokenByHash(ctx context.Context, hash domain.TokenHash) (domain.CIRepoToken, error)
	// GetRepoToken returns the token with that id, or domain.ErrNotFound.
	GetRepoToken(ctx context.Context, id string) (domain.CIRepoToken, error)
}

// CIRefTipStore keeps, per branch or tag (full ref name), who moves it to its
// object (domain.CIRefTip). The record is written before the ref moves.
// *kvstore.CI implements it; the records go with the repository.
type CIRefTipStore interface {
	// PutRefTip stores the record of a ref, replacing the earlier one. A
	// repository that does not exist is domain.ErrNotFound.
	PutRefTip(ctx context.Context, t domain.CIRefTip) error
	// GetRefTip returns the record of a ref, or domain.ErrNotFound.
	GetRefTip(ctx context.Context, repo domain.RepoName, ref string) (domain.CIRefTip, error)
	// DeleteRefTip removes the record of a ref; a missing one is not an error.
	DeleteRefTip(ctx context.Context, repo domain.RepoName, ref string) error
	// DeleteRefTipIf removes the record of a ref only when it still names
	// sha; a missing or different one is left alone and is not an error.
	DeleteRefTipIf(ctx context.Context, repo domain.RepoName, ref, sha string) error
}

// CIWorkflowStore keeps, per repository, the workflow files that are disabled
// (`ermite workflow disable`). Paths are normalized (domain.ParseWorkflowPath);
// the file does not have to exist. The records go with the repository.
// *kvstore.CI implements it.
type CIWorkflowStore interface {
	// DisableWorkflow records the path as disabled; one already disabled is
	// left as it is. A repository that does not exist is domain.ErrNotFound.
	DisableWorkflow(ctx context.Context, repo domain.RepoName, path string, at time.Time) error
	// EnableWorkflow removes the record; a path that is not disabled is not an
	// error.
	EnableWorkflow(ctx context.Context, repo domain.RepoName, path string) error
	// DisabledWorkflows returns the disabled paths of the repository, sorted.
	DisabledWorkflows(ctx context.Context, repo domain.RepoName) ([]string, error)
}

// CIStore keeps CI runs, jobs and checks. The store derives its own indexes
// (the queue of ready jobs, the active jobs, the open runs, the job tokens)
// from the records it writes.
type CIStore interface {
	// CreateRun takes the next run id and the next number of the workflow in
	// the repository (both never reused), calls build with them and stores
	// the draft it returns, in one batch that fails with domain.ErrNotFound
	// once the repository is deleted, and with domain.ErrWorkflowDisabled when
	// the repository disabled workflowPath (CIWorkflowStore) when the batch
	// applies: the state is checked in that transaction, not before it, so a
	// disable can never be overtaken, whatever event started the run and
	// whether the run is a failed one for an invalid file or not. The records
	// come back with their versions.
	CreateRun(ctx context.Context, repo domain.RepoName, workflowPath string, build func(id, number int64) CIRunDraft) (CIRunDraft, error)
	// Apply writes the records in one batch and returns the version they all
	// have afterwards.
	Apply(ctx context.Context, w CIWrite) (version uint64, err error)
	// GetRun returns the run, or domain.ErrNotFound.
	GetRun(ctx context.Context, repo domain.RepoName, id int64) (domain.CIRun, error)
	// GetActionResolution returns the resolution kept for the key in the run
	// attempt, or domain.ErrNotFound.
	GetActionResolution(ctx context.Context, runID int64, runAttempt int32, key string) (domain.CIActionResolution, error)
	// GetRunSource returns what the run was planned from, or
	// domain.ErrNotFound. It is how a run is found from its id alone.
	GetRunSource(ctx context.Context, runID int64) (domain.CIRunSource, error)
	// ListRuns returns the runs of the repository with an id below before (0
	// for no bound), newest first, at most limit.
	ListRuns(ctx context.Context, repo domain.RepoName, before int64, limit int) ([]domain.CIRun, error)
	// RunIDs returns the ids of the runs of the repository.
	RunIDs(ctx context.Context, repo domain.RepoName) ([]int64, error)
	// ListOpenRuns returns the runs that have not completed.
	ListOpenRuns(ctx context.Context) ([]domain.CIRun, error)
	// JobAnnotations returns the annotations of one execution of a job
	// (CIJob.Attempt), ordered by step then by the order the step produced
	// them. A job without annotation gives an empty list.
	JobAnnotations(ctx context.Context, runID int64, attempt int32, jobID string) ([]domain.CIAnnotation, error)
	// GetJob returns a job of a run at an attempt of the run, or
	// domain.ErrNotFound.
	GetJob(ctx context.Context, runID int64, runAttempt int32, jobID string) (domain.CIJob, error)
	// ListJobs returns the jobs of a run at an attempt, ordered by index then
	// combination.
	ListJobs(ctx context.Context, runID int64, runAttempt int32) ([]domain.CIJob, error)
	// FindJobByToken returns the job whose current token has that hash and
	// whose token is still valid at now, or domain.ErrNotFound.
	FindJobByToken(ctx context.Context, hash domain.TokenHash, now time.Time) (domain.CIJob, error)
	// ListReadyJobs returns the jobs that wait for a runner, oldest first, at
	// most limit.
	ListReadyJobs(ctx context.Context, limit int) ([]domain.CIJob, error)
	// ListActiveJobs returns the jobs given to a runner and not over.
	ListActiveJobs(ctx context.Context) ([]domain.CIJob, error)
	// ListChecks returns the checks of a commit, at most limit; more is true
	// when there are more.
	ListChecks(ctx context.Context, repo domain.RepoName, sha string, limit int) (checks []domain.CICheck, more bool, err error)
	// UpdateConcurrency reads a concurrency group of a repository (compared
	// with domain.ConcurrencyKey) with the marks of its entries, calls fn on a
	// copy and stores what fn leaves in it, in one batch: the record, the marks
	// that changed and the marks that fn removed. A group left with neither
	// record nor marks is removed. The batch carries the version precondition of
	// the group whatever fn does, and every change of the marks rewrites the
	// record, so no two transactions of a group both succeed on the same state:
	// on a conflict fn is called again on the new state and must be repeatable.
	// A deleted repository is domain.ErrNotFound.
	UpdateConcurrency(ctx context.Context, repo domain.RepoName, group string, fn func(*domain.CIConcurrencyGroupState) error) error
	// ListConcurrency returns every concurrency group that has an entry, for
	// the reconciliation at startup.
	ListConcurrency(ctx context.Context) ([]CIConcurrencyGroup, error)
	// Sweep removes the runs completed before the time, with their jobs,
	// checks and sources (not their numbers), and returns their ids so that
	// the logs can go too.
	Sweep(ctx context.Context, before time.Time) ([]int64, error)
	// CreateRunner stores the runner and the index of its token. A name in use
	// is domain.ErrAlreadyExists, or with replace the old runner is removed in
	// the same batch and returned.
	CreateRunner(ctx context.Context, r domain.CIRunner, replace bool) (removed []domain.CIRunner, err error)
	// GetRunner returns the runner, or domain.ErrNotFound.
	GetRunner(ctx context.Context, id string) (domain.CIRunner, error)
	// FindRunnerByToken returns the runner whose token has that hash, or
	// domain.ErrNotFound.
	FindRunnerByToken(ctx context.Context, hash domain.TokenHash) (domain.CIRunner, error)
	// ListRunners returns every runner ordered by name.
	ListRunners(ctx context.Context) ([]domain.CIRunner, error)
	// SetRunnerVersion records the version the runner runs now. It writes only
	// when the stored version differs, and returns domain.ErrNotFound for a
	// runner that is gone.
	SetRunnerVersion(ctx context.Context, id, version string) error
	// RemoveRunner removes the runner and its token, or returns
	// domain.ErrNotFound.
	RemoveRunner(ctx context.Context, id string) (domain.CIRunner, error)
}

// CIRunCleaner is what repository deletion needs from CI: the runs of the
// repository before its records go, and the removal of their logs and
// summaries after. *usecase.CI implements it.
type CIRunCleaner interface {
	RunIDs(ctx context.Context, repo domain.RepoName) ([]int64, error)
	RemoveLogs(ctx context.Context, runIDs []int64) error
	// ForgetRepo drops what CI keeps in memory for the deleted repository (its
	// cached schedules).
	ForgetRepo(repo domain.RepoName)
}

// CIChecksReader is the read side of the checks, for the merge rule and the
// pages that show them. *usecase.CI implements it.
type CIChecksReader interface {
	// CommitChecks returns the checks of a commit, without access control (the
	// caller has checked it). More than 1,000 is domain.ErrResourceExhausted,
	// not a part of them: a rule must not decide on some of the checks.
	CommitChecks(ctx context.Context, repo domain.RepoName, sha string) ([]domain.CICheck, error)
}

// CILogRef names one execution of a job: its log, its summaries and its
// annotations are kept under it.
type CILogRef struct {
	RunID   int64
	JobID   string
	Attempt int32
}

// ErrCILogLimit is returned by CILogStore.Append when the lines do not fit in
// the log limit; nothing was appended.
var ErrCILogLimit = errors.New("log limit reached")

// ErrCILogGap wraps domain.ErrOutOfRange when an offset is above the count.
type ErrCILogGap struct{ Count int64 }

// Error implements error.
func (e *ErrCILogGap) Error() string {
	return "out of range: the log holds lines up to the offset of " + strconv.FormatInt(e.Count, 10)
}

// Is makes errors.Is(err, domain.ErrOutOfRange) true.
func (e *ErrCILogGap) Is(target error) bool { return target == domain.ErrOutOfRange }

// CILogStore keeps the logs of the jobs: append-only files, one line per log
// line, never in the metadata store. Lines are numbered from 0.
type CILogStore interface {
	// Append appends the lines at offset (the number of the first line), with
	// the idempotence of AppendLog: lines already stored are skipped, a gap is
	// an *ErrCILogGap. It returns the number of lines after the call and how
	// many of the request were new. A log that would exceed limit bytes gets
	// nothing and ErrCILogLimit.
	Append(ctx context.Context, ref CILogRef, offset int64, lines []domain.CILogLine, limit int64) (next int64, added int, err error)
	// AppendForced appends lines whatever the limit (the last line of a log
	// that reached it).
	AppendForced(ctx context.Context, ref CILogRef, lines []domain.CILogLine) (next int64, err error)
	// Count returns the number of lines held, 0 for a log that does not exist.
	Count(ctx context.Context, ref CILogRef) (int64, error)
	// Read returns lines from the one numbered from, at most maxLines lines and
	// about maxBytes of text. It returns domain.ErrNotFound when the log does
	// not exist and domain.ErrOutOfRange when from is above the count.
	Read(ctx context.Context, ref CILogRef, from int64, maxLines, maxBytes int) ([]domain.CILogLine, error)
	// Changed returns a channel that is closed at the next append to the log.
	Changed(ref CILogRef) <-chan struct{}
	// Reset empties the log.
	Reset(ctx context.Context, ref CILogRef) error
	// RemoveRuns deletes the logs of the runs.
	RemoveRuns(ctx context.Context, runIDs []int64) error
	// RunIDs lists the runs that have a log directory, for the orphan sweep.
	RunIDs(ctx context.Context) ([]int64, error)
}

// CISummaryStore keeps the job summaries (the Markdown a step wrote to
// GITHUB_STEP_SUMMARY): one file per step of an execution of a job, never in
// the metadata store.
type CISummaryStore interface {
	// Put stores the summary of a step, replacing the one it has; the file
	// appears whole or not at all. An empty markdown removes the summary.
	Put(ctx context.Context, ref CILogRef, step int32, markdown string) error
	// List returns the summaries of the steps that have one, by step number,
	// at most limit; more is true when there are others. With maxBytes above
	// 0 the files are sized (stat) before they are read, and a summary that
	// would take the total of the Markdown read past maxBytes is returned
	// Omitted, unread, as are all the ones after it: a call never reads more
	// than maxBytes.
	List(ctx context.Context, ref CILogRef, limit int, maxBytes int64) (summaries []domain.CIStepSummary, more bool, err error)
	// RemoveJob deletes the summaries of one execution of a job.
	RemoveJob(ctx context.Context, ref CILogRef) error
	// RemoveRuns deletes the summaries of the runs.
	RemoveRuns(ctx context.Context, runIDs []int64) error
	// RunIDs lists the runs that have a summary directory, for the orphan
	// sweep.
	RunIDs(ctx context.Context) ([]int64, error)
}

// DispatchParams are the arguments of RunService.DispatchWorkflow.
type DispatchParams struct {
	// WorkflowPath is the path of the workflow file, or its file name.
	WorkflowPath string
	// Ref is a branch or a tag, short or full name; empty is the default branch.
	Ref string
	// Inputs are the values of the inputs by name, as text.
	Inputs map[string]string
}

// WorkflowInfo is a workflow file of a repository and whether it starts runs.
type WorkflowInfo struct {
	// Path is ".github/workflows/<file>".
	Path string
	// Name is the "name:" of the workflow at the tip of the default branch (the
	// path when it has none); empty when that file is absent or not a workflow.
	Name     string
	Disabled bool
}

// WorkflowTrigger starts the workflows that match pushed refs. OnPush only
// queues the work and returns: planning happens in the background and its
// errors never reach the push.
type WorkflowTrigger interface {
	OnPush(ctx context.Context, repo domain.RepoName, updates []domain.RefUpdate, pusher domain.Principal)
	// OnMerge is OnPush for the move of a target branch by a merge. GitHub shows
	// the owner as the actor of the push of a merge, whoever merged; but the
	// rights the run's actions are resolved with are those of merger, the
	// principal that merged, never the owner's.
	OnMerge(ctx context.Context, repo domain.RepoName, updates []domain.RefUpdate, merger domain.Principal)
}

// PullRequestTrigger starts the pull_request workflows of the merge requests
// (CI-3, D5), like WorkflowTrigger does for pushes. It only queues the work.
// The synchronize activity needs no call here: the push of the source branch
// reaches WorkflowTrigger.OnPush.
type PullRequestTrigger interface {
	// OnPullRequest queues the activity ev of a merge request. p is who caused
	// it: its trust is the run's, whatever happens to the branches before the
	// event is handled. A job token starts no run.
	OnPullRequest(ctx context.Context, repo domain.RepoName, ev PullRequestEvent, p domain.Principal)
}

// PullRequestEvent is one activity of a merge request, with the commits it is
// for fixed when it happened: it runs for exactly these, with the trust of
// whoever caused it, even when the branches have moved on by the time it is
// handled.
type PullRequestEvent struct {
	Number int64
	// Action is domain.PullOpened or domain.PullClosed.
	Action string
	// Head is the head of the source branch and Base the tip of the target at
	// that moment; either may be zero when unknown (the current one is read).
	Head, Base domain.ObjectID
	// Merged is set on the closing a merge makes: its runs have the merger's
	// trust (usecase.CI checks the merge against the request).
	Merged bool
}

// PullGate lets the CI change what depends on a merge request staying open
// (its refs/pull refs, the runs of its opening and updates) without racing a
// close or a merge. *usecase.MergeRequests implements it.
type PullGate interface {
	// WhileOpen runs fn holding the merge lock of the repository (the one
	// Close and Merge take) when merge request number is open, and returns
	// domain.ErrPreconditionFailed without running fn when it is not. fn must
	// be short and must not call the merge request use cases.
	WhileOpen(ctx context.Context, repo domain.RepoName, number int64, fn func() error) error
}

// MergeGate serializes a change with the merges of a repository.
// *usecase.MergeRequests implements it.
// Lock order is merge lock → store (meta.Mutate / kv). Nothing holding a
// store transaction or a CI lock may take the merge lock. MergeRequests never
// calls Repos.Update.
type MergeGate interface {
	// WithMergeLock runs fn holding the repository merge lock (also taken by
	// Merge, Close and auto-merge). fn must be short and must not call the
	// merge request use cases.
	WithMergeLock(ctx context.Context, repo domain.RepoName, fn func() error) error
}

// PullGit is what the refs/pull/<n>/{head,merge} of the merge requests need
// from Git. Every method returns domain.ErrNotFound when the repository has no
// directory.
type PullGit interface {
	// TestMerge merges head into base without touching a ref (`git merge-tree
	// --write-tree`) and returns the tree of the result. clean is false when
	// the merge conflicts, or when the commits share no history.
	TestMerge(ctx context.Context, repo domain.RepoName, base, head domain.ObjectID) (tree domain.ObjectID, clean bool, err error)
	// SetPullRef points a ref of domain.IsPullRef to the commit. Any other ref
	// name is domain.ErrInvalid.
	SetPullRef(ctx context.Context, repo domain.RepoName, ref string, id domain.ObjectID) error
	// PullRef returns the commit a ref of domain.IsPullRef points to, or
	// domain.ErrNotFound.
	PullRef(ctx context.Context, repo domain.RepoName, ref string) (domain.ObjectID, error)
}

// CIGit is what planning needs from Git beyond RepoReader and MergeGit.
type CIGit interface {
	// ChangedPaths returns the paths changed from base to head (a zero base
	// means the empty tree), in the order of the diff and without renames
	// detection, at most max; truncated is true when there were more.
	ChangedPaths(ctx context.Context, repo domain.RepoName, base, head domain.ObjectID, max int) (paths []string, truncated bool, err error)
	// CommitsOffBranches returns the commits reachable from head and from no
	// branch (refs/heads/*; tags do not count), newest first, at most limit
	// (1 to MaxCommitLimit); more is true when there are others. It is empty
	// for a commit that a branch already has.
	CommitsOffBranches(ctx context.Context, repo domain.RepoName, head domain.ObjectID, limit int) (commits []domain.Commit, more bool, err error)
}

// PushObjects tells where the objects of a push in progress are. Git receives a
// push into a quarantine directory and moves the objects into the repository
// only when pre-receive accepts it, so the objects a pre-receive check reads are
// not in the repository yet. Both fields are what git's own environment holds
// in the hook (GIT_OBJECT_DIRECTORY and GIT_ALTERNATE_OBJECT_DIRECTORIES). The
// zero value is a push with nothing in quarantine.
type PushObjects struct {
	Dir        string
	Alternates string
}

// PushTreeReader reads, for a push still in quarantine, the objects the
// workflow policy (domain.WorkflowChangeOf) judges. It knows nothing of the
// policy: it peels, classifies, walks and compares trees.
type PushTreeReader interface {
	// PushSubtrees reads what a push brings, and the entry at path (a
	// directory path without leading or trailing slash, e.g.
	// domain.WorkflowDir) in each object concerned.
	//
	// Updates that delete (New == ZeroOID) have no objects: they are skipped.
	// For every other update, Old (unless ZeroOID) and New are peeled
	// (`<oid>^{}`): the result is in Ends keyed by the oid as given. An end
	// that is missing, malformed, or peels to neither commit, tree nor blob is
	// an error matching domain.ErrInvalid.
	//
	// Commits is `rev-list --parents <tips> --not --all` where tips are the
	// distinct New ends that peel to a commit: the commits no ref has yet,
	// each with its parents, in rev-list order. More than bounds.Commits
	// commits, more than bounds.IDs ids (commits and parents, repeats
	// included) or a commit whose parent line exceeds the adapter's line
	// buffer is an error matching domain.ErrInvalid, raised as soon as the
	// line crossing the bound is read.
	//
	// Subtrees holds the entry at path for every end, every commit, every
	// parent, and, when a create has a non-blob New, the default branch head
	// (key domain.HeadKey, absent when HEAD is unborn or has no such entry). An object that peels to a blob, or an
	// object with no entry at path (including a path that crosses a
	// gitlink, symlink or file), has no key. Kind is EntryDirectory only for
	// mode 040000 and type tree.
	//
	// objects are the quarantine directories of the push; one outside the
	// repository's objects directory (symlinks resolved) is an error matching
	// domain.ErrInvalid, checked before anything is read.
	PushSubtrees(ctx context.Context, repo domain.RepoName, updates []domain.RefUpdate, objects PushObjects, path string, bounds domain.PushWalkBounds) (domain.PushSubtrees, error)

	// FirstDifferentEntry returns the name of the first top-level entry that
	// differs between the trees old and new (`diff-tree --name-only -z
	// --no-renames`, read as a stream up to the first name, then git is
	// stopped). A zero old is the empty tree. It returns "" when nothing
	// differs. A name longer than the adapter's path bound is an error
	// matching domain.ErrInvalid. objects as above.
	FirstDifferentEntry(ctx context.Context, repo domain.RepoName, objects PushObjects, old, new domain.ObjectID) (name string, err error)
}

// --- inbound ---

// RegisterRunnerParams describe a runner to register.
type RegisterRunnerParams struct {
	Name      string
	Labels    []string
	Version   string
	Isolation domain.CIIsolation
	Arch, OS  string
	Replace   bool
}

// RegisteredRunner is a new runner with its secret, shown once.
type RegisteredRunner struct {
	Runner domain.CIRunner
	Token  string
}

// RunnerJob is a job given to a runner.
type RunnerJob struct {
	Job domain.CIJob
	// Deps identifies a dependency update job; nil for workflow jobs.
	Deps  *domain.CIDepsJob
	Token string
	// GithubToken is the job repository token, the "github.token" of the job.
	GithubToken  string
	Repo         domain.RepoName
	Sha, Ref     string
	WorkflowPath string
	WorkflowYAML string
	// Environment is the environment the job deploys to, "" for none.
	Environment string
	// Vars are the merged variables of the job (environment over repository
	// over instance), already in its "vars" context.
	Vars map[string]string
	// Secrets are the secrets the job refers to, in plain text: empty when the
	// run is not trusted, when the job refers to none and when the server has
	// no key. They go to the runner and nowhere else: never into the run
	// record, the github context or a log.
	Secrets map[string]string
	// Trusted is the trusted flag of the run: secrets were allowed.
	Trusted bool
	// Permissions are the permissions of GithubToken (key to level), after the
	// trust rule: no write when the run is not trusted.
	Permissions map[string]string
	// Cache is the scope of the job in the runner's cache (Job.cache).
	Cache JobCache
	// IDTokenURL is the token endpoint of the job's OIDC ID tokens and
	// IDTokenRequestToken its bearer (Job.id_token_url and
	// id_token_request_token); both "" when the job may not have ID tokens.
	IDTokenURL          string
	IDTokenRequestToken string
}

// JobCache is what a job may do in the cache the runner serves: save entries
// under WriteRef only ("" for none) and restore those of ReadRefs, most
// specific first. Repository is "owner/name".
type JobCache struct {
	Repository string
	WriteRef   string
	ReadRefs   []string
}

// String describes the job without its secrets and tokens, so that a job
// printed by mistake leaks nothing.
func (j RunnerJob) String() string {
	return fmt.Sprintf("RunnerJob{job:%s run:%d repo:%s secrets:%d trusted:%t}", j.Job.ID, j.Job.RunID, j.Repo, len(j.Secrets), j.Trusted)
}

// GoString is String for %#v.
func (j RunnerJob) GoString() string { return j.String() }

// FetchJobParams selects the job a runner asks for.
type FetchJobParams struct {
	// Kinds are the supported job kinds; empty means workflow only.
	Kinds  []string
	Labels []string
	PollID string
	// Version is the version of the running runner, "" from an older runner.
	Version string
}

// FetchJobResult is the answer of a poll.
type FetchJobResult struct {
	Job *RunnerJob
	// RetryAfter is how long the runner waits before polling again.
	RetryAfter time.Duration
}

// StepUpdate is the state of one step as a runner reports it.
type StepUpdate struct {
	Number     int32
	Name       string
	State      domain.CIStepState
	Conclusion domain.CIConclusion
	// Outcome is how the step ended before continue-on-error; none when the
	// runner does not report it.
	Outcome     domain.CIConclusion
	StartedAt   time.Time
	CompletedAt time.Time
}

// CompleteJobParams end a job.
type CompleteJobParams struct {
	// DepsReport is present only for a deps job.
	DepsReport     *DepsReport
	JobID          string
	Conclusion     domain.CIConclusion
	Outputs        map[string]string
	RunnerShutdown bool
	Steps          []StepUpdate
	// Annotations replace those of each step they name, as in UpdateJob.
	Annotations []domain.CIAnnotation
}

// CompleteJobResult is the stored result of a job.
type CompleteJobResult struct {
	Conclusion domain.CIConclusion
	Error      string
	Applied    bool
}

// ResolvedAction is the resolution of one "uses" of ResolveActions: where to
// download the action, or Error (then only Ref is set).
type ResolvedAction struct {
	Ref           string
	NameWithOwner string
	Path          string
	ResolvedSHA   string
	TarballURL    string
	Token         string
	Source        domain.CIActionSource
	Error         string
}

// RemoteRef is a ref of a remote repository, as `git ls-remote` lists it.
type RemoteRef struct {
	// Name is the full ref name ("refs/heads/main", "refs/tags/v1").
	Name string
	// ID is the object the ref points to (the tag object of an annotated tag).
	ID string
	// Peeled is the commit an annotated tag points to; empty for other refs.
	Peeled string
}

// GitRemote lists the refs of a repository on github.com, with no
// credentials. It returns domain.ErrNotFound for a repository that does not
// exist or needs credentials (GitHub answers alike), and an error matching
// domain.ErrUnavailable when it cannot tell (network, timeout, too large an
// answer).
type GitRemote interface {
	ListRefs(ctx context.Context, owner, repo string) ([]RemoteRef, error)
}

// WorkflowFetcher downloads one reusable workflow file from github.com at a
// commit that was already resolved (CI-3, D7), with no credentials. It returns
// domain.ErrNotFound for a file or repository that does not exist or needs
// credentials, domain.ErrResourceExhausted for a file larger than maxBytes, and
// an error matching domain.ErrUnavailable when it cannot tell.
type WorkflowFetcher interface {
	FetchWorkflow(ctx context.Context, owner, repo, sha, path string, maxBytes int64) ([]byte, error)
}

// JobSignal is one message of WatchJob.
type JobSignal struct {
	Keepalive bool
	// Cancel is set for a cancel request.
	Cancel *JobCancel
	// Ended is set when the job has ended.
	Ended *domain.JobEndedError
}

// JobCancel is a cancel request for a running job.
type JobCancel struct {
	Reason      domain.CICancelReason
	Message     string
	RequestedAt time.Time
}

// RunnerService is what runners call. The secret is the bearer secret of the
// call: the runner token for FetchJob, the job token for the other calls.
// A bad secret is domain.ErrUnauthenticated; a job token of another job than
// the one named is domain.ErrForbidden. *usecase.CI implements it.
type RunnerService interface {
	Register(ctx context.Context, p domain.Principal, params RegisterRunnerParams) (RegisteredRunner, error)
	FetchJob(ctx context.Context, secret string, params FetchJobParams) (FetchJobResult, error)
	// UpdateJob applies the steps and the annotations whole or not at all: for
	// each step number present in annotations, the annotations of that step are
	// replaced (also for a step already completed, or not in steps).
	UpdateJob(ctx context.Context, secret, jobID string, steps []StepUpdate, annotations []domain.CIAnnotation) error
	AppendLog(ctx context.Context, secret, jobID string, step int32, offset int64, lines []domain.CILogLine) (next int64, err error)
	Heartbeat(ctx context.Context, secret, jobID string) error
	// WatchJob calls ready once the job token is valid and the stream is
	// registered, before any message: the adapter flushes the response headers
	// then, because a client's call returns only when they arrive.
	WatchJob(ctx context.Context, secret, jobID string, ready func(), send func(JobSignal) error) error
	CompleteJob(ctx context.Context, secret string, params CompleteJobParams) (CompleteJobResult, error)
	// ResolveActions resolves the "uses" of the job's steps, one answer per
	// reference in the same order. A reference that cannot be resolved is not
	// an error: its answer carries Error. A local ("./") or Docker
	// ("docker://") reference, or more than 100, is domain.ErrInvalid.
	ResolveActions(ctx context.Context, secret, jobID string, uses []string) ([]ResolvedAction, error)
	// UploadSummary stores the summary of a step, replacing the one it has; an
	// empty markdown removes it. More than 1 MiB, invalid UTF-8 or a step
	// number outside 1 to 1,000 is domain.ErrInvalid.
	UploadSummary(ctx context.Context, secret, jobID string, step int32, markdown string) error
}

// JobView is a job as owners see it.
type JobView struct {
	domain.CIJob
	// Lines is the number of lines of its log now.
	Lines int64
	// RunnerGone is set when the runner of the job was removed since.
	RunnerGone bool
}

// StepRanges returns where the lines of every step are in the log.
func (v JobView) StepRanges() []domain.StepRange { return domain.StepRanges(v.Steps, v.Lines) }

// ListRunsParams select a page of runs.
type ListRunsParams struct {
	PageSize  int
	PageToken string
	// Workflow restricts the result to a workflow file: its path or its file
	// name. Branch restricts it to the runs of a branch, by short name. Status
	// restricts it to the runs of that status. Empty means no restriction.
	Workflow string
	Branch   string
	Status   domain.CIStatus
}

// RunPage is a page of runs, newest first.
type RunPage struct {
	Runs          []domain.CIRun
	NextPageToken string
}

// RunDetail is a run with its jobs.
type RunDetail struct {
	Run           domain.CIRun
	Jobs          []JobView
	LatestAttempt int32
}

// LogBatch is a message of a log stream: consecutive lines from Offset.
type LogBatch struct {
	Offset int64
	Lines  []JobLogLine
}

// JobLogLine is a log line with its step.
type JobLogLine struct {
	Step int32
	domain.CILogLine
}

// GetJobLogParams select the log to stream.
type GetJobLogParams struct {
	JobID string
	// Attempt is the attempt of the job, 0 for the latest.
	Attempt int32
	Offset  int64
	Follow  bool
	// Ready, when not nil, is called once the access is checked and the
	// stream is counted, before any batch: the adapter flushes the response
	// headers then (a follow on a job with no line yet would keep the client's
	// call waiting for the first one).
	Ready func()
	// Client identifies where the call comes from, for the limit of streams
	// per client; empty counts against the total only.
	Client string
}

// CheckView is a check with what owners need to read it.
type CheckView struct {
	domain.CICheck
	// Event is the event of the run of the check (push, pull_request, ...), empty if unknown.
	Event string
	// WaitingForRunner is set for a queued check whose job no registered
	// runner can take (no runner has all its labels).
	WaitingForRunner bool
}

// ChecksResult are the checks of a commit.
type ChecksResult struct {
	Checks    []CheckView
	Truncated bool
}

// RunnerPage is a page of runners.
type RunnerPage struct {
	Runners       []RunnerView
	NextPageToken string
}

// RunnerView is a runner with what is known of it now.
type RunnerView struct {
	domain.CIRunner
	LastSeen time.Time
	Busy     bool
	// Online is decided by the server's clock (see usecase.RunnerOnlineWindow).
	Online bool
}

// JobAnnotation is an annotation with the job it belongs to.
type JobAnnotation struct {
	JobID      string
	Annotation domain.CIAnnotation
}

// AnnotationList are the annotations of a run, at most
// domain.CIMaxListedAnnotations.
type AnnotationList struct {
	Annotations []JobAnnotation
	Truncated   bool
}

// SummaryList are the summaries of a job, at most domain.CIMaxListedSummaries.
type SummaryList struct {
	Summaries []domain.CIStepSummary
	Truncated bool
}

// RunService is the owner side of CI. *usecase.CI implements it.
type RunService interface {
	ListRuns(ctx context.Context, p domain.Principal, repo domain.RepoName, params ListRunsParams) (RunPage, error)
	GetRun(ctx context.Context, p domain.Principal, repo domain.RepoName, runID int64, attempt int32) (RunDetail, error)
	GetJob(ctx context.Context, p domain.Principal, repo domain.RepoName, jobID string, attempt int32) (JobView, error)
	GetJobLog(ctx context.Context, p domain.Principal, repo domain.RepoName, params GetJobLogParams, send func(LogBatch) error) error
	CancelRun(ctx context.Context, p domain.Principal, repo domain.RepoName, runID int64) (domain.CIRun, error)
	RerunRun(ctx context.Context, p domain.Principal, repo domain.RepoName, runID int64, failedOnly bool) (domain.CIRun, error)
	// DispatchWorkflow starts a run of a workflow file by hand (workflow_dispatch).
	// The caller needs ci:write on the repository, or is a job token with the
	// permission actions: write. The run is returned queued.
	DispatchWorkflow(ctx context.Context, p domain.Principal, repo domain.RepoName, params DispatchParams) (domain.CIRun, error)
	ListChecks(ctx context.Context, p domain.Principal, repo domain.RepoName, sha string) (ChecksResult, error)
	// ListWorkflows returns the workflow files at the tip of the default branch
	// and the disabled ones whatever the branch holds, ordered by path. Reading
	// the repository is enough.
	ListWorkflows(ctx context.Context, p domain.Principal, repo domain.RepoName) ([]WorkflowInfo, error)
	// DisabledWorkflows returns the paths of the disabled workflows, sorted,
	// without reading any file: the cheap form of ListWorkflows for the pages.
	DisabledWorkflows(ctx context.Context, p domain.Principal, repo domain.RepoName) ([]string, error)
	// DisableWorkflow stops the workflow (its file name or path; it does not
	// have to exist) from starting runs for any event. EnableWorkflow undoes it.
	// Both need repo:admin on the repository and are idempotent.
	DisableWorkflow(ctx context.Context, p domain.Principal, repo domain.RepoName, file string) (WorkflowInfo, error)
	EnableWorkflow(ctx context.Context, p domain.Principal, repo domain.RepoName, file string) (WorkflowInfo, error)
	// ListAnnotations returns the annotations of a run attempt (0 for the
	// latest), of one job when jobID is not empty. Each job gives those of the
	// attempt in which it executed.
	ListAnnotations(ctx context.Context, p domain.Principal, repo domain.RepoName, runID int64, attempt int32, jobID string) (AnnotationList, error)
	// GetStepSummaries returns the summaries of a job at an attempt (0 for the
	// latest), by step number. maxBytes above 0 bounds the Markdown read: the
	// summaries past it come back Omitted, without their Markdown (the API
	// passes 0, no bound).
	GetStepSummaries(ctx context.Context, p domain.Principal, repo domain.RepoName, jobID string, attempt int32, maxBytes int64) (SummaryList, error)
	ListRunners(ctx context.Context, p domain.Principal, size int, token string) (RunnerPage, error)
	RemoveRunner(ctx context.Context, p domain.Principal, id string) error
}

// ErrSecretNotOpened means a sealed secret does not open: another key, other
// additional data (a value moved to another scope or name) or a damaged value.
var ErrSecretNotOpened = errors.New("secret does not open")

// SecretSealer encrypts the values of secrets at rest. The adapter holds the
// master key; without one the use cases get no sealer and refuse secrets.
type SecretSealer interface {
	// Seal returns plaintext sealed and bound to aad. It never fails.
	Seal(plaintext, aad []byte) []byte
	// Open reverses Seal with the same aad, or returns ErrSecretNotOpened.
	Open(sealed, aad []byte) ([]byte, error)
	// KeyCheck is a value derived from the key that does not reveal it.
	KeyCheck() []byte
}

// SealedSecret is a stored secret: its value is sealed.
type SealedSecret struct {
	Name      string
	Sealed    []byte
	UpdatedAt time.Time
}

// CISettingsStore keeps the secrets, variables and environments of CI (ADR
// 0019). A scope that names a missing repository or environment is
// domain.ErrNotFound for every write and every read of its content. Names are
// already upper-cased (domain.ParseSettingName).
type CISettingsStore interface {
	// BindSecretsKey records the key check beside the secrets when none is
	// stored, and returns domain.ErrSecretsKeyMismatch when the stored one
	// differs.
	BindSecretsKey(ctx context.Context, check []byte) error
	// PutSecret creates or replaces a secret.
	PutSecret(ctx context.Context, scope domain.SettingsScope, name string, sealed []byte, at time.Time) (domain.Secret, error)
	// GetSealedSecret reads one secret of a scope, or domain.ErrNotFound when
	// it, the repository or the environment does not exist.
	GetSealedSecret(ctx context.Context, scope domain.SettingsScope, name string) (SealedSecret, error)
	// ListSealedSecrets lists the secrets of one scope ordered by name.
	ListSealedSecrets(ctx context.Context, scope domain.SettingsScope) ([]SealedSecret, error)
	// DeleteSecret deletes a secret, or returns domain.ErrNotFound.
	DeleteSecret(ctx context.Context, scope domain.SettingsScope, name string) error
	// PutVariable creates or replaces a variable.
	PutVariable(ctx context.Context, scope domain.SettingsScope, name, value string, at time.Time) (domain.Variable, error)
	// GetVariable returns a variable, or domain.ErrNotFound.
	GetVariable(ctx context.Context, scope domain.SettingsScope, name string) (domain.Variable, error)
	// ListVariables lists the variables of one scope ordered by name.
	ListVariables(ctx context.Context, scope domain.SettingsScope) ([]domain.Variable, error)
	// DeleteVariable deletes a variable, or returns domain.ErrNotFound.
	DeleteVariable(ctx context.Context, scope domain.SettingsScope, name string) error
	// CreateEnvironment creates the environment, or returns the existing one
	// (compared without regard to case) unchanged.
	CreateEnvironment(ctx context.Context, repo domain.RepoName, name string, at time.Time) (domain.Environment, error)
	// ListEnvironments lists the environments of a repository ordered by name.
	ListEnvironments(ctx context.Context, repo domain.RepoName) ([]domain.Environment, error)
	// DeleteEnvironment deletes the environment with its secrets and
	// variables, or returns domain.ErrNotFound.
	DeleteEnvironment(ctx context.Context, repo domain.RepoName, name string) error
}

// JobSettingsRequest asks for the variables, and when wanted the secrets, a job
// sees.
type JobSettingsRequest struct {
	Repo domain.RepoName
	// Environment is the environment of the job, "" for none.
	Environment string
	// CreateEnvironment creates the environment first when it does not exist
	// (a job that names one creates it on first use).
	CreateEnvironment bool
	// WantSecrets asks for secrets at all: false for a run that is not
	// trusted. SecretNames are the (upper-cased) names to return, or AllSecrets
	// for every secret of the scopes.
	WantSecrets bool
	SecretNames []string
	AllSecrets  bool
}

// JobSettings are what a job sees: the merged variables and secrets, with the
// environment over the repository over the instance.
type JobSettings struct {
	Vars    map[string]string
	Secrets map[string]string
	// SecretsOff is set when secrets were wanted but the server has no
	// ERMITE_SECRETS_KEY, so none can be given.
	SecretsOff bool
}

// CIJobSettings resolves the variables and secrets of a job. *usecase.CISettings
// implements it.
type CIJobSettings interface {
	Resolve(ctx context.Context, req JobSettingsRequest) (JobSettings, error)
}

// CIActivity tells whether CI still has queued work that may create runs for a
// head. *usecase.CI implements it.
type CIActivity interface {
	// Settled reports that no push trigger of the repository and no pull event
	// of the merge request is queued or being planned.
	Settled(ctx context.Context, repo domain.RepoName, number int64) bool
}
