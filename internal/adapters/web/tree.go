// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"

	"github.com/a-h/templ"

	"bnema.dev/ermite/internal/domain"
)

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
//
//hexcheck:ignore suspicious-business-logic-in-adapter builds the directory page from the use case results; no rule is decided here
func (s *Server) tree(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	t, ok := d.locate(r.PathValue("rest"))
	if !ok {
		return nil, 0, errNotFound
	}
	res, err := s.getTree(ctx, d.repo.Name, t.rev, t.path, true)
	if err != nil {
		return nil, 0, err
	}
	entries := res.Entries
	now := s.cfg.Now()

	p := TreePage{}
	l := s.repoLayout(ctx, d, "tree")
	for i, e := range entries {
		row := TreeRow{Glyph: "·", Name: visibleText(e.Name), Size: humanSize(e.Size), Cursor: i == 0}
		switch e.Kind {
		case domain.EntryDirectory:
			row.Glyph, row.Name, row.Size = "▾", row.Name+"/", "-"
			row.Href = repoHref(d.name(), "tree", t.name, e.Path)
		case domain.EntrySymlink:
			row.Glyph = "@"
			row.Href = repoHref(d.name(), "blob", t.name, e.Path)
		case domain.EntrySubmodule:
			row.Glyph, row.Size = "+", "-"
		default:
			row.Href = repoHref(d.name(), "blob", t.name, e.Path)
			if len(l.Cmd) < maxFileTargets {
				l.Cmd = append(l.Cmd, CmdItem{Kind: "file", Label: visibleText(e.Path), Href: row.Href})
			}
		}
		if c := e.LastCommit; c != nil {
			last := commitRow(d.name(), *c, now)
			row.Subject, row.Author, row.CHref, row.When, row.WhenAt = last.Subject, last.Author, last.CHref, last.When, last.WhenAt
		}
		p.Rows = append(p.Rows, row)
	}
	if res.Truncated {
		p.Notes = append(p.Notes, "this directory has more entries than are listed here: the first "+itoa(len(entries))+" are shown.")
	}
	if res.LastCommitsOmitted {
		p.Notes = append(p.Notes, "the last commit is shown for the first entries only.")
	}
	rd, err := s.readmeIn(ctx, d.repo.Name, res.CommitID.String(), t.name, t.path, entries)
	if err != nil {
		return nil, 0, err
	}
	if rd != nil {
		p.Readme = rd.readme
	}

	pinRef(&l, t)
	l.Crumbs = pathCrumbs(d.name(), "tree", t)
	if t.path == "" {
		l.Title = "tree · " + d.name() + " · " + s.instance
	} else {
		l.Title = visibleText(t.path) + " · " + d.name() + " · " + s.instance
	}
	return Tree(l, p), http.StatusOK, nil
}
