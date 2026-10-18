// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/a-h/templ"

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
	signed := SignedIn(ctx)
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
	for i, repo := range repos {
		p.Rows = append(p.Rows, homeRow(repo, now, i == 0, signed))
	}

	l := s.base()
	l.Title = s.instance
	l.Live, l.LiveURL = true, liveURL("home", "")
	l.Cmd = append([]CmdItem{{Kind: "go", Label: "home", Href: "/"}}, repoCmds(repos, "")...)
	return Home(l, p), http.StatusOK, nil
}

// homeRow is the card of a repository in the home list. The private badge is
// for a signed-in visitor: an anonymous one never gets a private repository.
func homeRow(r ports.RepoView, now time.Time, cursor, signed bool) HomeRow {
	name := r.Name.String()
	row := HomeRow{
		ID: "repo-" + name, Name: name, Href: repoHref(name),
		Desc: r.Description, Branch: r.DefaultBranch, Cursor: cursor,
		Private: signed && r.Visibility == domain.VisibilityPrivate,
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
