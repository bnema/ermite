// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"fmt"
	"io"

	"connectrpc.com/connect"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// FeatureCIArtifacts is listed by GetVersion when the artifact services are
// served.
const FeatureCIArtifacts = "ci.artifacts"

func artifactToPB(a domain.CIArtifact) *pb.Artifact {
	return &pb.Artifact{
		Id: a.ID, Repo: a.Repo.String(), RunId: a.RunID, Attempt: a.Attempt, JobId: a.JobID,
		Name: a.Name, Size: a.Size, Digest: a.Digest, MediaType: a.MediaType,
		CreatedAt: timestamp(a.CreatedAt), ExpiresAt: timestamp(a.ExpiresAt),
	}
}

// jobArtifactService implements ermiteconnect.JobArtifactServiceHandler.
type jobArtifactService struct {
	artifacts ports.JobArtifactService
	runner    *runnerService // failed: the failures of job tokens count like the runner's
}

func (s *jobArtifactService) CreateJobArtifact(ctx context.Context, req *connect.Request[pb.CreateJobArtifactRequest]) (*connect.Response[pb.CreateJobArtifactResponse], error) {
	out, err := s.artifacts.CreateJobArtifact(ctx, secretFrom(ctx), ports.CreateJobArtifactParams{
		JobID: req.Msg.GetJobId(), Name: req.Msg.GetName(), MediaType: req.Msg.GetMediaType(), ExpiresAt: goTime(req.Msg.GetExpiresAt()),
	})
	if err != nil {
		return nil, s.runner.failed(ctx, err)
	}
	return connect.NewResponse(&pb.CreateJobArtifactResponse{ArtifactId: out.ID, ExpiresAt: timestamp(out.ExpiresAt)}), nil
}

// blockReader reads the data of a block stream: the first message's, then
// the following ones', which must carry data only.
type blockReader struct {
	stream *connect.ClientStream[pb.UploadJobArtifactBlockRequest]
	buf    []byte
}

func checkBlockData(data []byte) error {
	if len(data) > domain.CIArtifactChunk {
		return fmt.Errorf("%w: a message carries at most %d bytes of data", domain.ErrInvalid, domain.CIArtifactChunk)
	}
	return nil
}

func (r *blockReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if !r.stream.Receive() {
			if err := r.stream.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		m := r.stream.Msg()
		if m.GetJobId() != "" || m.GetArtifactId() != 0 || len(m.GetBlockId()) > 0 {
			return 0, fmt.Errorf("%w: job_id, artifact_id and block_id are in the first message only", domain.ErrInvalid)
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

func (s *jobArtifactService) UploadJobArtifactBlock(ctx context.Context, stream *connect.ClientStream[pb.UploadJobArtifactBlockRequest]) (*connect.Response[pb.UploadJobArtifactBlockResponse], error) {
	if !stream.Receive() {
		err := stream.Err()
		if err == nil {
			err = fmt.Errorf("%w: the stream has no message", domain.ErrInvalid)
		}
		return nil, s.runner.failed(ctx, err)
	}
	first := stream.Msg()
	if err := checkBlockData(first.GetData()); err != nil {
		return nil, s.runner.failed(ctx, err)
	}
	r := &blockReader{stream: stream, buf: first.GetData()}
	size, err := s.artifacts.UploadJobArtifactBlock(ctx, secretFrom(ctx), first.GetJobId(), first.GetArtifactId(), first.GetBlockId(), r)
	if err != nil {
		return nil, s.runner.failed(ctx, err)
	}
	return connect.NewResponse(&pb.UploadJobArtifactBlockResponse{Size: size}), nil
}

func (s *jobArtifactService) CommitJobArtifact(ctx context.Context, req *connect.Request[pb.CommitJobArtifactRequest]) (*connect.Response[pb.CommitJobArtifactResponse], error) {
	size, digest, err := s.artifacts.CommitJobArtifact(ctx, secretFrom(ctx), req.Msg.GetJobId(), req.Msg.GetArtifactId(), req.Msg.GetBlockIds())
	if err != nil {
		return nil, s.runner.failed(ctx, err)
	}
	return connect.NewResponse(&pb.CommitJobArtifactResponse{Size: size, Digest: digest}), nil
}

func (s *jobArtifactService) FinalizeJobArtifact(ctx context.Context, req *connect.Request[pb.FinalizeJobArtifactRequest]) (*connect.Response[pb.FinalizeJobArtifactResponse], error) {
	a, err := s.artifacts.FinalizeJobArtifact(ctx, secretFrom(ctx), req.Msg.GetJobId(), req.Msg.GetName(), req.Msg.GetSize(), req.Msg.GetDigest())
	if err != nil {
		return nil, s.runner.failed(ctx, err)
	}
	return connect.NewResponse(&pb.FinalizeJobArtifactResponse{Artifact: artifactToPB(a)}), nil
}

func (s *jobArtifactService) ListJobArtifacts(ctx context.Context, req *connect.Request[pb.ListJobArtifactsRequest]) (*connect.Response[pb.ListJobArtifactsResponse], error) {
	arts, err := s.artifacts.ListJobArtifacts(ctx, secretFrom(ctx), req.Msg.GetJobId(), req.Msg.GetName(), req.Msg.GetArtifactId())
	if err != nil {
		return nil, s.runner.failed(ctx, err)
	}
	out := &pb.ListJobArtifactsResponse{Artifacts: make([]*pb.Artifact, 0, len(arts))}
	for _, a := range arts {
		out.Artifacts = append(out.Artifacts, artifactToPB(a))
	}
	return connect.NewResponse(out), nil
}

func (s *jobArtifactService) DownloadJobArtifact(ctx context.Context, req *connect.Request[pb.DownloadJobArtifactRequest], stream *connect.ServerStream[pb.DownloadJobArtifactResponse]) error {
	err := s.artifacts.DownloadJobArtifact(ctx, secretFrom(ctx), req.Msg.GetJobId(), req.Msg.GetArtifactId(), req.Msg.GetOffset(), func(c ports.ArtifactChunk) error {
		m := &pb.DownloadJobArtifactResponse{Chunk: c.Data}
		if c.Artifact != nil {
			m.Artifact = artifactToPB(*c.Artifact)
		}
		return stream.Send(m)
	})
	return s.runner.failed(ctx, err)
}

func (s *jobArtifactService) DeleteJobArtifact(ctx context.Context, req *connect.Request[pb.DeleteJobArtifactRequest]) (*connect.Response[pb.DeleteJobArtifactResponse], error) {
	id, err := s.artifacts.DeleteJobArtifact(ctx, secretFrom(ctx), req.Msg.GetJobId(), req.Msg.GetName())
	if err != nil {
		return nil, s.runner.failed(ctx, err)
	}
	return connect.NewResponse(&pb.DeleteJobArtifactResponse{ArtifactId: id}), nil
}

// artifactService implements ermiteconnect.ArtifactServiceHandler.
type artifactService struct {
	artifacts ports.ArtifactService
}

func (s *artifactService) ListArtifacts(ctx context.Context, req *connect.Request[pb.ListArtifactsRequest]) (*connect.Response[pb.ListArtifactsResponse], error) {
	name, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	res, err := s.artifacts.ListArtifacts(ctx, caller(ctx), name, req.Msg.GetRunId())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	out := &pb.ListArtifactsResponse{UsedBytes: res.Used, QuotaBytes: res.Quota, Artifacts: make([]*pb.Artifact, 0, len(res.Artifacts))}
	for _, a := range res.Artifacts {
		out.Artifacts = append(out.Artifacts, artifactToPB(a))
	}
	return connect.NewResponse(out), nil
}

func (s *artifactService) DownloadArtifact(ctx context.Context, req *connect.Request[pb.DownloadArtifactRequest], stream *connect.ServerStream[pb.DownloadArtifactResponse]) error {
	name, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return ciError(ctx, err)
	}
	err = s.artifacts.DownloadArtifact(ctx, caller(ctx), name, req.Msg.GetArtifactId(), req.Msg.GetOffset(), func(c ports.ArtifactChunk) error {
		m := &pb.DownloadArtifactResponse{Chunk: c.Data}
		if c.Artifact != nil {
			m.Artifact = artifactToPB(*c.Artifact)
		}
		return stream.Send(m)
	})
	return ciError(ctx, err)
}

func (s *artifactService) DeleteArtifact(ctx context.Context, req *connect.Request[pb.DeleteArtifactRequest]) (*connect.Response[pb.DeleteArtifactResponse], error) {
	name, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	if err := s.artifacts.DeleteArtifact(ctx, caller(ctx), name, req.Msg.GetArtifactId()); err != nil {
		return nil, ciError(ctx, err)
	}
	return connect.NewResponse(&pb.DeleteArtifactResponse{}), nil
}
