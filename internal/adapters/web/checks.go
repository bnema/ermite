// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"fmt"

	"bnema.dev/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// ChecksSummary is the combined state of the checks of a commit.
type ChecksSummary struct {
	Total        int
	Kind, State  string
	Label, Title string
	Href         string
	// Deps is Bernard's dependency run of the merge request, shown when the
	// commit has no check yet; nil otherwise.
	Deps *CheckRow
}

// CheckRow is a check of a commit or a merge request.
type CheckRow struct {
	Name, Kind, State, Href string
	Waiting                 bool
	GroupLabel, GroupHref   string
}

// ChecksSection is the checks of the head of a merge request.
type ChecksSection struct {
	Rows []CheckRow
	// Note explains the rule that applies to them.
	Note string
}

// checksFor reads the checks of a commit as the caller of the request. A page
// shows them when it can: a failure of the read is logged and the page goes on
// without, since the code and the review do not depend on CI.
func (s *Server) checksFor(ctx context.Context, repo domain.RepoName, sha string) []ports.CheckView {
	if s.cfg.Runs == nil || sha == "" {
		return nil
	}
	res, err := s.cfg.Runs.ListChecks(ctx, principal(ctx), repo, sha)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			log := zerowrap.FromCtx(ctx)
			log.Warn().Err(err).Str("repo", repo.String()).Msg("read the checks of a commit")
		}
		return nil
	}
	return res.Checks
}

// checksSummary reduces checks to the mark a list shows next to a commit.
//
//hexcheck:ignore suspicious-business-logic-in-adapter renders domain check outcomes as a commit-list mark, passed count, label and actions link
func checksSummary(repo domain.RepoName, sha string, checks []ports.CheckView) ChecksSummary {
	if len(checks) == 0 {
		return ChecksSummary{}
	}
	plain := make([]domain.CICheck, 0, len(checks))
	passed := 0
	for _, c := range checks {
		plain = append(plain, c.CICheck)
		if c.Status == domain.CICompleted && c.Conclusion.PassesMerge() {
			passed++
		}
	}
	sum := ChecksSummary{Total: len(checks), Href: repoHref(repo.String(), "actions"), Label: fmt.Sprintf("%d/%d", passed, len(checks))}
	switch domain.CombineChecks(plain) {
	case domain.CheckStateSuccess:
		sum.Kind, sum.State = "ok", "checks passed"
	case domain.CheckStateFailure:
		sum.Kind, sum.State = "bad", "checks failed"
	default:
		sum.Kind, sum.State = "running", "checks pending"
	}
	sum.Title = sum.State + ": " + sum.Label
	return sum
}

// checksSection is the checks of the footer of a merge request, with the rule of
// the repository that applies to them.
func checksSection(repo domain.Repo, checks []ports.CheckView, deps *CheckRow) ChecksSection {
	sec := ChecksSection{}
	if deps != nil {
		sec.Rows = append(sec.Rows, *deps)
	}
	groups := checksByRun(checks)
	for _, group := range groups {
		for i, c := range group {
			row := CheckRow{
				Name: visibleText(c.Name), Kind: ciKind(c.Status, c.Conclusion), State: ciState(c.Status, c.Conclusion),
				Waiting: c.WaitingForRunner,
			}
			if c.JobID != "" {
				row.Href = jobHref(repo.Name.String(), c.RunID, c.JobID)
			} else {
				row.Href = runHref(repo.Name.String(), c.RunID)
			}
			if len(groups) > 1 && i == 0 {
				event := visibleText(c.Event)
				if event == "" {
					event = fmt.Sprintf("run %d", c.RunID)
				}
				row.GroupLabel = visibleText(c.WorkflowName) + " · " + event
				row.GroupHref = runHref(repo.Name.String(), c.RunID)
			}
			sec.Rows = append(sec.Rows, row)
		}
	}
	switch {
	case deps != nil && deps.Kind != "bad":
		sec.Note = "CI runs after the dependency run finishes."
	case len(checks) == 0 && deps != nil:
		sec.Note = "the dependency run failed: Bernard holds CI on this commit."
	case len(checks) == 0:
		sec.Note = "no check on this commit: nothing blocks the merge."
	default:
		sec.Note = "a failing or unfinished check blocks the merge."
	}
	return sec
}

// checksByRun keeps first-appearance order and the check order within each run.
func checksByRun(checks []ports.CheckView) [][]ports.CheckView {
	groups := make([][]ports.CheckView, 0)
	indices := make(map[int64]int)
	for _, c := range checks {
		i, ok := indices[c.RunID]
		if !ok {
			i = len(groups)
			indices[c.RunID] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], c)
	}
	return groups
}

// depsCheck is the row of Bernard's dependency run for merge request n of
// repo, nil when n is not the merge request of a batch Bernard still drives.
// The dependency records are the owner's: anyone else gets nil.
//
//hexcheck:ignore suspicious-business-logic-in-adapter picks the batch of the merge request page from the Inbox read of DepsService, display only
func (s *Server) depsCheck(ctx context.Context, repo domain.RepoName, n int64) *CheckRow {
	if s.cfg.Deps == nil || !SignedIn(ctx) || n == 0 {
		return nil
	}
	p := principal(ctx)
	recs, err := s.cfg.Deps.List(ctx, p, repo)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			log := zerowrap.FromCtx(ctx)
			log.Warn().Err(err).Str("repo", repo.String()).Msg("read the dependency record")
		}
		return nil
	}
	for _, rec := range recs {
		if rec.Repo != repo {
			continue
		}
		for _, b := range rec.Batches {
			if b.MR != n || b.Adopted {
				continue
			}
			pos := 0
			if b.Queued() {
				if _, order, err := s.cfg.Deps.Queue(ctx, p); err == nil {
					pos = domain.DepsQueuePosition(order, repo, b.Key)
				}
			}
			return depsCheckRow(repo, b, pos)
		}
	}
	return nil
}

// depsCheckRow is the check row of the state of batch b, at queue position
// pos (zero when unknown).
//
//hexcheck:ignore suspicious-business-logic-in-adapter renders a batch state as a check row, as depsStatus does for the Inbox
func depsCheckRow(repo domain.RepoName, b domain.DepBatch, pos int) *CheckRow {
	row := &CheckRow{Name: "Dependencies"}
	if b.RunID != 0 {
		row.Href = runHref(repo.String(), b.RunID)
	}
	switch {
	case b.Queued():
		row.Kind, row.State, row.Href = "queued", "queued", ""
		if pos > 0 {
			row.State = fmt.Sprintf("queued (#%d)", pos)
		}
	case b.State == domain.DepsApplying:
		row.Kind, row.State = "running", "running"
	case b.State == domain.DepsFailed:
		row.Kind, row.State = "bad", "failed"
	default:
		return nil
	}
	return row
}
