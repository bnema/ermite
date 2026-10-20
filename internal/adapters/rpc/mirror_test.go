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
	service.EXPECT().Sync(mock.Anything, domain.Principal{}, name, true).Return(ports.MirrorSyncResult{Repo: name, Status: status}, nil).Once()
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
	failed := watchEventToPB(ports.WatchEvent{Event: domain.MirrorFailed{Repo: name, State: domain.MirrorPaused, Class: domain.MirrorClassDiverged, At: now}})
	require.NotNil(t, failed)
	assert.Equal(t, now, failed.Time.AsTime())
	assert.Equal(t, &pb.MirrorFailed{Repo: "repo", State: pb.MirrorState_MIRROR_STATE_PAUSED, FailureClass: "diverged"}, failed.GetMirrorFailed())
	recovered := watchEventToPB(ports.WatchEvent{Event: domain.MirrorRecovered{Repo: name, At: now}})
	require.NotNil(t, recovered)
	assert.Equal(t, "repo", recovered.GetMirrorRecovered().Repo)
	// The public event messages deliberately expose neither URL nor stderr detail.
	fields := failed.GetMirrorFailed().ProtoReflect().Descriptor().Fields()
	assert.Nil(t, fields.ByName("url"))
	assert.Nil(t, fields.ByName("detail"))
	assert.Nil(t, fields.ByName("reason"))
}

func TestMirrorSyncAllReportsPerRepositoryErrors(t *testing.T) {
	service := mocks.NewMirrorService(t)
	good, bad := repoN(t, "good"), repoN(t, "bad")
	service.EXPECT().SyncAll(mock.Anything, domain.Principal{}, false).Return([]ports.MirrorSyncResult{
		{Repo: good, Status: domain.MirrorStatus{Repo: good, State: domain.MirrorOK}},
		{Repo: bad, Status: domain.MirrorStatus{Repo: bad, State: domain.MirrorPaused}, Err: &domain.MirrorError{Class: domain.MirrorClassDiverged, Detail: "rejected"}},
	}, nil).Once()
	service.EXPECT().GetStatus(mock.Anything, domain.Principal{}, good).Return(ports.MirrorStatusView{Status: domain.MirrorStatus{Repo: good, State: domain.MirrorOK}}, nil).Once()
	service.EXPECT().GetStatus(mock.Anything, domain.Principal{}, bad).Return(ports.MirrorStatusView{Status: domain.MirrorStatus{Repo: bad, State: domain.MirrorPaused}}, nil).Once()
	res, err := (&mirrorService{service}).SyncMirror(context.Background(), connect.NewRequest(&pb.SyncMirrorRequest{All: true}))
	require.NoError(t, err)
	require.Len(t, res.Msg.Results, 2)
	assert.Empty(t, res.Msg.Results[0].Error)
	assert.Equal(t, "mirror diverged: see ermite mirror status", res.Msg.Results[1].Error)
	assert.Equal(t, pb.MirrorState_MIRROR_STATE_PAUSED, res.Msg.Results[1].Status.State)
}

func TestMirrorRotateReturnsPreviousFingerprint(t *testing.T) {
	service := mocks.NewMirrorService(t)
	service.EXPECT().RotateKey(mock.Anything, domain.Principal{}).Return(ports.MirrorKeyRotation{
		Key: domain.MirrorPublicKey{Fingerprint: "SHA256:new"}, Previous: domain.MirrorPublicKey{Fingerprint: "SHA256:old"},
	}, nil).Once()
	res, err := (&mirrorService{service}).RotateMirrorKey(context.Background(), connect.NewRequest(&pb.RotateMirrorKeyRequest{}))
	require.NoError(t, err)
	assert.Equal(t, "SHA256:new", res.Msg.Key.Fingerprint)
	assert.Equal(t, "SHA256:old", res.Msg.PreviousFingerprint)
}

func repoN(t *testing.T, s string) domain.RepoName {
	t.Helper()
	n, err := domain.ParseRepoName(s)
	require.NoError(t, err)
	return n
}
