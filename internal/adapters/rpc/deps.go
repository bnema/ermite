// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// FeatureDeps is listed by GetVersion when DepsService is served.
const FeatureDeps = "deps"

// depsService implements ermiteconnect.DepsServiceHandler.
type depsService struct{ deps ports.DepsService }

var _ ermiteconnect.DepsServiceHandler = (*depsService)(nil)

func depKindToPB(k domain.DepKind) pb.DependencyUpdate_Kind {
	switch k {
	case domain.DepPatch:
		return pb.DependencyUpdate_KIND_PATCH
	case domain.DepMinor:
		return pb.DependencyUpdate_KIND_MINOR
	case domain.DepMajor:
		return pb.DependencyUpdate_KIND_MAJOR
	}
	return pb.DependencyUpdate_KIND_UNSPECIFIED
}

func depsStateToPB(s domain.DepsState) pb.RepoDependencyUpdates_State {
	switch s {
	case domain.DepsIdle:
		return pb.RepoDependencyUpdates_STATE_IDLE
	case domain.DepsApplying:
		return pb.RepoDependencyUpdates_STATE_APPLYING
	case domain.DepsFailed:
		return pb.RepoDependencyUpdates_STATE_FAILED
	case domain.DepsQueued:
		return pb.RepoDependencyUpdates_STATE_QUEUED
	case domain.DepsDrafted:
		return pb.RepoDependencyUpdates_STATE_DRAFTED
	}
	return pb.RepoDependencyUpdates_STATE_UNSPECIFIED
}

func repoDepsToPB(d domain.RepoDeps) *pb.RepoDependencyUpdates { return repoDepsToPBQueue(d, nil) }

// repoDepsToPBQueue is repoDepsToPB with the queue positions of order.
func repoDepsToPBQueue(d domain.RepoDeps, order []domain.DepsQueueEntry) *pb.RepoDependencyUpdates {
	base := d.Base()
	//nolint:staticcheck // fills the deprecated base fields older clients read
	out := &pb.RepoDependencyUpdates{
		Repo: d.Repo.String(), CheckedAt: timestamp(d.CheckedAt), SnoozedUntil: timestamp(d.SnoozedUntil),
		MergeRequestNumber: base.MR, State: depsStateToPB(base.State), Error: base.Error, RunId: base.RunID,
		Updates: repoDepsToPBUpdates(base.Updates), AutoOpen: !d.AutoOpenDisabled,
	}

	for _, n := range d.SplitOut {
		out.SplitOut = append(out.SplitOut, &pb.DependencyName{Ecosystem: string(n.Ecosystem), Name: n.Name})
	}

	for _, batch := range d.Batches {
		updates := repoDepsToPBUpdates(batch.Updates)
		out.Batches = append(out.Batches, &pb.DependencyBatch{
			Key: string(batch.Key), Updates: updates, State: depsStateToPB(batch.State), Error: batch.Error, RunId: batch.RunID,
			MergeRequestNumber: batch.MR, Outdated: batch.Outdated(), Adopted: batch.Adopted, Branch: batch.BranchName(), Repro: batch.Repro,
			QueuePosition: int32(domain.DepsQueuePosition(order, d.Repo, batch.Key)), //nolint:gosec // a queue position is small
		})
	}

	return out
}

func depRefsFromPB(in []*pb.DependencyVersion) []domain.DepRef {
	out := make([]domain.DepRef, 0, len(in))
	for _, v := range in {
		out = append(out, domain.DepRef{Ecosystem: domain.DepsEcosystem(v.GetEcosystem()), Directory: v.GetDirectory(), Name: v.GetName(), Version: v.GetVersion()})
	}
	return out
}

func (s *depsService) ListDependencyUpdates(ctx context.Context, req *pb.ListDependencyUpdatesRequest) (*pb.ListDependencyUpdatesResponse, error) {
	var repo domain.RepoName
	if req.Repo != nil {
		var err error
		if repo, err = domain.ParseRepoName(req.GetRepo()); err != nil {
			return nil, connectError(ctx, err)
		}
	}
	list, err := s.deps.List(ctx, caller(ctx), repo)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	queue, order, err := s.deps.Queue(ctx, caller(ctx))
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := &pb.ListDependencyUpdatesResponse{Queue: &pb.DependencyQueue{
		Running: int32(queue.Running), Limit: int32(queue.Limit), Queued: int32(queue.Queued), //nolint:gosec // small counts
	}}
	for _, d := range list {
		out.Repos = append(out.Repos, repoDepsToPBQueue(d, order))
	}
	return out, nil
}

func (s *depsService) CheckDependencies(ctx context.Context, req *pb.CheckDependenciesRequest) (*pb.CheckDependenciesResponse, error) {
	repo, err := domain.ParseRepoName(req.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	d, err := s.deps.Check(ctx, caller(ctx), repo)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.CheckDependenciesResponse{Updates: repoDepsToPB(d)}, nil
}

func (s *depsService) OpenDependencyUpdates(ctx context.Context, req *pb.OpenDependencyUpdatesRequest) (*pb.OpenDependencyUpdatesResponse, error) {
	repo, err := domain.ParseRepoName(req.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	d, err := s.deps.Open(ctx, caller(ctx), repo, domain.DepBatchKey(req.GetBatch()), depRefsFromPB(req.GetExpectedVersions()))
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.OpenDependencyUpdatesResponse{Updates: repoDepsToPB(d)}, nil
}

func (s *depsService) IgnoreDependencyUpdates(ctx context.Context, req *pb.IgnoreDependencyUpdatesRequest) (*pb.IgnoreDependencyUpdatesResponse, error) {
	repo, err := domain.ParseRepoName(req.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	d, err := s.deps.Ignore(ctx, caller(ctx), repo, domain.DepBatchKey(req.GetBatch()), depRefsFromPB(req.GetExpectedVersions()))
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.IgnoreDependencyUpdatesResponse{Updates: repoDepsToPB(d)}, nil
}

func (s *depsService) SnoozeDependencyUpdates(ctx context.Context, req *pb.SnoozeDependencyUpdatesRequest) (*pb.SnoozeDependencyUpdatesResponse, error) {
	repo, err := domain.ParseRepoName(req.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	d, err := s.deps.Snooze(ctx, caller(ctx), repo)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.SnoozeDependencyUpdatesResponse{Updates: repoDepsToPB(d)}, nil
}

func (s *depsService) SetDependencyAutoOpen(ctx context.Context, req *pb.SetDependencyAutoOpenRequest) (*pb.SetDependencyAutoOpenResponse, error) {
	repo, err := domain.ParseRepoName(req.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	d, err := s.deps.SetAutoOpen(ctx, caller(ctx), repo, req.GetEnabled())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.SetDependencyAutoOpenResponse{Updates: repoDepsToPB(d)}, nil
}

func (s *depsService) SplitDependency(ctx context.Context, req *pb.SplitDependencyRequest) (*pb.SplitDependencyResponse, error) {
	repo, err := domain.ParseRepoName(req.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	name := domain.DepName{Ecosystem: domain.DepsEcosystem(req.GetEcosystem()), Name: req.GetName()}
	d, err := s.deps.Split(ctx, caller(ctx), repo, name, req.GetSplit())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return &pb.SplitDependencyResponse{Updates: repoDepsToPB(d)}, nil
}

func repoDepsToPBUpdates(ups []domain.DepUpdate) []*pb.DependencyUpdate {
	out := make([]*pb.DependencyUpdate, 0, len(ups))
	for _, u := range ups {
		out = append(out, &pb.DependencyUpdate{Ecosystem: string(u.Ecosystem), Name: u.Name, Directory: u.Directory, Current: u.Current, Latest: u.Version, Kind: depKindToPB(u.Kind), New: u.New, NewName: u.NewName})
	}
	return out
}
