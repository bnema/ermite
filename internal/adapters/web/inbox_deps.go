// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
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

// InboxDepsStatus is the status of a batch.
type InboxDepsStatus struct {
	// Kind picks the icon: "" (no run to tell), "queued", "running" or "failed".
	Kind string
	// State is the word shown beside the icon ("queued #3", "running",
	// "failed"), empty for a plain merge request.
	State string
	// Text and Href are the link to the run ("run #12"); without Href the text
	// is plain (the run is gone).
	Text, Href string
	// Title is the full failure reason.
	Title string
	// MR and MRHref are the open merge request of the batch, 0 = none; Draft
	// tells it waits for its run, Adopted that a human pushed to it.
	MR      int64
	MRHref  string
	Draft   bool
	Adopted bool
}

// InboxDepsAction is the main button of a row: a POST form (Action) or a link
// (Href); both empty means no button.
type InboxDepsAction struct {
	Label, Action, Href string
}

// InboxDepsSplit is one Split or Join entry of the menu of a major batch.
type InboxDepsSplit struct {
	Label, Action, Ecosystem, Name string
	Split                          bool
}

// InboxDepsRow is one batch of a repository: its base batch (patch and minor)
// or a major family.
type InboxDepsRow struct {
	Batch, Title   string
	Major          bool
	Outdated       bool
	Repo, RepoHref string
	// ID is the id of the element: the live swap keeps its details open.
	ID    string
	Count int
	// Breakdown is "1 major · 2 minor · 3 patch", the empty kinds left out.
	Breakdown string
	// Packages are the short names of a major family ("bubbles, bubbletea"),
	// Target its majors ("v2"); both empty for the base batch.
	Packages, Target string
	New              int
	Status           InboxDepsStatus
	// Repro are the commands that redo the failed update by hand.
	Repro   string
	Primary InboxDepsAction
	// Ignore and Snooze are the POST targets of the menu; empty without the
	// button.
	Ignore, Snooze string
	// Splits pull a package out of its family, or join it back.
	Splits  []InboxDepsSplit
	Updates []InboxDepsUpdate
}

// InboxDepsRepo is the collapsible row of one repository: its base batch and
// its major families.
type InboxDepsRepo struct {
	Repo, RepoHref, ID string
	// Open: the row starts open (it holds a failure).
	Open bool
	// Failed counts the failed batches, Count the updates, BaseCount those of
	// the base batch, MajorCount the major families.
	Failed, Count, BaseCount, MajorCount, New int
	Base                                      *InboxDepsRow
	Majors                                    []InboxDepsRow
	// Recheck, Auto and Snooze are the POST targets of the repository actions;
	// AutoOn tells whether patch and minor updates open a draft on their own.
	Recheck, Auto, Snooze string
	AutoOn                bool
}

// InboxDepsFilter is one chip above the table.
type InboxDepsFilter struct {
	Label, Href string
	Count       int
	Current     bool
}

// depsQuery is the view of the table: its order (?sort=) and its filter
// (?filter=).
type depsQuery struct{ sort, filter string }

const (
	depsFilterFailures = "failures"
	depsFilterMajors   = "majors"
	depsFilterMRs      = "mrs"
)

func depsFilterParam(raw string) string {
	switch raw {
	case depsFilterFailures, depsFilterMajors, depsFilterMRs:
		return raw
	}
	return ""
}

func depsQueryOf(r *http.Request) depsQuery {
	q := r.URL.Query()
	return depsQuery{sort: depsSortParam(q.Get("sort")), filter: depsFilterParam(q.Get("filter"))}
}

// encode is the query the actions carry back, "" for the default view.
func (q depsQuery) encode() string {
	v := url.Values{}
	if q.sort != "" {
		v.Set("sort", q.sort)
	}
	if q.filter != "" {
		v.Set("filter", q.filter)
	}
	return v.Encode()
}

// keep reports whether the filter shows the batch.
func (q depsQuery) keep(b InboxDepsRow) bool {
	switch q.filter {
	case depsFilterFailures:
		return b.Status.Kind == "failed"
	case depsFilterMajors:
		return b.Major
	case depsFilterMRs:
		return b.Status.MR != 0
	}
	return true
}

type depsOp int

const (
	depsOpen depsOp = iota
	depsIgnore
	depsSnooze
	depsRecheck
	depsAuto
	depsSplit
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

// depsBusy reports whether Bernard is working on the batch: no button then.
func depsBusy(b domain.DepBatch) bool {
	return b.State == domain.DepsApplying || b.Queued()
}

// depsStatus is what the status cell says about the batch and its run.
func depsStatus(repo string, in ports.InboxDeps) InboxDepsStatus {
	batch, run := in.Batch, in.Run
	var st InboxDepsStatus
	if batch.MR != 0 {
		st.MR, st.MRHref = batch.MR, mrHref(repo, batch.MR)
		st.Adopted = batch.Adopted
		// Bernard opens the merge request as a draft and marks it ready once
		// its run passes.
		st.Draft = batch.State != domain.DepsIdle && !batch.Adopted
	}
	var runLink InboxDepsStatus
	if run != nil {
		n := run.Number
		if n == 0 {
			n = run.ID
		}
		runLink.Text, runLink.Href = fmt.Sprintf("run #%d", n), runHref(repo, run.ID)
	}
	queued := func() string {
		if in.QueuePos > 0 {
			return "queued #" + itoa(in.QueuePos)
		}
		return "queued"
	}
	switch batch.State {
	case domain.DepsQueued:
		st.Kind, st.State = "queued", queued()
		if in.QueuePos == 1 {
			st.Kind, st.State = "running", "drafting"
		}
	case domain.DepsDrafted:
		st.Kind, st.State = "queued", queued()
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

// depsMajorLabels are the title, the short package names and the target
// majors of a major batch.
func depsMajorLabels(batch domain.DepBatch) (title, packages, target string) {
	var names, majors []string
	family := ""
	for i, u := range batch.Updates {
		if i == 0 {
			family = domain.FamilyOf(u)
		} else if domain.FamilyOf(u) != family {
			family = ""
		}
		if !slices.Contains(names, u.Name) {
			names = append(names, u.Name)
		}
		if m := semver.Major(u.Version); m != "" && !slices.Contains(majors, m) {
			majors = append(majors, m)
		}
	}
	target = strings.Join(majors, ", ")
	switch {
	case len(names) == 1:
		title = names[0]
	case len(names) > 1 && family != "":
		title = family + "*"
		short := make([]string, len(names))
		for i, n := range names {
			short[i] = strings.TrimPrefix(n, family)
		}
		packages = strings.Join(short, ", ")
	case len(names) > 1:
		title, packages = names[0], strings.Join(names, ", ")
	default:
		title = strings.TrimPrefix(domain.BotSubject(batch), "build(deps): update ")
		title, target, _ = strings.Cut(title, " to ")
	}
	return title, packages, target
}

// depsRow is the row of one batch; query is appended to its actions.
func (s *Server) depsRow(in ports.InboxDeps, query string) InboxDepsRow {
	d := in.Rec
	batch := in.Batch
	repo := d.Repo.String()
	row := InboxDepsRow{
		Repo: repo, RepoHref: repoHref(repo), ID: depsRepoID(repo) + "-base",
		Count: len(batch.Updates), Breakdown: depsBreakdown(batch.Updates),
	}
	row.Batch, row.Title = string(batch.Key), "Patch & minor"
	if batch.Key != "" {
		row.Major = true
		row.ID = depsRepoID(repo) + "-" + strings.TrimPrefix(batch.Key.Branch(), "bernard/deps-major/")
		row.Title, row.Packages, row.Target = depsMajorLabels(batch)
	}
	row.Outdated = batch.Outdated()
	row.Status = depsStatus(repo, in)
	if batch.State == domain.DepsFailed {
		row.Repro = strings.Join(batch.Repro, "\n")
	}
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
	// The merge request already holds these versions, or a human took it
	// over: nothing to apply, only to view.
	merged := batch.MR != 0 && batch.State == domain.DepsIdle && (!row.Outdated || batch.Adopted)
	if merged {
		row.Primary = InboxDepsAction{Label: "View MR", Href: row.Status.MRHref}
	}
	if s.cfg.Deps == nil || depsBusy(batch) {
		return row
	}
	base := depsBase(repo)
	if !merged {
		row.Ignore = base + "ignore" + query
		label := "Open MR"
		switch {
		case failed:
			label = "Retry"
		case batch.MR != 0:
			label = "Update MR"
		}
		row.Primary = InboxDepsAction{Label: label, Action: base + "open" + query}
	}
	if row.Major {
		var names []domain.DepName
		for _, u := range batch.Updates {
			n := domain.DepName{Ecosystem: u.Ecosystem, Name: u.Name}
			if !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
		for _, n := range names {
			if slices.Contains(d.SplitOut, n) {
				row.Splits = append(row.Splits, InboxDepsSplit{Label: "Join family", Action: base + "split" + query, Ecosystem: string(n.Ecosystem), Name: n.Name})
			} else if len(names) > 1 {
				label := "Split " + strings.TrimPrefix(n.Name, domain.FamilyOf(domain.DepUpdate{DepRef: domain.DepRef{Ecosystem: n.Ecosystem, Name: n.Name}}))
				row.Splits = append(row.Splits, InboxDepsSplit{Label: label, Action: base + "split" + query, Ecosystem: string(n.Ecosystem), Name: n.Name, Split: true})
			}
		}
	}
	return row
}

func depsRepoID(repo string) string { return "deps-" + inboxIDPart(repo) }

func depsBase(repo string) string { return inboxPath + "/deps/" + url.PathEscape(repo) + "/" }

// depsRepos groups the batches of the overview by repository, keeps those the
// filter shows, and orders them: by name, or the repositories with a failure
// first, then the most updates. The base batch comes first in a repository,
// then the majors, the failed ones first.
func (s *Server) depsRepos(deps []ports.InboxDeps, q depsQuery) []InboxDepsRepo {
	suffix := ""
	if e := q.encode(); e != "" {
		suffix = "?" + e
	}
	var repos []InboxDepsRepo
	at := map[string]int{}
	for _, in := range deps {
		row := s.depsRow(in, suffix)
		i, ok := at[row.Repo]
		if !ok {
			i = len(repos)
			at[row.Repo] = i
			r := InboxDepsRepo{Repo: row.Repo, RepoHref: row.RepoHref, ID: depsRepoID(row.Repo), AutoOn: !in.Rec.AutoOpenDisabled}
			if s.cfg.Deps != nil {
				base := depsBase(row.Repo)
				r.Recheck, r.Auto, r.Snooze = base+"recheck"+suffix, base+"auto"+suffix, base+"snooze"+suffix
			}
			repos = append(repos, r)
		}
		if !q.keep(row) {
			continue
		}
		r := &repos[i]
		r.Count += row.Count
		r.New += row.New
		if row.Status.Kind == "failed" {
			r.Failed++
		}
		if row.Major {
			r.MajorCount++
			r.Majors = append(r.Majors, row)
		} else {
			r.BaseCount = row.Count
			r.Base = &row
		}
	}
	repos = slices.DeleteFunc(repos, func(r InboxDepsRepo) bool { return r.Base == nil && len(r.Majors) == 0 })
	for i := range repos {
		r := &repos[i]
		r.Open = r.Failed > 0
		slices.SortStableFunc(r.Majors, func(a, b InboxDepsRow) int {
			if c := cmp.Compare(boolFlag(b.Status.Kind == "failed"), boolFlag(a.Status.Kind == "failed")); c != 0 {
				return c
			}
			return strings.Compare(a.Title, b.Title)
		})
	}
	slices.SortStableFunc(repos, func(a, b InboxDepsRepo) int {
		if q.sort != depsSortName {
			if c := cmp.Compare(boolFlag(b.Failed > 0), boolFlag(a.Failed > 0)); c != 0 {
				return c
			}
			if c := cmp.Compare(b.Count, a.Count); c != 0 {
				return c
			}
		}
		return strings.Compare(a.Repo, b.Repo)
	})
	return repos
}

// depsFilters are the chips above the table, with the batches each shows.
func depsFilters(rows []InboxDepsRow, q depsQuery, rng int) []InboxDepsFilter {
	var out []InboxDepsFilter
	for _, f := range []struct{ param, label string }{{"", "All"}, {depsFilterFailures, "Failures"}, {depsFilterMajors, "Majors"}, {depsFilterMRs, "MRs"}} {
		v := depsQuery{sort: q.sort, filter: f.param}
		n := 0
		for _, r := range rows {
			if v.keep(r) {
				n++
			}
		}
		href := inboxHref("deps", rng)
		if e := v.encode(); e != "" {
			href += "&" + e
		}
		out = append(out, InboxDepsFilter{Label: f.label, Href: href + "#inbox-deps", Count: n, Current: f.param == q.filter})
	}
	return out
}

// depsQueueText is the progress of Bernard's queue, empty when idle.
func depsQueueText(q ports.DepsQueue) string {
	if q.Running == 0 && q.Queued == 0 {
		return ""
	}
	limit := ""
	if q.Limit > 0 {
		limit = "/" + itoa(q.Limit)
	}
	return fmt.Sprintf("queue %d%s running, %d waiting", q.Running, limit, q.Queued)
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

// depsFlag reads a "1"/"0" field.
func depsFlag(r *http.Request, field string) (bool, error) {
	switch r.PostFormValue(field) {
	case "1":
		return true, nil
	case "0":
		return false, nil
	}
	return false, fmt.Errorf("%w: %s must be 1 or 0", domain.ErrInvalid, field)
}

// depsAction is POST /-/inbox/deps/<repo>/open, ignore, snooze, recheck, auto
// or split.
func (s *Server) depsAction(op depsOp) func(ctx context.Context, r *http.Request) (ActionResult, error) {
	return func(ctx context.Context, r *http.Request) (ActionResult, error) {
		if !SignedIn(ctx) {
			return ActionResult{}, errNotFound
		}
		name, err := domain.ParseRepoName(r.PathValue("repo"))
		if err != nil {
			return ActionResult{}, err
		}
		p, deps := principal(ctx), s.cfg.Deps
		switch op {
		case depsSnooze:
			_, err = deps.Snooze(ctx, p, name)
		case depsRecheck:
			_, err = deps.Check(ctx, p, name)
		case depsAuto:
			var on bool
			if on, err = depsFlag(r, "on"); err == nil {
				_, err = deps.SetAutoOpen(ctx, p, name, on)
			}
		case depsSplit:
			var split bool
			dep := domain.DepName{Ecosystem: domain.DepsEcosystem(strings.TrimSpace(r.PostFormValue("ecosystem"))), Name: r.PostFormValue("name")}
			if split, err = depsFlag(r, "split"); err == nil {
				if dep.Ecosystem == "" || dep.Name == "" {
					err = fmt.Errorf("%w: missing dependency", domain.ErrInvalid)
				} else {
					_, err = deps.Split(ctx, p, name, dep, split)
				}
			}
		default:
			var refs []domain.DepRef
			if refs, err = depsRefs(r); err != nil {
				return ActionResult{}, err
			}
			key := domain.DepBatchKey(r.FormValue("batch"))
			if op == depsOpen {
				_, err = deps.Open(ctx, p, name, key, refs)
			} else {
				_, err = deps.Ignore(ctx, p, name, key, refs)
			}
		}
		if err != nil {
			return ActionResult{}, err
		}
		return s.depsResult(ctx, r)
	}
}

func depsBack(r *http.Request) string {
	back := inboxHref("deps", 0)
	if e := depsQueryOf(r).encode(); e != "" {
		back += "&" + e
	}
	return back
}

// depsResult re-renders the row of the repository, open since the owner acted
// inside it, or removes it when nothing is left to show.
func (s *Server) depsResult(ctx context.Context, r *http.Request) (ActionResult, error) {
	result := ActionResult{Redirect: depsBack(r)}
	if !isHX(r) {
		return result, nil
	}
	ov, err := s.cfg.Inbox.Overview(ctx, principal(ctx))
	if err != nil {
		return s.actionRefreshFailed(ctx, r, result, result.Redirect, err)
	}
	repo := r.PathValue("repo")
	for _, row := range s.depsRepos(ov.Deps, depsQueryOf(r)) {
		if row.Repo == repo {
			row.Open = true
			result.Fragment = inboxDepsChanged([]InboxDepsRepo{row}, nil)
			return result, nil
		}
	}
	result.Fragment = inboxDepsChanged(nil, []string{depsRepoID(repo)})
	return result, nil
}

func boolFlag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
