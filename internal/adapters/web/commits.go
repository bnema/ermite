// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"strings"

	"connectrpc.com/connect"
	"github.com/a-h/templ"

	pb "bnema.dev/ermite-api/gen/ermite"
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
}

// revSel is what the URL of a history page selects.
type revSel struct {
	rev  string // what the API is asked for: a commit id
	name string // what the page shows
	path string // the path limit, "" for none
	// detached is true when a commit id, not a ref, was named.
	detached bool
}

// isCommitID reports whether s can be a commit id or an abbreviation of one:
// 7 to 40 lowercase hexadecimal digits.
func isCommitID(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, c := range []byte(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// selectRev reads "<ref>[/<path>]" or "<commit>[/<path>]". The ref is the
// longest prefix that names a branch or a tag; an empty rest selects the head.
func (d *repoData) selectRev(rest string) (revSel, bool) {
	rest = strings.Trim(rest, "/")
	if rest == "" {
		if d.head == nil {
			return revSel{}, false
		}
		return revSel{rev: d.head.GetCommitId(), name: d.head.GetShortName()}, true
	}
	all := append(append([]*pb.Ref(nil), d.branches...), d.tags...)
	if ref, p, ok := resolveRef(all, rest); ok && ref.GetCommitId() != "" {
		return revSel{rev: ref.GetCommitId(), name: ref.GetShortName(), path: p}, true
	}
	first, p, _ := strings.Cut(rest, "/")
	if isCommitID(first) {
		return revSel{rev: first, name: first, path: p, detached: true}, true
	}
	return revSel{}, false
}

// commits lists the history of a ref, newest first, a page at a time, with
// an optional path to limit it to. The page token of the API travels in the
// "page" query parameter.
func (s *Server) commits(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	sel, ok := d.selectRev(r.PathValue("rest"))
	if !ok {
		return nil, 0, errNotFound
	}
	token := r.URL.Query().Get("page")
	res, err := s.cfg.Git.ListCommits(ctx, connect.NewRequest(&pb.ListCommitsRequest{
		Repo: d.name(), Revision: sel.rev, Path: sel.path, PageSize: commitsPerPage, PageToken: token,
	}))
	if err != nil {
		return nil, 0, err
	}
	cs := res.Msg.GetCommits()
	if len(cs) == 0 && token == "" && sel.path != "" {
		// Git answers an empty history for a path that never existed.
		return nil, 0, errNotFound
	}

	p := CommitsPage{Ref: sel.name, Path: visibleText(sel.path), Count: len(cs)}
	base := repoHref(d.name(), "commits", sel.name, sel.path)
	if t := res.Msg.GetNextPageToken(); t != "" {
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
	s.setRef(&l, sel)
	l.Crumbs = commitsCrumbs(d, sel)
	if sel.path == "" {
		l.Title = "commits · " + d.name() + " · " + s.instance
	} else {
		l.Title = "history of " + visibleText(sel.path) + " · " + d.name() + " · " + s.instance
	}
	what := plural(len(cs), "commit", "commits")
	if p.Older != "" {
		what += " · more"
	}
	if p.Path != "" {
		what = "history of " + p.Path + " · " + what
	}
	l.Status = Status{
		Mode: "commits", Context: sel.name + " · " + what,
		Hints: "j/k move · enter open · / filter · ? help", Cursor: true, Count: len(cs),
	}
	l.HasRail, l.RailSum = true, "history · refs on this page"
	return Commits(l, p), http.StatusOK, nil
}

// setRef points the ref switcher of the layout at what the page shows.
func (s *Server) setRef(l *Layout, sel revSel) {
	if sel.detached {
		l.Ref, l.RefPath, l.Short, l.SHA = "detached", "", short(sel.rev), ""
		if len(sel.rev) == 40 {
			l.SHA = sel.rev
		}
		return
	}
	l.Ref, l.RefPath, l.SHA, l.Short = sel.name, sel.name, sel.rev, short(sel.rev)
}

// commitsCrumbs is the path in the top bar: repo, commits, ref, then the
// directories of the path, each of which links to its tree.
func commitsCrumbs(d *repoData, sel revSel) []Crumb {
	home := repoHref(d.name(), "commits", sel.name)
	cs := []Crumb{
		{Label: d.name(), Href: repoHref(d.name())},
		{Label: "commits", Href: home},
		{Label: sel.name, Href: home, Current: sel.path == ""},
	}
	if sel.path == "" {
		return cs
	}
	segs := strings.Split(sel.path, "/")
	for i, seg := range segs {
		last := i == len(segs)-1
		c := Crumb{Label: visibleText(seg), Current: last}
		if !last {
			c.Href = repoHref(d.name(), "tree", sel.name, strings.Join(segs[:i+1], "/"))
		}
		cs = append(cs, c)
	}
	return cs
}

// fillDays groups the commits of a page by day and lists the refs among them.
func (s *Server) fillDays(p *CommitsPage, d *repoData, cs []*pb.Commit) {
	now := s.cfg.Now()
	branchesAt, tagsAt := namesAt(d.branches), namesAt(d.tags)
	for i, c := range cs {
		row := CommitRow{
			ID: "c-" + short(c.GetId()), Short: short(c.GetId()), Subject: visibleText(c.GetSummary()),
			CHref:  repoHref(d.name(), "commit", c.GetId()),
			Author: visibleText(c.GetAuthor().GetName()), Merge: len(c.GetParentIds()) > 1,
			Branches: branchesAt[c.GetId()], Tags: tagsAt[c.GetId()], Cursor: i == 0,
		}
		day := "unknown date"
		if t := c.GetCommitter().GetTime(); t.IsValid() {
			row.When, row.WhenAt = ago(now, t.AsTime()), rfc3339(t.AsTime())
			day = t.AsTime().UTC().Format("2006-01-02")
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
func (s *Server) pathLink(ctx context.Context, d *repoData, sel revSel) *Link {
	dir := path.Dir(sel.path)
	if dir == "." {
		dir = ""
	}
	tree, err := s.cfg.Git.GetTree(ctx, connect.NewRequest(&pb.GetTreeRequest{Repo: d.name(), Revision: sel.rev, Path: dir}))
	if err != nil {
		return nil
	}
	for _, e := range tree.Msg.GetEntries() {
		if e.GetPath() != sel.path {
			continue
		}
		if e.GetKind() == pb.EntryKind_ENTRY_KIND_DIRECTORY {
			return &Link{Label: "open", Href: repoHref(d.name(), "tree", sel.name, sel.path)}
		}
		return &Link{Label: "open", Href: repoHref(d.name(), "blob", sel.name, sel.path)}
	}
	return nil
}
