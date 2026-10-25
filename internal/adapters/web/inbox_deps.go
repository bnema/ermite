// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// InboxDepsUpdate is one line of the sub-table of a repository. Ecosystem only
// feeds the hidden fields of the forms: the page never shows it.
type InboxDepsUpdate struct {
	Ecosystem, Directory, Name, Current, Latest, Kind string
	New                                               bool
}

// InboxDepsStatus is the status cell of a row.
type InboxDepsStatus struct {
	// Kind is "" (nothing to say), "queued", "running", "failed", "mr" (a merge
	// request is open) or "uptodate" (it holds exactly the pending versions).
	Kind string
	// State is the word shown beside the icon, empty for a merge request.
	State string
	// Text and Href are the link to the run ("run #12"); without Href the text
	// is plain (the run is gone).
	Text, Href string
	// Title is the failure reason, a tooltip.
	Title string
	// MR and MRHref are the open merge request of the repository, 0 = none.
	MR     int64
	MRHref string
}

// InboxDepsAction is the main button of a row: a POST form (Action) or a link
// (Href); both empty means no button.
type InboxDepsAction struct {
	Label, Action, Href string
}

// InboxDepsRow is the row of one repository with pending updates.
type InboxDepsRow struct {
	Repo, RepoHref string
	// ID is the id of the element: the live swap keeps its details open.
	ID    string
	Count int
	// Breakdown is "1 major · 2 minor · 3 patch", the empty kinds left out.
	Breakdown string
	New       int
	Status    InboxDepsStatus
	Primary   InboxDepsAction
	// Ignore and Snooze are the POST targets of the menu; empty without the
	// button.
	Ignore, Snooze string
	Updates        []InboxDepsUpdate
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
	return "other"
}

// depsBreakdown counts the updates by kind, the most significant first.
func depsBreakdown(ups []domain.DepUpdate) string {
	n := map[string]int{}
	for _, u := range ups {
		n[depsKindLabel(u.Kind)]++
	}
	var parts []string
	for _, k := range []string{"major", "minor", "patch", "other"} {
		if n[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n[k], k))
		}
	}
	return strings.Join(parts, " · ")
}

// depsIconKind is the CheckIcon kind of a state.
func depsIconKind(s InboxDepsStatus) string {
	switch s.Kind {
	case "queued":
		return "queued"
	case "failed":
		return "bad"
	}
	return "running"
}

// depsStatus is what the status cell says about the record and its run.
func depsStatus(d domain.RepoDeps, run *domain.CIRun, upToDate bool) InboxDepsStatus {
	repo := d.Repo.String()
	var st InboxDepsStatus
	if d.MR != 0 {
		st.MR, st.MRHref = d.MR, mrHref(repo, d.MR)
		st.Kind = "mr"
		if upToDate {
			st.Kind = "uptodate"
		}
	}
	var runLink InboxDepsStatus
	if run != nil {
		n := run.Number
		if n == 0 {
			n = run.ID
		}
		runLink.Text, runLink.Href = fmt.Sprintf("run #%d", n), runHref(repo, run.ID)
	}
	switch d.State {
	case domain.DepsApplying:
		st.Kind, st.State = "running", "running"
		if run == nil {
			st.State = "applying"
		} else if run.Status == domain.CIQueued || run.Status == domain.CIPending {
			st.Kind, st.State = "queued", "queued"
		}
		st.Text, st.Href = runLink.Text, runLink.Href
	case domain.DepsFailed:
		st.Kind, st.State, st.Title = "failed", "failed", d.Error
		st.Text, st.Href = runLink.Text, runLink.Href
	}
	return st
}

func (s *Server) depsRow(in ports.InboxDeps, filter domain.RepoName) InboxDepsRow {
	d := in.Rec
	repo := d.Repo.String()
	row := InboxDepsRow{
		Repo: repo, RepoHref: repoHref(repo), ID: "deps-" + repo,
		Count: len(d.Updates), Breakdown: depsBreakdown(d.Updates),
	}
	upToDate := d.MR != 0 && domain.SameRefs(d.Refs(), d.MRVersions)
	row.Status = depsStatus(d, in.Run, upToDate)
	for _, u := range d.Updates {
		if u.New {
			row.New++
		}
		row.Updates = append(row.Updates, InboxDepsUpdate{
			Ecosystem: string(u.Ecosystem), Directory: u.Directory, Name: u.Name,
			Current: u.Current, Latest: u.Version, Kind: depsKindLabel(u.Kind), New: u.New,
		})
	}
	failed := d.State == domain.DepsFailed
	// The merge request already holds these versions: nothing to apply, only to
	// view. A run in progress has no button at all.
	merged := upToDate && d.State == domain.DepsIdle
	if merged {
		row.Primary = InboxDepsAction{Label: "View MR", Href: row.Status.MRHref}
	}
	if s.cfg.Deps != nil && d.State != domain.DepsApplying {
		base := inboxPath + "/deps/" + url.PathEscape(repo) + "/"
		var q string
		if !filter.IsZero() {
			q = "?repo=" + url.QueryEscape(filter.String())
		}
		row.Snooze = base + "snooze" + q
		if !merged {
			row.Ignore = base + "ignore" + q
			label := "Open MR"
			switch {
			case failed:
				label = "Retry"
			case d.MR != 0:
				label = "Update MR"
			}
			row.Primary = InboxDepsAction{Label: label, Action: base + "open" + q}
		}
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
