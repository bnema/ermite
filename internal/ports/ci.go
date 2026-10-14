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

// CIStore keeps CI runs, jobs and checks. The store derives its own indexes
// (the queue of ready jobs, the active jobs, the open runs, the job tokens)
// from the records it writes.
type CIStore interface {
	// CreateRun takes the next run id and the next number of the workflow in
	// the repository (both never reused), calls build with them and stores
	// the draft it returns, in one batch that fails with domain.ErrNotFound
	// once the repository is deleted. The records come back with their
	// versions.
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

// WorkflowGuard finds the changes of a push to the workflow files of a
// repository.
type WorkflowGuard interface {
	// ChangedWorkflow returns the first path under .github/workflows/ that
	// the updates create or change, from the old tree of each ref (the empty tree
	// for a new ref) to its new one. A deleted ref changes nothing. "" means
	// none. objects are those of the push in progress; a directory that is not
	// inside the repository is refused.
	ChangedWorkflow(ctx context.Context, repo domain.RepoName, updates []domain.RefUpdate, objects PushObjects) (path string, err error)
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
	Job   domain.CIJob
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
	Labels []string
	PollID string
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
	ListChecks(ctx context.Context, p domain.Principal, repo domain.RepoName, sha string) (ChecksResult, error)
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
