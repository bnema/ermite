// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"
	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// maxRepos bounds the repositories one page lists.
const maxRepos = 1000

// pageSize is the page size asked from the commit lists: the largest the use
// case accepts for them.
const pageSize = 100

// listRepos reads the repositories the visitor may see, ordered by name.
// Every page of ListRepos scans the whole repository list and reads the HEAD
// of each repository of the page, so the list is asked in one page of up to
// maxRepos: one scan in the common case, whatever the cap of the use case is
// (a lower one only means more pages, followed by their token).
func (s *Server) listRepos(ctx context.Context) ([]ports.RepoView, error) {
	var repos []ports.RepoView
	token := ""
	for len(repos) < maxRepos {
		page, err := s.cfg.Repos.ListPage(ctx, principal(ctx), maxRepos, token)
		if err != nil {
			return nil, err
		}
		for _, repo := range page.Repos {
			view, err := s.cfg.Repos.View(ctx, repo)
			if err != nil {
				return nil, err
			}
			repos = append(repos, view)
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	return repos, nil
}

// homeSorts are the orders the home list offers, in order. The query also
// accepts "desc" and "branch", which the page does not link to.
var homeSorts = []struct{ key, label string }{
	{"push", "last push"},
	{"name", "name"},
}

// homeKeys are the keys of ?sort= that sortRepos knows.
var homeKeys = []string{"push", "name", "desc", "branch"}

// sortRepos orders repos by the column key; ties keep the order by name.
func sortRepos(repos []ports.RepoView, key string, desc bool) {
	less := func(a, b ports.RepoView) bool {
		switch key {
		case "desc":
			return strings.ToLower(a.Description) < strings.ToLower(b.Description)
		case "branch":
			return a.DefaultBranch < b.DefaultBranch
		case "push":
			return a.PushedAt.Before(b.PushedAt)
		}
		return a.Name.String() < b.Name.String()
	}
	sort.SliceStable(repos, func(i, j int) bool {
		if desc {
			return less(repos[j], repos[i])
		}
		return less(repos[i], repos[j])
	})
}

// home lists the repositories: /?sort=<col>&dir=asc|desc, newest push first
// by default.
func (s *Server) home(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	repos, err := s.listRepos(ctx)
	if err != nil {
		return nil, 0, err
	}
	key, desc := "push", true
	for _, k := range homeKeys {
		if r.URL.Query().Get("sort") == k {
			key, desc = k, k == "push"
		}
	}
	if d := r.URL.Query().Get("dir"); d == "asc" || d == "desc" {
		desc = d == "desc"
	}
	sortRepos(repos, key, desc)

	now := s.cfg.Now()
	p := HomePage{}
	for _, c := range homeSorts {
		sl := SortLink{Label: c.label}
		next := "asc"
		if c.key == "push" {
			next = "desc"
		}
		if c.key == key {
			sl.Mark, sl.State = "▲", "ascending, select for descending"
			next = "desc"
			if desc {
				sl.Mark, sl.State, next = "▼", "descending, select for ascending", "asc"
			}
		}
		sl.Href = "/?sort=" + c.key + "&dir=" + next
		p.Sorts = append(p.Sorts, sl)
	}
	p.Rows = s.homeRows(ctx, repos, now)

	l := s.base()
	l.Title = s.instance
	l.Live, l.LiveURL = true, liveURL("home", "")
	l.Cmd = append([]CmdItem{{Kind: "go", Label: "home", Href: "/"}}, repoCmds(repos, "")...)
	return Home(l, p), http.StatusOK, nil
}

// homeRecent is how many commits a row of the home list shows.
const homeRecent = 5

// homeWorkers bounds the repositories whose commits and run are read at once.
const homeWorkers = 8

// homeRows builds the rows of repos, in order: each repository costs a read
// of its commits and of its latest run, so they are read a few at a time. A
// repository whose reads fail still has its row.
func (s *Server) homeRows(ctx context.Context, repos []ports.RepoView, now time.Time) []HomeRow {
	rows := make([]HomeRow, len(repos))
	var (
		sem = make(chan struct{}, homeWorkers)
		wg  sync.WaitGroup
	)
	for i, repo := range repos {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			rows[i] = s.homeRowOf(ctx, repo, now, i == 0)
		}()
	}
	wg.Wait()
	return rows
}

// homeRowOf is the row of a repository with what the visitor may read of it:
// the newest commits of its default branch and the state of its latest run.
func (s *Server) homeRowOf(ctx context.Context, r ports.RepoView, now time.Time, cursor bool) HomeRow {
	row := homeRow(r, now, cursor, SignedIn(ctx))
	if r.PushedAt.IsZero() || r.DefaultBranch == "" {
		row.NoCommits = r.PushedAt.IsZero()
		return row
	}
	var head string
	row.Commits, head, row.NoCommits = s.homeCommits(ctx, r, now)
	if head != "" {
		row.CI = s.homeCI(ctx, r, head)
		row.Stats = s.homeStats(ctx, r, head, now)
	}
	return row
}

// homeCommits reads the newest commits of the default branch. It reports an
// empty branch; a failed read is logged and gives neither commits nor that
// note, since the row does not depend on them.
func (s *Server) homeCommits(ctx context.Context, r ports.RepoView, now time.Time) (rows []CommitRow, head string, none bool) {
	// By its full name, so that a tag of the same name is not read.
	page, err := s.commitList(ctx, r.Name, "refs/heads/"+r.DefaultBranch, "", homeRecent, "")
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return nil, "", true
	case err != nil:
		log := zerowrap.FromCtx(ctx)
		log.Warn().Err(err).Str("repo", r.Name.String()).Msg("read the recent commits of the home page")
		return nil, "", false
	}
	for _, c := range page.Commits[:min(len(page.Commits), homeRecent)] {
		rows = append(rows, commitRow(r.Name.String(), c, now))
	}
	if len(page.Commits) > 0 {
		head = page.Commits[0].ID.String()
	}
	return rows, head, len(rows) == 0
}

// homeStats are the figures of a row; nil when the read fails (logged), since
// the row does not depend on them.
func (s *Server) homeStats(ctx context.Context, r ports.RepoView, head string, now time.Time) *HomeStats {
	id, err := domain.ParseObjectID(head)
	if err != nil {
		return nil
	}
	st, err := s.cfg.Browse.RepoStats(ctx, principal(ctx), r.Name, id, now)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			log := zerowrap.FromCtx(ctx)
			log.Warn().Err(err).Str("repo", r.Name.String()).Msg("read the figures of the home page")
		}
		return nil
	}
	hs := &HomeStats{Commits: grouped(st.Commits), Recent: grouped(st.Recent)}
	if t := st.LatestTag; t != nil {
		hs.Tag, hs.TagWhen = t.Name, ago(now, t.At)
		if !t.CommitID.IsZero() {
			// By its commit: a branch of the same name would win over the
			// tag in a tree path.
			hs.TagHref = repoHref(r.Name.String(), "tree", t.CommitID.String())
		}
	}
	return hs
}

// homeCI is the state of the CI on the head of the default branch, as the
// caller of the request sees it: the combined checks of that commit, read by
// its id so the cost does not grow with the CI history. nil when the server
// runs no CI, the caller cannot read it, the commit has no check, or the read
// fails (checksFor logs it).
func (s *Server) homeCI(ctx context.Context, r ports.RepoView, head string) *HomeCI {
	checks := s.checksFor(ctx, r.Name, head)
	if len(checks) == 0 {
		return nil
	}
	sum := checksSummary(r.Name, head, checks)
	ci := &HomeCI{Kind: sum.Kind, Label: ciLabel(sum.Kind), Title: sum.Title, Href: repoHref(r.Name.String(), "commit", head)}
	if run := checks[0].RunID; !slices.ContainsFunc(checks, func(c ports.CheckView) bool { return c.RunID != run }) {
		ci.Href = runHref(r.Name.String(), run) // one run: straight to it
	}
	return ci
}

// ciLabel is the word next to the mark of the CI state on the home page.
func ciLabel(kind string) string {
	switch kind {
	case "ok":
		return "passing"
	case "bad":
		return "failing"
	}
	return "running"
}

// homeRow is the row of a repository in the home list, without its commits
// and run. The private badge is for a signed-in visitor: an anonymous one
// never gets a private repository.
func homeRow(r ports.RepoView, now time.Time, cursor, signed bool) HomeRow {
	name := r.Name.String()
	row := HomeRow{
		ID: "repo-" + name, Name: name, Href: repoHref(name),
		Desc: r.Description, Cursor: cursor,
		Private: signed && r.Visibility == domain.VisibilityPrivate,
	}
	if !r.PushedAt.IsZero() {
		row.Active, row.ActiveAt = ago(now, r.PushedAt), rfc3339(r.PushedAt)
	}
	return row
}

// repoCmds are the repositories as targets of the ":" command line, but the
// one being viewed.
func repoCmds(repos []ports.RepoView, except string) []CmdItem {
	var out []CmdItem
	for _, r := range repos {
		if name := r.Name.String(); name != except {
			out = append(out, CmdItem{Kind: "repo", Label: name, Href: repoHref(name)})
		}
	}
	return out
}
