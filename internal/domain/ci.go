// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// CIStatus is where a run, a job or a check is in its life (GitHub's status).
type CIStatus string

// Statuses.
const (
	CIQueued     CIStatus = "queued"
	CIInProgress CIStatus = "in_progress"
	CICompleted  CIStatus = "completed"
)

// CIConclusion is how a run, a job, a step or a check ended, with GitHub's
// values. It is empty until the status is completed.
type CIConclusion string

// Conclusions.
const (
	CINoConclusion   CIConclusion = ""
	CISuccess        CIConclusion = "success"
	CIFailure        CIConclusion = "failure"
	CICancelled      CIConclusion = "cancelled"
	CISkipped        CIConclusion = "skipped"
	CITimedOut       CIConclusion = "timed_out"
	CINeutral        CIConclusion = "neutral"
	CIActionRequired CIConclusion = "action_required"
)

// PassesMerge reports whether a check that ended this way lets a merge go
// through under FailingChecksBlock: success, skipped and neutral do.
func (c CIConclusion) PassesMerge() bool {
	return c == CISuccess || c == CISkipped || c == CINeutral
}

// IsFailure reports whether the conclusion is a failure or a timeout.
func (c CIConclusion) IsFailure() bool { return c == CIFailure || c == CITimedOut }

// CIStepState is where a step is in its life.
type CIStepState string

// Step states.
const (
	CIStepPending    CIStepState = "pending"
	CIStepInProgress CIStepState = "in_progress"
	CIStepCompleted  CIStepState = "completed"
)

// CIJobPhase is where a job is for the server, finer than the status owners
// see.
type CIJobPhase string

// Job phases.
const (
	// CIPhaseWaiting: waits for the jobs it needs or for a max-parallel slot.
	CIPhaseWaiting CIJobPhase = "waiting"
	// CIPhaseReady: planned, waits for a runner.
	CIPhaseReady CIJobPhase = "ready"
	// CIPhaseAssigned: given to a runner that has not acknowledged it.
	CIPhaseAssigned CIJobPhase = "assigned"
	// CIPhaseRunning: acknowledged by its runner.
	CIPhaseRunning CIJobPhase = "running"
	// CIPhaseEnded: over.
	CIPhaseEnded CIJobPhase = "ended"
)

// CIIsolation is how a runner isolates jobs.
type CIIsolation string

// Isolations.
const (
	CIIsolationNone                CIIsolation = ""
	CIIsolationVM                  CIIsolation = "vm"
	CIIsolationContainerSysbox     CIIsolation = "container-sysbox"
	CIIsolationContainerPrivileged CIIsolation = "container-privileged"
)

// CICancelReason says why a job is asked to stop.
type CICancelReason string

// Cancel reasons.
const (
	CICancelNone     CICancelReason = ""
	CICancelUser     CICancelReason = "user"
	CICancelFailFast CICancelReason = "fail-fast"
	CICancelTimeout  CICancelReason = "timeout"
)

// Limits and durations of the CI protocol (runner.proto).
const (
	// CIMaxLogBytes is the size of a job log, as stored (512 MiB).
	CIMaxLogBytes = 512 << 20
	// CIMaxOutputBytes bounds one output and all outputs together (1 MiB).
	CIMaxOutputBytes = 1 << 20
	// CIMaxLogLines and CIMaxLogChunkBytes bound one AppendLog request.
	CIMaxLogLines      = 1000
	CIMaxLogChunkBytes = 64 << 10
	// CIMaxSteps bounds the steps of a job.
	CIMaxSteps = 1000
	// CIMaxStepName bounds a step name, in characters.
	CIMaxStepName = 1024
	// CIMaxLabels and CIMaxLabelLen bound the labels of a runner.
	CIMaxLabels   = 64
	CIMaxLabelLen = 64
	// CIDefaultTimeoutMinutes is the timeout of a job without timeout-minutes.
	CIDefaultTimeoutMinutes = 360
	// CIMaxAssignCancels is how many times in a row an assignment may be
	// cancelled before the job fails.
	CIMaxAssignCancels = 3
	// CIRetentionDays is the retention of runs, jobs, checks and logs.
	CIRetentionDays = 90
)

// Durations of the CI protocol.
const (
	// CIAckTimeout is how long a runner has to send its first heartbeat.
	CIAckTimeout = 30 * time.Second
	// CIHeartbeatDeadline is how long a running job may go without heartbeat.
	CIHeartbeatDeadline = 2 * time.Minute
	// CICancelGrace is how long a runner has to conclude after a cancel request.
	CICancelGrace = 60 * time.Second
	// CITimeoutSlack is added to timeout-minutes by the server.
	CITimeoutSlack = 60 * time.Second
	// CIQueueTimeout is how long a job waits for a runner.
	CIQueueTimeout = 24 * time.Hour
	// CITokenGrace is how long a job token is recognised after the job ended.
	CITokenGrace = 24 * time.Hour
)

// Messages of jobs ended by the server, GitHub's where it has them.
const (
	// CIQueueTimeoutMessage is GitHub's message for a job that no runner took.
	CIQueueTimeoutMessage = "The job was not acquired by Runner of type self-hosted even after multiple attempts"
	// CIShutdownMessage is GitHub's message for a runner that stops.
	CIShutdownMessage = "The runner has received a shutdown signal. This can happen when the runner service is stopped, or a manually started runner is canceled."
	// CILogLimitMessage ends a log that reached its limit.
	CILogLimitMessage = "The log of this job reached the limit of 512 MiB; the job is stopped."
)

// CILostMessage is GitHub's message for a runner that stopped sending heartbeats.
func CILostMessage(runner string) string {
	return "The self-hosted runner: " + runner + " lost communication with the server. Verify the machine is running and has a healthy network connection. Anything in your workflow that terminates the runner process, starves it for CPU/Memory, or blocks its network access can cause this error."
}

// CIRun is one execution of one workflow file for one event.
type CIRun struct {
	Repo         RepoName
	ID           int64
	Number       int64
	Attempt      int32
	WorkflowPath string
	WorkflowName string
	// Name is GitHub's name of the run: the evaluated run-name when the
	// workflow has one, else WorkflowName. WorkflowName stays the name of the
	// workflow (github.workflow, the checks).
	Name              string
	Title             string
	Event             string
	Ref               string
	Sha               string
	Actor             string
	Status            CIStatus
	Conclusion        CIConclusion
	Error             string
	CreatedAt         time.Time
	StartedAt         time.Time
	CompletedAt       time.Time
	CancelRequested   bool
	CancelRequestedAt time.Time
	// Version is the version of the stored record, which grows at every
	// change and never goes back.
	Version uint64
}

// CIRunSource is what a run was planned from: the workflow file at the sha of
// the run, and the "github" context without the parts that depend on the job
// and the attempt.
type CIRunSource struct {
	RunID        int64
	Repo         RepoName
	WorkflowYAML string
	GithubJSON   string
}

// CIStep is a step of a job with where its lines are in the job log.
type CIStep struct {
	Number      int32
	Name        string
	State       CIStepState
	Conclusion  CIConclusion
	StartedAt   time.Time
	CompletedAt time.Time
	LogStart    int64
	// HasLines is set once a line of the step reached the log; LogStart is
	// then the number of its first line. LogEnd is not stored: see StepRanges.
	HasLines bool
}

// CIJob is one job instance (one matrix combination) in one attempt.
type CIJob struct {
	ID    string
	RunID int64
	// Attempt is the attempt in which the job executed, RunAttempt the attempt
	// of the run the record belongs to: they differ for a job kept by a re-run
	// of the failed jobs.
	Attempt    int32
	RunAttempt int32
	Repo       RepoName
	Key        string
	Name       string
	Phase      CIJobPhase
	Conclusion CIConclusion
	Error      string
	Labels     []string
	Needs      []string
	RunnerID   string
	RunnerName string
	Isolation  CIIsolation
	Steps      []CIStep

	CreatedAt   time.Time
	StartedAt   time.Time
	CompletedAt time.Time

	// Index is the position of the job in the workflow file, Combo the
	// position of the instance in its matrix.
	Index int
	Combo int
	// Planned is set once runs-on, the name and the contexts are evaluated.
	// A job that is not planned has not been expanded yet.
	Planned    bool
	MatrixJSON string
	// ContextsJSON is the JSON object of contexts given to the runner, computed
	// when the job is planned.
	ContextsJSON string
	// ComboTotal is the number of instances of the job's matrix.
	ComboTotal int
	// TimeoutMinutes is the timeout of the job, at least 1.
	TimeoutMinutes int32
	FailFast       bool
	// ContinueOnError is the job's continue-on-error, evaluated when the job is
	// planned: its failure leaves the run, its siblings and the jobs that
	// need it unaffected (needs.<job>.result is success). The conclusion of
	// the job itself stays failure.
	ContinueOnError bool
	// MaxParallel is the max-parallel of the strategy, 0 for no limit.
	MaxParallel int
	Outputs     map[string]string

	// TokenHash is the hash of the job token of the current assignment; zero
	// when there is none.
	TokenHash       TokenHash
	TokenValidUntil time.Time
	ReadyAt         time.Time
	AssignedAt      time.Time
	AckedAt         time.Time
	// AssignCancels counts the assignments cancelled in a row.
	AssignCancels int

	CancelReason      CICancelReason
	CancelMessage     string
	CancelRequestedAt time.Time

	Version uint64
}

// Status is the status owners see: a job is queued until its runner
// acknowledged it.
func (j CIJob) Status() CIStatus {
	switch j.Phase {
	case CIPhaseRunning:
		return CIInProgress
	case CIPhaseEnded:
		return CICompleted
	}
	return CIQueued
}

// Active reports whether the job is given to a runner and not over.
func (j CIJob) Active() bool { return j.Phase == CIPhaseAssigned || j.Phase == CIPhaseRunning }

// Ended reports whether the job is over.
func (j CIJob) Ended() bool { return j.Phase == CIPhaseEnded }

// HasToken reports whether a job token is set.
func (j CIJob) HasToken() bool { return j.TokenHash != TokenHash{} }

// CICheck is the result of one job on one commit.
type CICheck struct {
	ID           string
	Repo         RepoName
	Sha          string
	Name         string
	Status       CIStatus
	Conclusion   CIConclusion
	RunID        int64
	JobID        string
	Attempt      int32
	WorkflowName string
	CreatedAt    time.Time
	StartedAt    time.Time
	CompletedAt  time.Time
	Version      uint64
}

// CIRunner is a registered CI runner. TokenHash is never sent to clients.
type CIRunner struct {
	ID           string
	Name         string
	Labels       []string
	Isolation    CIIsolation
	Version      string
	Arch         string
	OS           string
	RegisteredAt time.Time
	TokenHash    TokenHash
}

// RunnerTokenPrefix and JobTokenPrefix start the secrets of runners and jobs.
const (
	RunnerTokenPrefix = "ermiterun_"
	JobTokenPrefix    = "ermitejob_"
	// CITokenRandomLen is the length of the base64url part of both.
	CITokenRandomLen = 43
)

// IsRunnerToken and IsJobToken report whether s has the shape of the secret.
func IsRunnerToken(s string) bool { return hasTokenShape(s, RunnerTokenPrefix) }
func IsJobToken(s string) bool    { return hasTokenShape(s, JobTokenPrefix) }

func hasTokenShape(s, prefix string) bool {
	rest, ok := strings.CutPrefix(s, prefix)
	if !ok || len(rest) != CITokenRandomLen {
		return false
	}
	for _, r := range rest {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// NewJobID names a job instance: "<run>-<index>-<combo>". It is the same in
// every attempt of the run.
func NewJobID(runID int64, index, combo int) string {
	return fmt.Sprintf("%d-%d-%d", runID, index, combo)
}

// ParseJobID returns the run of a job id, or ok false when s is not one.
func ParseJobID(s string) (runID int64, index, combo int, ok bool) {
	parts := strings.Split(s, "-")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	r, err1 := strconv.ParseInt(parts[0], 10, 64)
	i, err2 := strconv.Atoi(parts[1])
	c, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || r < 1 || i < 0 || c < 0 || NewJobID(r, i, c) != s {
		return 0, 0, 0, false
	}
	return r, i, c, true
}

var labelRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ParseRunnerName validates a runner name.
func ParseRunnerName(s string) error {
	if !labelRE.MatchString(s) {
		return fmt.Errorf("%w: a runner name has 1 to 64 letters, digits, '.', '_' or '-'", ErrInvalid)
	}
	return nil
}

// ParseRunnerLabels validates labels and returns them lowercased, without
// duplicates, in order of first appearance.
func ParseRunnerLabels(labels []string) ([]string, error) {
	if len(labels) > CIMaxLabels {
		return nil, fmt.Errorf("%w: at most %d labels", ErrInvalid, CIMaxLabels)
	}
	var out []string
	for _, l := range labels {
		if !labelRE.MatchString(l) {
			return nil, fmt.Errorf("%w: label %q: 1 to %d letters, digits, '.', '_' or '-'", ErrInvalid, l, CIMaxLabelLen)
		}
		l = strings.ToLower(l)
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out, nil
}

// RunnerLabels returns the labels a runner matches: the ones it chose plus
// the implicit "self-hosted", its os and its arch, lowercase and sorted.
func RunnerLabels(chosen []string, os, arch string) []string {
	out := slices.Clone(chosen)
	for _, l := range []string{"self-hosted", strings.ToLower(os), strings.ToLower(arch)} {
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	slices.Sort(out)
	return out
}

// LabelsMatch reports whether every wanted label is among have, ignoring case.
func LabelsMatch(want, have []string) bool {
	for _, w := range want {
		if !slices.ContainsFunc(have, func(h string) bool { return strings.EqualFold(h, w) }) {
			return false
		}
	}
	return true
}

var outputNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,254}$`)

// CheckOutputs validates the outputs of a job: names, case-insensitive
// uniqueness and the 1 MiB limits.
func CheckOutputs(outputs map[string]string) error {
	seen := make(map[string]bool, len(outputs))
	total := 0
	for k, v := range outputs {
		if !outputNameRE.MatchString(k) {
			return fmt.Errorf("%w: output name %q: 1 to 255 letters, digits, '_' or '-', starting with a letter or '_'", ErrInvalid, k)
		}
		lk := strings.ToLower(k)
		if seen[lk] {
			return fmt.Errorf("%w: output names differ only by case: %q", ErrInvalid, k)
		}
		seen[lk] = true
		if len(v) > CIMaxOutputBytes {
			return fmt.Errorf("%w: output %q is larger than 1 MiB", ErrInvalid, k)
		}
		total += len(k) + len(v)
	}
	if total > CIMaxOutputBytes {
		return fmt.Errorf("%w: the outputs are larger than 1 MiB together", ErrInvalid)
	}
	return nil
}

// JobEndedError is returned for a call that needs a running job when the job
// has ended. It matches ErrPreconditionFailed; the API adds the JobEnded
// detail.
type JobEndedError struct {
	Conclusion CIConclusion
	Message    string
}

// Error implements error.
func (e *JobEndedError) Error() string {
	return fmt.Sprintf("%v: the job has ended (%s)", ErrPreconditionFailed, e.Conclusion)
}

// Is makes errors.Is(err, ErrPreconditionFailed) true.
func (e *JobEndedError) Is(target error) bool { return target == ErrPreconditionFailed }

// IsJobEnded returns the JobEndedError in err.
func IsJobEnded(err error) (*JobEndedError, bool) {
	var e *JobEndedError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// ErrOutOfRange means an offset is beyond what the server holds.
var ErrOutOfRange = errors.New("out of range")

// CILogLine is one line of a job log.
type CILogLine struct {
	Time time.Time
	// Text has no line feed.
	Text string
	// Continued marks a piece of a line that was too long to send whole.
	Continued bool
}

// StepRange is where the lines of a step are in the job log.
type StepRange struct {
	Start, End int64
}

// StepRanges returns the range of every step of the job for a log of lines
// lines: a step with lines ends where the next step with lines begins (the
// last ends at the end of the log); a step without lines is empty, placed
// where the previous step ends.
func StepRanges(steps []CIStep, lines int64) []StepRange {
	out := make([]StepRange, len(steps))
	var pos int64
	for i, s := range steps {
		if !s.HasLines {
			out[i] = StepRange{Start: pos, End: pos}
			continue
		}
		end := lines
		for _, n := range steps[i+1:] {
			if n.HasLines {
				end = n.LogStart
				break
			}
		}
		out[i] = StepRange{Start: s.LogStart, End: max(end, s.LogStart)}
		pos = out[i].End
	}
	return out
}

// StepOfLine returns the number of the step a log line belongs to, 0 when no
// step has that line.
func StepOfLine(steps []CIStep, line int64) int32 {
	var found int32
	for _, s := range steps {
		if s.HasLines && s.LogStart <= line {
			found = s.Number
		}
	}
	return found
}

// RunUpdated is published when a CI run is created or changes. It carries the
// visibility and the creation time of the repository, like PushEvent.
type RunUpdated struct {
	Run        CIRun
	At         time.Time
	Visibility Visibility
	CreatedAt  time.Time
}

// EventKind implements Event.
func (RunUpdated) EventKind() string { return "ci.run_updated" }

// JobUpdated is published when a CI job is created or changes (status,
// conclusion, runner or steps; never per log line). Lines is the number of
// lines of the job log at that time, for the end of the step in progress.
type JobUpdated struct {
	Job        CIJob
	Lines      int64
	At         time.Time
	Visibility Visibility
	CreatedAt  time.Time
}

// EventKind implements Event.
func (JobUpdated) EventKind() string { return "ci.job_updated" }

// CheckState is the combined state of the checks of a commit.
type CheckState string

// Combined check states.
const (
	// CheckStateNone: the commit has no check.
	CheckStateNone CheckState = ""
	// CheckStatePending: a check is queued or in progress, none has failed.
	CheckStatePending CheckState = "pending"
	// CheckStateFailure: a check failed (or needs an action), whatever the others.
	CheckStateFailure CheckState = "failure"
	// CheckStateSuccess: every check passed.
	CheckStateSuccess CheckState = "success"
)

// CombineChecks reduces the checks of a commit to one state, the one the
// merge rule uses too: a failed check makes the commit failing even while
// others are pending; else a pending check makes it pending; else it is a
// success.
func CombineChecks(checks []CICheck) CheckState {
	if len(checks) == 0 {
		return CheckStateNone
	}
	pending := false
	for _, c := range checks {
		switch {
		case c.Status != CICompleted:
			pending = true
		case !c.Conclusion.PassesMerge():
			return CheckStateFailure
		}
	}
	if pending {
		return CheckStatePending
	}
	return CheckStateSuccess
}
