// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"bnema.dev/ermite-actions/workflow"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// jobTokenActor is the actor of a run dispatched with a job token, GitHub's name
// for the account behind a GITHUB_TOKEN.
const jobTokenActor = "github-actions[bot]"

// DispatchWorkflow starts a run of a workflow file by hand (workflow_dispatch,
// CI-3 D5), like `gh workflow run`.
//
// Who may: a principal with ci:write on the repository, or the holder of a job
// token whose permissions include "actions: write" (GitHub's one exception to
// "events made with GITHUB_TOKEN start no run"; a token of a run that is not
// trusted never has it, TokenPermissions lowers it, so a job token that gets
// here belongs to a trusted run). A token limited to branch prefixes may only
// dispatch on a branch under them, and its run is not trusted (D1). Whoever
// calls, the run is trusted only when the ref it runs on was moved to its commit
// by a trusted principal (ci_tips.go).
//
// The workflow is read at ref (the default branch when empty) and must declare
// workflow_dispatch; the inputs are checked against its declaration.
func (c *CI) DispatchWorkflow(ctx context.Context, p domain.Principal, name domain.RepoName, params ports.DispatchParams) (domain.CIRun, error) {
	repo, actor, pusher, trusted, err := c.dispatchCaller(ctx, p, name)
	if err != nil {
		return domain.CIRun{}, err
	}
	wfPath, err := dispatchPath(params.WorkflowPath)
	if err != nil {
		return domain.CIRun{}, err
	}
	pin, err := c.dispatchRef(ctx, name, params.Ref)
	if err != nil {
		return domain.CIRun{}, err
	}
	ref, commit := pin.ref, pin.commit
	if c.cfg.DispatchResolved != nil {
		c.cfg.DispatchResolved()
	}
	if !p.AllowsRef(ref) {
		return domain.CIRun{}, fmt.Errorf("%w: this token may not write %s", domain.ErrForbidden, ref)
	}
	content, err := c.readWorkflowAt(ctx, name, commit, wfPath)
	if err != nil {
		return domain.CIRun{}, err
	}
	wf, perrs := workflow.Parse(wfPath, content)
	if len(perrs) > 0 {
		lines := make([]string, 0, len(perrs))
		for _, e := range perrs {
			lines = append(lines, e.Error())
		}
		return domain.CIRun{}, fmt.Errorf("%w: %s is not a valid workflow:\n%s", domain.ErrInvalid, wfPath, strings.Join(lines, "\n"))
	}
	ev := wf.On.Event(eventDispatch)
	if ev == nil {
		return domain.CIRun{}, fmt.Errorf("%w: %s does not have a %s trigger", domain.ErrPreconditionFailed, wfPath, eventDispatch)
	}
	values, err := resolveDispatchInputs(ev.Inputs, params.Inputs)
	if err != nil {
		return domain.CIRun{}, err
	}

	f := pushFacts{
		update: domain.RefUpdate{Ref: ref, Old: domain.ZeroOID, New: commit.String()}, commit: commit,
		custom: &customEvent{name: eventDispatch, title: workflowName(wf, wfPath), inputs: values},
	}
	if def, _, err := c.cfg.Repos.DefaultBranch(ctx, name); err == nil {
		f.defaultBranch = def
	}
	// The run executes the code of the ref, which the caller did not necessarily
	// write: it is trusted only if the caller is AND whoever moved the ref to this
	// commit was (domain.CIRefTip), as a re-run of an agent's run by the owner
	// does not unlock secrets. A ref with no record is not trusted. This holds for
	// every caller, the owner and a job token alike.
	trusted = trusted && c.refTrusted(ctx, name, pin)
	t := ciTrigger{repo: name, actor: actor, pusher: pusher, trusted: trusted, at: c.now()}
	if problem := c.matrixProblem(ctx, repo, t, f, wfPath, wf); problem.text != "" {
		return domain.CIRun{}, fmt.Errorf("%w: %s", domain.ErrInvalid, problem.text)
	}
	run, err := c.launchRun(ctx, repo, t, f, wfPath, string(content), wf)
	if err != nil {
		return domain.CIRun{}, err
	}
	if run.ID == 0 {
		return domain.CIRun{}, domain.ErrNotFound
	}
	ctxLog(ctx).Info().Str("repo", name.String()).Int64("run_id", run.ID).Str("workflow", wfPath).Str("ref", ref).
		Str("principal", p.Label()).Bool("trusted", trusted).Msg("ci workflow dispatched")
	return run, nil
}

// dispatchCaller applies the access rule of a dispatch and returns who the run
// is for.
func (c *CI) dispatchCaller(ctx context.Context, p domain.Principal, name domain.RepoName) (repo domain.Repo, actor string, pusher domain.CIPusher, trusted bool, err error) {
	if p.ViaJobToken() {
		repo, err = lookupRepo(ctx, c.cfg.Meta, name)
		if err != nil {
			return repo, "", pusher, false, err
		}
		// The permission is judged on its own: a job of this repository with
		// actions: write may dispatch even when its contents permission is none
		// (a private repository is then unreadable to it). JobPermits is true only
		// for the job's own token on its own repository, so any other caller of
		// a repository it cannot read still gets not found.
		switch {
		case p.JobPermits("actions", domain.PermissionWrite, name):
			return repo, jobTokenActor, domain.PusherOf(p), true, nil
		case !canRead(p, repo):
			return repo, "", pusher, false, domain.ErrNotFound
		}
		return repo, "", pusher, false, fmt.Errorf("%w: a job token needs the permission actions: write to dispatch a workflow", domain.ErrForbidden)
	}
	repo, err = c.writableRepo(ctx, p, name)
	if err != nil {
		return repo, "", pusher, false, err
	}
	return repo, authorOf(p), domain.PusherOf(p), domain.TrustedActor(p), nil
}

// dispatchPath checks the workflow file of a dispatch: a file of
// .github/workflows, named by its path or by its file name.
func dispatchPath(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("%w: the workflow file is required", domain.ErrInvalid)
	}
	if !strings.Contains(s, "/") {
		s = workflowDir + "/" + s
	}
	ext := strings.ToLower(path.Ext(s))
	if path.Dir(s) != workflowDir || (ext != ".yml" && ext != ".yaml") {
		return "", fmt.Errorf("%w: %q is not a workflow file of %s", domain.ErrInvalid, s, workflowDir)
	}
	return s, nil
}

// pinnedRef is what a dispatch resolved its ref to, once: the full name, the
// commit the run executes and the object the ref points to (the commit for a
// branch and a lightweight tag, the tag object for an annotated tag). The trust
// of the run is judged on exactly these, never on the ref as it is read later
// (ci_tips.go).
type pinnedRef struct {
	ref            string
	commit, object domain.ObjectID
}

// dispatchRef resolves the ref of a dispatch: a branch or a tag, by short or
// full name; the default branch when empty. A tag is resolved to its commit and
// its own object together; a tag that moves between the two reads is refused,
// so the commit and the object always belong to one state of the ref.
func (c *CI) dispatchRef(ctx context.Context, name domain.RepoName, given string) (pinnedRef, error) {
	given = strings.TrimSpace(given)
	if given == "" {
		def, exists, err := c.cfg.Repos.DefaultBranch(ctx, name)
		if err != nil {
			return pinnedRef{}, err
		}
		if !exists {
			return pinnedRef{}, fmt.Errorf("%w: the default branch %q does not exist", domain.ErrNotFound, def)
		}
		given = "refs/heads/" + def
	}
	rev, err := domain.ParseRevision(given)
	if err != nil {
		return pinnedRef{}, err
	}
	res, err := c.cfg.Reader.ResolveRevision(ctx, name, rev)
	if err != nil {
		return pinnedRef{}, err
	}
	pin := pinnedRef{ref: res.Ref, commit: res.CommitID, object: res.CommitID}
	switch {
	case strings.HasPrefix(res.Ref, "refs/heads/"):
		return pin, nil
	case !strings.HasPrefix(res.Ref, "refs/tags/"):
		return pinnedRef{}, fmt.Errorf("%w: %q is not a branch or a tag", domain.ErrInvalid, given)
	}
	refs, err := c.cfg.Reader.ListRefs(ctx, name)
	if err != nil {
		return pinnedRef{}, err
	}
	for _, r := range refs {
		if r.Name != res.Ref {
			continue
		}
		if r.CommitID != res.CommitID || r.ID.IsZero() {
			break // moved between the two reads
		}
		pin.object = r.ID
		return pin, nil
	}
	return pinnedRef{}, fmt.Errorf("%w: tag %q moved while it was resolved, try again", domain.ErrConflict, res.Ref)
}

// readWorkflowAt reads a workflow file at a commit, bounded; a missing file is
// domain.ErrNotFound.
func (c *CI) readWorkflowAt(ctx context.Context, repo domain.RepoName, commit domain.ObjectID, wfPath string) ([]byte, error) {
	tp, err := domain.ParseTreePath(wfPath)
	if err != nil {
		return nil, err
	}
	_, rc, err := c.cfg.Reader.OpenBlob(ctx, repo, domain.RevisionOf(commit), tp)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrNotFile) {
			return nil, fmt.Errorf("%w: workflow %s", domain.ErrNotFound, wfPath)
		}
		return nil, fmt.Errorf("read %s: %w", wfPath, err)
	}
	content, err := io.ReadAll(io.LimitReader(rc, maxCIWorkflowBytes+1))
	_ = rc.Close()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", wfPath, err)
	}
	if len(content) > maxCIWorkflowBytes {
		return nil, fmt.Errorf("%w: %s is larger than 1 MiB", domain.ErrInvalid, wfPath)
	}
	return content, nil
}
