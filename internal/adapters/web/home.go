// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/a-h/templ"

	pb "bnema.dev/ermite-api/gen/ermite"
)

// maxRepos bounds the repositories one page lists.
const maxRepos = 1000

// pageSize is the page size asked from the list RPCs: the largest the API
// accepts.
const pageSize = 100

// listRepos reads the repositories the visitor may see, ordered by name.
func (s *Server) listRepos(ctx context.Context) ([]*pb.Repo, error) {
	var repos []*pb.Repo
	token := ""
	for len(repos) < maxRepos {
		res, err := s.cfg.Repos.ListRepos(ctx, connect.NewRequest(&pb.ListReposRequest{PageSize: pageSize, PageToken: token}))
		if err != nil {
			return nil, err
		}
		repos = append(repos, res.Msg.GetRepos()...)
		if token = res.Msg.GetNextPageToken(); token == "" {
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
func sortRepos(repos []*pb.Repo, key string, desc bool) {
	less := func(a, b *pb.Repo) bool {
		switch key {
		case "desc":
			return strings.ToLower(a.GetDescription()) < strings.ToLower(b.GetDescription())
		case "branch":
			return a.GetDefaultBranch() < b.GetDefaultBranch()
		case "push":
			return a.GetPushedAt().AsTime().Before(b.GetPushedAt().AsTime())
		}
		return a.GetName() < b.GetName()
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
func homeRow(r *pb.Repo, now time.Time, cursor bool) HomeRow {
	row := HomeRow{
		ID: "repo-" + r.GetName(), Name: r.GetName(), Href: repoHref(r.GetName()),
		Desc: r.GetDescription(), Pushed: "never", Branch: r.GetDefaultBranch(), Cursor: cursor,
	}
	if r.GetPushedAt().IsValid() && r.GetPushedAt().GetSeconds() != 0 {
		t := r.GetPushedAt().AsTime()
		row.Pushed, row.PushedAt = ago(now, t), rfc3339(t)
	}
	return row
}

// repoCmds are the repositories as targets of the ":" command line, but the
// one being viewed.
func repoCmds(repos []*pb.Repo, except string) []CmdItem {
	var out []CmdItem
	for _, r := range repos {
		if r.GetName() != except {
			out = append(out, CmdItem{Kind: "repo", Label: r.GetName(), Href: repoHref(r.GetName())})
		}
	}
	return out
}
