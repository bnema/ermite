// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"fmt"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// ChecksSummary is the combined state of the checks of a commit.
type ChecksSummary struct {
	Total        int
	Kind, State  string
	Label, Title string
	Href         string
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

// checksSection is the checks of the rail of a merge request, with the rule of
// the repository that applies to them.
func checksSection(repo domain.Repo, checks []ports.CheckView) ChecksSection {
	sec := ChecksSection{}
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
	case len(checks) == 0:
		sec.Note = "no check on this commit: nothing blocks the merge."
	case repo.Settings.FailingChecksBlock:
		sec.Note = "a failing or unfinished check blocks the merge."
	default:
		sec.Note = "checks do not block the merge in this repository."
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
