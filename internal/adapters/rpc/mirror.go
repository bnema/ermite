// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"errors"
	"fmt"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"connectrpc.com/connect"
)

const FeatureMirror = "mirror"

type mirrorService struct{ mirrors ports.MirrorService }

var _ ermiteconnect.MirrorServiceHandler = (*mirrorService)(nil)

func (s *mirrorService) GetMirrorKey(ctx context.Context, _ *connect.Request[pb.GetMirrorKeyRequest]) (*connect.Response[pb.GetMirrorKeyResponse], error) {
	k, err := s.mirrors.ShowKey(ctx, caller(ctx))
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.GetMirrorKeyResponse{Key: mirrorKeyToPB(k)}), nil
}
func (s *mirrorService) RotateMirrorKey(ctx context.Context, _ *connect.Request[pb.RotateMirrorKeyRequest]) (*connect.Response[pb.RotateMirrorKeyResponse], error) {
	r, err := s.mirrors.RotateKey(ctx, caller(ctx))
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.RotateMirrorKeyResponse{Key: mirrorKeyToPB(r.Key), PreviousFingerprint: r.Previous.Fingerprint}), nil
}
func (s *mirrorService) GetMirrorConfig(ctx context.Context, _ *connect.Request[pb.GetMirrorConfigRequest]) (*connect.Response[pb.GetMirrorConfigResponse], error) {
	g, err := s.mirrors.GetGlobalConfig(ctx, caller(ctx))
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.GetMirrorConfigResponse{Config: mirrorConfigToPB(g)}), nil
}
func (s *mirrorService) SetMirrorConfig(ctx context.Context, req *connect.Request[pb.SetMirrorConfigRequest]) (*connect.Response[pb.SetMirrorConfigResponse], error) {
	m := req.Msg
	g, err := s.mirrors.SetGlobalConfig(ctx, caller(ctx), ports.MirrorGlobalPatch{Enabled: m.Enabled, URLTemplate: m.UrlTemplate, Cron: m.Cron, AllBranches: m.AllBranches})
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.SetMirrorConfigResponse{Config: mirrorConfigToPB(g)}), nil
}
func (s *mirrorService) SetRepoMirror(ctx context.Context, req *connect.Request[pb.SetRepoMirrorRequest]) (*connect.Response[pb.SetRepoMirrorResponse], error) {
	m := req.Msg
	name, err := domain.ParseRepoName(m.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	t, err := s.mirrors.SetRepoMirror(ctx, caller(ctx), domain.MirrorRepoConfig{Repo: name, URL: m.GetUrl(), Cron: m.GetCron(), AllBranches: m.AllBranches})
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.SetRepoMirrorResponse{Target: mirrorTargetToPB(t)}), nil
}
func (s *mirrorService) RemoveRepoMirror(ctx context.Context, req *connect.Request[pb.RemoveRepoMirrorRequest]) (*connect.Response[pb.RemoveRepoMirrorResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	if err := s.mirrors.RemoveRepoMirror(ctx, caller(ctx), name); err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.RemoveRepoMirrorResponse{}), nil
}
func (s *mirrorService) SetRepoMirrorExcluded(ctx context.Context, req *connect.Request[pb.SetRepoMirrorExcludedRequest]) (*connect.Response[pb.SetRepoMirrorExcludedResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	t, err := s.mirrors.SetRepoExcluded(ctx, caller(ctx), name, req.Msg.GetExcluded())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.SetRepoMirrorExcludedResponse{Target: mirrorTargetToPB(t)}), nil
}
func (s *mirrorService) GetMirrorStatus(ctx context.Context, req *connect.Request[pb.GetMirrorStatusRequest]) (*connect.Response[pb.GetMirrorStatusResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	v, err := s.mirrors.GetStatus(ctx, caller(ctx), name)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.GetMirrorStatusResponse{Status: mirrorStatusToPB(v)}), nil
}
func (s *mirrorService) ListMirrorStatus(ctx context.Context, _ *connect.Request[pb.ListMirrorStatusRequest]) (*connect.Response[pb.ListMirrorStatusResponse], error) {
	list, err := s.mirrors.ListStatus(ctx, caller(ctx))
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := make([]*pb.MirrorStatus, 0, len(list))
	for _, v := range list {
		out = append(out, mirrorStatusToPB(v))
	}
	return connect.NewResponse(&pb.ListMirrorStatusResponse{Statuses: out}), nil
}
func (s *mirrorService) SyncMirror(ctx context.Context, req *connect.Request[pb.SyncMirrorRequest]) (*connect.Response[pb.SyncMirrorResponse], error) {
	m := req.Msg
	if m.GetAll() == (m.GetRepo() != "") {
		return nil, connectError(ctx, fmt.Errorf("%w: specify either repo or all", domain.ErrInvalid))
	}
	var results []ports.MirrorSyncResult
	if m.GetAll() {
		list, err := s.mirrors.SyncAll(ctx, caller(ctx), m.GetForce())
		if err != nil {
			return nil, connectError(ctx, err)
		}
		results = list
	} else {
		name, err := domain.ParseRepoName(m.GetRepo())
		if err != nil {
			return nil, connectError(ctx, err)
		}
		result, err := s.mirrors.Sync(ctx, caller(ctx), name, m.GetForce())
		if err != nil {
			return nil, connectError(ctx, err)
		}
		result.Repo = name
		results = []ports.MirrorSyncResult{result}
	}
	out := make([]*pb.SyncMirrorResult, 0, len(results))
	for _, result := range results {
		v, err := s.mirrors.GetStatus(ctx, caller(ctx), result.Repo)
		if err != nil {
			return nil, connectError(ctx, err)
		}
		if !result.Queued {
			v.Status = result.Status
		}
		item := &pb.SyncMirrorResult{Repo: result.Repo.String(), Queued: result.Queued, NoOp: result.NoOp, Status: mirrorStatusToPB(v)}
		if result.Err != nil {
			item.Error = syncErrorText(result.Err)
		}
		out = append(out, item)
	}
	return connect.NewResponse(&pb.SyncMirrorResponse{Results: out}), nil
}

// syncErrorText is the human reason a repository failed in SyncAll. The caller is an owner;
// a mirror error is already redacted by the transport and carries no URL.
func syncErrorText(err error) string {
	var me *domain.MirrorError
	if errors.As(err, &me) {
		return "mirror " + string(me.Class) + ": " + me.Detail
	}
	switch {
	case errors.Is(err, domain.ErrResourceExhausted):
		return err.Error()
	case errors.Is(err, domain.ErrNotFound):
		return "repository not found"
	case errors.Is(err, domain.ErrInvalid):
		return "invalid mirror configuration"
	}
	return "sync failed"
}

func mirrorKeyToPB(k domain.MirrorPublicKey) *pb.MirrorKey {
	return &pb.MirrorKey{AuthorizedKey: k.AuthorizedKey, Fingerprint: k.Fingerprint, CreatedAt: timestamp(k.CreatedAt)}
}
func mirrorConfigToPB(g domain.MirrorGlobalConfig) *pb.MirrorConfig {
	return &pb.MirrorConfig{Enabled: g.Enabled, UrlTemplate: g.URLTemplate, Cron: g.Cron, AllBranches: g.AllBranches, UpdatedAt: timestamp(g.UpdatedAt)}
}
func mirrorStateToPB(s domain.MirrorState) pb.MirrorState {
	switch s {
	case domain.MirrorDisabled:
		return pb.MirrorState_MIRROR_STATE_DISABLED
	case domain.MirrorOK:
		return pb.MirrorState_MIRROR_STATE_OK
	case domain.MirrorFailing:
		return pb.MirrorState_MIRROR_STATE_FAILING
	case domain.MirrorPaused:
		return pb.MirrorState_MIRROR_STATE_PAUSED
	default:
		return pb.MirrorState_MIRROR_STATE_UNSPECIFIED
	}
}
func mirrorTargetToPB(t domain.MirrorTarget) *pb.MirrorTarget {
	source := pb.MirrorSource_MIRROR_SOURCE_UNSPECIFIED
	switch t.Source {
	case domain.MirrorSourceNone:
		source = pb.MirrorSource_MIRROR_SOURCE_DISABLED
	case domain.MirrorSourceGlobal:
		source = pb.MirrorSource_MIRROR_SOURCE_GLOBAL
	case domain.MirrorSourceOverride:
		source = pb.MirrorSource_MIRROR_SOURCE_OVERRIDE
	case domain.MirrorSourceExcluded:
		source = pb.MirrorSource_MIRROR_SOURCE_EXCLUDED
	}
	return &pb.MirrorTarget{Repo: t.Repo.String(), Enabled: t.Enabled, Url: t.URL, Cron: t.CronSpec, AllBranches: t.AllBranches, Source: source}
}
func mirrorStatusToPB(v ports.MirrorStatusView) *pb.MirrorStatus {
	s := v.Status
	return &pb.MirrorStatus{Target: mirrorTargetToPB(v.Target), State: mirrorStateToPB(s.State), FailureClass: string(s.Class), Reason: s.Reason, Detail: s.Detail, ConsecutiveFailures: int32(s.ConsecutiveFailures), LastAttemptAt: timestamp(s.LastAttemptAt), LastSuccessAt: timestamp(s.LastSuccessAt), NextRetryAt: timestamp(s.NextRetryAt), NextRunAt: timestamp(v.NextRunAt), PausedAt: timestamp(s.PausedAt)}
}
