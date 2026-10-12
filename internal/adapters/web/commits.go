// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"net/url"
	"path"

	"github.com/a-h/templ"

	"bnema.dev/ermite/internal/domain"
)

// commitsPerPage is how many commits one page of history lists.
const commitsPerPage = 50

// DayGroup is the commits of one day (UTC, by committer date).
type DayGroup struct {
	Day  string
	Rows []CommitRow
}

// CommitsPage is the history of a ref, or of a path at a ref.
type CommitsPage struct {
	Ref string
	// Path is the path the history is limited to, ready to display; "" for
	// the whole history.
	Path  string
	Days  []DayGroup
	Count int
	// Older is the link to the next page, "" on the last page. Newest is the
	// link back to the first page, "" on the first page: page tokens only go
	// forward.
	Older, Newest string
	// Open links to the file or directory at this ref, when it exists there.
	Open *Link
	// Refs are the branch heads and tags among the commits of this page.
	Refs []Link
	// Live is true when the page announces new commits of its branch.
	Live bool
}

// commits lists the history of a ref, newest first, a page at a time, with
// an optional path to limit it to. The page token of the use case travels in the
// "page" query parameter.
func (s *Server) commits(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	sel, ok := d.locate(r.PathValue("rest"))
	if !ok {
		return nil, 0, errNotFound
	}
	token := r.URL.Query().Get("page")
	res, err := s.commitList(ctx, d.repo.Name, sel.rev, sel.path, commitsPerPage, token)
	if err != nil {
		return nil, 0, err
	}
	cs := res.Commits
	if len(cs) == 0 && token == "" && sel.path != "" {
		// Git answers an empty history for a path that never existed.
		return nil, 0, errNotFound
	}

	p := CommitsPage{Ref: sel.name, Path: visibleText(sel.path), Count: len(cs)}
	base := repoHref(d.name(), "commits", sel.name, sel.path)
	if t := res.NextPageToken; t != "" {
		p.Older = base + "?page=" + url.QueryEscape(t)
	}
	if token != "" {
		p.Newest = base
	}
	s.fillDays(&p, d, cs)
	if sel.path != "" && token == "" {
		p.Open = s.pathLink(ctx, d, sel)
	}

	l := s.repoLayout(ctx, d, "commits")
	pinRef(&l, sel)
	if d.isBranchHead(sel) && token == "" && sel.path == "" && len(cs) > 0 {
		// The first page of a branch announces the commits pushed to it.
		p.Live = true
		l.Live, l.LiveURL = true, liveURL("repo:"+d.name(), "view=commits&ref="+url.QueryEscape(sel.name)+"&head="+sel.rev)
	}
	l.Crumbs = pathCrumbs(d.name(), "commits", sel)
	if sel.path == "" {
		l.Title = "commits · " + d.name() + " · " + s.instance
	} else {
		l.Title = "history of " + visibleText(sel.path) + " · " + d.name() + " · " + s.instance
	}
	l.HasRail = true
	return Commits(l, p), http.StatusOK, nil
}

// fillDays groups the commits of a page by day and lists the refs among them.
func (s *Server) fillDays(p *CommitsPage, d *repoData, cs []domain.Commit) {
	now := s.cfg.Now()
	branchesAt, tagsAt := namesAt(d.branches), namesAt(d.tags)
	for i, c := range cs {
		row := commitRow(d.name(), c, now)
		row.ID, row.Cursor = "c-"+row.Short, i == 0
		row.Branches, row.Tags = branchesAt[c.ID.String()], tagsAt[c.ID.String()]
		day := "unknown date"
		if t := c.Committer.Time; !t.IsZero() {
			day = t.UTC().Format("2006-01-02")
		}
		if n := len(p.Days); n == 0 || p.Days[n-1].Day != day {
			p.Days = append(p.Days, DayGroup{Day: day})
		}
		g := &p.Days[len(p.Days)-1]
		g.Rows = append(g.Rows, row)
		for _, t := range row.Tags {
			p.Refs = append(p.Refs, Link{Label: "tag " + t, Href: "#" + row.ID})
		}
		for _, b := range row.Branches {
			p.Refs = append(p.Refs, Link{Label: "branch " + b, Href: "#" + row.ID})
		}
	}
}

// pathLink finds what the path of a history is at the ref: a directory
// (tree) or a file (blob). It is nil when the path is not there, a deleted
// file for one, or when the listing fails: the link is a convenience.
func (s *Server) pathLink(ctx context.Context, d *repoData, sel target) *Link {
	dir := path.Dir(sel.path)
	if dir == "." {
		dir = ""
	}
	tree, err := s.getTree(ctx, d.repo.Name, sel.rev, dir, false)
	if err != nil {
		return nil
	}
	for _, e := range tree.Entries {
		if e.Path != sel.path {
			continue
		}
		if e.Kind == domain.EntryDirectory {
			return &Link{Label: "open", Href: repoHref(d.name(), "tree", sel.name, sel.path)}
		}
		return &Link{Label: "open", Href: repoHref(d.name(), "blob", sel.name, sel.path)}
	}
	return nil
}
