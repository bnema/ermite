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

// workPath is the owner's open merge requests and issues.
const workPath = "/-/work"

// work is the page of the owner's open merge requests and issues.
func (s *Server) work(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	if !SignedIn(ctx) {
		return nil, 0, errNotFound
	}
	w, err := s.cfg.Inbox.Work(ctx, principal(ctx))
	if err != nil {
		return nil, 0, err
	}
	var p InboxPage
	p.MRs, p.IssueRows = s.workRows(ctx, w)
	l := s.base()
	l.Title = "Work · " + s.instance
	l.OwnerNav = s.ownerNav(ctx, "work")
	l.Cmd = []CmdItem{{Kind: "go", Label: "home", Href: "/"}}
	// No live updates yet: the owner stream accepts view=work later.
	return Work(l, p), http.StatusOK, nil
}

// workRows are the rows of the open merge requests and issues, the first row
// under the cursor. The checks are read once per (repo, head).
func (s *Server) workRows(ctx context.Context, w ports.InboxWork) (mrs, issues []InboxRow) {
	now := s.cfg.Now()
	row := func(repo domain.RepoName, n int64, href, title string, updated, created time.Time) InboxRow {
		ts := updated
		if ts.IsZero() {
			ts = created
		}
		r := InboxRow{
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
