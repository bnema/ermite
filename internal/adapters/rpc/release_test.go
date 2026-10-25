// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

func releaseClient(t *testing.T, rel ports.ReleaseService) (ermiteconnect.ReleaseServiceClient, ermiteconnect.ServerServiceClient) {
	t.Helper()
	srv := httptest.NewUnstartedServer(New(Config{Releases: rel, Sessions: mocks.NewSessionService(t), SigningKey: mocks.NewSigningKeyService(t)}).Handler())
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return ermiteconnect.NewReleaseServiceClient(srv.Client(), srv.URL), ermiteconnect.NewServerServiceClient(srv.Client(), srv.URL)
}

func TestReleaseRPCUnary(t *testing.T) {
	ctx := context.Background()
	repo := mustRepoName(t, "demo")
	now := time.Now().UTC().Truncate(time.Second)
	r := domain.Release{ID: 7, Repo: repo, Tag: "v1", Title: "title", Notes: "notes", Prerelease: true, TargetCommitish: "abc", Author: "owner", CreatedAt: now, PublishedAt: now, UpdatedAt: now}
	a := domain.ReleaseAsset{ID: 9, ReleaseID: 7, Repo: repo, Name: "a.zip", Label: "label", ContentType: "application/zip", Size: 3, Digest: "sha256:123", Uploader: "owner", CreatedAt: now, UpdatedAt: now}
	expected := releaseToPB(r)
	expected.Latest = true
	expected.Assets = []*pb.ReleaseAsset{assetToPB(a)}
	for _, method := range []string{"create", "get id", "get tag", "get latest", "list", "update", "delete", "update asset", "delete asset", "list assets", "usage"} {
		t.Run(method, func(t *testing.T) {
			rel := mocks.NewReleaseService(t)
			c, _ := releaseClient(t, rel)
			p := domain.Anonymous()
			hydrate := func() {
				rel.EXPECT().GetRelease(mock.Anything, p, repo, ports.ReleaseRef{Latest: true}).Return(r, nil).Once()
				rel.EXPECT().ListAssets(mock.Anything, p, repo, r.ID).Return([]domain.ReleaseAsset{a}, nil).Once()
			}
			switch method {
			case "create":
				rel.EXPECT().CreateRelease(mock.Anything, p, repo, ports.CreateReleaseParams{Tag: "v1", TargetCommitish: "abc", Title: "title", Notes: "notes", Draft: true, Prerelease: true, MakeLatest: true}).Return(r, nil).Once()
				hydrate()
				res, err := c.CreateRelease(ctx, connect.NewRequest(&pb.CreateReleaseRequest{Repo: "demo", Tag: "v1", TargetCommitish: "abc", Title: "title", Notes: "notes", Draft: true, Prerelease: true, MakeLatest: true}))
				require.NoError(t, err)
				assert.True(t, proto.Equal(expected, res.Msg.GetRelease()))
			case "get id", "get tag", "get latest":
				req := &pb.GetReleaseRequest{Repo: "demo"}
				ref := ports.ReleaseRef{}
				switch method {
				case "get id":
					req.Selector = &pb.GetReleaseRequest_Id{Id: 7}
					ref.ID = 7
				case "get tag":
					req.Selector = &pb.GetReleaseRequest_Tag{Tag: "v1"}
					ref.Tag = "v1"
				default:
					req.Selector = &pb.GetReleaseRequest_Latest{Latest: true}
					ref.Latest = true
				}
				rel.EXPECT().GetRelease(mock.Anything, p, repo, ref).Return(r, nil).Once()
				hydrate()
				res, err := c.GetRelease(ctx, connect.NewRequest(req))
				require.NoError(t, err)
				assert.True(t, proto.Equal(expected, res.Msg.GetRelease()))
			case "list":
				rel.EXPECT().ListReleases(mock.Anything, p, repo, 17, "cursor").Return(ports.ReleasePage{Releases: []domain.Release{r}, NextPageToken: "next"}, nil).Once()
				hydrate()
				res, err := c.ListReleases(ctx, connect.NewRequest(&pb.ListReleasesRequest{Repo: "demo", PageSize: 17, PageToken: "cursor"}))
				require.NoError(t, err)
				require.Len(t, res.Msg.Releases, 1)
				assert.True(t, proto.Equal(expected, res.Msg.Releases[0]))
				assert.Equal(t, "next", res.Msg.NextPageToken)
			case "update":
				rel.EXPECT().UpdateRelease(mock.Anything, p, repo, int64(7), ports.UpdateReleaseParams{Title: new(""), Draft: new(false)}).Return(r, nil).Once()
				hydrate()
				res, err := c.UpdateRelease(ctx, connect.NewRequest(&pb.UpdateReleaseRequest{Repo: "demo", Id: 7, Tag: "ignored", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"title", "draft"}}}))
				require.NoError(t, err)
				assert.True(t, proto.Equal(expected, res.Msg.GetRelease()))
			case "delete":
				rel.EXPECT().DeleteRelease(mock.Anything, p, repo, int64(7)).Return(nil).Once()
				_, err := c.DeleteRelease(ctx, connect.NewRequest(&pb.DeleteReleaseRequest{Repo: "demo", Id: 7}))
				require.NoError(t, err)
			case "update asset":
				rel.EXPECT().UpdateAsset(mock.Anything, p, repo, int64(9), ports.UpdateAssetParams{Name: new("new.zip"), Label: new("")}).Return(a, nil).Once()
				res, err := c.UpdateReleaseAsset(ctx, connect.NewRequest(&pb.UpdateReleaseAssetRequest{Repo: "demo", AssetId: 9, Name: "new.zip", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"name", "label"}}}))
				require.NoError(t, err)
				assert.True(t, proto.Equal(assetToPB(a), res.Msg.GetAsset()))
			case "delete asset":
				rel.EXPECT().DeleteAsset(mock.Anything, p, repo, int64(9)).Return(nil).Once()
				_, err := c.DeleteReleaseAsset(ctx, connect.NewRequest(&pb.DeleteReleaseAssetRequest{Repo: "demo", AssetId: 9}))
				require.NoError(t, err)
			case "list assets":
				rel.EXPECT().ListAssets(mock.Anything, p, repo, int64(7)).Return([]domain.ReleaseAsset{a}, nil).Once()
				res, err := c.ListReleaseAssets(ctx, connect.NewRequest(&pb.ListReleaseAssetsRequest{Repo: "demo", ReleaseId: 7}))
				require.NoError(t, err)
				require.Len(t, res.Msg.Assets, 1)
				assert.True(t, proto.Equal(assetToPB(a), res.Msg.Assets[0]))
			case "usage":
				rel.EXPECT().Usage(mock.Anything, p, repo).Return(ports.ReleaseUsage{Used: 1, Limits: ports.ReleaseLimits{Quota: 2, AssetMax: 3, AssetsMax: 4}}, nil).Once()
				res, err := c.GetReleaseUsage(ctx, connect.NewRequest(&pb.GetReleaseUsageRequest{Repo: "demo"}))
				require.NoError(t, err)
				assert.True(t, proto.Equal(&pb.GetReleaseUsageResponse{UsedBytes: 1, QuotaBytes: 2, AssetMaxBytes: 3, AssetsMax: 4}, res.Msg))
			}
		})
	}
}

func TestReleaseFeatureAndMount(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "present"}[enabled], func(t *testing.T) {
			var rel ports.ReleaseService
			if enabled {
				rel = mocks.NewReleaseService(t)
			}
			c, server := releaseClient(t, rel)
			res, err := server.GetVersion(context.Background(), connect.NewRequest(&pb.GetVersionRequest{}))
			require.NoError(t, err)
			assert.Equal(t, enabled, containsReleaseFeature(res.Msg.Features))
			assert.NotContains(t, Features, FeatureReleases)
			if !enabled {
				_, err := c.GetReleaseUsage(context.Background(), connect.NewRequest(&pb.GetReleaseUsageRequest{Repo: "demo"}))
				requireCode(t, connect.CodeUnimplemented, err)
			}
		})
	}
}
func containsReleaseFeature(features []string) bool {
	for _, f := range features {
		if f == FeatureReleases {
			return true
		}
	}
	return false
}

func TestReleaseRPCUpload(t *testing.T) {
	for _, firstData := range []bool{false, true} {
		t.Run(map[bool]string{false: "header only", true: "header with data"}[firstData], func(t *testing.T) {
			rel := mocks.NewReleaseService(t)
			c, _ := releaseClient(t, rel)
			full := bytes.Repeat([]byte("x"), domain.CIArtifactChunk)
			tail := []byte("tail")
			want := append(bytes.Clone(full), tail...)
			rel.EXPECT().UploadAsset(mock.Anything, domain.Anonymous(), mustRepoName(t, "demo"), ports.UploadAssetParams{ReleaseID: 7, Name: "a", Label: "l", ContentType: "application/octet-stream", Size: int64(len(want)), Replace: true}, mock.Anything).RunAndReturn(func(_ context.Context, _ domain.Principal, _ domain.RepoName, _ ports.UploadAssetParams, r io.Reader) (domain.ReleaseAsset, error) {
				got, err := io.ReadAll(r)
				assert.NoError(t, err)
				assert.Equal(t, want, got)
				return domain.ReleaseAsset{ID: 9, Size: int64(len(got))}, err
			}).Once()
			stream := c.UploadReleaseAsset(context.Background())
			first := &pb.UploadReleaseAssetRequest{Repo: "demo", ReleaseId: 7, Name: "a", Label: "l", ContentType: "application/octet-stream", Size: int64(len(want)), Replace: true}
			if firstData {
				first.Data = full
			}
			require.NoError(t, stream.Send(first))
			if !firstData {
				require.NoError(t, stream.Send(&pb.UploadReleaseAssetRequest{Data: full}))
			}
			require.NoError(t, stream.Send(&pb.UploadReleaseAssetRequest{Data: tail}))
			res, err := stream.CloseAndReceive()
			require.NoError(t, err)
			assert.EqualValues(t, 9, res.Msg.Asset.Id)
			assert.EqualValues(t, len(want), res.Msg.Asset.Size)
		})
	}
}

func TestReleaseRPCUploadInvalid(t *testing.T) {
	tests := []struct {
		name     string
		messages []*pb.UploadReleaseAssetRequest
		read     bool
		code     connect.Code
	}{
		{name: "empty", code: connect.CodeInvalidArgument},
		{name: "negative size", messages: []*pb.UploadReleaseAssetRequest{{Repo: "demo", Size: -1}}, code: connect.CodeInvalidArgument},
		{name: "bad repo", messages: []*pb.UploadReleaseAssetRequest{{Repo: "../bad"}}, code: connect.CodeInvalidArgument},
		{name: "first chunk too big", messages: []*pb.UploadReleaseAssetRequest{{Repo: "demo", Data: make([]byte, domain.CIArtifactChunk+1)}}, code: connect.CodeInvalidArgument},
		{name: "later chunk too big", messages: []*pb.UploadReleaseAssetRequest{{Repo: "demo"}, {Data: make([]byte, domain.CIArtifactChunk+1)}}, read: true, code: connect.CodeInvalidArgument},
		{name: "message exceeds envelope", messages: []*pb.UploadReleaseAssetRequest{{Repo: "demo", Data: make([]byte, RunnerMaxRequestBytes+1)}}, code: connect.CodeResourceExhausted},
	}
	for _, m := range []*pb.UploadReleaseAssetRequest{{Repo: "demo"}, {ReleaseId: 1}, {Name: "a"}, {Label: "l"}, {ContentType: "text/plain"}, {Size: 1}, {Replace: true}} {
		tests = append(tests, struct {
			name     string
			messages []*pb.UploadReleaseAssetRequest
			read     bool
			code     connect.Code
		}{name: "repeated metadata " + m.String(), messages: []*pb.UploadReleaseAssetRequest{{Repo: "demo"}, m}, read: true, code: connect.CodeInvalidArgument})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rel := mocks.NewReleaseService(t)
			c, _ := releaseClient(t, rel)
			if tt.read {
				rel.EXPECT().UploadAsset(mock.Anything, domain.Anonymous(), mustRepoName(t, "demo"), mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, _ domain.Principal, _ domain.RepoName, _ ports.UploadAssetParams, r io.Reader) (domain.ReleaseAsset, error) {
					_, err := io.ReadAll(r)
					return domain.ReleaseAsset{}, err
				}).Once()
			}
			stream := c.UploadReleaseAsset(context.Background())
			for _, m := range tt.messages {
				if err := stream.Send(m); err != nil {
					break
				}
			}
			_, err := stream.CloseAndReceive()
			requireCode(t, tt.code, err)
		})
	}
}

func TestReleaseRPCDownload(t *testing.T) {
	for _, selector := range []string{"id", "tag", "latest"} {
		t.Run(selector, func(t *testing.T) {
			rel := mocks.NewReleaseService(t)
			c, _ := releaseClient(t, rel)
			req := &pb.DownloadReleaseAssetRequest{Repo: "demo", Offset: 3}
			ref := ports.AssetRef{}
			switch selector {
			case "id":
				req.Selector = &pb.DownloadReleaseAssetRequest_AssetId{AssetId: 9}
				ref.ID = 9
			case "tag":
				req.Selector = &pb.DownloadReleaseAssetRequest_Tag{Tag: &pb.ReleaseAssetTag{Tag: "v1", Name: "a"}}
				ref.Tag = "v1"
				ref.Name = "a"
			default:
				req.Selector = &pb.DownloadReleaseAssetRequest_Latest{Latest: &pb.ReleaseAssetLatest{Latest: true, Name: "a"}}
				ref.Latest = true
				ref.Name = "a"
			}
			a := domain.ReleaseAsset{ID: 9, ReleaseID: 7, Name: "a", Size: 8}
			rel.EXPECT().DownloadAsset(mock.Anything, domain.Anonymous(), mustRepoName(t, "demo"), ref, int64(3), int64(-1), mock.Anything).RunAndReturn(func(_ context.Context, _ domain.Principal, _ domain.RepoName, _ ports.AssetRef, _, _ int64, send func(ports.AssetChunk) error) error {
				if err := send(ports.AssetChunk{Asset: &a}); err != nil {
					return err
				}
				if err := send(ports.AssetChunk{Data: []byte("12")}); err != nil {
					return err
				}
				return send(ports.AssetChunk{Data: []byte("345")})
			}).Once()
			stream, err := c.DownloadReleaseAsset(context.Background(), connect.NewRequest(req))
			require.NoError(t, err)
			defer func() { _ = stream.Close() }()
			require.True(t, stream.Receive())
			assert.True(t, proto.Equal(assetToPB(a), stream.Msg().Asset))
			assert.Empty(t, stream.Msg().Chunk)
			var got []byte
			for stream.Receive() {
				assert.Nil(t, stream.Msg().Asset)
				got = append(got, stream.Msg().Chunk...)
			}
			require.NoError(t, stream.Err())
			assert.Equal(t, "12345", string(got))
		})
	}
}

func TestReleaseRPCErrors(t *testing.T) {
	for _, tt := range []struct {
		err  error
		code connect.Code
	}{{domain.ErrNotFound, connect.CodeNotFound}, {domain.ErrAlreadyExists, connect.CodeAlreadyExists}, {domain.ErrResourceExhausted, connect.CodeResourceExhausted}, {domain.ErrForbidden, connect.CodePermissionDenied}, {domain.ErrOutOfRange, connect.CodeOutOfRange}, {domain.ErrPreconditionFailed, connect.CodeFailedPrecondition}, {domain.ErrInvalid, connect.CodeInvalidArgument}} {
		t.Run(tt.code.String(), func(t *testing.T) {
			rel := mocks.NewReleaseService(t)
			c, _ := releaseClient(t, rel)
			rel.EXPECT().GetRelease(mock.Anything, domain.Anonymous(), mustRepoName(t, "demo"), ports.ReleaseRef{ID: 7}).Return(domain.Release{}, tt.err).Once()
			_, err := c.GetRelease(context.Background(), connect.NewRequest(&pb.GetReleaseRequest{Repo: "demo", Selector: &pb.GetReleaseRequest_Id{Id: 7}}))
			requireCode(t, tt.code, err)
			rel.EXPECT().DownloadAsset(mock.Anything, domain.Anonymous(), mustRepoName(t, "demo"), ports.AssetRef{ID: 9}, int64(0), int64(-1), mock.Anything).Return(tt.err).Once()
			stream, err := c.DownloadReleaseAsset(context.Background(), connect.NewRequest(&pb.DownloadReleaseAssetRequest{Repo: "demo", Selector: &pb.DownloadReleaseAssetRequest_AssetId{AssetId: 9}}))
			if err == nil {
				defer func() { _ = stream.Close() }()
				assert.False(t, stream.Receive())
				err = stream.Err()
			}
			requireCode(t, tt.code, err)
		})
	}
}

func TestReleaseMasksAndSelectors(t *testing.T) {
	paths := []string{"tag", "target_commitish", "title", "notes", "draft", "prerelease", "make_latest"}
	p, err := releaseUpdateParams(&pb.UpdateReleaseRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
	require.NoError(t, err)
	assert.Equal(t, ports.UpdateReleaseParams{Tag: new(""), TargetCommitish: new(""), Title: new(""), Notes: new(""), Draft: new(false), Prerelease: new(false), MakeLatest: new(false)}, p)
	for _, paths := range [][]string{nil, {"unknown"}, {"*"}, {"title", "assets"}} {
		_, err := releaseUpdateParams(&pb.UpdateReleaseRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
		assert.ErrorIs(t, err, domain.ErrInvalid)
		_, err = assetUpdateParams(&pb.UpdateReleaseAssetRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
		assert.ErrorIs(t, err, domain.ErrInvalid)
	}
	for _, req := range []*pb.GetReleaseRequest{{}, {Selector: &pb.GetReleaseRequest_Id{Id: -1}}, {Selector: &pb.GetReleaseRequest_Tag{}}, {Selector: &pb.GetReleaseRequest_Latest{Latest: false}}} {
		_, err := releaseRef(req)
		assert.ErrorIs(t, err, domain.ErrInvalid)
	}
	for _, req := range []*pb.DownloadReleaseAssetRequest{{}, {Selector: &pb.DownloadReleaseAssetRequest_AssetId{}}, {Selector: &pb.DownloadReleaseAssetRequest_Tag{}}, {Selector: &pb.DownloadReleaseAssetRequest_Tag{Tag: &pb.ReleaseAssetTag{Tag: "v1"}}}, {Selector: &pb.DownloadReleaseAssetRequest_Latest{Latest: &pb.ReleaseAssetLatest{Name: "a"}}}} {
		_, err := assetRef(req)
		assert.ErrorIs(t, err, domain.ErrInvalid)
	}
}

func TestReleaseEvents(t *testing.T) {
	r := domain.Release{ID: 7, Repo: mustRepoName(t, "demo"), Tag: "v1", Draft: true, Prerelease: true}
	at := time.Now().UTC()
	events := []domain.Event{domain.ReleasePublished{Release: r, At: at}, domain.ReleaseUpdated{Release: r, At: at}, domain.ReleaseDeleted{Release: r, At: at}, domain.ReleaseUnpublished{Release: r, At: at}}
	expected := []*pb.Event{
		{Kind: &pb.Event_ReleasePublished{ReleasePublished: &pb.ReleasePublished{Repo: "demo", Id: 7, Tag: "v1", Draft: true, Prerelease: true}}},
		{Kind: &pb.Event_ReleaseUpdated{ReleaseUpdated: &pb.ReleaseUpdated{Repo: "demo", Id: 7, Tag: "v1", Draft: true, Prerelease: true}}},
		{Kind: &pb.Event_ReleaseDeleted{ReleaseDeleted: &pb.ReleaseDeleted{Repo: "demo", Id: 7, Tag: "v1"}}},
		{Kind: &pb.Event_ReleaseUnpublished{ReleaseUnpublished: &pb.ReleaseUnpublished{Repo: "demo", Id: 7, Tag: "v1", Draft: true, Prerelease: true}}},
	}
	for i, ev := range events {
		t.Run(ev.EventKind(), func(t *testing.T) {
			expected[i].Id = "event"
			expected[i].Time = timestamp(at)
			assert.True(t, proto.Equal(expected[i], watchEventToPB(ports.WatchEvent{ID: "event", Event: ev})))
		})
	}
}

func TestReleaseResponseEnrichment(t *testing.T) {
	repo := mustRepoName(t, "demo")
	for _, tt := range []struct {
		name                 string
		latest               domain.Release
		latestErr, assetsErr error
		code                 connect.Code
		wantLatest           bool
	}{
		{name: "draft without latest", latestErr: domain.ErrNotFound},
		{name: "different latest", latest: domain.Release{ID: 8}},
		{name: "current latest", latest: domain.Release{ID: 7}, wantLatest: true},
		{name: "latest lookup fails", latestErr: domain.ErrUnavailable, code: connect.CodeUnavailable},
		{name: "assets lookup fails", assetsErr: domain.ErrNotFound, code: connect.CodeNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rel := mocks.NewReleaseService(t)
			rel.EXPECT().GetRelease(mock.Anything, domain.Owner(), repo, ports.ReleaseRef{ID: 7}).Return(domain.Release{ID: 7, Repo: repo, Draft: true}, nil).Once()
			rel.EXPECT().GetRelease(mock.Anything, domain.Owner(), repo, ports.ReleaseRef{Latest: true}).Return(tt.latest, tt.latestErr).Once()
			if tt.code != connect.CodeUnavailable {
				rel.EXPECT().ListAssets(mock.Anything, domain.Owner(), repo, int64(7)).Return(nil, tt.assetsErr).Once()
			}
			res, err := (&releaseService{rel: rel}).GetRelease(ownerCtx(), connect.NewRequest(&pb.GetReleaseRequest{Repo: "demo", Selector: &pb.GetReleaseRequest_Id{Id: 7}}))
			if tt.code != 0 {
				requireCode(t, tt.code, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantLatest, res.Msg.Release.Latest)
			assert.Empty(t, res.Msg.Release.Assets)
			assert.Nil(t, res.Msg.Release.PublishedAt)
		})
	}
}

func TestReleaseRPCAuthenticatedCaller(t *testing.T) {
	rel := mocks.NewReleaseService(t)
	auth := mocks.NewAuthService(t)
	auth.EXPECT().Authenticate(mock.Anything, "secret").Return(domain.Owner(), nil).Once()
	rel.EXPECT().DeleteRelease(mock.Anything, domain.Owner(), mustRepoName(t, "demo"), int64(7)).Return(nil).Once()
	srv := httptest.NewServer(New(Config{Releases: rel, Auth: auth, Sessions: mocks.NewSessionService(t)}).Handler())
	defer srv.Close()
	c := ermiteconnect.NewReleaseServiceClient(srv.Client(), srv.URL)
	req := connect.NewRequest(&pb.DeleteReleaseRequest{Repo: "demo", Id: 7})
	req.Header().Set("Authorization", "Bearer secret")
	_, err := c.DeleteRelease(context.Background(), req)
	require.NoError(t, err)
}

func TestReleaseRPCDownloadFullChunk(t *testing.T) {
	rel := mocks.NewReleaseService(t)
	c, _ := releaseClient(t, rel)
	data := bytes.Repeat([]byte("x"), domain.CIArtifactChunk)
	rel.EXPECT().DownloadAsset(mock.Anything, domain.Anonymous(), mustRepoName(t, "demo"), ports.AssetRef{ID: 9}, int64(0), int64(-1), mock.Anything).RunAndReturn(func(_ context.Context, _ domain.Principal, _ domain.RepoName, _ ports.AssetRef, _, _ int64, send func(ports.AssetChunk) error) error {
		if err := send(ports.AssetChunk{Asset: &domain.ReleaseAsset{ID: 9, Size: int64(len(data))}}); err != nil {
			return err
		}
		return send(ports.AssetChunk{Data: data})
	}).Once()
	stream, err := c.DownloadReleaseAsset(context.Background(), connect.NewRequest(&pb.DownloadReleaseAssetRequest{Repo: "demo", Selector: &pb.DownloadReleaseAssetRequest_AssetId{AssetId: 9}}))
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()
	require.True(t, stream.Receive())
	require.NotNil(t, stream.Msg().Asset)
	require.True(t, stream.Receive())
	assert.Equal(t, data, stream.Msg().Chunk)
	assert.False(t, stream.Receive())
	require.NoError(t, stream.Err())
}

func TestReleaseRPCRejectsInvalidMasksAndSelectors(t *testing.T) {
	c, _ := releaseClient(t, mocks.NewReleaseService(t))
	_, err := c.UpdateRelease(context.Background(), connect.NewRequest(&pb.UpdateReleaseRequest{Repo: "demo", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"unknown"}}}))
	requireCode(t, connect.CodeInvalidArgument, err)
	_, err = c.UpdateReleaseAsset(context.Background(), connect.NewRequest(&pb.UpdateReleaseAssetRequest{Repo: "demo"}))
	requireCode(t, connect.CodeInvalidArgument, err)
	_, err = c.GetRelease(context.Background(), connect.NewRequest(&pb.GetReleaseRequest{Repo: "demo", Selector: &pb.GetReleaseRequest_Latest{Latest: false}}))
	requireCode(t, connect.CodeInvalidArgument, err)
	stream, err := c.DownloadReleaseAsset(context.Background(), connect.NewRequest(&pb.DownloadReleaseAssetRequest{Repo: "demo", Selector: &pb.DownloadReleaseAssetRequest_Latest{Latest: &pb.ReleaseAssetLatest{Name: "a"}}}))
	if err == nil {
		defer func() { _ = stream.Close() }()
		assert.False(t, stream.Receive())
		err = stream.Err()
	}
	requireCode(t, connect.CodeInvalidArgument, err)
}

func TestReleaseRPCListResolvesLatestOnce(t *testing.T) {
	repo := mustRepoName(t, "demo")
	rel := mocks.NewReleaseService(t)
	c, _ := releaseClient(t, rel)
	p := domain.Anonymous()
	rows := []domain.Release{{ID: 3, Repo: repo, Tag: "c"}, {ID: 2, Repo: repo, Tag: "b"}, {ID: 1, Repo: repo, Tag: "a"}}
	rel.EXPECT().ListReleases(mock.Anything, p, repo, 0, "").Return(ports.ReleasePage{Releases: rows}, nil).Once()
	rel.EXPECT().GetRelease(mock.Anything, p, repo, ports.ReleaseRef{Latest: true}).Return(rows[1], nil).Once()
	for _, r := range rows {
		rel.EXPECT().ListAssets(mock.Anything, p, repo, r.ID).Return(nil, nil).Once()
	}
	res, err := c.ListReleases(context.Background(), connect.NewRequest(&pb.ListReleasesRequest{Repo: "demo"}))
	require.NoError(t, err)
	require.Len(t, res.Msg.Releases, 3)
	for _, m := range res.Msg.Releases {
		assert.Equal(t, m.Id == 2, m.Latest, m.Tag)
	}
}

func TestReleaseRPCListWithoutLatest(t *testing.T) {
	repo := mustRepoName(t, "demo")
	rel := mocks.NewReleaseService(t)
	c, _ := releaseClient(t, rel)
	p := domain.Anonymous()
	rel.EXPECT().ListReleases(mock.Anything, p, repo, 0, "").Return(ports.ReleasePage{Releases: []domain.Release{{ID: 1, Repo: repo}}}, nil).Once()
	rel.EXPECT().GetRelease(mock.Anything, p, repo, ports.ReleaseRef{Latest: true}).Return(domain.Release{}, domain.ErrNotFound).Once()
	rel.EXPECT().ListAssets(mock.Anything, p, repo, int64(1)).Return(nil, nil).Once()
	res, err := c.ListReleases(context.Background(), connect.NewRequest(&pb.ListReleasesRequest{Repo: "demo"}))
	require.NoError(t, err)
	require.Len(t, res.Msg.Releases, 1)
	assert.False(t, res.Msg.Releases[0].Latest)
}

func TestReleaseRPCDownloadZeroByteAsset(t *testing.T) {
	rel := mocks.NewReleaseService(t)
	c, _ := releaseClient(t, rel)
	a := domain.ReleaseAsset{ID: 9, ReleaseID: 7, Name: "empty", Size: 0}
	// The use case sends the metadata and no data for an empty asset.
	rel.EXPECT().DownloadAsset(mock.Anything, domain.Anonymous(), mustRepoName(t, "demo"), ports.AssetRef{ID: 9}, int64(0), int64(-1), mock.Anything).RunAndReturn(func(_ context.Context, _ domain.Principal, _ domain.RepoName, _ ports.AssetRef, _, _ int64, send func(ports.AssetChunk) error) error {
		return send(ports.AssetChunk{Asset: &a})
	}).Once()
	stream, err := c.DownloadReleaseAsset(context.Background(), connect.NewRequest(&pb.DownloadReleaseAssetRequest{Repo: "demo", Selector: &pb.DownloadReleaseAssetRequest_AssetId{AssetId: 9}}))
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()
	require.True(t, stream.Receive())
	assert.True(t, proto.Equal(assetToPB(a), stream.Msg().Asset))
	assert.False(t, stream.Receive())
	require.NoError(t, stream.Err())
}
