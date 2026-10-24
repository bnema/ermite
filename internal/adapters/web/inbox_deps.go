// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"bnema.dev/ermite/internal/domain"
)

// InboxDepsUpdate is one line of a dependency card.
type InboxDepsUpdate struct {
	Ecosystem, Directory, Name, Current, Latest, Kind string
	New                                               bool
}

// InboxDepsRow is Bernard's card of one repository.
type InboxDepsRow struct {
	Repo, RepoHref string
	// State is "APPLYING" or "FAILED", empty when idle.
	State, Error string
	Updates      []InboxDepsUpdate
	// Open, Ignore and Snooze are the POST targets; empty without buttons.
	Open, Ignore, Snooze string
	// MR is the open Bernard merge request, 0 = none; the Open button then
	// reads "Update MR". MRHref links to it.
	MR     int64
	MRHref string
	// UpToDate is set when the open merge request already holds exactly the
	// pending versions.
	UpToDate bool
}

type depsOp int

const (
	depsOpen depsOp = iota
	depsIgnore
	depsSnooze
)

func depsKindLabel(k domain.DepKind) string {
	switch k {
	case domain.DepPatch:
		return "patch"
	case domain.DepMinor:
		return "minor"
	case domain.DepMajor:
		return "major"
	}
	return "update"
}

func (s *Server) depsRow(d domain.RepoDeps, filter domain.RepoName) InboxDepsRow {
	repo := d.Repo.String()
	row := InboxDepsRow{Repo: repo, RepoHref: repoHref(repo), MR: d.MR}
	if d.MR != 0 {
		row.MRHref = mrHref(repo, d.MR)
		row.UpToDate = domain.SameRefs(d.Refs(), d.MRVersions)
	}
	switch d.State {
	case domain.DepsApplying:
		row.State = "APPLYING"
	case domain.DepsFailed:
		row.State, row.Error = "FAILED", d.Error
	}
	for _, u := range d.Updates {
		row.Updates = append(row.Updates, InboxDepsUpdate{
			Ecosystem: string(u.Ecosystem), Directory: u.Directory, Name: u.Name,
			Current: u.Current, Latest: u.Version, Kind: depsKindLabel(u.Kind), New: u.New,
		})
	}
	if s.cfg.Deps != nil {
		base := inboxPath + "/deps/" + url.PathEscape(repo) + "/"
		var q string
		if !filter.IsZero() {
			q = "?repo=" + url.QueryEscape(filter.String())
		}
		row.Open, row.Ignore, row.Snooze = base+"open"+q, base+"ignore"+q, base+"snooze"+q
	}
	return row
}

// depsRefs reads the expected versions of the form: four parallel fields.
func depsRefs(r *http.Request) ([]domain.DepRef, error) {
	eco, dir, name, ver := r.PostForm["ecosystem"], r.PostForm["directory"], r.PostForm["name"], r.PostForm["version"]
	if len(eco) != len(dir) || len(eco) != len(name) || len(eco) != len(ver) {
		return nil, fmt.Errorf("%w: malformed dependency versions", domain.ErrInvalid)
	}
	refs := make([]domain.DepRef, len(eco))
	for i := range eco {
		refs[i] = domain.DepRef{Ecosystem: domain.DepsEcosystem(strings.TrimSpace(eco[i])), Directory: dir[i], Name: name[i], Version: ver[i]}
	}
	return refs, nil
}

// depsAction is POST /-/inbox/deps/<repo>/open, ignore or snooze.
func (s *Server) depsAction(op depsOp) func(ctx context.Context, r *http.Request) (string, string, error) {
	return func(ctx context.Context, r *http.Request) (string, string, error) {
		back := pendingBack(r)
		if !SignedIn(ctx) {
			return "", back, errNotFound
		}
		name, err := domain.ParseRepoName(r.PathValue("repo"))
		if err != nil {
			return "", back, err
		}
		if op == depsSnooze {
			_, err = s.cfg.Deps.Snooze(ctx, principal(ctx), name)
			return back, back, err
		}
		refs, err := depsRefs(r)
		if err != nil {
			return "", back, err
		}
		if op == depsOpen {
			_, err = s.cfg.Deps.Open(ctx, principal(ctx), name, refs)
		} else {
			_, err = s.cfg.Deps.Ignore(ctx, principal(ctx), name, refs)
		}
		return back, back, err
	}
}
