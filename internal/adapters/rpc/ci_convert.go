// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"fmt"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

func statusToPB(s domain.CIStatus) pb.CiStatus {
	switch s {
	case domain.CIQueued:
		return pb.CiStatus_CI_STATUS_QUEUED
	case domain.CIInProgress:
		return pb.CiStatus_CI_STATUS_IN_PROGRESS
	case domain.CICompleted:
		return pb.CiStatus_CI_STATUS_COMPLETED
	}
	return pb.CiStatus_CI_STATUS_UNSPECIFIED
}

func statusFromPB(s pb.CiStatus) (domain.CIStatus, error) {
	switch s {
	case pb.CiStatus_CI_STATUS_UNSPECIFIED:
		return "", nil
	case pb.CiStatus_CI_STATUS_QUEUED:
		return domain.CIQueued, nil
	case pb.CiStatus_CI_STATUS_IN_PROGRESS:
		return domain.CIInProgress, nil
	case pb.CiStatus_CI_STATUS_COMPLETED:
		return domain.CICompleted, nil
	}
	return "", fmt.Errorf("%w: status %v", domain.ErrInvalid, s)
}

var conclusionPairs = []struct {
	d domain.CIConclusion
	p pb.CiConclusion
}{
	{domain.CISuccess, pb.CiConclusion_CI_CONCLUSION_SUCCESS},
	{domain.CIFailure, pb.CiConclusion_CI_CONCLUSION_FAILURE},
	{domain.CICancelled, pb.CiConclusion_CI_CONCLUSION_CANCELLED},
	{domain.CISkipped, pb.CiConclusion_CI_CONCLUSION_SKIPPED},
	{domain.CITimedOut, pb.CiConclusion_CI_CONCLUSION_TIMED_OUT},
	{domain.CINeutral, pb.CiConclusion_CI_CONCLUSION_NEUTRAL},
	{domain.CIActionRequired, pb.CiConclusion_CI_CONCLUSION_ACTION_REQUIRED},
}

func conclusionToPB(c domain.CIConclusion) pb.CiConclusion {
	for _, e := range conclusionPairs {
		if e.d == c {
			return e.p
		}
	}
	return pb.CiConclusion_CI_CONCLUSION_UNSPECIFIED
}

func conclusionFromPB(c pb.CiConclusion) (domain.CIConclusion, error) {
	if c == pb.CiConclusion_CI_CONCLUSION_UNSPECIFIED {
		return domain.CINoConclusion, nil
	}
	for _, e := range conclusionPairs {
		if e.p == c {
			return e.d, nil
		}
	}
	return "", fmt.Errorf("%w: conclusion %v", domain.ErrInvalid, c)
}

func isolationToPB(i domain.CIIsolation) pb.RunnerIsolation {
	switch i {
	case domain.CIIsolationVM:
		return pb.RunnerIsolation_RUNNER_ISOLATION_VM
	case domain.CIIsolationContainerSysbox:
		return pb.RunnerIsolation_RUNNER_ISOLATION_CONTAINER_SYSBOX
	case domain.CIIsolationContainerPrivileged:
		return pb.RunnerIsolation_RUNNER_ISOLATION_CONTAINER_PRIVILEGED
	}
	return pb.RunnerIsolation_RUNNER_ISOLATION_UNSPECIFIED
}

func isolationFromPB(i pb.RunnerIsolation) (domain.CIIsolation, error) {
	switch i {
	case pb.RunnerIsolation_RUNNER_ISOLATION_VM:
		return domain.CIIsolationVM, nil
	case pb.RunnerIsolation_RUNNER_ISOLATION_CONTAINER_SYSBOX:
		return domain.CIIsolationContainerSysbox, nil
	case pb.RunnerIsolation_RUNNER_ISOLATION_CONTAINER_PRIVILEGED:
		return domain.CIIsolationContainerPrivileged, nil
	}
	return "", fmt.Errorf("%w: isolation is required", domain.ErrInvalid)
}

func stepStateToPB(s domain.CIStepState) pb.StepState {
	switch s {
	case domain.CIStepPending:
		return pb.StepState_STEP_STATE_PENDING
	case domain.CIStepInProgress:
		return pb.StepState_STEP_STATE_IN_PROGRESS
	case domain.CIStepCompleted:
		return pb.StepState_STEP_STATE_COMPLETED
	}
	return pb.StepState_STEP_STATE_UNSPECIFIED
}

func stepStateFromPB(s pb.StepState) domain.CIStepState {
	switch s {
	case pb.StepState_STEP_STATE_PENDING:
		return domain.CIStepPending
	case pb.StepState_STEP_STATE_IN_PROGRESS:
		return domain.CIStepInProgress
	case pb.StepState_STEP_STATE_COMPLETED:
		return domain.CIStepCompleted
	}
	return ""
}

func cancelReasonToPB(r domain.CICancelReason) pb.CancelReason {
	switch r {
	case domain.CICancelUser:
		return pb.CancelReason_CANCEL_REASON_USER
	case domain.CICancelFailFast:
		return pb.CancelReason_CANCEL_REASON_FAIL_FAST
	case domain.CICancelTimeout:
		return pb.CancelReason_CANCEL_REASON_TIMEOUT
	}
	return pb.CancelReason_CANCEL_REASON_UNSPECIFIED
}

// runName is Run.name: the name of the run, the workflow name for a run that
// has none of its own.
func runName(r domain.CIRun) string {
	if r.Name == "" {
		return r.WorkflowName
	}
	return r.Name
}

func runToPB(r domain.CIRun) *pb.Run {
	return &pb.Run{
		Repo: r.Repo.String(), Id: r.ID, Number: r.Number, Attempt: r.Attempt,
		WorkflowPath: r.WorkflowPath, WorkflowName: r.WorkflowName, Name: runName(r), Title: r.Title, Event: r.Event,
		Ref: r.Ref, Sha: r.Sha, Actor: r.Actor, Status: statusToPB(r.Status), Conclusion: conclusionToPB(r.Conclusion),
		Error: r.Error, CreatedAt: timestamp(r.CreatedAt), StartedAt: timestamp(r.StartedAt), CompletedAt: timestamp(r.CompletedAt),
		Version: int64(r.Version), //nolint:gosec // a store sequence, far below 2^63
	}
}

func stepsToPB(steps []domain.CIStep, lines int64) []*pb.Step {
	ranges := domain.StepRanges(steps, lines)
	out := make([]*pb.Step, 0, len(steps))
	for i, s := range steps {
		out = append(out, &pb.Step{
			Number: s.Number, Name: s.Name, State: stepStateToPB(s.State), Conclusion: conclusionToPB(s.Conclusion),
			StartedAt: timestamp(s.StartedAt), CompletedAt: timestamp(s.CompletedAt),
			LogStart: ranges[i].Start, LogEnd: ranges[i].End,
		})
	}
	return out
}

func jobToPB(j domain.CIJob, lines int64) *pb.RunJob {
	return &pb.RunJob{
		Id: j.ID, RunId: j.RunID, Attempt: j.Attempt, Repo: j.Repo.String(), Key: j.Key, Name: j.Name,
		Status: statusToPB(j.Status()), Conclusion: conclusionToPB(j.Conclusion), Error: j.Error,
		Labels: j.Labels, Needs: j.Needs, RunnerId: j.RunnerID, RunnerName: j.RunnerName, Isolation: isolationToPB(j.Isolation),
		Steps: stepsToPB(j.Steps, lines), CreatedAt: timestamp(j.CreatedAt), StartedAt: timestamp(j.StartedAt),
		CompletedAt: timestamp(j.CompletedAt),
		Version:     int64(j.Version), //nolint:gosec // a store sequence, far below 2^63
	}
}

func jobViewToPB(v ports.JobView) *pb.RunJob {
	out := jobToPB(v.CIJob, v.Lines)
	if v.RunnerGone {
		out.RunnerId = ""
	}
	return out
}

func checkToPB(c domain.CICheck) *pb.Check {
	return &pb.Check{
		Id: c.ID, Repo: c.Repo.String(), Sha: c.Sha, Name: c.Name, Status: statusToPB(c.Status),
		Conclusion: conclusionToPB(c.Conclusion), RunId: c.RunID, JobId: c.JobID, Attempt: c.Attempt,
		WorkflowName: c.WorkflowName, StartedAt: timestamp(c.StartedAt), CompletedAt: timestamp(c.CompletedAt),
	}
}

func runnerToPB(r ports.RunnerView) *pb.Runner {
	return &pb.Runner{
		Id: r.ID, Name: r.Name, Labels: r.Labels, Isolation: isolationToPB(r.Isolation), Version: r.Version,
		Arch: r.Arch, Os: r.OS, RegisteredAt: timestamp(r.RegisteredAt), LastSeen: timestamp(r.LastSeen), Busy: r.Busy,
	}
}

func logLineToPB(l ports.JobLogLine) *pb.JobLogLine {
	return &pb.JobLogLine{Step: l.Step, Time: timestamp(l.Time), Text: l.Text, Continued: l.Continued}
}

func runnerJobToPB(j *ports.RunnerJob) *pb.Job {
	return &pb.Job{
		JobId: j.Job.ID, RunId: j.Job.RunID, Attempt: j.Job.RunAttempt, JobToken: j.Token,
		WorkflowPath: j.WorkflowPath, WorkflowYaml: j.WorkflowYAML, JobKey: j.Job.Key, Matrix: j.Job.MatrixJSON,
		Contexts: j.Job.ContextsJSON, Labels: j.Job.Labels, TimeoutMinutes: j.Job.TimeoutMinutes,
		Repo: j.Repo.String(), Sha: j.Sha, Ref: j.Ref,
	}
}

func stepUpdatesFromPB(steps []*pb.StepUpdate) ([]ports.StepUpdate, error) {
	out := make([]ports.StepUpdate, 0, len(steps))
	for _, s := range steps {
		concl, err := conclusionFromPB(s.GetConclusion())
		if err != nil {
			return nil, err
		}
		out = append(out, ports.StepUpdate{
			Number: s.GetNumber(), Name: s.GetName(), State: stepStateFromPB(s.GetState()), Conclusion: concl,
			StartedAt: goTime(s.GetStartedAt()), CompletedAt: goTime(s.GetCompletedAt()),
		})
	}
	return out, nil
}

func jobEndedToPB(e *domain.JobEndedError) *pb.JobEnded {
	return &pb.JobEnded{Conclusion: conclusionToPB(e.Conclusion), Error: e.Message}
}

func runEventToPB(ev domain.RunUpdated) *pb.Event_RunUpdated {
	return &pb.Event_RunUpdated{RunUpdated: &pb.RunUpdated{Run: runToPB(ev.Run)}}
}

func jobEventToPB(ev domain.JobUpdated) *pb.Event_JobUpdated {
	return &pb.Event_JobUpdated{JobUpdated: &pb.JobUpdated{Job: jobToPB(ev.Job, ev.Lines)}}
}
