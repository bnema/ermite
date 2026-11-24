// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/a-h/templ"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// WorkRow is one open merge request or issue.
type WorkRow struct {
	Repo     string
	RepoHref string
	Number   string
	Href     string
	Title    string
	When     string
	WhenAt   string
	Cursor   bool
	// Checks is the CI state of the head of a merge request; zero otherwise.
	Checks ChecksSummary
}

// WorkPage is the open merge requests and issues.
type WorkPage struct {
	MRs, Issues []WorkRow
}

// workPath is the owner's open merge requests and issues.
const workPath = "/-/work"

// work is the page of the owner's open merge requests and issues.
func (s *Server) work(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	if !SignedIn(ctx) {
		return nil, 0, errNotFound
	}
	p, err := s.workPage(ctx)
	if err != nil {
		return nil, 0, err
	}
	return Work(s.ownerLayout(ctx, "work", "Work", ""), p), http.StatusOK, nil
}

// workPage reads the open merge requests and issues into their rows.
func (s *Server) workPage(ctx context.Context) (WorkPage, error) {
	w, err := s.cfg.Inbox.Work(ctx, principal(ctx))
	if err != nil {
		return WorkPage{}, err
	}
	var p WorkPage
	p.MRs, p.Issues = s.workRows(ctx, w)
	return p, nil
}

// workRows are the rows of the open merge requests and issues, the first row
// under the cursor. The checks are read once per (repo, head).
func (s *Server) workRows(ctx context.Context, w ports.InboxWork) (mrs, issues []WorkRow) {
	now := s.cfg.Now()
	row := func(repo domain.RepoName, n int64, href, title string, updated, created time.Time) WorkRow {
		ts := updated
		if ts.IsZero() {
			ts = created
		}
		r := WorkRow{
			Repo: repo.String(), RepoHref: repoHref(repo.String()),
			Number: "#" + strconv.FormatInt(n, 10), Href: href, Title: visibleText(title),
		}
		r.When, r.WhenAt = whenOf(now, ts)
		return r
	}
	sums := make(map[string]ChecksSummary) // one read per (repo, head)
	for _, m := range w.MergeRequests {
		r := row(m.Repo, m.Number, mrHref(m.Repo.String(), m.Number), m.Title.String(), m.UpdatedAt, m.CreatedAt)
		if !m.HeadID.IsZero() {
			head := m.HeadID.String()
			key := m.Repo.String() + "@" + head
			sum, ok := sums[key]
			if !ok {
				sum = checksSummary(m.Repo, head, s.checksFor(ctx, m.Repo, head))
				sums[key] = sum
			}
			r.Checks = sum
		}
		mrs = append(mrs, r)
	}
	for _, i := range w.Issues {
		issues = append(issues, row(i.Repo, i.Number, issueHref(i.Repo.String(), i.Number), i.Title.String(), i.UpdatedAt, i.CreatedAt))
	}
	switch {
	case len(mrs) > 0:
		mrs[0].Cursor = true
	case len(issues) > 0:
		issues[0].Cursor = true
	}
	return mrs, issues
}
