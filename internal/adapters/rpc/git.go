// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"errors"
	"fmt"
	"io"

	"connectrpc.com/connect"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// BlobChunkSize is the largest chunk of a GetBlob stream.
const BlobChunkSize = 64 << 10

// gitService implements ermiteconnect.GitServiceHandler.
type gitService struct{ browse ports.BrowseService }

// target is the repository and revision most requests start with.
func target(repo, revision string) (domain.RepoName, domain.Revision, error) {
	name, err := domain.ParseRepoName(repo)
	if err != nil {
		return domain.RepoName{}, domain.Revision{}, err
	}
	rev, err := domain.ParseRevision(revision)
	if err != nil {
		return domain.RepoName{}, domain.Revision{}, err
	}
	return name, rev, nil
}

func (s *gitService) ListRefs(ctx context.Context, req *connect.Request[pb.ListRefsRequest]) (*connect.Response[pb.ListRefsResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	params := ports.ListRefsParams{PageSize: int(req.Msg.GetPageSize()), PageToken: req.Msg.GetPageToken()}
	for _, k := range req.Msg.GetKinds() {
		kind, err := refKindFromPB(k)
		if err != nil {
			return nil, connectError(ctx, err)
		}
		params.Kinds = append(params.Kinds, kind)
	}
	page, err := s.browse.ListRefs(ctx, caller(ctx), name, params)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := &pb.ListRefsResponse{NextPageToken: page.NextPageToken, Refs: make([]*pb.Ref, 0, len(page.Refs))}
	for _, r := range page.Refs {
		out.Refs = append(out.Refs, refToPB(r))
	}
	return connect.NewResponse(out), nil
}

func (s *gitService) GetTree(ctx context.Context, req *connect.Request[pb.GetTreeRequest]) (*connect.Response[pb.GetTreeResponse], error) {
	name, rev, err := target(req.Msg.GetRepo(), req.Msg.GetRevision())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	path, err := domain.ParseTreePath(req.Msg.GetPath())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	tree, err := s.browse.GetTree(ctx, caller(ctx), name, rev, path, req.Msg.GetIncludeLastCommit())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := &pb.GetTreeResponse{
		CommitId: tree.CommitID.String(),
		TreeId:   tree.ID.String(),
		Entries:  make([]*pb.TreeEntry, 0, len(tree.Entries)),

		LastCommitsTruncated: tree.LastCommitsOmitted,
		Truncated:            tree.Truncated,
	}
	for _, e := range tree.Entries {
		out.Entries = append(out.Entries, treeEntryToPB(e))
	}
	return connect.NewResponse(out), nil
}

// ListPaths lists the paths of every file of a revision.
func (s *gitService) ListPaths(ctx context.Context, req *connect.Request[pb.ListPathsRequest]) (*connect.Response[pb.ListPathsResponse], error) {
	name, rev, err := target(req.Msg.GetRepo(), req.Msg.GetRevision())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	list, err := s.browse.ListPaths(ctx, caller(ctx), name, rev)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.ListPathsResponse{
		CommitId:  list.CommitID.String(),
		TreeId:    list.TreeID.String(),
		Paths:     list.Paths,
		Truncated: list.Truncated,
	}), nil
}

// GetBlob streams the blob in chunks of BlobChunkSize. The first message
// carries the metadata and the first chunk.
func (s *gitService) GetBlob(ctx context.Context, req *connect.Request[pb.GetBlobRequest], stream *connect.ServerStream[pb.GetBlobResponse]) error {
	name, rev, err := target(req.Msg.GetRepo(), req.Msg.GetRevision())
	if err != nil {
		return connectError(ctx, err)
	}
	if req.Msg.GetPath() == "" {
		return connectError(ctx, invalidPath("path is required"))
	}
	path, err := domain.ParseTreePath(req.Msg.GetPath())
	if err != nil {
		return connectError(ctx, err)
	}
	info, body, err := s.browse.OpenBlob(ctx, caller(ctx), name, rev, path)
	if err != nil {
		return connectError(ctx, err)
	}
	defer func() { _ = body.Close() }()

	// Send marshals the message before it returns, so the buffer is reused.
	buf := make([]byte, BlobChunkSize)
	first := true
	for {
		n, rerr := io.ReadFull(body, buf)
		eof := errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF)
		if rerr != nil && !eof {
			return connectError(ctx, rerr)
		}
		// An empty file still gets the message with the metadata.
		if n > 0 || first {
			msg := &pb.GetBlobResponse{Chunk: buf[:n]}
			if first {
				msg.Info = blobInfoToPB(info)
				first = false
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
		if eof {
			return nil
		}
	}
}

func invalidPath(msg string) error {
	return fmt.Errorf("%w: %s", domain.ErrInvalidPath, msg)
}

func (s *gitService) ListCommits(ctx context.Context, req *connect.Request[pb.ListCommitsRequest]) (*connect.Response[pb.ListCommitsResponse], error) {
	name, rev, err := target(req.Msg.GetRepo(), req.Msg.GetRevision())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	path, err := domain.ParseTreePath(req.Msg.GetPath())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	page, err := s.browse.ListCommits(ctx, caller(ctx), name, ports.ListCommitsParams{
		Revision: rev, Path: path, PageSize: int(req.Msg.GetPageSize()), PageToken: req.Msg.GetPageToken(),
	})
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := &pb.ListCommitsResponse{NextPageToken: page.NextPageToken, Commits: make([]*pb.Commit, 0, len(page.Commits))}
	for _, c := range page.Commits {
		out.Commits = append(out.Commits, commitToPB(c))
	}
	return connect.NewResponse(out), nil
}

func (s *gitService) GetCommit(ctx context.Context, req *connect.Request[pb.GetCommitRequest]) (*connect.Response[pb.GetCommitResponse], error) {
	name, rev, err := target(req.Msg.GetRepo(), req.Msg.GetRevision())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	detail, err := s.browse.GetCommit(ctx, caller(ctx), name, rev)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(commitDetailToPB(detail)), nil
}

func (s *gitService) ResolveRevision(ctx context.Context, req *connect.Request[pb.ResolveRevisionRequest]) (*connect.Response[pb.ResolveRevisionResponse], error) {
	name, rev, err := target(req.Msg.GetRepo(), req.Msg.GetRevision())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	res, err := s.browse.ResolveRevision(ctx, caller(ctx), name, rev)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.ResolveRevisionResponse{CommitId: res.CommitID.String(), Ref: res.Ref}), nil
}
