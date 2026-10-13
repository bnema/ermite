// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"fmt"
	"slices"
	"unicode/utf8"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The results of a job beyond its steps and its log: annotations and step
// summaries (runner.proto, run.proto).

// annotationWrites returns the replacements an UpdateJob or CompleteJob makes:
// one per step the request names whose stored annotations differ, so that
// repeating a call changes nothing. The request was validated.
func (c *CI) annotationWrites(ctx context.Context, j domain.CIJob, anns []domain.CIAnnotation) ([]ports.CIStepAnnotations, error) {
	if len(anns) == 0 {
		return nil, nil
	}
	stored, err := c.cfg.Store.JobAnnotations(ctx, j.RunID, j.Attempt, j.ID)
	if err != nil {
		return nil, fmt.Errorf("read the annotations of job %s: %w", j.ID, err)
	}
	have, _ := domain.GroupAnnotations(stored)
	want, steps := domain.GroupAnnotations(anns)
	var out []ports.CIStepAnnotations
	for _, step := range steps {
		if slices.Equal(have[step], want[step]) {
			continue
		}
		out = append(out, ports.CIStepAnnotations{Repo: j.Repo, Exec: logRef(j), Step: step, Items: want[step]})
	}
	return out, nil
}

// UploadSummary stores the summary of a step, replacing the one it has. An
// empty Markdown removes it.
func (c *CI) UploadSummary(ctx context.Context, secret, jobID string, step int32, markdown string) error {
	j, err := c.jobFor(ctx, secret, jobID)
	if err != nil {
		return err
	}
	if j.Ended() {
		return endedError(j)
	}
	switch {
	case step < 1 || step > domain.CIMaxSteps:
		return fmt.Errorf("%w: step %d is not between 1 and %d", domain.ErrInvalid, step, domain.CIMaxSteps)
	case len(markdown) > domain.CIMaxSummaryBytes:
		return fmt.Errorf("%w: the summary is larger than 1 MiB", domain.ErrInvalid)
	case !utf8.ValidString(markdown):
		return fmt.Errorf("%w: the summary is not valid UTF-8", domain.ErrInvalid)
	}
	// Under the lock of the run, so that the end of the job and the summary do
	// not cross: after the end nothing is stored.
	return c.lockedJob(ctx, j, func(cur *domain.CIJob) error {
		if cur.Ended() {
			return endedError(*cur)
		}
		return c.cfg.Summaries.Put(ctx, logRef(*cur), step, markdown)
	})
}

// ListAnnotations returns the annotations of a run attempt (0 for the latest),
// of one job when jobID is set, ordered by job, step and production order.
// Each job gives those of the attempt in which it executed.
func (c *CI) ListAnnotations(ctx context.Context, p domain.Principal, name domain.RepoName, runID int64, attempt int32, jobID string) (ports.AnnotationList, error) {
	if _, err := c.readableRepo(ctx, p, name); err != nil {
		return ports.AnnotationList{}, err
	}
	run, err := c.runOf(ctx, name, runID)
	if err != nil {
		return ports.AnnotationList{}, err
	}
	if attempt < 0 || attempt > run.Attempt {
		return ports.AnnotationList{}, domain.ErrNotFound
	}
	if attempt == 0 {
		attempt = run.Attempt
	}
	jobs, err := c.cfg.Store.ListJobs(ctx, runID, attempt)
	if err != nil {
		return ports.AnnotationList{}, err
	}
	if jobID != "" {
		jobs = slices.DeleteFunc(jobs, func(j domain.CIJob) bool { return j.ID != jobID })
		if len(jobs) == 0 {
			return ports.AnnotationList{}, domain.ErrNotFound
		}
	}
	out := ports.AnnotationList{Annotations: []ports.JobAnnotation{}}
	for _, j := range jobs {
		anns, err := c.cfg.Store.JobAnnotations(ctx, j.RunID, j.Attempt, j.ID)
		if err != nil {
			return ports.AnnotationList{}, err
		}
		for _, a := range anns {
			if len(out.Annotations) >= domain.CIMaxListedAnnotations {
				out.Truncated = true
				return out, nil
			}
			out.Annotations = append(out.Annotations, ports.JobAnnotation{JobID: j.ID, Annotation: a})
		}
	}
	return out, nil
}

// GetStepSummaries returns the summaries of a job at an attempt (0 for the
// latest): the first 20 steps that have one, by step number.
func (c *CI) GetStepSummaries(ctx context.Context, p domain.Principal, name domain.RepoName, jobID string, attempt int32) (ports.SummaryList, error) {
	if _, err := c.readableRepo(ctx, p, name); err != nil {
		return ports.SummaryList{}, err
	}
	_, j, err := c.jobOf(ctx, name, jobID, attempt)
	if err != nil {
		return ports.SummaryList{}, err
	}
	list, more, err := c.cfg.Summaries.List(ctx, logRef(j), domain.CIMaxListedSummaries)
	if err != nil {
		return ports.SummaryList{}, err
	}
	return ports.SummaryList{Summaries: list, Truncated: more}, nil
}
