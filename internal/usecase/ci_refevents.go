// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"bnema.dev/ermite-actions/workflow"
	"bnema.dev/ermite/internal/domain"
)

// The events create and delete (CI-4, D6): a branch or a tag is created or
// deleted. As on GitHub ("Events that trigger workflows"), a create run is on
// the created ref, its last commit, and runs the workflows of that commit; a
// delete run is on the default branch, its last commit, and runs the workflows
// of that commit: the deleted ref is gone. Neither has filters, and GitHub's
// skip directives do not apply to them. A push that creates or deletes more
// than three tags creates no event for them.
//
// Trust: a create run is trusted when the pusher is and the record of the
// created ref (ci_tips.go) names the commit the run executes; a delete run when
// the deleter is and a trusted principal set the default branch to the commit
// it executes. The deleted ref's record went with the ref and is never looked
// at. Job tokens start neither (OnPush).

const (
	eventCreate = "create"
	eventDelete = "delete"
)

// startCreateRuns starts the create workflows of a ref a push created, from
// the files of the commit it points to (files; an invalid file is reported by
// the push run).
func (c *CI) startCreateRuns(ctx context.Context, repo domain.Repo, t ciTrigger, f pushFacts, files []workflowFile) error {
	newID, err := domain.ParseObjectID(f.update.New)
	if err != nil {
		return err
	}
	ct := t
	ct.trusted = t.trusted && c.refTrusted(ctx, repo.Name, pinnedRef{ref: f.update.Ref, commit: f.commit, object: newID})
	if t.trusted && !ct.trusted {
		ctxLog(ctx).Info().Str("repo", repo.Name.String()).Str("ref", f.update.Ref).
			Msg("ci create runs are not trusted: the created ref is not at a commit a trusted principal pushed")
	}
	cf := f
	cf.payload = nil
	cf.custom = &customEvent{name: eventCreate, ref: f.update.Ref}
	return c.startRefEventRuns(ctx, repo, ct, cf, files)
}

// handleDelete starts the delete workflows of the default branch for a deleted
// branch or tag; deletedTags is the number of tags the push deletes.
func (c *CI) handleDelete(ctx context.Context, t ciTrigger, u domain.RefUpdate, deletedTags int) error {
	if strings.HasPrefix(u.Ref, "refs/tags/") && deletedTags > workflow.MaxTagsPerPush {
		return nil
	}
	repo, err := c.cfg.Meta.Get(ctx, t.repo)
	if err != nil {
		return nil // deleted meanwhile
	}
	tip := c.defaultTipOf(ctx, t.repo)
	if tip.commit.IsZero() {
		return nil // no default branch to run from
	}
	head, _, err := c.cfg.Git.ListRange(ctx, t.repo, domain.ObjectID{}, tip.commit, 1)
	if err != nil {
		return fmt.Errorf("read the head commit of %s: %w", tip.branch, err)
	}
	if len(head) == 0 {
		return fmt.Errorf("read the head commit of %s: %w", tip.branch, domain.ErrNotFound)
	}
	files, err := c.workflowFiles(ctx, t.repo, tip.commit)
	if err != nil || len(files) == 0 {
		return err
	}
	dt := t
	dt.trusted = t.trusted && tip.trusted
	if t.trusted && !tip.trusted {
		ctxLog(ctx).Info().Str("repo", t.repo.String()).Str("ref", u.Ref).Str("tip", tip.commit.String()).
			Msg("ci delete runs are not trusted: the default branch tip was not pushed by a trusted principal")
	}
	f := pushFacts{
		update: domain.RefUpdate{Ref: "refs/heads/" + tip.branch, Old: domain.ZeroOID, New: tip.commit.String()},
		commit: tip.commit, head: head[0], defaultBranch: tip.branch,
		custom: &customEvent{name: eventDelete, ref: u.Ref},
	}
	return c.startRefEventRuns(ctx, repo, dt, f, files)
}

// startRefEventRuns starts the workflows of files that are on the event of
// f.custom (create or delete).
func (c *CI) startRefEventRuns(ctx context.Context, repo domain.Repo, t ciTrigger, f pushFacts, files []workflowFile) error {
	var errs []error
	for _, file := range files {
		if file.problem != "" {
			continue
		}
		wf, perrs := parseWorkflow(file.path, file.content)
		if len(perrs) > 0 || wf.On == nil || !wf.On.Has(f.custom.name) {
			continue
		}
		if problem := c.matrixProblem(ctx, repo, t, f, file.path, wf); problem.text != "" {
			name := file.path
			if problem.named {
				name = workflowName(wf, file.path)
			}
			errs = append(errs, c.startInvalid(ctx, repo, t, f, file.path, name, problem.text))
			continue
		}
		errs = append(errs, c.startRun(ctx, repo, t, f, file.path, string(file.content), wf))
	}
	return errors.Join(errs...)
}

// refEventPayload is the payload of a create or a delete event: the ref by its
// short name and its type; create adds the default branch and the description
// of the repository (ermite has none: null). pusher_type is "user": a job
// token, GitHub's "deploy key" case aside, starts no run.
func refEventPayload(name, ref, defaultBranch string, event map[string]any) {
	refName, refType := strings.TrimPrefix(ref, "refs/heads/"), "branch"
	if tag, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
		refName, refType = tag, "tag"
	}
	event["ref"], event["ref_type"], event["pusher_type"] = refName, refType, "user"
	if name == eventCreate {
		event["master_branch"], event["description"] = defaultBranch, nil
	}
}
