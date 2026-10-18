// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"fmt"
	"slices"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Disabled workflows (`ermite workflow disable|enable|list`, the equivalent of
// `gh workflow disable`). The state is per repository and per workflow file
// path, kept in the store apart from Git: it holds for every branch and every
// commit, and the file does not have to exist, so it can be set on an empty
// repository before the first push.
//
// A disabled workflow starts no run for any event. The workflow files are read
// through workflowFiles for push, pull_request, pull_request_target, create,
// delete, schedule and workflow_run, and that one place leaves the disabled
// ones out; workflow_dispatch and the firing of a cached schedule check the
// state themselves (the cache holds every workflow). Whatever was checked
// before, the store refuses to create a run for a disabled workflow in the very
// transaction that creates it (CIStore.CreateRun, domain.ErrWorkflowDisabled),
// so a disable that lands while a push, a dispatch or a schedule is being
// planned cannot be overtaken. A run already started is not touched, and the events that
// happened while a workflow was disabled are not replayed when it is enabled.

// workflowDisabled reports whether the path is disabled; nothing is when no
// store is configured.
func (c *CI) workflowDisabled(ctx context.Context, repo domain.RepoName, wfPath string) (bool, error) {
	off, err := c.disabledPaths(ctx, repo)
	if err != nil {
		return false, err
	}
	return slices.Contains(off, wfPath), nil
}

// disabledPaths are the disabled paths of a repository, sorted.
func (c *CI) disabledPaths(ctx context.Context, repo domain.RepoName) ([]string, error) {
	if c.cfg.WorkflowState == nil {
		return nil, nil
	}
	off, err := c.cfg.WorkflowState.DisabledWorkflows(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("read the disabled workflows: %w", err)
	}
	return off, nil
}

// ListWorkflows returns the workflow files at the tip of the default branch and
// the disabled ones whatever the branch holds, ordered by path. Reading the
// repository is enough.
func (c *CI) ListWorkflows(ctx context.Context, p domain.Principal, name domain.RepoName) ([]ports.WorkflowInfo, error) {
	if _, err := c.readableRepo(ctx, p, name); err != nil {
		return nil, err
	}
	off, err := c.disabledPaths(ctx, name)
	if err != nil {
		return nil, err
	}
	byPath := map[string]*ports.WorkflowInfo{}
	if tip, ok := c.defaultTip(ctx, name); ok {
		files, err := c.listWorkflowFiles(ctx, name, tip)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			info := &ports.WorkflowInfo{Path: f.path}
			if f.problem == "" {
				if wf, perrs := parseWorkflow(f.path, f.content); len(perrs) == 0 && wf != nil {
					info.Name = workflowName(wf, f.path)
				}
			}
			byPath[f.path] = info
		}
	}
	for _, path := range off {
		if byPath[path] == nil {
			byPath[path] = &ports.WorkflowInfo{Path: path}
		}
		byPath[path].Disabled = true
	}
	out := make([]ports.WorkflowInfo, 0, len(byPath))
	for _, info := range byPath {
		out = append(out, *info)
	}
	slices.SortFunc(out, func(a, b ports.WorkflowInfo) int {
		switch {
		case a.Path < b.Path:
			return -1
		case a.Path > b.Path:
			return 1
		}
		return 0
	})
	return out, nil
}

// DisabledWorkflows returns the disabled paths of a repository, sorted, without
// reading any file.
func (c *CI) DisabledWorkflows(ctx context.Context, p domain.Principal, name domain.RepoName) ([]string, error) {
	if _, err := c.readableRepo(ctx, p, name); err != nil {
		return nil, err
	}
	return c.disabledPaths(ctx, name)
}

// DisableWorkflow stops a workflow file from starting runs. The file is named by
// its path or its file name and does not have to exist. It needs repo:admin on
// the repository (adminAccess), and is idempotent.
func (c *CI) DisableWorkflow(ctx context.Context, p domain.Principal, name domain.RepoName, file string) (ports.WorkflowInfo, error) {
	return c.setWorkflowState(ctx, p, name, file, true)
}

// EnableWorkflow undoes DisableWorkflow. Same access; idempotent.
func (c *CI) EnableWorkflow(ctx context.Context, p domain.Principal, name domain.RepoName, file string) (ports.WorkflowInfo, error) {
	return c.setWorkflowState(ctx, p, name, file, false)
}

func (c *CI) setWorkflowState(ctx context.Context, p domain.Principal, name domain.RepoName, file string, disable bool) (ports.WorkflowInfo, error) {
	repo, err := lookupRepo(ctx, c.cfg.Meta, name)
	if err != nil {
		return ports.WorkflowInfo{}, err
	}
	if err := adminAccess(p, repo); err != nil {
		return ports.WorkflowInfo{}, err
	}
	wfPath, err := domain.ParseWorkflowPath(file)
	if err != nil {
		return ports.WorkflowInfo{}, err
	}
	if c.cfg.WorkflowState == nil {
		return ports.WorkflowInfo{}, fmt.Errorf("%w: this server keeps no workflow state", domain.ErrPreconditionFailed)
	}
	if disable {
		err = c.cfg.WorkflowState.DisableWorkflow(ctx, name, wfPath, c.now())
	} else {
		err = c.cfg.WorkflowState.EnableWorkflow(ctx, name, wfPath)
	}
	if err != nil {
		return ports.WorkflowInfo{}, err
	}
	state := "enabled"
	if disable {
		state = "disabled"
	}
	ctxLog(ctx).Info().Str("repo", name.String()).Str("workflow", wfPath).Str("principal", p.Label()).Msg("ci workflow " + state)
	return ports.WorkflowInfo{Path: wfPath, Disabled: disable}, nil
}
