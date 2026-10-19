// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"io"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The reads of the pages go through BrowseService as the caller of the
// request. The revisions and paths the URLs carry are text: they are parsed
// here, and one the domain refuses is an invalid argument, which the pages
// answer like a missing one (fail).

// commitList reads a page of the history that starts at rev, limited to path
// when it is not empty.
func (s *Server) commitList(ctx context.Context, repo domain.RepoName, rev, path string, size int, token string) (ports.HistoryPage, error) {
	r, err := domain.ParseRevision(rev)
	if err != nil {
		return ports.HistoryPage{}, err
	}
	p, err := domain.ParseTreePath(path)
	if err != nil {
		return ports.HistoryPage{}, err
	}
	return s.cfg.Browse.ListCommits(ctx, principal(ctx), repo, ports.ListCommitsParams{
		Revision: r, Path: p, PageSize: size, PageToken: token,
	})
}

// getTree lists the directory path of the commit rev.
func (s *Server) getTree(ctx context.Context, repo domain.RepoName, rev, path string, lastCommit bool) (domain.Tree, error) {
	r, err := domain.ParseRevision(rev)
	if err != nil {
		return domain.Tree{}, err
	}
	p, err := domain.ParseTreePath(path)
	if err != nil {
		return domain.Tree{}, err
	}
	return s.cfg.Browse.GetTree(ctx, principal(ctx), repo, r, p, lastCommit)
}

// getCommit reads a commit with its diff.
func (s *Server) getCommit(ctx context.Context, repo domain.RepoName, rev string) (domain.CommitDetail, error) {
	r, err := domain.ParseRevision(rev)
	if err != nil {
		return domain.CommitDetail{}, err
	}
	return s.cfg.Browse.GetCommit(ctx, principal(ctx), repo, r)
}

// openBlobAt opens the file path of the commit rev. The caller closes the
// body.
func (s *Server) openBlobAt(ctx context.Context, repo domain.RepoName, rev, path string) (domain.BlobInfo, io.ReadCloser, error) {
	r, err := domain.ParseRevision(rev)
	if err != nil {
		return domain.BlobInfo{}, nil, err
	}
	if path == "" {
		return domain.BlobInfo{}, nil, errNotFound
	}
	p, err := domain.ParseTreePath(path)
	if err != nil {
		return domain.BlobInfo{}, nil, err
	}
	return s.cfg.Browse.OpenBlob(ctx, principal(ctx), repo, r, p)
}

// resolveRevision resolves rev to the commit it designates.
func (s *Server) resolveRevision(ctx context.Context, repo domain.RepoName, rev string) (domain.ResolvedRevision, error) {
	r, err := domain.ParseRevision(rev)
	if err != nil {
		return domain.ResolvedRevision{}, err
	}
	return s.cfg.Browse.ResolveRevision(ctx, principal(ctx), repo, r)
}

// listPaths lists the file paths of the commit rev.
func (s *Server) listPaths(ctx context.Context, repo domain.RepoName, rev string) (domain.PathList, error) {
	r, err := domain.ParseRevision(rev)
	if err != nil {
		return domain.PathList{}, err
	}
	return s.cfg.Browse.ListPaths(ctx, principal(ctx), repo, r)
}
