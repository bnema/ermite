// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"github.com/a-h/templ"

	pb "bnema.dev/ermite-api/gen/ermite"
)

// target is what the URL of a tree, blob or raw page designates: a commit,
// the name it was reached by, and a path in its tree.
type target struct {
	// rev is what the API is asked for: the id of the commit of a ref, or
	// the commit id of the URL itself.
	rev string
	// name is how the URL spells the ref: a branch or a tag, or the commit id
	// when detached.
	name string
	path string
	// detached is true when the URL names a commit and not a ref.
	detached bool
}

// locate reads "<ref>[/<path>]" or "<commit>[/<path>]". The ref is the
// longest prefix of the path that is the short name of a branch or a tag
// (names may contain "/"); a branch wins over a tag of the same name. An
// empty rest designates the head of the repository. Whether the commit or
// the path exists is for the API to say.
func (d *repoData) locate(rest string) (target, bool) {
	rest = strings.Trim(rest, "/")
	if rest == "" {
		if d.head == nil {
			return target{}, false
		}
		return target{rev: d.head.GetCommitId(), name: d.head.GetShortName()}, true
	}
	all := append(append([]*pb.Ref(nil), d.branches...), d.tags...)
	if ref, p, ok := resolveRef(all, rest); ok && ref.GetCommitId() != "" {
		return target{rev: ref.GetCommitId(), name: ref.GetShortName(), path: p}, true
	}
	first, p, _ := strings.Cut(rest, "/")
	if looksLikeCommit(first) {
		return target{rev: first, name: first, path: p, detached: true}, true
	}
	return target{}, false
}

// looksLikeCommit reports whether s can be a commit id or an abbreviation of
// one (7 to 40 lowercase hexadecimal digits).
func looksLikeCommit(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// pinRef points the ref switcher of the layout at the ref a page shows.
func pinRef(l *Layout, t target) {
	if t.detached {
		l.Ref, l.RefPath, l.Short, l.SHA = "detached", "", short(t.rev), ""
		if len(t.rev) == 40 {
			l.SHA = t.rev
		}
		return
	}
	l.Ref, l.RefPath, l.SHA, l.Short = t.name, t.name, t.rev, short(t.rev)
	for i := range l.Heads {
		l.Heads[i].Current = l.Heads[i].Name == t.name
	}
	for i := range l.Tags {
		l.Tags[i].Current = l.Tags[i].Name == t.name && !hasName(l.Heads, t.name)
	}
}

func hasName(items []RefItem, name string) bool {
	for _, it := range items {
		if it.Name == name {
			return true
		}
	}
	return false
}

// fileCrumbs is the path in the top bar: repo, kind ("tree" or "blob"), ref,
// then the segments of the path. The directories link to their tree.
func fileCrumbs(repo, kind string, t target) []Crumb {
	root := repoHref(repo, "tree", t.name)
	cs := []Crumb{
		{Label: repo, Href: repoHref(repo)},
		{Label: kind, Href: root},
		{Label: t.name, Href: root, Current: t.path == ""},
	}
	if t.path == "" {
		return cs
	}
	segs := strings.Split(t.path, "/")
	for i, seg := range segs {
		last := i == len(segs)-1
		c := Crumb{Label: markedText(seg), Current: last}
		if !last || kind == "tree" {
			c.Href = repoHref(repo, "tree", t.name, strings.Join(segs[:i+1], "/"))
		} else {
			c.Href = repoHref(repo, "blob", t.name, t.path)
		}
		cs = append(cs, c)
	}
	return cs
}

// maxFileTargets bounds the files of a directory offered in the ":" list.
const maxFileTargets = 200

// TreeRow is one entry of a directory.
type TreeRow struct {
	Glyph   string // ▾ directory, @ symbolic link, + submodule, · file
	Name    string
	Href    string // "" for a submodule
	Size    string
	Subject string
	CHref   string
	Author  string
	When    string
	WhenAt  string
	Cursor  bool
}

// TreePage is a directory.
type TreePage struct {
	Rows   []TreeRow
	Readme *Readme
	// Notes say what the listing leaves out.
	Notes []string
}

// tree lists a directory of a ref, with the last commit of each entry, and
// renders the README of the directory below.
func (s *Server) tree(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	t, ok := d.locate(r.PathValue("rest"))
	if !ok {
		return nil, 0, errNotFound
	}
	res, err := s.cfg.Git.GetTree(ctx, connect.NewRequest(&pb.GetTreeRequest{
		Repo: d.name(), Revision: t.rev, Path: t.path, IncludeLastCommit: true,
	}))
	if err != nil {
		return nil, 0, err
	}
	entries := res.Msg.GetEntries()
	now := s.cfg.Now()

	p := TreePage{}
	l := s.repoLayout(ctx, d, "tree")
	for i, e := range entries {
		row := TreeRow{Glyph: "·", Name: markedText(e.GetName()), Size: humanSize(e.GetSize()), Cursor: i == 0}
		switch e.GetKind() {
		case pb.EntryKind_ENTRY_KIND_DIRECTORY:
			row.Glyph, row.Name, row.Size = "▾", row.Name+"/", "-"
			row.Href = repoHref(d.name(), "tree", t.name, e.GetPath())
		case pb.EntryKind_ENTRY_KIND_SYMLINK:
			row.Glyph = "@"
			row.Href = repoHref(d.name(), "blob", t.name, e.GetPath())
		case pb.EntryKind_ENTRY_KIND_SUBMODULE:
			row.Glyph, row.Size = "+", "-"
		default:
			row.Href = repoHref(d.name(), "blob", t.name, e.GetPath())
			if len(l.Cmd) < maxFileTargets {
				l.Cmd = append(l.Cmd, CmdItem{Kind: "file", Label: markedText(e.GetPath()), Href: row.Href})
			}
		}
		if c := e.GetLastCommit(); c != nil {
			row.Subject, row.Author = markedText(c.GetSummary()), c.GetAuthor().GetName()
			row.CHref = repoHref(d.name(), "commit", c.GetId())
			if ts := c.GetCommitter().GetTime(); ts.IsValid() {
				row.When, row.WhenAt = ago(now, ts.AsTime()), rfc3339(ts.AsTime())
			}
		}
		p.Rows = append(p.Rows, row)
	}
	if res.Msg.GetTruncated() {
		p.Notes = append(p.Notes, "this directory has more entries than are listed here: the first "+itoa(len(entries))+" are shown.")
	}
	if res.Msg.GetLastCommitsTruncated() {
		p.Notes = append(p.Notes, "the last commit is shown for the first entries only.")
	}
	rd, err := s.readmeIn(ctx, d.name(), res.Msg.GetCommitId(), t.name, t.path, entries)
	if err != nil {
		return nil, 0, err
	}
	if rd != nil {
		p.Readme = rd.readme
	}

	pinRef(&l, t)
	l.Crumbs = fileCrumbs(d.name(), "tree", t)
	if t.path == "" {
		l.Title = "tree · " + d.name() + " · " + s.instance
	} else {
		l.Title = markedText(t.path) + " · " + d.name() + " · " + s.instance
	}
	l.Status = Status{
		Mode: "tree", Context: "tree · " + plural(len(entries), "entry", "entries"),
		Hints: "j/k move · enter open · / filter · b ref · ? help", Cursor: true, Count: len(p.Rows),
	}
	return Tree(l, p), http.StatusOK, nil
}
