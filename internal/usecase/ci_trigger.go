// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-actions/workflow"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

const (
	workflowDir = ".github/workflows"
	// maxRunError bounds Run.error (8 KiB, run.proto).
	maxRunError = 8 << 10
	// maxPushCommits is the number of commits of the push event payload.
	maxPushCommits = 20
)

// ciTrigger is a push waiting to be planned.
type ciTrigger struct {
	repo    domain.RepoName
	updates []domain.RefUpdate
	actor   string
	at      time.Time
	log     zerowrap.Logger
}

// OnPush queues the refs of a push or a merge for planning and returns at once;
// a worker plans them. A full queue drops the trigger, logged at error: a push
// is never slowed down or failed by CI.
func (c *CI) OnPush(ctx context.Context, repo domain.RepoName, updates []domain.RefUpdate, pusher domain.Principal) {
	var kept []domain.RefUpdate
	for _, u := range updates {
		if !u.IsDelete() && (strings.HasPrefix(u.Ref, "refs/heads/") || strings.HasPrefix(u.Ref, "refs/tags/")) {
			kept = append(kept, u)
		}
	}
	if len(kept) == 0 {
		return
	}
	t := ciTrigger{repo: repo, updates: kept, actor: authorOf(pusher), at: c.now(), log: zerowrap.FromCtx(ctx)}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return
	}
	select {
	case c.queue <- t:
	default:
		t.log.Error().Str("repo", repo.String()).Int("refs", len(kept)).Msg("ci trigger queue is full: the workflows of this push are not run")
	}
}

func (c *CI) triggerLoop() {
	defer c.wg.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case t := <-c.queue:
			c.planPush(t)
		}
	}
}

// planPush plans one push; a panic or an error is logged and never escapes.
func (c *CI) planPush(t ciTrigger) {
	ctx := zerowrap.WithCtx(c.ctx, t.log)
	defer func() {
		if r := recover(); r != nil {
			t.log.Error().Interface("panic", r).Bytes("stack", debug.Stack()).Str("repo", t.repo.String()).Msg("ci trigger panicked")
		}
	}()
	if err := c.handlePush(ctx, t); err != nil && ctx.Err() == nil {
		t.log.Error().Err(err).Str("repo", t.repo.String()).Msg("ci trigger failed")
	}
}

// handlePush starts the workflows that match each ref of the push.
func (c *CI) handlePush(ctx context.Context, t ciTrigger) error {
	tags := 0
	for _, u := range t.updates {
		if strings.HasPrefix(u.Ref, "refs/tags/") {
			tags++
		}
	}
	var errs []error
	for _, u := range t.updates {
		if err := c.handleRef(ctx, t, u, tags); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", u.Ref, err))
		}
	}
	return errors.Join(errs...)
}

// pushFacts is what is known of one pushed ref.
type pushFacts struct {
	update  domain.RefUpdate
	commit  domain.ObjectID // the commit the ref points to, tags followed
	head    domain.Commit
	commits []domain.Commit // oldest first, at most maxPushCommits (branches only)
	forced  bool
	created bool
	files   []string
	// filesTruncated: the list of changed files is not available.
	filesTruncated bool
	tagCount       int
	isTag          bool
}

func (c *CI) handleRef(ctx context.Context, t ciTrigger, u domain.RefUpdate, tagCount int) error {
	isTag := strings.HasPrefix(u.Ref, "refs/tags/")
	if isTag && tagCount > workflow.MaxTagsPerPush {
		return nil // GitHub creates no event for the tags of such a push
	}
	repo, err := c.cfg.Meta.Get(ctx, t.repo)
	if err != nil {
		return nil // deleted meanwhile
	}
	facts, err := c.collect(ctx, t.repo, u, isTag, tagCount)
	if err != nil {
		return err
	}
	files, err := c.workflowFiles(ctx, t.repo, facts.commit)
	if err != nil || len(files) == 0 {
		return err
	}
	skip := workflow.SkipCI(facts.head.Message)
	var errs []error
	for _, f := range files {
		wf, perrs := workflow.Parse(f.path, f.content)
		switch {
		case f.problem != "":
			errs = append(errs, c.startInvalid(ctx, repo, t, facts, f.path, f.problem))
		case len(perrs) > 0:
			var lines []string
			for _, e := range perrs {
				lines = append(lines, e.Error())
			}
			errs = append(errs, c.startInvalid(ctx, repo, t, facts, f.path, strings.Join(lines, "\n")))
		case skip || !wf.MatchPush(workflow.PushInfo{
			Ref: u.Ref, Created: facts.created, ChangedFiles: facts.files, FilesTruncated: facts.filesTruncated, TagCount: tagCount,
		}):
		default:
			errs = append(errs, c.startRun(ctx, repo, t, facts, f.path, string(f.content), wf))
		}
	}
	return errors.Join(errs...)
}

// collect reads what the payload and the filters need from Git.
func (c *CI) collect(ctx context.Context, repo domain.RepoName, u domain.RefUpdate, isTag bool, tagCount int) (pushFacts, error) {
	newID, err := domain.ParseObjectID(u.New)
	if err != nil {
		return pushFacts{}, err
	}
	f := pushFacts{update: u, created: u.IsCreate(), isTag: isTag, tagCount: tagCount}
	res, err := c.cfg.Reader.ResolveRevision(ctx, repo, domain.RevisionOf(newID))
	if err != nil {
		return f, fmt.Errorf("resolve %s: %w", u.New, err)
	}
	f.commit = res.CommitID
	if isTag {
		cs, _, err := c.cfg.Git.ListRange(ctx, repo, domain.ObjectID{}, f.commit, 1)
		if err != nil || len(cs) == 0 {
			return f, fmt.Errorf("read the commit of the tag: %w", err)
		}
		f.head = cs[0]
		return f, nil
	}

	var exclude domain.ObjectID
	if !f.created {
		if old, err := domain.ParseObjectID(u.Old); err == nil {
			exclude = old
			if ok, err := c.cfg.Git.IsAncestor(ctx, repo, old, f.commit); err == nil && !ok {
				f.forced = true
			}
		}
	}
	base := exclude
	if f.created {
		base = c.defaultBase(ctx, repo, strings.TrimPrefix(u.Ref, "refs/heads/"), f.commit)
		exclude = base
	}
	cs, more, err := c.cfg.Git.ListRange(ctx, repo, exclude, f.commit, ports.MaxCommitLimit)
	if err != nil || len(cs) == 0 {
		return f, fmt.Errorf("read the commits of the push: %w", err)
	}
	f.head = cs[0]
	n := min(len(cs), maxPushCommits)
	f.commits = slices.Clone(cs[:n])
	slices.Reverse(f.commits)
	if more || base == f.commit {
		// More than 1,000 commits: the diff is not looked at (it runs).
		f.filesTruncated = more
	}
	if !more && base != f.commit {
		paths, _, err := c.cfg.Diff.ChangedPaths(ctx, repo, base, f.commit, workflow.MaxFilesEvaluated)
		if err != nil {
			f.filesTruncated = true // diff unavailable: runs
		} else {
			f.files = paths
		}
	}
	return f, nil
}

// defaultBase is the merge base of a new branch with the default branch, the
// zero id when there is none (every file of the branch counts).
func (c *CI) defaultBase(ctx context.Context, repo domain.RepoName, branch string, head domain.ObjectID) domain.ObjectID {
	def, exists, err := c.cfg.Repos.DefaultBranch(ctx, repo)
	if err != nil || !exists || def == branch {
		return domain.ObjectID{}
	}
	tip, err := c.cfg.Git.BranchHead(ctx, repo, def)
	if err != nil {
		return domain.ObjectID{}
	}
	base, err := c.cfg.Git.MergeBase(ctx, repo, tip, head)
	if err != nil {
		return domain.ObjectID{}
	}
	return base
}

type workflowFile struct {
	path    string
	content []byte
	// problem is set when the file cannot even be parsed (too large).
	problem string
}

// workflowFiles reads .github/workflows/*.yml|*.yaml at the commit.
func (c *CI) workflowFiles(ctx context.Context, repo domain.RepoName, commit domain.ObjectID) ([]workflowFile, error) {
	dir, err := domain.ParseTreePath(workflowDir)
	if err != nil {
		return nil, err
	}
	tree, err := c.cfg.Reader.GetTree(ctx, repo, domain.RevisionOf(commit), dir, false)
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrNotDirectory) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", workflowDir, err)
	}
	var out []workflowFile
	for _, e := range tree.Entries {
		ext := strings.ToLower(path.Ext(e.Name))
		if (e.Kind != domain.EntryFile && e.Kind != domain.EntryExecutable) || (ext != ".yml" && ext != ".yaml") {
			continue
		}
		if len(out) == maxCIWorkflowFiles {
			break
		}
		wf := workflowFile{path: e.Path}
		if wf.path == "" {
			wf.path = workflowDir + "/" + e.Name
		}
		if e.Size > maxCIWorkflowBytes {
			wf.problem = fmt.Sprintf("%s: the workflow file is larger than 1 MiB", wf.path)
			out = append(out, wf)
			continue
		}
		tp, err := domain.ParseTreePath(wf.path)
		if err != nil {
			continue
		}
		_, rc, err := c.cfg.Reader.OpenBlob(ctx, repo, domain.RevisionOf(commit), tp)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", wf.path, err)
		}
		wf.content, err = io.ReadAll(io.LimitReader(rc, maxCIWorkflowBytes+1))
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", wf.path, err)
		}
		if len(wf.content) > maxCIWorkflowBytes {
			wf.content, wf.problem = nil, fmt.Sprintf("%s: the workflow file is larger than 1 MiB", wf.path)
		}
		out = append(out, wf)
	}
	return out, nil
}

// --- the run ---

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return truncateRunes(strings.TrimSpace(line), 256)
}

// startInvalid records the failed run of an invalid workflow file: no job, the
// errors as Run.error, like GitHub's "Invalid workflow file".
func (c *CI) startInvalid(ctx context.Context, repo domain.Repo, t ciTrigger, f pushFacts, wfPath, problem string) error {
	now := c.now()
	draft, err := c.cfg.Store.CreateRun(ctx, repo.Name, wfPath, func(id, number int64) ports.CIRunDraft {
		run := domain.CIRun{
			Repo: repo.Name, ID: id, Number: number, Attempt: 1, WorkflowPath: wfPath, WorkflowName: wfPath,
			Title: firstLine(f.head.Message), Event: "push", Ref: f.update.Ref, Sha: f.commit.String(), Actor: t.actor,
			Status: domain.CICompleted, Conclusion: domain.CIFailure, Error: cutText(problem, maxRunError),
			CreatedAt: now, StartedAt: now, CompletedAt: now,
		}
		return ports.CIRunDraft{
			Run: run, Source: domain.CIRunSource{Repo: repo.Name, WorkflowYAML: "", GithubJSON: "{}"},
			Checks: []domain.CICheck{{
				ID: "run-" + strconv.FormatInt(id, 10), Repo: repo.Name, Sha: run.Sha, Name: wfPath, Status: domain.CICompleted,
				Conclusion: domain.CIFailure, RunID: id, Attempt: 1, WorkflowName: wfPath, CreatedAt: now, StartedAt: now, CompletedAt: now,
			}},
		}
	})
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	c.publishRun(ctx, repo, draft.Run)
	log := logRun(ctx, draft.Run)
	log.Info().Str("workflow", wfPath).Msg("ci run failed: invalid workflow file")
	return nil
}

func (c *CI) publishRun(ctx context.Context, repo domain.Repo, run domain.CIRun) {
	c.cfg.Bus.Publish(ctx, domain.RunUpdated{Run: run, At: c.now(), Visibility: repo.Visibility, CreatedAt: repo.CreatedAt})
}

// startRun creates the run of a matching workflow with one waiting record per
// job of the file, then plans the jobs that can start.
func (c *CI) startRun(ctx context.Context, repo domain.Repo, t ciTrigger, f pushFacts, wfPath, yaml string, wf *workflow.Workflow) error {
	now := c.now()
	name := wf.Name
	if name == "" {
		name = wfPath
	}
	draft, err := c.cfg.Store.CreateRun(ctx, repo.Name, wfPath, func(id, number int64) ports.CIRunDraft {
		run := domain.CIRun{
			Repo: repo.Name, ID: id, Number: number, Attempt: 1, WorkflowPath: wfPath, WorkflowName: name,
			Title: firstLine(f.head.Message), Event: "push", Ref: f.update.Ref, Sha: f.commit.String(), Actor: t.actor,
			Status: domain.CIQueued, CreatedAt: now,
		}
		d := ports.CIRunDraft{Run: run, Source: domain.CIRunSource{
			Repo: repo.Name, WorkflowYAML: yaml, GithubJSON: c.githubJSON(repo, run, f, wf),
		}}
		for i, j := range wf.Jobs {
			job := domain.CIJob{
				ID: domain.NewJobID(id, i, 0), RunID: id, Attempt: 1, RunAttempt: 1, Repo: repo.Name, Key: j.ID, Name: j.ID,
				Phase: domain.CIPhaseWaiting, Needs: slices.Clone(j.Needs), Index: i, CreatedAt: now,
			}
			d.Jobs = append(d.Jobs, job)
			d.Checks = append(d.Checks, domain.CICheck{
				ID: job.ID, Repo: repo.Name, Sha: run.Sha, Name: job.Name, Status: domain.CIQueued, RunID: id, JobID: job.ID,
				Attempt: 1, WorkflowName: name, CreatedAt: now,
			})
		}
		return d
	})
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	c.publishRun(ctx, repo, draft.Run)
	for _, j := range draft.Jobs {
		c.cfg.Bus.Publish(ctx, domain.JobUpdated{Job: j, At: now, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt})
	}
	log := logRun(ctx, draft.Run)
	log.Info().Str("workflow", wfPath).Str("ref", f.update.Ref).Int("jobs", len(draft.Jobs)).Msg("ci run created")
	return c.advanceRun(ctx, draft.Run.ID)
}

// --- the github context ---

type obj = map[string]any

func person(name, email string) obj { return obj{"name": name, "email": email} }

func (c *CI) commitJSON(repo domain.Repo, cm domain.Commit) obj {
	ts := cm.Committer.Time.UTC().Format(time.RFC3339)
	return obj{
		"id": cm.ID.String(), "tree_id": "", "distinct": true, "message": cm.Message, "timestamp": ts,
		"url":       c.cfg.PublicURL + "/" + repo.Name.String() + "/commit/" + cm.ID.String(),
		"author":    obj{"name": cm.Author.Name, "email": cm.Author.Email, "username": ""},
		"committer": obj{"name": cm.Committer.Name, "email": cm.Committer.Email, "username": ""},
		"added":     []string{}, "removed": []string{}, "modified": []string{},
	}
}

// githubJSON builds the "github" context of the run as GitHub does for a push
// (the job-dependent keys are added when a job is planned). Values that
// GitHub's documentation does not settle are marked for the recordings.
//
// TODO(verify with recording): the exact types of run_id, run_number,
// run_attempt (strings) and retention_days (a number), the empty tree_id and
// the empty added/removed/modified lists of the push commits.
func (c *CI) githubJSON(repo domain.Repo, run domain.CIRun, f pushFacts, wf *workflow.Workflow) string {
	full := c.cfg.Owner + "/" + repo.Name.String()
	refName, refType := strings.TrimPrefix(run.Ref, "refs/heads/"), "branch"
	if f.isTag {
		refName, refType = strings.TrimPrefix(run.Ref, "refs/tags/"), "tag"
	}
	repoURL := c.cfg.PublicURL + "/" + repo.Name.String()
	before := f.update.Old
	if f.created {
		before = domain.ZeroOID
	}
	compare := repoURL + "/compare/" + before + "..." + f.update.New
	if f.created {
		compare = repoURL + "/commit/" + f.update.New
	}
	commits := make([]any, 0, len(f.commits))
	for _, cm := range f.commits {
		commits = append(commits, c.commitJSON(repo, cm))
	}
	event := obj{
		"ref": run.Ref, "before": before, "after": f.update.New, "created": f.created, "deleted": false, "forced": f.forced,
		"base_ref": nil, "compare": compare, "commits": commits, "head_commit": c.commitJSON(repo, f.head),
		"pusher": person(run.Actor, ""),
		"repository": obj{
			"name": repo.Name.String(), "full_name": full, "private": repo.Visibility == domain.VisibilityPrivate,
			"owner": obj{"login": c.cfg.Owner, "name": c.cfg.Owner}, "html_url": repoURL, "clone_url": repoURL + ".git",
			"default_branch": "", "url": repoURL,
		},
		"sender": obj{"login": run.Actor, "type": "User"},
	}
	wfRef := full + "/" + run.WorkflowPath + "@" + run.Ref
	gh := obj{
		"event_name": "push", "event": event, "ref": run.Ref, "ref_name": refName, "ref_type": refType, "ref_protected": false,
		"sha": run.Sha, "repository": full, "repository_owner": c.cfg.Owner, "repositoryUrl": repoURL + ".git",
		"actor": run.Actor, "triggering_actor": run.Actor, "workflow": run.WorkflowName, "workflow_ref": wfRef,
		"workflow_sha": run.Sha, "run_id": strconv.FormatInt(run.ID, 10), "run_number": strconv.FormatInt(run.Number, 10),
		"server_url": c.cfg.PublicURL, "api_url": c.cfg.PublicURL + "/-/github/api/v3",
		"graphql_url": c.cfg.PublicURL + "/-/github/api/graphql", "retention_days": domain.CIRetentionDays,
		"head_ref": "", "base_ref": "",
	}
	b, err := json.Marshal(gh)
	if err != nil {
		return "{}"
	}
	return string(b)
}
