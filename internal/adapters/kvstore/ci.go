// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	kv "bnema.dev/ermite-kv"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Key layout of CI. Repository names hold no '/', so a prefix ending in "/"
// never matches another repository; ids are zero-padded so that keys sort
// numerically. Job records are keyed by run, attempt of the run and job id.
//
//	run/<repo>/<id:020d>                 CIRun
//	runsrc/<id:020d>                     CIRunSource
//	job/<run:020d>/<attempt:010d>/<id>   CIJob
//	check/<repo>/<sha>/<check id>        CICheck
//	runseq                               Sequence: last run id
//	runnum/<repo>/<workflow path>        Sequence: last run number
//	runner/<id>, runnername/<name>, runnertoken/<hash>
//
// Indexes, derived from the job and run records by every write:
//
//	ciopen/<id:020d>                     run not completed (value: repo)
//	ciready/<ready_at:020d>/<job key>    job waiting for a runner
//	ciactive/<job key>                   job assigned or running
//	jobtoken/<hash>                      job token (value: job key)
//	concur/<repo>/<key(group)>           concurrency group (ci_concurrency.go)
//	cireftip/<repo>/<ref>                who moves a branch or tag to its object (ci_tip.go)
//
// The job repository tokens and the action resolutions are in ci_token.go, the
// annotations in ci_annotation.go.
const (
	runPrefix     = "run/"
	runSrcPrefix  = "runsrc/"
	jobPrefix     = "job/"
	checkPrefix   = "check/"
	runSeqKey     = "runseq"
	runNumPrefix  = "runnum/"
	runnerPrefix  = "runner/"
	runnerName    = "runnername/"
	runnerTokPref = "runnertoken/"
	openPrefix    = "ciopen/"
	readyPrefix   = "ciready/"
	activePrefix  = "ciactive/"
	jobTokPrefix  = "jobtoken/"
	idFormat      = "%020d"
)

// CI is the ermite-kv backed ports.CIStore. It shares the database of the
// Store it comes from.
type CI struct {
	db *kv.DB
}

var _ ports.CIStore = (*CI)(nil)

// CI returns the CI store over the same database. It is valid until the Store
// is closed.
func (s *Store) CI() *CI { return &CI{db: s.db} }

func runKey(repo domain.RepoName, id int64) []byte {
	return []byte(runPrefix + repo.String() + "/" + fmt.Sprintf(idFormat, id))
}
func runsPrefix(repo domain.RepoName) string { return runPrefix + repo.String() + "/" }
func runSrcKey(id int64) []byte              { return []byte(runSrcPrefix + fmt.Sprintf(idFormat, id)) }
func openKey(id int64) []byte                { return []byte(openPrefix + fmt.Sprintf(idFormat, id)) }
func jobsPrefix(runID int64) string          { return jobPrefix + fmt.Sprintf(idFormat, runID) + "/" }
func jobAttemptPrefix(runID int64, attempt int32) string {
	return jobsPrefix(runID) + fmt.Sprintf("%010d", attempt) + "/"
}
func jobRef(runID int64, attempt int32, id string) string {
	return jobAttemptPrefix(runID, attempt) + id
}
func jobKey(runID int64, attempt int32, id string) []byte { return []byte(jobRef(runID, attempt, id)) }
func checksPrefix(repo domain.RepoName, sha string) string {
	return checkPrefix + repo.String() + "/" + sha + "/"
}
func checkKey(repo domain.RepoName, sha, id string) []byte {
	return []byte(checksPrefix(repo, sha) + id)
}
func runNumKey(repo domain.RepoName, workflow string) []byte {
	return []byte(runNumPrefix + repo.String() + "/" + workflow)
}
func readyKey(at time.Time, ref string) []byte {
	return []byte(readyPrefix + fmt.Sprintf(idFormat, at.UnixNano()) + "/" + ref)
}
func activeKey(ref string) []byte { return []byte(activePrefix + ref) }
func jobTokenKey(h domain.TokenHash) []byte {
	return []byte(jobTokPrefix + h.String())
}

// --- encoding ---

func encodeStatus(s domain.CIStatus) storagepb.CIStatus {
	switch s {
	case domain.CIQueued:
		return storagepb.CIStatus_CI_STATUS_QUEUED
	case domain.CIInProgress:
		return storagepb.CIStatus_CI_STATUS_IN_PROGRESS
	case domain.CICompleted:
		return storagepb.CIStatus_CI_STATUS_COMPLETED
	case domain.CIPending:
		return storagepb.CIStatus_CI_STATUS_PENDING
	}
	return storagepb.CIStatus_CI_STATUS_UNSPECIFIED
}

func decodeStatus(s storagepb.CIStatus) domain.CIStatus {
	switch s {
	case storagepb.CIStatus_CI_STATUS_IN_PROGRESS:
		return domain.CIInProgress
	case storagepb.CIStatus_CI_STATUS_COMPLETED:
		return domain.CICompleted
	case storagepb.CIStatus_CI_STATUS_PENDING:
		return domain.CIPending
	}
	return domain.CIQueued
}

var conclusions = []struct {
	d domain.CIConclusion
	p storagepb.CIConclusion
}{
	{domain.CISuccess, storagepb.CIConclusion_CI_CONCLUSION_SUCCESS},
	{domain.CIFailure, storagepb.CIConclusion_CI_CONCLUSION_FAILURE},
	{domain.CICancelled, storagepb.CIConclusion_CI_CONCLUSION_CANCELLED},
	{domain.CISkipped, storagepb.CIConclusion_CI_CONCLUSION_SKIPPED},
	{domain.CITimedOut, storagepb.CIConclusion_CI_CONCLUSION_TIMED_OUT},
	{domain.CINeutral, storagepb.CIConclusion_CI_CONCLUSION_NEUTRAL},
	{domain.CIActionRequired, storagepb.CIConclusion_CI_CONCLUSION_ACTION_REQUIRED},
}

func encodeConclusion(c domain.CIConclusion) storagepb.CIConclusion {
	for _, e := range conclusions {
		if e.d == c {
			return e.p
		}
	}
	return storagepb.CIConclusion_CI_CONCLUSION_UNSPECIFIED
}

func decodeConclusion(c storagepb.CIConclusion) domain.CIConclusion {
	for _, e := range conclusions {
		if e.p == c {
			return e.d
		}
	}
	return domain.CINoConclusion
}

var phases = []struct {
	d domain.CIJobPhase
	p storagepb.CIJobPhase
}{
	{domain.CIPhaseWaiting, storagepb.CIJobPhase_CI_JOB_PHASE_WAITING},
	{domain.CIPhaseReady, storagepb.CIJobPhase_CI_JOB_PHASE_READY},
	{domain.CIPhaseAssigned, storagepb.CIJobPhase_CI_JOB_PHASE_ASSIGNED},
	{domain.CIPhaseRunning, storagepb.CIJobPhase_CI_JOB_PHASE_RUNNING},
	{domain.CIPhaseEnded, storagepb.CIJobPhase_CI_JOB_PHASE_ENDED},
}

func encodePhase(p domain.CIJobPhase) (storagepb.CIJobPhase, error) {
	for _, e := range phases {
		if e.d == p {
			return e.p, nil
		}
	}
	return 0, fmt.Errorf("invalid job phase %q", p)
}

func decodePhase(p storagepb.CIJobPhase) domain.CIJobPhase {
	for _, e := range phases {
		if e.p == p {
			return e.d
		}
	}
	return domain.CIPhaseWaiting
}

var isolations = []struct {
	d domain.CIIsolation
	p storagepb.CIIsolation
}{
	{domain.CIIsolationVM, storagepb.CIIsolation_CI_ISOLATION_VM},
	{domain.CIIsolationContainerSysbox, storagepb.CIIsolation_CI_ISOLATION_CONTAINER_SYSBOX},
	{domain.CIIsolationContainerPrivileged, storagepb.CIIsolation_CI_ISOLATION_CONTAINER_PRIVILEGED},
}

func encodeIsolation(i domain.CIIsolation) storagepb.CIIsolation {
	for _, e := range isolations {
		if e.d == i {
			return e.p
		}
	}
	return storagepb.CIIsolation_CI_ISOLATION_UNSPECIFIED
}

func decodeIsolation(i storagepb.CIIsolation) domain.CIIsolation {
	for _, e := range isolations {
		if e.p == i {
			return e.d
		}
	}
	return domain.CIIsolationNone
}

var stepStates = []struct {
	d domain.CIStepState
	p storagepb.CIStepState
}{
	{domain.CIStepPending, storagepb.CIStepState_CI_STEP_STATE_PENDING},
	{domain.CIStepInProgress, storagepb.CIStepState_CI_STEP_STATE_IN_PROGRESS},
	{domain.CIStepCompleted, storagepb.CIStepState_CI_STEP_STATE_COMPLETED},
}

func encodeStepState(s domain.CIStepState) storagepb.CIStepState {
	for _, e := range stepStates {
		if e.d == s {
			return e.p
		}
	}
	return storagepb.CIStepState_CI_STEP_STATE_UNSPECIFIED
}

func decodeStepState(s storagepb.CIStepState) domain.CIStepState {
	for _, e := range stepStates {
		if e.p == s {
			return e.d
		}
	}
	return domain.CIStepPending
}

var cancelReasons = []struct {
	d domain.CICancelReason
	p storagepb.CICancelReason
}{
	{domain.CICancelUser, storagepb.CICancelReason_CI_CANCEL_REASON_USER},
	{domain.CICancelFailFast, storagepb.CICancelReason_CI_CANCEL_REASON_FAIL_FAST},
	{domain.CICancelTimeout, storagepb.CICancelReason_CI_CANCEL_REASON_TIMEOUT},
}

func encodeCancelReason(r domain.CICancelReason) storagepb.CICancelReason {
	for _, e := range cancelReasons {
		if e.d == r {
			return e.p
		}
	}
	return storagepb.CICancelReason_CI_CANCEL_REASON_UNSPECIFIED
}

func decodeCancelReason(r storagepb.CICancelReason) domain.CICancelReason {
	for _, e := range cancelReasons {
		if e.p == r {
			return e.d
		}
	}
	return domain.CICancelNone
}

func encodeRun(r domain.CIRun) ([]byte, error) {
	if r.Repo.IsZero() {
		return nil, fmt.Errorf("%w: empty name", domain.ErrInvalidName)
	}
	return mustMarshal(&storagepb.CIRun{
		Repo: r.Repo.String(), Id: r.ID, Number: r.Number, Attempt: r.Attempt,
		WorkflowPath: r.WorkflowPath, WorkflowName: r.WorkflowName, Name: r.Name, Title: r.Title,
		Event: r.Event, Ref: r.Ref, Sha: r.Sha, HeadSha: r.HeadSha, Actor: r.Actor,
		Status: encodeStatus(r.Status), Conclusion: encodeConclusion(r.Conclusion), Error: r.Error,
		CreatedAt: encodeTime(r.CreatedAt), StartedAt: encodeTime(r.StartedAt), CompletedAt: encodeTime(r.CompletedAt),
		CancelRequested: r.CancelRequested, CancelRequestedAt: encodeTime(r.CancelRequestedAt), Trusted: r.Trusted,
		Concurrency: encodeConcurrency(r.Concurrency),
	}), nil
}

func decodeRun(item kv.Item) (domain.CIRun, error) {
	var pb storagepb.CIRun
	if err := proto.Unmarshal(item.Value, &pb); err != nil {
		return domain.CIRun{}, fmt.Errorf("decode run: %w", err)
	}
	repo, err := domain.ParseRepoName(pb.GetRepo())
	if err != nil {
		return domain.CIRun{}, fmt.Errorf("decode run: %w", err)
	}
	name := pb.GetName()
	if name == "" {
		name = pb.GetWorkflowName() // stored before the field existed
	}
	return domain.CIRun{
		Repo: repo, ID: pb.GetId(), Number: pb.GetNumber(), Attempt: pb.GetAttempt(),
		WorkflowPath: pb.GetWorkflowPath(), WorkflowName: pb.GetWorkflowName(), Name: name, Title: pb.GetTitle(),
		Event: pb.GetEvent(), Ref: pb.GetRef(), Sha: pb.GetSha(), HeadSha: pb.GetHeadSha(), Actor: pb.GetActor(),
		Status: decodeStatus(pb.GetStatus()), Conclusion: decodeConclusion(pb.GetConclusion()), Error: pb.GetError(),
		CreatedAt: decodeTime(pb.GetCreatedAt()), StartedAt: decodeTime(pb.GetStartedAt()), CompletedAt: decodeTime(pb.GetCompletedAt()),
		CancelRequested: pb.GetCancelRequested(), CancelRequestedAt: decodeTime(pb.GetCancelRequestedAt()),
		Trusted: pb.GetTrusted(), Concurrency: decodeConcurrency(pb.GetConcurrency()), Version: item.Version,
	}, nil
}

func encodeJob(j domain.CIJob) ([]byte, error) {
	if j.Repo.IsZero() {
		return nil, fmt.Errorf("%w: empty name", domain.ErrInvalidName)
	}
	phase, err := encodePhase(j.Phase)
	if err != nil {
		return nil, err
	}
	steps := make([]*storagepb.CIStep, 0, len(j.Steps))
	for _, s := range j.Steps {
		steps = append(steps, &storagepb.CIStep{
			Number: s.Number, Name: s.Name, State: encodeStepState(s.State), Conclusion: encodeConclusion(s.Conclusion),
			StartedAt: encodeTime(s.StartedAt), CompletedAt: encodeTime(s.CompletedAt),
			LogStart: s.LogStart, HasLines: s.HasLines, Outcome: encodeConclusion(s.Outcome),
		})
	}
	pb := &storagepb.CIJob{
		Id: j.ID, RunId: j.RunID, Attempt: j.Attempt, RunAttempt: j.RunAttempt, Repo: j.Repo.String(),
		Key: j.Key, Name: j.Name, Phase: phase, Conclusion: encodeConclusion(j.Conclusion), Error: j.Error,
		Labels: j.Labels, Needs: j.Needs, RunnerId: j.RunnerID, RunnerName: j.RunnerName,
		Isolation: encodeIsolation(j.Isolation), Steps: steps,
		CreatedAt: encodeTime(j.CreatedAt), StartedAt: encodeTime(j.StartedAt), CompletedAt: encodeTime(j.CompletedAt),
		Index: int32(j.Index), Combo: int32(j.Combo), MatrixJson: j.MatrixJSON, ContextsJson: j.ContextsJSON, ComboTotal: int32(j.ComboTotal), //nolint:gosec // bounded by the 256 job limit
		TimeoutMinutes: j.TimeoutMinutes, FailFast: j.FailFast, ContinueOnError: j.ContinueOnError, MaxParallel: int32(j.MaxParallel), //nolint:gosec // validated at planning
		Outputs: j.Outputs, ReadyAt: encodeTime(j.ReadyAt), AssignedAt: encodeTime(j.AssignedAt),
		AckedAt: encodeTime(j.AckedAt), AssignCancels: int32(j.AssignCancels), //nolint:gosec // at most 3
		TokenValidUntil: encodeTime(j.TokenValidUntil), CancelReason: encodeCancelReason(j.CancelReason), CancelMessage: j.CancelMessage,
		CancelRequestedAt: encodeTime(j.CancelRequestedAt), Planned: j.Planned, Environment: j.Environment, Permissions: j.Permissions,
		Concurrency: encodeConcurrency(j.Concurrency), Scope: j.Scope, CallId: j.CallID, Call: encodeCall(j.Call),
	}
	if j.HasToken() {
		pb.TokenHash = j.TokenHash[:]
	}
	return mustMarshal(pb), nil
}

//hexcheck:ignore suspicious-business-logic-in-adapter maps the stored record to the domain job field by field: no rule is decided here
func decodeJob(item kv.Item) (domain.CIJob, error) {
	var pb storagepb.CIJob
	if err := proto.Unmarshal(item.Value, &pb); err != nil {
		return domain.CIJob{}, fmt.Errorf("decode job: %w", err)
	}
	repo, err := domain.ParseRepoName(pb.GetRepo())
	if err != nil {
		return domain.CIJob{}, fmt.Errorf("decode job: %w", err)
	}
	j := domain.CIJob{
		ID: pb.GetId(), RunID: pb.GetRunId(), Attempt: pb.GetAttempt(), RunAttempt: pb.GetRunAttempt(), Repo: repo,
		Key: pb.GetKey(), Name: pb.GetName(), Phase: decodePhase(pb.GetPhase()), Conclusion: decodeConclusion(pb.GetConclusion()),
		Error: pb.GetError(), Labels: pb.GetLabels(), Needs: pb.GetNeeds(), RunnerID: pb.GetRunnerId(), RunnerName: pb.GetRunnerName(),
		Isolation: decodeIsolation(pb.GetIsolation()),
		CreatedAt: decodeTime(pb.GetCreatedAt()), StartedAt: decodeTime(pb.GetStartedAt()), CompletedAt: decodeTime(pb.GetCompletedAt()),
		Index: int(pb.GetIndex()), Combo: int(pb.GetCombo()), Planned: pb.GetPlanned(), MatrixJSON: pb.GetMatrixJson(), ContextsJSON: pb.GetContextsJson(),
		ComboTotal: int(pb.GetComboTotal()), TimeoutMinutes: pb.GetTimeoutMinutes(), FailFast: pb.GetFailFast(), ContinueOnError: pb.GetContinueOnError(),
		MaxParallel: int(pb.GetMaxParallel()), Outputs: pb.GetOutputs(),
		TokenValidUntil: decodeTime(pb.GetTokenValidUntil()), ReadyAt: decodeTime(pb.GetReadyAt()),
		AssignedAt: decodeTime(pb.GetAssignedAt()), AckedAt: decodeTime(pb.GetAckedAt()), AssignCancels: int(pb.GetAssignCancels()),
		CancelMessage: pb.GetCancelMessage(), CancelRequestedAt: decodeTime(pb.GetCancelRequestedAt()),
		Environment: pb.GetEnvironment(), Permissions: pb.GetPermissions(), Concurrency: decodeConcurrency(pb.GetConcurrency()), Version: item.Version,
		Scope: pb.GetScope(), CallID: pb.GetCallId(), Call: decodeCall(pb.GetCall()),
	}
	j.CancelReason = decodeCancelReason(pb.GetCancelReason())
	copy(j.TokenHash[:], pb.GetTokenHash())
	for _, s := range pb.GetSteps() {
		j.Steps = append(j.Steps, domain.CIStep{
			Number: s.GetNumber(), Name: s.GetName(), State: decodeStepState(s.GetState()), Conclusion: decodeConclusion(s.GetConclusion()),
			StartedAt: decodeTime(s.GetStartedAt()), CompletedAt: decodeTime(s.GetCompletedAt()),
			LogStart: s.GetLogStart(), HasLines: s.GetHasLines(), Outcome: decodeConclusion(s.GetOutcome()),
		})
	}
	return j, nil
}

func encodeCheck(c domain.CICheck) []byte {
	return mustMarshal(&storagepb.CICheck{
		Id: c.ID, Repo: c.Repo.String(), Sha: c.Sha, Name: c.Name, Status: encodeStatus(c.Status),
		Conclusion: encodeConclusion(c.Conclusion), RunId: c.RunID, JobId: c.JobID, Attempt: c.Attempt,
		WorkflowName: c.WorkflowName, CreatedAt: encodeTime(c.CreatedAt), StartedAt: encodeTime(c.StartedAt),
		CompletedAt: encodeTime(c.CompletedAt),
	})
}

func decodeCheck(item kv.Item) (domain.CICheck, error) {
	var pb storagepb.CICheck
	if err := proto.Unmarshal(item.Value, &pb); err != nil {
		return domain.CICheck{}, fmt.Errorf("decode check: %w", err)
	}
	repo, err := domain.ParseRepoName(pb.GetRepo())
	if err != nil {
		return domain.CICheck{}, fmt.Errorf("decode check: %w", err)
	}
	return domain.CICheck{
		ID: pb.GetId(), Repo: repo, Sha: pb.GetSha(), Name: pb.GetName(), Status: decodeStatus(pb.GetStatus()),
		Conclusion: decodeConclusion(pb.GetConclusion()), RunID: pb.GetRunId(), JobID: pb.GetJobId(), Attempt: pb.GetAttempt(),
		WorkflowName: pb.GetWorkflowName(), CreatedAt: decodeTime(pb.GetCreatedAt()), StartedAt: decodeTime(pb.GetStartedAt()),
		CompletedAt: decodeTime(pb.GetCompletedAt()), Version: item.Version,
	}, nil
}

func encodeRunner(r domain.CIRunner) []byte {
	return mustMarshal(&storagepb.CIRunner{
		Id: r.ID, Name: r.Name, Labels: r.Labels, Isolation: encodeIsolation(r.Isolation), Version: r.Version,
		Arch: r.Arch, Os: r.OS, RegisteredAt: encodeTime(r.RegisteredAt), TokenHash: r.TokenHash[:],
	})
}

func decodeRunner(val []byte) (domain.CIRunner, error) {
	var pb storagepb.CIRunner
	if err := proto.Unmarshal(val, &pb); err != nil {
		return domain.CIRunner{}, fmt.Errorf("decode runner: %w", err)
	}
	r := domain.CIRunner{
		ID: pb.GetId(), Name: pb.GetName(), Labels: pb.GetLabels(), Isolation: decodeIsolation(pb.GetIsolation()),
		Version: pb.GetVersion(), Arch: pb.GetArch(), OS: pb.GetOs(), RegisteredAt: decodeTime(pb.GetRegisteredAt()),
	}
	copy(r.TokenHash[:], pb.GetTokenHash())
	return r, nil
}

// --- writes ---

// stage adds the preconditions and writes of one record to the batch.
func stage(b *kv.Batch, key, val []byte, version uint64) {
	if version == 0 {
		b.RequireAbsent(key)
	} else {
		b.RequireVersion(key, version)
	}
	b.Put(key, val)
}

// runIndexes stages the open-run index of the run.
func runIndexes(b *kv.Batch, r domain.CIRun) {
	if r.Status == domain.CICompleted {
		b.Delete(openKey(r.ID))
		return
	}
	b.Put(openKey(r.ID), []byte(r.Repo.String()))
}

// jobIndexes stages the ready, active and token indexes of the job. old is
// the job as stored before, if any: its token index goes when the token
// changed.
func (c *CI) jobIndexes(b *kv.Batch, j domain.CIJob, old *domain.CIJob) {
	ref := jobRef(j.RunID, j.RunAttempt, j.ID)
	if j.Phase == domain.CIPhaseReady {
		b.Put(readyKey(j.ReadyAt, ref), nil)
	} else {
		b.Delete(readyKey(j.ReadyAt, ref))
	}
	if old != nil && old.Phase == domain.CIPhaseReady && (j.Phase != domain.CIPhaseReady || !old.ReadyAt.Equal(j.ReadyAt)) {
		b.Delete(readyKey(old.ReadyAt, ref))
	}
	if j.Active() {
		b.Put(activeKey(ref), nil)
	} else {
		b.Delete(activeKey(ref))
	}
	if old != nil && old.HasToken() && old.TokenHash != j.TokenHash {
		b.Delete(jobTokenKey(old.TokenHash))
	}
	if j.HasToken() {
		b.Put(jobTokenKey(j.TokenHash), []byte(ref))
	}
}

// guardRepo is guardRepoKey on the database of the CI store, for the writes that
// create keys under a repository.
func (c *CI) guardRepo(ctx context.Context, b *kv.Batch, repo domain.RepoName) error {
	return guardRepoKey(ctx, c.db, b, repo)
}

// stageRecords stages the writes of w. guarded collects the repositories that
// need guarding.
func (c *CI) stageRecords(ctx context.Context, b *kv.Batch, w ports.CIWrite) (guarded map[domain.RepoName]bool, err error) {
	guarded = map[domain.RepoName]bool{}
	for _, r := range w.Runs {
		val, err := encodeRun(r)
		if err != nil {
			return nil, err
		}
		stage(b, runKey(r.Repo, r.ID), val, r.Version)
		runIndexes(b, r)
		if r.Version == 0 {
			guarded[r.Repo] = true
		}
	}
	for _, j := range w.Jobs {
		val, err := encodeJob(j)
		if err != nil {
			return nil, err
		}
		key := jobKey(j.RunID, j.RunAttempt, j.ID)
		var old *domain.CIJob
		if j.Version != 0 {
			item, ok := c.db.Get(ctx, key)
			if !ok {
				return nil, domain.ErrConflict
			}
			if o, err := decodeJob(item); err == nil {
				old = &o
			}
		}
		stage(b, key, val, j.Version)
		c.jobIndexes(b, j, old)
		if j.Version == 0 {
			guarded[j.Repo] = true
		}
	}
	c.stageRepoTokens(ctx, b, w, guarded)
	c.stageAnnotations(ctx, b, w, guarded)
	// Checks are derived from the jobs, written in the same batch and under
	// the lock of their run: they are upserted without a precondition.
	for _, ck := range w.Checks {
		b.Put(checkKey(ck.Repo, ck.Sha, ck.ID), encodeCheck(ck))
	}
	return guarded, nil
}

// Apply writes the records in one batch.
//
//hexcheck:ignore suspicious-business-logic-in-adapter stages records and their derived indexes under version preconditions: a storage concern, no policy
func (c *CI) Apply(ctx context.Context, w ports.CIWrite) (uint64, error) {
	var b kv.Batch
	guarded, err := c.stageRecords(ctx, &b, w)
	if err != nil {
		return 0, err
	}
	for repo := range guarded {
		if err := c.guardRepo(ctx, &b, repo); err != nil {
			return 0, err
		}
	}
	v, err := c.db.Apply(ctx, &b)
	if errors.Is(err, kv.ErrConflict) {
		return 0, domain.ErrConflict
	}
	if err != nil {
		return 0, fmt.Errorf("write CI records: %w", err)
	}
	return v, nil
}

func (c *CI) readSeq(ctx context.Context, key []byte) (last int64, item kv.Item, ok bool, err error) {
	item, ok = c.db.Get(ctx, key)
	if !ok {
		return 0, kv.Item{}, false, nil
	}
	var seq storagepb.Sequence
	if err := proto.Unmarshal(item.Value, &seq); err != nil {
		return 0, kv.Item{}, false, fmt.Errorf("decode sequence %s: %w", key, err)
	}
	return seq.GetLast(), item, true, nil
}

func stageSeq(b *kv.Batch, key []byte, item kv.Item, ok bool, last int64) {
	if ok {
		b.RequireVersion(key, item.Version)
	} else {
		b.RequireAbsent(key)
	}
	b.Put(key, mustMarshal(&storagepb.Sequence{Last: last}))
}

const createRunTries = 20

// CreateRun takes the next run id and number and stores the draft.
//
//hexcheck:ignore suspicious-business-logic-in-adapter takes the next numbers under version preconditions: a storage concern, no policy
func (c *CI) CreateRun(ctx context.Context, repo domain.RepoName, workflowPath string, build func(id, number int64) ports.CIRunDraft) (ports.CIRunDraft, error) {
	for range createRunTries {
		lastID, idItem, idOK, err := c.readSeq(ctx, []byte(runSeqKey))
		if err != nil {
			return ports.CIRunDraft{}, err
		}
		numKey := runNumKey(repo, workflowPath)
		lastNum, numItem, numOK, err := c.readSeq(ctx, numKey)
		if err != nil {
			return ports.CIRunDraft{}, err
		}
		draft := build(lastID+1, lastNum+1)
		var b kv.Batch
		stageSeq(&b, []byte(runSeqKey), idItem, idOK, lastID+1)
		stageSeq(&b, numKey, numItem, numOK, lastNum+1)
		w := ports.CIWrite{Runs: []domain.CIRun{draft.Run}, Jobs: draft.Jobs, Checks: draft.Checks}
		guarded, err := c.stageRecords(ctx, &b, w)
		if err != nil {
			return ports.CIRunDraft{}, err
		}
		src := mustMarshal(&storagepb.CIRunSource{
			RunId: draft.Run.ID, Repo: repo.String(), WorkflowYaml: draft.Source.WorkflowYAML, GithubJson: draft.Source.GithubJSON,
			PusherKind: string(draft.Source.Pusher.Kind), PusherTokenId: draft.Source.Pusher.TokenID,
		})
		b.RequireAbsent(runSrcKey(draft.Run.ID))
		b.Put(runSrcKey(draft.Run.ID), src)
		for r := range guarded {
			if err := c.guardRepo(ctx, &b, r); err != nil {
				return ports.CIRunDraft{}, err
			}
		}
		v, err := c.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue
		}
		if err != nil {
			return ports.CIRunDraft{}, fmt.Errorf("create run of %q: %w", repo, err)
		}
		draft.Run.Version = v
		for i := range draft.Jobs {
			draft.Jobs[i].Version = v
		}
		for i := range draft.Checks {
			draft.Checks[i].Version = v
		}
		return draft, nil
	}
	return ports.CIRunDraft{}, fmt.Errorf("create run of %q: %w", repo, domain.ErrConflict)
}

// --- reads ---

// GetRun reads run/<repo>/<id>.
func (c *CI) GetRun(ctx context.Context, repo domain.RepoName, id int64) (domain.CIRun, error) {
	item, ok := c.db.Get(ctx, runKey(repo, id))
	if !ok || id < 1 {
		return domain.CIRun{}, domain.ErrNotFound
	}
	return decodeRun(item)
}

// GetRunSource reads runsrc/<id>.
func (c *CI) GetRunSource(ctx context.Context, runID int64) (domain.CIRunSource, error) {
	item, ok := c.db.Get(ctx, runSrcKey(runID))
	if !ok || runID < 1 {
		return domain.CIRunSource{}, domain.ErrNotFound
	}
	var pb storagepb.CIRunSource
	if err := proto.Unmarshal(item.Value, &pb); err != nil {
		return domain.CIRunSource{}, fmt.Errorf("decode run source: %w", err)
	}
	repo, err := domain.ParseRepoName(pb.GetRepo())
	if err != nil {
		return domain.CIRunSource{}, fmt.Errorf("decode run source: %w", err)
	}
	return domain.CIRunSource{
		RunID: pb.GetRunId(), Repo: repo, WorkflowYAML: pb.GetWorkflowYaml(), GithubJSON: pb.GetGithubJson(),
		Pusher: domain.CIPusher{Kind: domain.CIPusherKind(pb.GetPusherKind()), TokenID: pb.GetPusherTokenId()},
	}, nil
}

// ListRuns returns the runs of the repository, newest first.
//
//hexcheck:ignore suspicious-business-logic-in-adapter port method name only: reads a key range and reverses it
func (c *CI) ListRuns(ctx context.Context, repo domain.RepoName, before int64, limit int) ([]domain.CIRun, error) {
	start := []byte(runsPrefix(repo))
	end := []byte(runPrefix + repo.String() + "0") // '/' + 1
	if before > 0 {
		end = runKey(repo, before)
	}
	// The snapshot iterates in key order only: keep the last limit items in a
	// ring and decode only those, so the cost of a page does not grow with the
	// decoded history of the repository.
	type entry struct {
		key  []byte
		item kv.Item
	}
	var ring []entry
	next := 0
	for key, item := range c.db.Snapshot(ctx).Range(ctx, start, end) {
		if limit <= 0 || len(ring) < limit {
			ring = append(ring, entry{key, item})
			continue
		}
		ring[next] = entry{key, item}
		next = (next + 1) % limit
	}
	ordered := append(ring[next:len(ring):len(ring)], ring[:next]...)
	all := make([]domain.CIRun, 0, len(ordered))
	for i := len(ordered) - 1; i >= 0; i-- {
		r, err := decodeRun(ordered[i].item)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", ordered[i].key, err)
		}
		all = append(all, r)
	}
	return all, nil
}

// RunIDs returns the ids of the runs of the repository.
func (c *CI) RunIDs(ctx context.Context, repo domain.RepoName) ([]int64, error) {
	var ids []int64
	prefix := runsPrefix(repo)
	for key := range c.db.Snapshot(ctx).Prefix(ctx, []byte(prefix)) {
		id, err := strconv.ParseInt(strings.TrimPrefix(string(key), prefix), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// ListOpenRuns returns the runs that have not completed.
func (c *CI) ListOpenRuns(ctx context.Context) ([]domain.CIRun, error) {
	snap := c.db.Snapshot(ctx)
	var out []domain.CIRun
	for key, item := range snap.Prefix(ctx, []byte(openPrefix)) {
		id, err := strconv.ParseInt(strings.TrimPrefix(string(key), openPrefix), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		repo, err := domain.ParseRepoName(string(item.Value))
		if err != nil {
			return nil, err
		}
		it, ok := snap.Get(ctx, runKey(repo, id))
		if !ok {
			continue
		}
		r, err := decodeRun(it)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// GetJob reads one job record.
func (c *CI) GetJob(ctx context.Context, runID int64, runAttempt int32, jobID string) (domain.CIJob, error) {
	item, ok := c.db.Get(ctx, jobKey(runID, runAttempt, jobID))
	if !ok {
		return domain.CIJob{}, domain.ErrNotFound
	}
	return decodeJob(item)
}

// ListJobs returns the jobs of a run at an attempt, ordered by index then
// combination.
func (c *CI) ListJobs(ctx context.Context, runID int64, runAttempt int32) ([]domain.CIJob, error) {
	var out []domain.CIJob
	for key, item := range c.db.Snapshot(ctx).Prefix(ctx, []byte(jobAttemptPrefix(runID, runAttempt))) {
		j, err := decodeJob(item)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		out = append(out, j)
	}
	slices.SortFunc(out, func(a, b domain.CIJob) int {
		if a.Index != b.Index {
			return a.Index - b.Index
		}
		return a.Combo - b.Combo
	})
	return out, nil
}

func (c *CI) jobByRef(snap *kv.Snapshot, ctx context.Context, ref string) (domain.CIJob, error) {
	item, ok := snap.Get(ctx, []byte(ref))
	if !ok {
		return domain.CIJob{}, domain.ErrNotFound
	}
	return decodeJob(item)
}

// FindJobByToken resolves the token index, then reads the job.
func (c *CI) FindJobByToken(ctx context.Context, hash domain.TokenHash, now time.Time) (domain.CIJob, error) {
	snap := c.db.Snapshot(ctx)
	idx, ok := snap.Get(ctx, jobTokenKey(hash))
	if !ok {
		return domain.CIJob{}, domain.ErrNotFound
	}
	j, err := c.jobByRef(snap, ctx, string(idx.Value))
	if err != nil {
		return domain.CIJob{}, err
	}
	if j.TokenHash != hash || (!j.TokenValidUntil.IsZero() && !now.Before(j.TokenValidUntil)) {
		return domain.CIJob{}, domain.ErrNotFound
	}
	return j, nil
}

// ListReadyJobs returns the jobs waiting for a runner, oldest first.
func (c *CI) ListReadyJobs(ctx context.Context, limit int) ([]domain.CIJob, error) {
	snap := c.db.Snapshot(ctx)
	var out []domain.CIJob
	for key := range snap.Prefix(ctx, []byte(readyPrefix)) {
		ref := string(key)[len(readyPrefix)+len(fmt.Sprintf(idFormat, 0))+1:]
		j, err := c.jobByRef(snap, ctx, ref)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, j)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// ListActiveJobs returns the jobs given to a runner and not over.
func (c *CI) ListActiveJobs(ctx context.Context) ([]domain.CIJob, error) {
	snap := c.db.Snapshot(ctx)
	var out []domain.CIJob
	for key := range snap.Prefix(ctx, []byte(activePrefix)) {
		j, err := c.jobByRef(snap, ctx, strings.TrimPrefix(string(key), activePrefix))
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, nil
}

// ListChecks returns the checks of a commit ordered by creation time then
// name.
func (c *CI) ListChecks(ctx context.Context, repo domain.RepoName, sha string, limit int) ([]domain.CICheck, bool, error) {
	var out []domain.CICheck
	for key, item := range c.db.Snapshot(ctx).Prefix(ctx, []byte(checksPrefix(repo, sha))) {
		ck, err := decodeCheck(item)
		if err != nil {
			return nil, false, fmt.Errorf("decode %s: %w", key, err)
		}
		out = append(out, ck)
	}
	slices.SortStableFunc(out, func(a, b domain.CICheck) int {
		if d := a.CreatedAt.Compare(b.CreatedAt); d != 0 {
			return d
		}
		return strings.Compare(a.Name, b.Name)
	})
	more := limit > 0 && len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

// --- retention and deletion ---

// runKeys lists every key of a run except its record: source, jobs, indexes.
func (c *CI) runKeys(ctx context.Context, snap *kv.Snapshot, repo domain.RepoName, runID int64) [][]byte {
	keys := [][]byte{runKey(repo, runID), runSrcKey(runID), openKey(runID)}
	keys = append(keys, repoTokenKeys(ctx, snap, runID)...)
	keys = append(keys, annotationKeys(ctx, snap, runID)...)
	for key, item := range snap.Prefix(ctx, []byte(jobsPrefix(runID))) {
		keys = append(keys, slices.Clone(key))
		if j, err := decodeJob(item); err == nil {
			ref := jobRef(j.RunID, j.RunAttempt, j.ID)
			keys = append(keys, readyKey(j.ReadyAt, ref), activeKey(ref))
			if j.HasToken() {
				keys = append(keys, jobTokenKey(j.TokenHash))
			}
		}
	}
	return keys
}

// ciKeys lists every key of the snapshot that belongs to the repository's CI
// runs, checks and numbers.
func ciKeys(ctx context.Context, snap *kv.Snapshot, repo domain.RepoName) [][]byte {
	c := &CI{}
	var keys [][]byte
	for key, item := range snap.Prefix(ctx, []byte(runsPrefix(repo))) {
		if r, err := decodeRun(item); err == nil {
			keys = append(keys, c.runKeys(ctx, snap, repo, r.ID)...)
		}
		keys = append(keys, slices.Clone(key))
	}
	keys = append(keys, repoReaderKeys(ctx, snap, repo)...)
	for _, p := range []string{checkPrefix + repo.String() + "/", runNumPrefix + repo.String() + "/", concurPrefix + repo.String() + "/", concurMarkPrefixAll + repo.String() + "/", refTipsPrefix(repo)} {
		for k := range snap.Prefix(ctx, []byte(p)) {
			keys = append(keys, slices.Clone(k))
		}
	}
	return keys
}

const sweepChunk = 500

// Sweep removes the runs completed before the time.
//
//hexcheck:ignore suspicious-business-logic-in-adapter chunked delete of expired records: a storage concern, the retention is decided by the caller
func (c *CI) Sweep(ctx context.Context, before time.Time) ([]int64, error) {
	snap := c.db.Snapshot(ctx)
	var ids []int64
	var keys [][]byte
	for _, rp := range []string{runPrefix} {
		for _, item := range snap.Prefix(ctx, []byte(rp)) {
			r, err := decodeRun(item)
			if err != nil {
				return nil, err
			}
			if r.Status != domain.CICompleted || r.CompletedAt.IsZero() || !r.CompletedAt.Before(before) {
				continue
			}
			ids = append(ids, r.ID)
			keys = append(keys, c.runKeys(ctx, snap, r.Repo, r.ID)...)
			for ck := range snap.Prefix(ctx, []byte(checkPrefix+r.Repo.String()+"/"+r.CheckSha()+"/")) {
				// A check belongs to the run only while it is the latest one
				// for its job: leave it when another run replaced it.
				if it, ok := snap.Get(ctx, ck); ok {
					if chk, err := decodeCheck(it); err == nil && chk.RunID == r.ID {
						keys = append(keys, slices.Clone(ck))
					}
				}
			}
		}
	}
	for len(keys) > 0 {
		n := min(sweepChunk, len(keys))
		var b kv.Batch
		for _, k := range keys[:n] {
			b.Delete(k)
		}
		if _, err := c.db.Apply(ctx, &b); err != nil {
			return nil, fmt.Errorf("sweep CI runs: %w", err)
		}
		keys = keys[n:]
	}
	return ids, nil
}

// --- runners ---

func runnerKey(id string) []byte               { return []byte(runnerPrefix + id) }
func runnerNameKey(n string) []byte            { return []byte(runnerName + strings.ToLower(n)) }
func runnerTokenKey(h domain.TokenHash) []byte { return []byte(runnerTokPref + h.String()) }

// CreateRunner stores the runner, its name index and its token index.
//
//hexcheck:ignore suspicious-business-logic-in-adapter replaces a runner of the same name in one batch: a storage concern, the rule is decided by the caller
func (c *CI) CreateRunner(ctx context.Context, r domain.CIRunner, replace bool) ([]domain.CIRunner, error) {
	for range createRunTries {
		var b kv.Batch
		var removed []domain.CIRunner
		if idx, ok := c.db.Get(ctx, runnerNameKey(r.Name)); ok {
			if !replace {
				return nil, domain.ErrAlreadyExists
			}
			old, err := c.GetRunner(ctx, string(idx.Value))
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return nil, err
			}
			b.RequireVersion(runnerNameKey(r.Name), idx.Version)
			if err == nil {
				removed = append(removed, old)
				b.Delete(runnerKey(old.ID))
				b.Delete(runnerTokenKey(old.TokenHash))
			}
		} else {
			b.RequireAbsent(runnerNameKey(r.Name))
		}
		b.RequireAbsent(runnerKey(r.ID))
		b.RequireAbsent(runnerTokenKey(r.TokenHash))
		b.Put(runnerKey(r.ID), encodeRunner(r))
		b.Put(runnerNameKey(r.Name), []byte(r.ID))
		b.Put(runnerTokenKey(r.TokenHash), []byte(r.ID))
		_, err := c.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			if !replace {
				if _, ok := c.db.Get(ctx, runnerNameKey(r.Name)); ok {
					return nil, domain.ErrAlreadyExists
				}
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("create runner %q: %w", r.Name, err)
		}
		return removed, nil
	}
	return nil, fmt.Errorf("create runner %q: %w", r.Name, domain.ErrConflict)
}

// GetRunner reads runner/<id>.
func (c *CI) GetRunner(ctx context.Context, id string) (domain.CIRunner, error) {
	item, ok := c.db.Get(ctx, runnerKey(id))
	if !ok || id == "" || strings.Contains(id, "/") {
		return domain.CIRunner{}, domain.ErrNotFound
	}
	return decodeRunner(item.Value)
}

// FindRunnerByToken resolves the token index, then reads the runner.
func (c *CI) FindRunnerByToken(ctx context.Context, hash domain.TokenHash) (domain.CIRunner, error) {
	snap := c.db.Snapshot(ctx)
	idx, ok := snap.Get(ctx, runnerTokenKey(hash))
	if !ok {
		return domain.CIRunner{}, domain.ErrNotFound
	}
	item, ok := snap.Get(ctx, runnerKey(string(idx.Value)))
	if !ok {
		return domain.CIRunner{}, domain.ErrNotFound
	}
	r, err := decodeRunner(item.Value)
	if err != nil {
		return domain.CIRunner{}, err
	}
	if r.TokenHash != hash {
		return domain.CIRunner{}, fmt.Errorf("runner %q: token index does not match the stored hash", r.ID)
	}
	return r, nil
}

// ListRunners returns every runner ordered by name.
func (c *CI) ListRunners(ctx context.Context) ([]domain.CIRunner, error) {
	var out []domain.CIRunner
	for key, item := range c.db.Snapshot(ctx).Prefix(ctx, []byte(runnerPrefix)) {
		r, err := decodeRunner(item.Value)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b domain.CIRunner) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out, nil
}

// RemoveRunner removes the runner, its name and its token.
func (c *CI) RemoveRunner(ctx context.Context, id string) (domain.CIRunner, error) {
	for range createRunTries {
		item, ok := c.db.Get(ctx, runnerKey(id))
		if !ok || id == "" || strings.Contains(id, "/") {
			return domain.CIRunner{}, domain.ErrNotFound
		}
		r, err := decodeRunner(item.Value)
		if err != nil {
			return domain.CIRunner{}, err
		}
		var b kv.Batch
		b.RequireVersion(runnerKey(id), item.Version)
		b.Delete(runnerKey(id))
		b.Delete(runnerNameKey(r.Name))
		b.Delete(runnerTokenKey(r.TokenHash))
		_, err = c.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue
		}
		if err != nil {
			return domain.CIRunner{}, fmt.Errorf("remove runner %q: %w", id, err)
		}
		return r, nil
	}
	return domain.CIRunner{}, fmt.Errorf("remove runner %q: %w", id, domain.ErrConflict)
}

func encodeCall(c *domain.CICall) *storagepb.CICall {
	if c == nil {
		return nil
	}
	return &storagepb.CICall{
		Uses: c.Uses, Path: c.Path, Repository: c.Repository, Ref: c.Ref, Sha: c.Sha, Yaml: c.YAML, InputsJson: c.InputsJSON,
		Secrets: c.Secrets, Inherit: c.Inherit, Depth: int32(c.Depth), Expanded: c.Expanded, Concurrency: encodeConcurrency(c.Concurrency), Admitted: c.Admitted, SecretCtx: c.SecretCtx, //nolint:gosec // at most 4
	}
}

//hexcheck:ignore suspicious-business-logic-in-adapter maps the stored call to the domain call field by field: no rule is decided here
func decodeCall(pb *storagepb.CICall) *domain.CICall {
	if pb == nil {
		return nil
	}
	return &domain.CICall{
		Uses: pb.GetUses(), Path: pb.GetPath(), Repository: pb.GetRepository(), Ref: pb.GetRef(), Sha: pb.GetSha(), YAML: pb.GetYaml(),
		InputsJSON: pb.GetInputsJson(), Secrets: pb.GetSecrets(), Inherit: pb.GetInherit(), Depth: int(pb.GetDepth()), Expanded: pb.GetExpanded(), Concurrency: decodeConcurrency(pb.GetConcurrency()),
		Admitted: pb.GetAdmitted(), SecretCtx: pb.GetSecretCtx(),
	}
}
