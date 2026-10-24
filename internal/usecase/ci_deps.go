// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Deps jobs (Bernard, P4.2). Their ids start with depsJobPrefix and they live
// in the queue of ports.DepsJobs, never in a CIRun. The runner authenticates
// with its runner secret on every call of such a job; the queue checks that the
// job is assigned to that runner.
//
// A deps job gets one repository token at its assignment: the token of an
// untrusted run (read only, no secrets, vars or ID token), stored through the
// CI store with the job id and run id 0. It is revoked by RevokeDepsJob, which
// Bernard calls whenever a job leaves its queue (completed, failed, timed
// out); whatever happens, it expires at the job deadline plus
// domain.RepoTokenGrace. A server restart loses the queue: the tokens of the
// lost jobs only expire.

const depsJobPrefix = "deps-"

func isDepsJob(id string) bool { return strings.HasPrefix(id, depsJobPrefix) }

// SetDepsJobs installs the deps job queue after construction (Bernard is built
// after CI); a nil queue turns deps jobs off.
func (c *CI) SetDepsJobs(q ports.DepsJobs) {
	c.mu.Lock()
	c.cfg.DepsJobs = q
	c.mu.Unlock()
}

func (c *CI) depsQueue() ports.DepsJobs {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg.DepsJobs
}

// RevokeDepsJob deletes the repository token of a deps job.
func (c *CI) RevokeDepsJob(ctx context.Context, jobID string) {
	_, err := c.cfg.Store.Apply(context.WithoutCancel(ctx), ports.CIWrite{RevokeJobs: []ports.CIJobRef{{JobID: jobID}}})
	if err != nil {
		ctxLog(ctx).Error().Err(err).Str("job_id", jobID).Msg("revoke the token of a deps job")
	}
}

// nextDeps gives the runner a deps job when it asked for the kind and offers
// the deps label. Failures are logged: the poll goes on with workflow jobs.
func (c *CI) nextDeps(ctx context.Context, runner domain.CIRunner, kinds, offered []string, pollID string) (*ports.RunnerJob, bool) {
	q := c.depsQueue()
	if q == nil || c.cfg.DepsRunnerLabel == "" || !slices.Contains(kinds, "deps") ||
		!slices.Contains(offered, strings.ToLower(c.cfg.DepsRunnerLabel)) {
		return nil, false
	}
	log := ctxLog(ctx)
	a, ok, err := q.Next(ctx, runner.ID)
	if err != nil {
		log.Error().Err(err).Msg("next deps job")
		return nil, false
	}
	if !ok {
		return nil, false
	}
	secret, err := c.issueDepsToken(ctx, a)
	if err != nil {
		log.Error().Err(err).Str("job_id", a.JobID).Msg("issue the token of a deps job")
		fail := &ports.DepsReport{Error: "could not issue the repository token"}
		if cerr := q.Complete(ctx, runner.ID, a.JobID, fail, domain.CIFailure); cerr != nil {
			log.Error().Err(cerr).Str("job_id", a.JobID).Msg("fail a deps job")
		}
		return nil, false
	}
	job := a.Job
	out := &ports.RunnerJob{
		Job:         domain.CIJob{ID: a.JobID, Repo: a.Repo, Key: "deps", Name: "deps", Labels: slices.Clone(offered), RunnerID: runner.ID, RunnerName: runner.Name},
		Deps:        &job,
		Repo:        a.Repo,
		Sha:         a.Sha,
		GithubToken: secret,
		Trusted:     false,
		Permissions: domain.TokenPermissions(nil, false),
	}
	if pollID != "" {
		// Like a CI assignment: a retried poll with the same poll_id gets this
		// one again, until the runner shows it has it.
		c.mu.Lock()
		c.polls[runner.ID] = pollRecord{pollID: pollID, ref: a.JobID, deps: out, until: a.Deadline.Add(domain.RepoTokenGrace)}
		c.mu.Unlock()
	}
	return out, true
}

// repeatDeps is the deps assignment a poll made, for the same poll_id, while
// it is not acknowledged and not past its deadline.
func (c *CI) repeatDeps(rec pollRecord, runnerID string) (*ports.RunnerJob, bool) {
	if rec.deps == nil {
		return nil, false
	}
	if !c.now().Before(rec.until) {
		c.mu.Lock()
		if cur, ok := c.polls[runnerID]; ok && cur.ref == rec.ref {
			delete(c.polls, runnerID)
		}
		c.mu.Unlock()
		return nil, false
	}
	j := *rec.deps
	return &j, true
}

// issueDepsToken stores a read-only repository token for the job.
func (c *CI) issueDepsToken(ctx context.Context, a ports.DepsAssignment) (string, error) {
	now := c.now()
	secret := newToken(domain.RepoTokenPrefix)
	t := domain.CIRepoToken{
		ID: newRepoTokenID(), Hash: domain.HashTokenSecret(secret), Repo: a.Repo, JobID: a.JobID,
		Permissions: domain.TokenPermissions(nil, false), ExpiresAt: a.Deadline.Add(domain.RepoTokenGrace), CreatedAt: now,
	}
	if _, err := c.cfg.Store.Apply(ctx, ports.CIWrite{RepoTokens: []domain.CIRepoToken{t}}); err != nil {
		return "", err
	}
	return secret, nil
}

func (c *CI) completeDeps(ctx context.Context, secret string, params ports.CompleteJobParams) (ports.CompleteJobResult, error) {
	r, err := c.runnerFor(ctx, secret)
	if err != nil {
		return ports.CompleteJobResult{}, err
	}
	q := c.depsQueue()
	if q == nil {
		return ports.CompleteJobResult{}, domain.ErrNotFound
	}
	switch params.Conclusion {
	case domain.CISuccess, domain.CIFailure, domain.CICancelled, domain.CITimedOut:
	default:
		return ports.CompleteJobResult{}, fmt.Errorf("%w: a runner reports success, failure, cancelled or timed_out", domain.ErrInvalid)
	}
	c.forgetPoll(params.JobID)
	if err := q.Complete(ctx, r.ID, params.JobID, params.DepsReport, params.Conclusion); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return ports.CompleteJobResult{}, domain.ErrNotFound
		}
		return ports.CompleteJobResult{}, err
	}
	return ports.CompleteJobResult{Conclusion: params.Conclusion, Applied: true}, nil
}
