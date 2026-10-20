// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"testing"
	"time"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestMirrorConfigPatchPresence(t *testing.T) {
	service := mocks.NewMirrorService(t)
	disabled := false
	empty := ""
	patch := ports.MirrorGlobalPatch{Enabled: &disabled, URLTemplate: &empty}
	service.EXPECT().SetGlobalConfig(mock.Anything, domain.Principal{}, patch).Return(domain.MirrorGlobalConfig{}, nil).Once()
	s := &mirrorService{service}
	res, err := s.SetMirrorConfig(context.Background(), connect.NewRequest(&pb.SetMirrorConfigRequest{Enabled: &disabled, UrlTemplate: &empty}))
	require.NoError(t, err)
	assert.False(t, res.Msg.GetConfig().GetEnabled())
}

func TestMirrorSyncSelector(t *testing.T) {
	s := &mirrorService{mocks.NewMirrorService(t)}
	for _, req := range []*pb.SyncMirrorRequest{{}, {Repo: "repo", All: true}, {Repo: "../invalid"}} {
		_, err := s.SyncMirror(context.Background(), connect.NewRequest(req))
		assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
	}
}

func TestMirrorSyncResponse(t *testing.T) {
	service := mocks.NewMirrorService(t)
	name, err := domain.ParseRepoName("repo")
	require.NoError(t, err)
	status := domain.MirrorStatus{Repo: name, State: domain.MirrorPaused, Class: domain.MirrorClassDiverged, Reason: "remote diverged", ConsecutiveFailures: 1}
	target := domain.MirrorTarget{Repo: name, Enabled: true, Source: domain.MirrorSourceOverride}
	service.EXPECT().Sync(mock.Anything, domain.Principal{}, name, true).Return(ports.MirrorSyncResult{Status: status}, nil).Once()
	service.EXPECT().GetStatus(mock.Anything, domain.Principal{}, name).Return(ports.MirrorStatusView{Target: target, Status: status}, nil).Once()
	s := &mirrorService{service}
	res, err := s.SyncMirror(context.Background(), connect.NewRequest(&pb.SyncMirrorRequest{Repo: "repo", Force: true}))
	require.NoError(t, err)
	require.Len(t, res.Msg.Results, 1)
	assert.Equal(t, "repo", res.Msg.Results[0].Repo)
	assert.Equal(t, pb.MirrorState_MIRROR_STATE_PAUSED, res.Msg.Results[0].Status.State)
	assert.Equal(t, "diverged", res.Msg.Results[0].Status.FailureClass)
}

func TestMirrorEvents(t *testing.T) {
	name, err := domain.ParseRepoName("repo")
	require.NoError(t, err)
	now := time.Now().UTC()
	failed := watchEventToPB(ports.WatchEvent{Event: domain.MirrorFailed{Repo: name, State: domain.MirrorPaused, Class: domain.MirrorClassDiverged, Reason: "remote diverged", At: now}})
	require.NotNil(t, failed)
	assert.Equal(t, now, failed.Time.AsTime())
	assert.Equal(t, &pb.MirrorFailed{Repo: "repo", State: pb.MirrorState_MIRROR_STATE_PAUSED, FailureClass: "diverged", Reason: "remote diverged"}, failed.GetMirrorFailed())
	recovered := watchEventToPB(ports.WatchEvent{Event: domain.MirrorRecovered{Repo: name, At: now}})
	require.NotNil(t, recovered)
	assert.Equal(t, "repo", recovered.GetMirrorRecovered().Repo)
	// The public event messages deliberately expose neither URL nor stderr detail.
	fields := failed.GetMirrorFailed().ProtoReflect().Descriptor().Fields()
	assert.Nil(t, fields.ByName("url"))
	assert.Nil(t, fields.ByName("detail"))
}
