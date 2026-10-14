// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
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

	"bnema.dev/ermite-actions/expr"
	"bnema.dev/ermite-actions/template"
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
	pusher  domain.CIPusher
	// trusted: the principal that pushed or merged is the owner
	// (domain.TrustedActor), so the run gets secrets.
	trusted bool
	at      time.Time
	log     zerowrap.Logger
}

// OnPush queues the refs of a push or a merge for planning and returns at once;
// a worker plans them. A full queue drops the trigger, logged at error: a push
// is never slowed down or failed by CI.
//
// A push made with a job token starts nothing, as on GitHub (events created
// with GITHUB_TOKEN start no run): that is what stops a workflow that pushes
// from running itself again.
func (c *CI) OnPush(ctx context.Context, repo domain.RepoName, updates []domain.RefUpdate, pusher domain.Principal) {
	if pusher.ViaJobToken() {
		log := zerowrap.FromCtx(ctx)
		log.Debug().Str("repo", repo.String()).Int("refs", len(updates)).Msg("push by a job token starts no run")
		return
	}
	c.enqueue(ctx, repo, updates, authorOf(pusher), domain.PusherOf(pusher), domain.TrustedActor(pusher))
}

// OnMerge queues the move of a target branch by a merge. The actor of the run is
// the owner, as GitHub shows; the pusher recorded with it, whose rights the
// actions of other repositories are resolved with, is the principal that merged:
// a token limited to some repositories does not gain the owner's reach by
// merging. For the same reason the run is trusted (gets secrets) only when the
// merger is: the displayed actor is not the one that decides.
func (c *CI) OnMerge(ctx context.Context, repo domain.RepoName, updates []domain.RefUpdate, merger domain.Principal) {
	if merger.ViaJobToken() {
		log := zerowrap.FromCtx(ctx)
		log.Debug().Str("repo", repo.String()).Int("refs", len(updates)).Msg("merge by a job token starts no run")
		return
	}
	c.enqueue(ctx, repo, updates, authorOf(domain.Owner()), domain.PusherOf(merger), domain.TrustedActor(merger))
}

func (c *CI) enqueue(ctx context.Context, repo domain.RepoName, updates []domain.RefUpdate, actor string, pusher domain.CIPusher, trusted bool) {
	var kept []domain.RefUpdate
	for _, u := range updates {
		if !u.IsDelete() && (strings.HasPrefix(u.Ref, "refs/heads/") || strings.HasPrefix(u.Ref, "refs/tags/")) {
			kept = append(kept, u)
		}
	}
	if len(kept) == 0 {
		return
	}
	t := ciTrigger{repo: repo, updates: kept, actor: actor, pusher: pusher, trusted: trusted, at: c.now(), log: zerowrap.FromCtx(ctx)}
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
	// CI is on once a runner is registered (plan, "Config"): without one a push
	// makes no run and no check, so a forge that does not use CI shows nothing.
	runners, err := c.cfg.Store.ListRunners(ctx)
	if err != nil {
		return fmt.Errorf("list the runners: %w", err)
	}
	if len(runners) == 0 {
		return nil
	}
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
	commits []domain.Commit // oldest first, at most maxPushCommits
	forced  bool
	created bool
	files   []string
	// filesTruncated: the list of changed files is not available.
	filesTruncated bool
	tagCount       int
	isTag          bool
	// defaultBranch is the default branch of the repository, "" when unknown.
	defaultBranch string
	// payload is the commit part of the push event, shared by the workflows
	// of the ref; handleRef builds it once, before the first one that matches.
	payload *pushPayload
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
			errs = append(errs, c.startInvalid(ctx, repo, t, facts, f.path, f.path, f.problem))
		case len(perrs) > 0:
			var lines []string
			for _, e := range perrs {
				lines = append(lines, e.Error())
			}
			errs = append(errs, c.startInvalid(ctx, repo, t, facts, f.path, f.path, strings.Join(lines, "\n")))
		case skip || !wf.MatchPush(workflow.PushInfo{
			Ref: u.Ref, Created: facts.created, ChangedFiles: facts.files, FilesTruncated: facts.filesTruncated, TagCount: tagCount,
		}):
		default:
			if facts.payload == nil {
				facts.payload = c.pushPayload(ctx, repo, facts)
			}
			if problem := c.matrixProblem(ctx, repo, t, facts, f.path, wf); problem.text != "" {
				name := f.path
				if problem.named {
					name = workflowName(wf, f.path)
				}
				errs = append(errs, c.startInvalid(ctx, repo, t, facts, f.path, name, problem.text))
				continue
			}
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
	if def, _, err := c.cfg.Repos.DefaultBranch(ctx, repo); err == nil {
		f.defaultBranch = def
	}
	res, err := c.cfg.Reader.ResolveRevision(ctx, repo, domain.RevisionOf(newID))
	if err != nil {
		return f, fmt.Errorf("resolve %s: %w", u.New, err)
	}
	f.commit = res.CommitID
	if isTag {
		// Recorded (push-tag, skip-directive-tag, push-tags-limit): the event of
		// a tag carries the commits that no branch has, newest as head_commit
		// ("compare" is then that commit's page, or "<first>^...<head>" for
		// several). Not the range from the previous push, and not the commits
		// the default branch lacks: in push-tag the branch commit under the
		// tagged one is not listed, in skip-directive-tag, which has no branch,
		// both commits are. A tag on a commit that a branch has lists that
		// commit alone (not recorded). No file list: paths filters are not
		// evaluated for tags.
		cs, _, err := c.cfg.Diff.CommitsOffBranches(ctx, repo, f.commit, ports.MaxCommitLimit)
		if err != nil {
			return f, fmt.Errorf("read the commits of the tag: %w", err)
		}
		if len(cs) == 0 {
			cs, _, err = c.cfg.Git.ListRange(ctx, repo, domain.ObjectID{}, f.commit, 1)
			if err != nil {
				return f, fmt.Errorf("read the commit of the tag: %w", err)
			}
			if len(cs) == 0 {
				return f, fmt.Errorf("read the commit of the tag: %w", domain.ErrNotFound)
			}
		}
		f.head = cs[0]
		f.commits = slices.Clone(cs[:min(len(cs), maxPushCommits)])
		slices.Reverse(f.commits)
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
	if err != nil {
		return f, fmt.Errorf("read the commits of the push: %w", err)
	}
	if len(cs) == 0 {
		// A new branch at a commit the default branch has, or a branch rewound
		// to an ancestor: no new commit, no changed file. GitHub still runs
		// the push workflows that have no paths filter, for the head commit.
		head, _, err := c.cfg.Git.ListRange(ctx, repo, domain.ObjectID{}, f.commit, 1)
		if err != nil {
			return f, fmt.Errorf("read the head commit of the push: %w", err)
		}
		if len(head) == 0 {
			return f, fmt.Errorf("read the head commit of the push: %w", domain.ErrNotFound)
		}
		f.head = head[0]
		return f, nil
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
//
// name is what the run is called: the path of the file when the file is not a
// workflow GitHub accepts, or the workflow's own name for a workflow that
// parses but whose strategy GitHub refuses (recorded: matrix-over-256).
func (c *CI) startInvalid(ctx context.Context, repo domain.Repo, t ciTrigger, f pushFacts, wfPath, name, problem string) error {
	now := c.now()
	draft, err := c.cfg.Store.CreateRun(ctx, repo.Name, wfPath, func(id, number int64) ports.CIRunDraft {
		run := domain.CIRun{
			Repo: repo.Name, ID: id, Number: number, Attempt: 1, WorkflowPath: wfPath, WorkflowName: name, Name: name,
			Title: firstLine(f.head.Message), Event: "push", Ref: f.update.Ref, Sha: f.commit.String(), Actor: t.actor,
			Status: domain.CICompleted, Conclusion: domain.CIFailure, Error: cutText(problem, maxRunError), Trusted: t.trusted,
			CreatedAt: now, StartedAt: now, CompletedAt: now,
		}
		return ports.CIRunDraft{
			Run: run, Source: domain.CIRunSource{Repo: repo.Name, WorkflowYAML: "", GithubJSON: "{}", Pusher: t.pusher},
			Checks: []domain.CICheck{{
				ID: "run-" + strconv.FormatInt(id, 10), Repo: repo.Name, Sha: run.Sha, Name: wfPath, Status: domain.CICompleted,
				Conclusion: domain.CIFailure, RunID: id, Attempt: 1, WorkflowName: name, CreatedAt: now, StartedAt: now, CompletedAt: now,
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
	name := workflowName(wf, wfPath)
	// The title of a run without run-name is the commit message for a branch.
	// For a tag GitHub shows the name of the workflow registered for the path,
	// not the commit message (recorded: filter-tags, filter-paths-tag,
	// filter-branches-ignore, and push-tag, push-tags-limit, push-delete-tag,
	// skip-directive-tag, whose ci.yml is called "push-branch-create" on the
	// default branch of the recording repository although each run's own
	// workflow has another name: 14 of 14 tag runs). Registered is read as the
	// "name:" of the file on the default branch; the first version GitHub saw
	// is the same file in the recording and cannot be told apart.
	title := firstLine(f.head.Message)
	if f.isTag {
		title = c.registeredName(ctx, repo.Name, wfPath, name)
	}
	vars, err := c.varsContext(ctx, repo.Name)
	if err != nil {
		return err
	}
	draft, err := c.cfg.Store.CreateRun(ctx, repo.Name, wfPath, func(id, number int64) ports.CIRunDraft {
		run := domain.CIRun{
			Repo: repo.Name, ID: id, Number: number, Attempt: 1, WorkflowPath: wfPath, WorkflowName: name, Name: name,
			Title: title, Event: "push", Ref: f.update.Ref, Sha: f.commit.String(), Actor: t.actor,
			Status: domain.CIQueued, Trusted: t.trusted, CreatedAt: now,
		}
		githubJSON := c.githubJSON(ctx, repo, run, f)
		if runName, ok := c.evalRunName(ctx, wf, githubJSON, vars); ok {
			run.Name, run.Title = runName, runName
		}
		d := ports.CIRunDraft{Run: run, Source: domain.CIRunSource{
			Repo: repo.Name, WorkflowYAML: yaml, GithubJSON: githubJSON, Pusher: t.pusher,
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

// workflowName is the name of a workflow, the path of its file when it has no
// "name:".
func workflowName(wf *workflow.Workflow, path string) string {
	if wf.Name == "" {
		return path
	}
	return wf.Name
}

// registeredName is the name GitHub shows for a tag run without run-name: the
// name of the workflow registered for the path, here the "name:" of the file on
// the default branch (the path when that file has none). It falls back to own,
// the name of the workflow of the run, when the default branch has no such
// file, the file does not parse, or anything cannot be read; a failure other
// than a missing file is logged at debug.
func (c *CI) registeredName(ctx context.Context, repo domain.RepoName, wfPath, own string) string {
	name, err := c.registeredNameOf(ctx, repo, wfPath)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			log := zerowrap.FromCtx(ctx)
			log.Debug().Err(err).Str("workflow", wfPath).Msg("ci tag run title: the registered workflow name is not readable, using the run's own")
		}
		return own
	}
	return name
}

// registeredNameOf is registeredName without the fallback; a missing default
// branch or file is domain.ErrNotFound.
func (c *CI) registeredNameOf(ctx context.Context, repo domain.RepoName, wfPath string) (string, error) {
	def, exists, err := c.cfg.Repos.DefaultBranch(ctx, repo)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", domain.ErrNotFound
	}
	tip, err := c.cfg.Git.BranchHead(ctx, repo, def)
	if err != nil {
		return "", err
	}
	tp, err := domain.ParseTreePath(wfPath)
	if err != nil {
		return "", err
	}
	_, rc, err := c.cfg.Reader.OpenBlob(ctx, repo, domain.RevisionOf(tip), tp)
	if err != nil {
		return "", err
	}
	content, err := io.ReadAll(io.LimitReader(rc, maxCIWorkflowBytes+1))
	_ = rc.Close()
	if err != nil {
		return "", err
	}
	if len(content) > maxCIWorkflowBytes {
		return "", fmt.Errorf("workflow file over %d bytes", maxCIWorkflowBytes)
	}
	wf, errs := workflow.Parse(wfPath, content)
	if len(errs) > 0 {
		return "", fmt.Errorf("workflow does not parse: %v", errs[0])
	}
	return workflowName(wf, wfPath), nil
}

// evalRunName evaluates the run-name of a workflow with the github, inputs and
// vars contexts (the contexts the schema allows there). ok is false when the
// workflow has none, when it does not evaluate (logged), or when it evaluates
// to nothing: the run then has no name of its own, GitHub's display title falls
// back to the commit message (a branch) or the registered name (a tag), and
// the name of the run to the name of the workflow.
func (c *CI) evalRunName(ctx context.Context, wf *workflow.Workflow, githubJSON string, vars *expr.Dictionary) (string, bool) {
	if wf.RunName == nil {
		return "", false
	}
	gh, err := expr.FromJSON([]byte(githubJSON))
	if err != nil {
		return "", false
	}
	env := template.Env{Contexts: map[string]expr.Value{
		"github": gh, "inputs": expr.NewDictionary(), "vars": vars,
	}}
	name, err := template.EvaluateString(ctx, wf.RunName, env)
	if err != nil {
		log := zerowrap.FromCtx(ctx)
		log.Warn().Err(err).Msg("ci run-name does not evaluate: the run keeps the workflow name and the default title")
		return "", false
	}
	if name = firstLine(name); name == "" {
		return "", false
	}
	return name, true
}

// matrixProblem expands the strategy of every job of the file, whatever its
// "if:" says, with an empty needs context (a matrix built from a needs output does not
// evaluate here, and that error is ignored),
// and returns the error text of the first strategy that produces more
// configurations than GitHub allows: GitHub then fails the run with no job,
// like an invalid workflow file (recorded: matrix-over-256; the run keeps the
// name of the workflow). An "exclude" entry with a key that is no vector of the
// matrix is refused too, and there GitHub names the run after the file path
// (recorded: matrix-edge-values, a-exclude-unknown-key.yml). Any other error
// of a strategy is ignored here and stays a job failure, found when the job is
// planned. Whether GitHub counts the matrix of a job that "if:" skips is not
// recorded yet (needs an "if: false" scenario over 256). The github context
// is that of a run with no id yet, on the commits shared by the ref.
func (c *CI) matrixProblem(ctx context.Context, repo domain.Repo, t ciTrigger, f pushFacts, wfPath string, wf *workflow.Workflow) matrixIssue {
	if !slices.ContainsFunc(wf.Jobs, func(j *workflow.Job) bool { return j.Strategy != nil }) {
		return matrixIssue{} // nothing to expand: no context to build
	}
	probe := domain.CIRun{Repo: repo.Name, Ref: f.update.Ref, Sha: f.commit.String(), Actor: t.actor, WorkflowPath: wfPath, WorkflowName: wf.Name, CreatedAt: c.now()}
	gh, err := expr.FromJSON([]byte(c.githubJSON(ctx, repo, probe, f)))
	if err != nil {
		return matrixIssue{}
	}
	vars, err := c.varsContext(ctx, repo.Name)
	if err != nil {
		// Only the sizes of the matrices are looked at here: the job fails on
		// the same error when it is planned.
		vars = expr.NewDictionary()
	}
	env := template.Env{Contexts: map[string]expr.Value{
		"github": gh, "needs": expr.NewDictionary(), "inputs": expr.NewDictionary(), "vars": vars,
	}}
	for _, j := range wf.Jobs {
		_, err := j.EvaluateStrategy(ctx, env)
		switch {
		case errors.Is(err, workflow.ErrTooManyConfigurations):
			return matrixIssue{text: wfPath + ": " + err.Error(), named: true}
		case errors.Is(err, workflow.ErrExcludeUnknownKey):
			// One line per offending entry, each with the file.
			return matrixIssue{text: wfPath + ": " + strings.ReplaceAll(err.Error(), "\n", "\n"+wfPath+": ")}
		}
	}
	return matrixIssue{}
}

// matrixIssue is a strategy GitHub refuses: the error text, and whether the run
// keeps the name of the workflow (else it is named after the file).
type matrixIssue struct {
	text  string
	named bool
}

// --- the github context (see ci_github.go) ---

// shortID is the 12-character form of an object id that GitHub puts in the
// compare URL of a push.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// retentionDays is the retention in days, as github.retention_days shows it
// (at least 1).
func (c *CI) retentionDays() int {
	return max(int(c.cfg.Retention/(24*time.Hour)), 1)
}
