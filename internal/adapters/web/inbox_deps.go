// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/mod/semver"

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
	// Kind picks the icon: "" (no run to tell), "queued", "running" or "failed".
	Kind string
	// State is the word shown beside the icon, empty for a merge request.
	State string
	// Text and Href are the link to the run ("run #12"); without Href the text
	// is plain (the run is gone).
	Text, Href string
	// Title is the full failure reason.
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
	Batch, Title   string
	Outdated       bool
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
func depsStatus(repo string, batch domain.DepBatch, run *domain.CIRun) InboxDepsStatus {
	var st InboxDepsStatus
	if batch.MR != 0 {
		st.MR, st.MRHref = batch.MR, mrHref(repo, batch.MR)
	}
	var runLink InboxDepsStatus
	if run != nil {
		n := run.Number
		if n == 0 {
			n = run.ID
		}
		runLink.Text, runLink.Href = fmt.Sprintf("run #%d", n), runHref(repo, run.ID)
	}
	switch batch.State {
	case domain.DepsApplying:
		st.Kind, st.State = "running", "running"
		switch {
		case run == nil || run.Status == domain.CICompleted:
			// No run to tell, or the run ended and Bernard has not settled
			// the record yet.
			st.State = "applying"
		case run.Status == domain.CIQueued || run.Status == domain.CIPending:
			st.Kind, st.State = "queued", "queued"
		}
		st.Text, st.Href = runLink.Text, runLink.Href
	case domain.DepsFailed:
		st.Kind, st.State, st.Title = "failed", "failed", batch.Error
		st.Text, st.Href = runLink.Text, runLink.Href
	}
	return st
}

func (s *Server) depsRow(in ports.InboxDeps) InboxDepsRow {
	d := in.Rec
	batch := in.Batch
	repo := d.Repo.String()
	row := InboxDepsRow{
		Repo: repo, RepoHref: repoHref(repo), ID: "deps-" + repo,
		Count: len(batch.Updates), Breakdown: depsBreakdown(batch.Updates),
	}
	row.Batch, row.Title = string(batch.Key), "Updates"
	if batch.Key != "" {
		row.ID += "-" + strings.TrimPrefix(batch.Key.Branch(), "bernard/deps-major/")
		if len(batch.Updates) > 0 {
			row.Title = batch.Updates[0].Name + " → " + semver.Major(batch.Updates[0].Version)
		} else {
			row.Title = strings.TrimPrefix(domain.BotSubject(batch), "build(deps): update ")
			row.Title = strings.Replace(row.Title, " to ", " → ", 1)
		}
	}
	row.Outdated = batch.Outdated()
	upToDate := batch.MR != 0 && !row.Outdated
	row.Status = depsStatus(repo, batch, in.Run)
	for _, u := range batch.Updates {
		if u.New {
			row.New++
		}
		row.Updates = append(row.Updates, InboxDepsUpdate{
			Ecosystem: string(u.Ecosystem), Directory: u.Directory, Name: u.Name,
			Current: u.Current, Latest: u.Version, Kind: depsKindLabel(u.Kind), New: u.New,
		})
	}
	failed := batch.State == domain.DepsFailed
	// The merge request already holds these versions: nothing to apply, only to
	// view. A run in progress has no button at all.
	merged := upToDate && batch.State == domain.DepsIdle
	if merged {
		row.Primary = InboxDepsAction{Label: "View MR", Href: row.Status.MRHref}
	}
	if s.cfg.Deps != nil && batch.State != domain.DepsApplying {
		base := inboxPath + "/deps/" + url.PathEscape(repo) + "/"
		row.Snooze = base + "snooze"
		if !merged {
			row.Ignore = base + "ignore"
			label := "Open MR"
			switch {
			case failed:
				label = "Retry"
			case batch.MR != 0:
				label = "Update MR"
			}
			row.Primary = InboxDepsAction{Label: label, Action: base + "open"}
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
func (s *Server) depsAction(op depsOp) func(ctx context.Context, r *http.Request) (ActionResult, error) {
	return func(ctx context.Context, r *http.Request) (ActionResult, error) {
		back := depsBack(r)
		if !SignedIn(ctx) {
			return ActionResult{}, errNotFound
		}
		name, err := domain.ParseRepoName(r.PathValue("repo"))
		if err != nil {
			return ActionResult{}, err
		}
		if op == depsSnooze {
			_, err = s.cfg.Deps.Snooze(ctx, principal(ctx), name)
			return ActionResult{Redirect: back}, err
		}
		refs, err := depsRefs(r)
		if err != nil {
			return ActionResult{}, err
		}
		if op == depsOpen {
			_, err = s.cfg.Deps.Open(ctx, principal(ctx), name, domain.DepBatchKey(r.FormValue("batch")), refs)
		} else {
			_, err = s.cfg.Deps.Ignore(ctx, principal(ctx), name, domain.DepBatchKey(r.FormValue("batch")), refs)
		}
		return ActionResult{Redirect: back}, err
	}
}

// depsErrorTail is the last nonblank output line for the collapsed row.
func depsErrorTail(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func depsBack(r *http.Request) string {
	back := inboxHref("deps", 0)
	if sort := depsSortParam(r.URL.Query().Get("sort")); sort != "" {
		back += "&sort=" + sort
	}
	return back
}
