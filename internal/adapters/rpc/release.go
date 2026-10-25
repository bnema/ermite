// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"errors"
	"fmt"
	"io"

	"connectrpc.com/connect"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// FeatureReleases is listed by GetVersion when ReleaseService is served.
const FeatureReleases = "releases"

// releaseService implements ermiteconnect.ReleaseServiceHandler. journal
// serves ListUnreleased; nil answers unimplemented.
type releaseService struct {
	rel     ports.ReleaseService
	journal ports.JournalService
}

var _ ermiteconnect.ReleaseServiceHandler = (*releaseService)(nil)

// releaseMessage fills the computed latest flag and uploaded assets using the
// same caller's read permissions. No latest release is a normal repository state.
func (s *releaseService) releaseMessage(ctx context.Context, repo domain.RepoName, r domain.Release) (*pb.Release, error) {
	latest, err := s.latestID(ctx, repo)
	if err != nil {
		return nil, err
	}
	return s.releaseRow(ctx, repo, r, latest)
}

// latestID is the id of the latest release, or 0 when the repository has none.
func (s *releaseService) latestID(ctx context.Context, repo domain.RepoName) (int64, error) {
	latest, err := s.rel.GetRelease(ctx, caller(ctx), repo, ports.ReleaseRef{Latest: true})
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return 0, err
	}
	return latest.ID, nil
}

// releaseRow converts one release given the id of the latest one, so a list
// resolves the latest release once.
func (s *releaseService) releaseRow(ctx context.Context, repo domain.RepoName, r domain.Release, latest int64) (*pb.Release, error) {
	assets, err := s.rel.ListAssets(ctx, caller(ctx), repo, r.ID)
	if err != nil {
		return nil, err
	}
	out := releaseToPB(r)
	out.Latest = latest != 0 && latest == r.ID
	out.Assets = assetsToPB(assets)
	return out, nil
}

func (s *releaseService) CreateRelease(ctx context.Context, req *connect.Request[pb.CreateReleaseRequest]) (*connect.Response[pb.CreateReleaseResponse], error) {
	m := req.Msg
	repo, err := repoOf(m.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	r, err := s.rel.CreateRelease(ctx, caller(ctx), repo, ports.CreateReleaseParams{Tag: m.GetTag(), TargetCommitish: m.GetTargetCommitish(), Title: m.GetTitle(), Notes: m.GetNotes(), Draft: m.GetDraft(), Prerelease: m.GetPrerelease(), MakeLatest: m.GetMakeLatest()})
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out, err := s.releaseMessage(ctx, repo, r)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.CreateReleaseResponse{Release: out}), nil
}

func releaseRef(m *pb.GetReleaseRequest) (ports.ReleaseRef, error) {
	switch v := m.GetSelector().(type) {
	case *pb.GetReleaseRequest_Id:
		if v.Id > 0 {
			return ports.ReleaseRef{ID: v.Id}, nil
		}
	case *pb.GetReleaseRequest_Tag:
		if v.Tag != "" {
			return ports.ReleaseRef{Tag: v.Tag}, nil
		}
	case *pb.GetReleaseRequest_Latest:
		if v.Latest {
			return ports.ReleaseRef{Latest: true}, nil
		}
	}
	return ports.ReleaseRef{}, fmt.Errorf("%w: select an id, tag or latest=true", domain.ErrInvalid)
}

func (s *releaseService) GetRelease(ctx context.Context, req *connect.Request[pb.GetReleaseRequest]) (*connect.Response[pb.GetReleaseResponse], error) {
	repo, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	ref, err := releaseRef(req.Msg)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	r, err := s.rel.GetRelease(ctx, caller(ctx), repo, ref)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out, err := s.releaseMessage(ctx, repo, r)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.GetReleaseResponse{Release: out}), nil
}

func (s *releaseService) ListReleases(ctx context.Context, req *connect.Request[pb.ListReleasesRequest]) (*connect.Response[pb.ListReleasesResponse], error) {
	repo, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	page, err := s.rel.ListReleases(ctx, caller(ctx), repo, int(req.Msg.GetPageSize()), req.Msg.GetPageToken())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := &pb.ListReleasesResponse{Releases: make([]*pb.Release, 0, len(page.Releases)), NextPageToken: page.NextPageToken}
	latest, err := s.latestID(ctx, repo)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	for _, r := range page.Releases {
		m, err := s.releaseRow(ctx, repo, r, latest)
		if err != nil {
			return nil, connectError(ctx, err)
		}
		out.Releases = append(out.Releases, m)
	}
	return connect.NewResponse(out), nil
}

func releaseUpdateParams(m *pb.UpdateReleaseRequest) (ports.UpdateReleaseParams, error) {
	var p ports.UpdateReleaseParams
	if len(m.GetUpdateMask().GetPaths()) == 0 {
		return p, fmt.Errorf("%w: update_mask is empty", domain.ErrInvalid)
	}
	for _, path := range m.GetUpdateMask().GetPaths() {
		switch path {
		case "tag":
			p.Tag = new(m.GetTag())
		case "target_commitish":
			p.TargetCommitish = new(m.GetTargetCommitish())
		case "title":
			p.Title = new(m.GetTitle())
		case "notes":
			p.Notes = new(m.GetNotes())
		case "draft":
			p.Draft = new(m.GetDraft())
		case "prerelease":
			p.Prerelease = new(m.GetPrerelease())
		case "make_latest":
			p.MakeLatest = new(m.GetMakeLatest())
		default:
			return p, fmt.Errorf("%w: unknown update_mask path %q", domain.ErrInvalid, path)
		}
	}
	return p, nil
}

func (s *releaseService) UpdateRelease(ctx context.Context, req *connect.Request[pb.UpdateReleaseRequest]) (*connect.Response[pb.UpdateReleaseResponse], error) {
	repo, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	p, err := releaseUpdateParams(req.Msg)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	r, err := s.rel.UpdateRelease(ctx, caller(ctx), repo, req.Msg.GetId(), p)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out, err := s.releaseMessage(ctx, repo, r)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.UpdateReleaseResponse{Release: out}), nil
}

func (s *releaseService) DeleteRelease(ctx context.Context, req *connect.Request[pb.DeleteReleaseRequest]) (*connect.Response[pb.DeleteReleaseResponse], error) {
	repo, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	if err := s.rel.DeleteRelease(ctx, caller(ctx), repo, req.Msg.GetId()); err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.DeleteReleaseResponse{}), nil
}

// chunkReader retains at most one message; metadata belongs to the first only.
type chunkReader struct {
	stream *connect.ClientStream[pb.UploadReleaseAssetRequest]
	buf    []byte
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.buf) == 0 {
		if !r.stream.Receive() {
			if err := r.stream.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		m := r.stream.Msg()
		if m.GetRepo() != "" || m.GetReleaseId() != 0 || m.GetName() != "" || m.GetLabel() != "" || m.GetContentType() != "" || m.GetSize() != 0 || m.GetReplace() {
			return 0, fmt.Errorf("%w: upload metadata is in the first message only", domain.ErrInvalid)
		}
		if err := checkBlockData(m.GetData()); err != nil {
			return 0, err
		}
		r.buf = m.GetData()
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (s *releaseService) UploadReleaseAsset(ctx context.Context, stream *connect.ClientStream[pb.UploadReleaseAssetRequest]) (*connect.Response[pb.UploadReleaseAssetResponse], error) {
	if !stream.Receive() {
		err := stream.Err()
		if err == nil {
			err = fmt.Errorf("%w: the stream has no message", domain.ErrInvalid)
		}
		return nil, connectError(ctx, err)
	}
	m := stream.Msg()
	repo, err := repoOf(m.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	if m.GetSize() < 0 {
		return nil, connectError(ctx, fmt.Errorf("%w: size must be nonnegative", domain.ErrInvalid))
	}
	if err := checkBlockData(m.GetData()); err != nil {
		return nil, connectError(ctx, err)
	}
	a, err := s.rel.UploadAsset(ctx, caller(ctx), repo, ports.UploadAssetParams{ReleaseID: m.GetReleaseId(), Name: m.GetName(), Label: m.GetLabel(), ContentType: m.GetContentType(), Size: m.GetSize(), Replace: m.GetReplace()}, &chunkReader{stream: stream, buf: m.GetData()})
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.UploadReleaseAssetResponse{Asset: assetToPB(a)}), nil
}

func assetUpdateParams(m *pb.UpdateReleaseAssetRequest) (ports.UpdateAssetParams, error) {
	var p ports.UpdateAssetParams
	if len(m.GetUpdateMask().GetPaths()) == 0 {
		return p, fmt.Errorf("%w: update_mask is empty", domain.ErrInvalid)
	}
	for _, path := range m.GetUpdateMask().GetPaths() {
		switch path {
		case "name":
			p.Name = new(m.GetName())
		case "label":
			p.Label = new(m.GetLabel())
		default:
			return p, fmt.Errorf("%w: unknown update_mask path %q", domain.ErrInvalid, path)
		}
	}
	return p, nil
}

func (s *releaseService) UpdateReleaseAsset(ctx context.Context, req *connect.Request[pb.UpdateReleaseAssetRequest]) (*connect.Response[pb.UpdateReleaseAssetResponse], error) {
	repo, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	p, err := assetUpdateParams(req.Msg)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	a, err := s.rel.UpdateAsset(ctx, caller(ctx), repo, req.Msg.GetAssetId(), p)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.UpdateReleaseAssetResponse{Asset: assetToPB(a)}), nil
}

func (s *releaseService) DeleteReleaseAsset(ctx context.Context, req *connect.Request[pb.DeleteReleaseAssetRequest]) (*connect.Response[pb.DeleteReleaseAssetResponse], error) {
	repo, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	if err := s.rel.DeleteAsset(ctx, caller(ctx), repo, req.Msg.GetAssetId()); err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.DeleteReleaseAssetResponse{}), nil
}

func assetRef(m *pb.DownloadReleaseAssetRequest) (ports.AssetRef, error) {
	switch v := m.GetSelector().(type) {
	case *pb.DownloadReleaseAssetRequest_AssetId:
		if v.AssetId > 0 {
			return ports.AssetRef{ID: v.AssetId}, nil
		}
	case *pb.DownloadReleaseAssetRequest_Tag:
		if v.Tag.GetTag() != "" && v.Tag.GetName() != "" {
			return ports.AssetRef{Tag: v.Tag.GetTag(), Name: v.Tag.GetName()}, nil
		}
	case *pb.DownloadReleaseAssetRequest_Latest:
		if v.Latest.GetLatest() && v.Latest.GetName() != "" {
			return ports.AssetRef{Latest: true, Name: v.Latest.GetName()}, nil
		}
	}
	return ports.AssetRef{}, fmt.Errorf("%w: select an asset id, tag/name or latest=true/name", domain.ErrInvalid)
}

func (s *releaseService) DownloadReleaseAsset(ctx context.Context, req *connect.Request[pb.DownloadReleaseAssetRequest], stream *connect.ServerStream[pb.DownloadReleaseAssetResponse]) error {
	repo, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return connectError(ctx, err)
	}
	ref, err := assetRef(req.Msg)
	if err != nil {
		return connectError(ctx, err)
	}
	err = s.rel.DownloadAsset(ctx, caller(ctx), repo, ref, req.Msg.GetOffset(), -1, func(c ports.AssetChunk) error {
		m := &pb.DownloadReleaseAssetResponse{Chunk: c.Data}
		if c.Asset != nil {
			m.Asset = assetToPB(*c.Asset)
		}
		return stream.Send(m)
	})
	return connectError(ctx, err)
}

func (s *releaseService) ListReleaseAssets(ctx context.Context, req *connect.Request[pb.ListReleaseAssetsRequest]) (*connect.Response[pb.ListReleaseAssetsResponse], error) {
	repo, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	assets, err := s.rel.ListAssets(ctx, caller(ctx), repo, req.Msg.GetReleaseId())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.ListReleaseAssetsResponse{Assets: assetsToPB(assets)}), nil
}

func (s *releaseService) GetReleaseUsage(ctx context.Context, req *connect.Request[pb.GetReleaseUsageRequest]) (*connect.Response[pb.GetReleaseUsageResponse], error) {
	repo, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	u, err := s.rel.Usage(ctx, caller(ctx), repo)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.GetReleaseUsageResponse{UsedBytes: u.Used, QuotaBytes: u.Limits.Quota, AssetMaxBytes: u.Limits.AssetMax, AssetsMax: int32(u.Limits.AssetsMax)}), nil
}
