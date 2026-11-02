// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"errors"
	"fmt"
	"io"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
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

func (s *jobArtifactService) CreateJobArtifact(ctx context.Context, req *pb.CreateJobArtifactRequest) (*pb.CreateJobArtifactResponse, error) {
	out, err := s.artifacts.CreateJobArtifact(ctx, secretFrom(ctx), ports.CreateJobArtifactParams{
		JobID: req.GetJobId(), Name: req.GetName(), MediaType: req.GetMediaType(), ExpiresAt: goTime(req.GetExpiresAt()),
	})
	if err != nil {
		return nil, s.runner.failed(ctx, err)
	}
	return &pb.CreateJobArtifactResponse{ArtifactId: out.ID, ExpiresAt: timestamp(out.ExpiresAt)}, nil
}

// blockReader reads the data of a block stream: the first message's, then
// the following ones', which must carry data only.
type blockReader struct {
	stream ermiteconnect.JobArtifactServiceUploadJobArtifactBlockServerStream
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
		m, err := r.stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0, io.EOF
			}
			return 0, err
		}
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

func (s *jobArtifactService) UploadJobArtifactBlock(ctx context.Context, stream ermiteconnect.JobArtifactServiceUploadJobArtifactBlockServerStream) (*pb.UploadJobArtifactBlockResponse, error) {
	first, err := stream.Receive()
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = fmt.Errorf("%w: the stream has no message", domain.ErrInvalid)
		}
		return nil, s.runner.failed(ctx, err)
	}
	if err := checkBlockData(first.GetData()); err != nil {
		return nil, s.runner.failed(ctx, err)
	}
	r := &blockReader{stream: stream, buf: first.GetData()}
	size, err := s.artifacts.UploadJobArtifactBlock(ctx, secretFrom(ctx), first.GetJobId(), first.GetArtifactId(), first.GetBlockId(), r)
	if err != nil {
		return nil, s.runner.failed(ctx, err)
	}
	return &pb.UploadJobArtifactBlockResponse{Size: size}, nil
}

func (s *jobArtifactService) CommitJobArtifact(ctx context.Context, req *pb.CommitJobArtifactRequest) (*pb.CommitJobArtifactResponse, error) {
	size, digest, err := s.artifacts.CommitJobArtifact(ctx, secretFrom(ctx), req.GetJobId(), req.GetArtifactId(), req.GetBlockIds())
	if err != nil {
		return nil, s.runner.failed(ctx, err)
	}
	return &pb.CommitJobArtifactResponse{Size: size, Digest: digest}, nil
}

func (s *jobArtifactService) FinalizeJobArtifact(ctx context.Context, req *pb.FinalizeJobArtifactRequest) (*pb.FinalizeJobArtifactResponse, error) {
	a, err := s.artifacts.FinalizeJobArtifact(ctx, secretFrom(ctx), req.GetJobId(), req.GetName(), req.GetSize(), req.GetDigest())
	if err != nil {
		return nil, s.runner.failed(ctx, err)
	}
	return &pb.FinalizeJobArtifactResponse{Artifact: artifactToPB(a)}, nil
}

func (s *jobArtifactService) ListJobArtifacts(ctx context.Context, req *pb.ListJobArtifactsRequest) (*pb.ListJobArtifactsResponse, error) {
	arts, err := s.artifacts.ListJobArtifacts(ctx, secretFrom(ctx), req.GetJobId(), req.GetName(), req.GetArtifactId())
	if err != nil {
		return nil, s.runner.failed(ctx, err)
	}
	out := &pb.ListJobArtifactsResponse{Artifacts: make([]*pb.Artifact, 0, len(arts))}
	for _, a := range arts {
		out.Artifacts = append(out.Artifacts, artifactToPB(a))
	}
	return out, nil
}

func (s *jobArtifactService) DownloadJobArtifact(ctx context.Context, req *pb.DownloadJobArtifactRequest, stream ermiteconnect.JobArtifactServiceDownloadJobArtifactServerStream) error {
	err := s.artifacts.DownloadJobArtifact(ctx, secretFrom(ctx), req.GetJobId(), req.GetArtifactId(), req.GetOffset(), func(c ports.ArtifactChunk) error {
		m := &pb.DownloadJobArtifactResponse{Chunk: c.Data}
		if c.Artifact != nil {
			m.Artifact = artifactToPB(*c.Artifact)
		}
		return stream.Send(m)
	})
	return s.runner.failed(ctx, err)
}

func (s *jobArtifactService) DeleteJobArtifact(ctx context.Context, req *pb.DeleteJobArtifactRequest) (*pb.DeleteJobArtifactResponse, error) {
	id, err := s.artifacts.DeleteJobArtifact(ctx, secretFrom(ctx), req.GetJobId(), req.GetName())
	if err != nil {
		return nil, s.runner.failed(ctx, err)
	}
	return &pb.DeleteJobArtifactResponse{ArtifactId: id}, nil
}

// artifactService implements ermiteconnect.ArtifactServiceHandler.
type artifactService struct {
	artifacts ports.ArtifactService
}

func (s *artifactService) ListArtifacts(ctx context.Context, req *pb.ListArtifactsRequest) (*pb.ListArtifactsResponse, error) {
	name, err := repoOf(req.GetRepo())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	res, err := s.artifacts.ListArtifacts(ctx, caller(ctx), name, req.GetRunId())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	out := &pb.ListArtifactsResponse{UsedBytes: res.Used, QuotaBytes: res.Quota, Artifacts: make([]*pb.Artifact, 0, len(res.Artifacts))}
	for _, a := range res.Artifacts {
		out.Artifacts = append(out.Artifacts, artifactToPB(a))
	}
	return out, nil
}

func (s *artifactService) DownloadArtifact(ctx context.Context, req *pb.DownloadArtifactRequest, stream ermiteconnect.ArtifactServiceDownloadArtifactServerStream) error {
	name, err := repoOf(req.GetRepo())
	if err != nil {
		return ciError(ctx, err)
	}
	err = s.artifacts.DownloadArtifact(ctx, caller(ctx), name, req.GetArtifactId(), req.GetOffset(), func(c ports.ArtifactChunk) error {
		m := &pb.DownloadArtifactResponse{Chunk: c.Data}
		if c.Artifact != nil {
			m.Artifact = artifactToPB(*c.Artifact)
		}
		return stream.Send(m)
	})
	return ciError(ctx, err)
}

func (s *artifactService) DeleteArtifact(ctx context.Context, req *pb.DeleteArtifactRequest) (*pb.DeleteArtifactResponse, error) {
	name, err := repoOf(req.GetRepo())
	if err != nil {
		return nil, ciError(ctx, err)
	}
	if err := s.artifacts.DeleteArtifact(ctx, caller(ctx), name, req.GetArtifactId()); err != nil {
		return nil, ciError(ctx, err)
	}
	return &pb.DeleteArtifactResponse{}, nil
}
