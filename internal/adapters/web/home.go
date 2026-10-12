// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/a-h/templ"

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

// homeCols are the sortable columns of the home list, in order.
var homeCols = []struct{ key, label, class string }{
	{"name", "name", "c-name"},
	{"desc", "description", "c-desc"},
	{"push", "last push", "c-when"},
	{"branch", "branch", "c-ref"},
}

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
	for _, c := range homeCols {
		if r.URL.Query().Get("sort") == c.key {
			key, desc = c.key, c.key == "push"
		}
	}
	if d := r.URL.Query().Get("dir"); d == "asc" || d == "desc" {
		desc = d == "desc"
	}
	sortRepos(repos, key, desc)

	now := s.cfg.Now()
	p := HomePage{}
	for _, c := range homeCols {
		sc := SortCol{Label: c.label, Class: c.class, Dir: "none"}
		next := "asc"
		if c.key == key {
			sc.Dir, sc.Mark = "ascending", "▲"
			if desc {
				sc.Dir, sc.Mark = "descending", "▼"
			} else {
				next = "desc"
			}
		}
		sc.Href = "/?sort=" + c.key + "&dir=" + next
		p.Cols = append(p.Cols, sc)
	}
	for i, repo := range repos {
		p.Rows = append(p.Rows, homeRow(repo, now, i == 0))
	}

	l := s.base()
	l.Title = s.instance
	l.Live, l.LiveURL = true, liveURL("home", "")
	l.Cmd = append([]CmdItem{{Kind: "go", Label: "home", Href: "/"}}, repoCmds(repos, "")...)
	return Home(l, p), http.StatusOK, nil
}

// homeRow is the row of a repository in the home list.
func homeRow(r ports.RepoView, now time.Time, cursor bool) HomeRow {
	name := r.Name.String()
	row := HomeRow{
		ID: "repo-" + name, Name: name, Href: repoHref(name),
		Desc: r.Description, Pushed: "never", Branch: r.DefaultBranch, Cursor: cursor,
	}
	if !r.PushedAt.IsZero() {
		row.Pushed, row.PushedAt = ago(now, r.PushedAt), rfc3339(r.PushedAt)
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
