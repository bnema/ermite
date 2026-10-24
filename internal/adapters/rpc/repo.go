// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// repoService implements ermiteconnect.RepoServiceHandler.
type repoService struct{ repos ports.RepoService }

func (s *repoService) ListRepos(ctx context.Context, req *connect.Request[pb.ListReposRequest]) (*connect.Response[pb.ListReposResponse], error) {
	page, err := s.repos.ListPage(ctx, caller(ctx), int(req.Msg.GetPageSize()), req.Msg.GetPageToken())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := &pb.ListReposResponse{NextPageToken: page.NextPageToken, Repos: make([]*pb.Repo, 0, len(page.Repos))}
	for _, repo := range page.Repos {
		view, err := s.repos.View(ctx, repo)
		if err != nil {
			return nil, connectError(ctx, err)
		}
		out.Repos = append(out.Repos, repoToPB(view))
	}
	return connect.NewResponse(out), nil
}

func (s *repoService) GetRepo(ctx context.Context, req *connect.Request[pb.GetRepoRequest]) (*connect.Response[pb.GetRepoResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	repo, err := s.repos.Get(ctx, caller(ctx), name)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return respondRepo(ctx, s.repos, repo, func(r *pb.Repo) *pb.GetRepoResponse { return &pb.GetRepoResponse{Repo: r} })
}

func (s *repoService) CreateRepo(ctx context.Context, req *connect.Request[pb.CreateRepoRequest]) (*connect.Response[pb.CreateRepoResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetName())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	vis, err := visibilityFromPB(req.Msg.GetVisibility())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	repo, err := s.repos.Create(ctx, caller(ctx), ports.CreateRepoParams{
		Name: name, Description: req.Msg.GetDescription(), Topics: req.Msg.GetTopics(), Visibility: vis,
	})
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return respondRepo(ctx, s.repos, repo, func(r *pb.Repo) *pb.CreateRepoResponse { return &pb.CreateRepoResponse{Repo: r} })
}

func (s *repoService) UpdateRepo(ctx context.Context, req *connect.Request[pb.UpdateRepoRequest]) (*connect.Response[pb.UpdateRepoResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	params, err := updateParams(req.Msg)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	repo, err := s.repos.Update(ctx, caller(ctx), name, params)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return respondRepo(ctx, s.repos, repo, func(r *pb.Repo) *pb.UpdateRepoResponse { return &pb.UpdateRepoResponse{Repo: r} })
}

// updateParams maps the field mask of an update. The mask must name at least
// one of "description", "topics", "visibility", "default_branch", "settings",
// "settings.allowed_merge_modes" and
// "settings.allow_auto_merge", "archived", and nothing else; the optional issues field stands outside the mask, and alone
// it is a valid update.
func updateParams(msg *pb.UpdateRepoRequest) (ports.UpdateRepoParams, error) {
	var params ports.UpdateRepoParams
	paths := msg.GetUpdateMask().GetPaths()
	if len(paths) == 0 && msg.Issues == nil {
		return params, fmt.Errorf("%w: update_mask is empty", domain.ErrInvalid)
	}
	if msg.Issues != nil {
		mode, err := issuesModeFromPB(msg.GetIssues())
		if err != nil {
			return params, err
		}
		params.Issues = &mode
	}
	for _, path := range paths {
		switch path {
		case "description":
			params.Description = new(msg.GetDescription())
		case "archived":
			params.Archived = new(msg.GetArchived())
		case "topics":
			params.Topics = new(msg.GetTopics())
		case "visibility":
			vis, err := visibilityFromPB(msg.GetVisibility())
			if err != nil {
				return params, err
			}
			if vis == "" {
				return params, fmt.Errorf("%w: visibility is unspecified", domain.ErrInvalid)
			}
			params.Visibility = &vis
		case "default_branch":
			params.DefaultBranch = new(msg.GetDefaultBranch())
		case "settings", "settings.allowed_merge_modes", "settings.allow_auto_merge":
			all := path == "settings"
			if all || path == "settings.allowed_merge_modes" {
				modes, err := mergeModesFromPB(msg.GetSettings().GetAllowedMergeModes())
				if err != nil {
					return params, err
				}
				params.AllowedMergeModes = &modes
			}
			if all || path == "settings.allow_auto_merge" {
				params.AllowAutoMerge = new(msg.GetSettings().GetAllowAutoMerge())
			}
		default:
			return params, fmt.Errorf("%w: unknown path %q in update_mask", domain.ErrInvalid, path)
		}
	}
	return params, nil
}

func (s *repoService) DeleteRepo(ctx context.Context, req *connect.Request[pb.DeleteRepoRequest]) (*connect.Response[pb.DeleteRepoResponse], error) {
	name, err := domain.ParseRepoName(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	if err := s.repos.Delete(ctx, caller(ctx), name); err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.DeleteRepoResponse{}), nil
}

// respondRepo adds the default branch to repo and wraps it in a response.
func respondRepo[T any](ctx context.Context, repos ports.RepoService, repo domain.Repo, wrap func(*pb.Repo) *T) (*connect.Response[T], error) {
	view, err := repos.View(ctx, repo)
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(wrap(repoToPB(view))), nil
}
