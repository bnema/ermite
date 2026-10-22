// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"html"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The merge request pages (ADR 0017). They read through MergeRequestService
// and BrowseService as the visitor of the request: anonymous sees the merge
// requests of public repositories, the signed-in owner those of every
// repository, and only the owner gets the forms (mr_post.go handles them).
//
// Routes:
//
//	GET  /<repo>/merge-requests              the list, open ones first
//	GET  /<repo>/merge-requests/<n>          the request with its diff
//	GET  /<repo>/merge-requests/<n>/since    the diff since the last read
//	GET  /<repo>/compare/<branch>            a branch against the default branch
//	GET  /<repo>/merge-requests/<n>/files    a window of files of the diff
//
// The page lists every changed file and reads the hunks of the first window
// of files only (mrWindow); the files after it are placeholders that the
// browser fills in, a window per request, as they come near the screen
// (/files?from=&base=&head=). Without scripts each placeholder links to the
// page of its window (?from=), which shows the hunks of that window.
//
// Reading marks nothing as read. MarkRead stores state, and a GET must stay
// safe: a prefetcher, a link preview or a reload of the owner's tab would move
// the read position, and the "since your last read" diff would lose what the
// owner never saw. So the owner marks a head read with a POST button
// ("[mark read]", /<n>/read), which carries the head the page showed; the
// button only shows when that head is not the one already read. The since page
// is the owner's alone (the position is the owner's), and answers 404 to
// anyone else.
//
// Every page here is rendered through page(), so for the owner it is
// "private, no-store" with "Vary: Cookie", and for a visitor "no-cache": the
// forms and the CLI lines exist only in the owner's copy.

const (
	// mrWindow is how many files of a merge request one read gets the hunks
	// of: the page shows the first window and loads the others.
	mrWindow = 25
	// mrPerPage is how many decided merge requests a page of the list shows,
	// at least; pages of the use case are read whole.
	mrPerPage = 50
	// maxMRPages bounds the pages of the use case one list page reads.
	maxMRPages = 20
	// maxOpenPages bounds the pages read for the open merge requests, which
	// the first list page shows in full.
	maxOpenPages = 5
	// railBranches is how many branches the list offers to compare.
	railBranches = 15
)

// MRRow is one merge request in the list.
type MRRow struct {
	Number   string
	Href     string
	Title    string
	Branches string
	Author   string
	State    string
	When     string
	WhenAt   string
	Cursor   bool
}

// BranchLink is a branch the list offers to open a merge request for.
type BranchLink struct{ Name, Href string }

// MRListPage is the list of the merge requests of a repository.
type MRListPage struct {
	Open, Done []MRRow
	// OpenNote says the open list was cut.
	OpenNote string
	// Older and Newest are the paging links of the decided requests.
	Older, Newest string
	Branches      []BranchLink
	Default       string
	CLI           Clone
	// Live is true when the page announces changes.
	Live bool
}

// CommentView is a comment of a merge request, ready to show.
type CommentView struct {
	ID              string
	Author          string
	When            string
	WhenAt          string
	Abs             string
	Body, BodySplit template.HTML
	// Parts is a plain-text body (an issue comment); Body is the Markdown of
	// a merge request note.
	Parts                 []TextPart
	Replies               []CommentView
	Edited, Deleted       bool
	Verdict, Range, Quote string
	Edit                  *EditForm
	Delete                *DeleteForm
	Reply                 *CommentForm
	Resolved              bool
	// Outdated is true when the head moved since the comment was written, so
	// its line may have moved too.
	Outdated bool
	// Where names the line of a comment that is not under a line of the diff
	// shown ("path:12 (new)").
	Where string
	// Resolve is the form that resolves or unresolves it; nil when the reader
	// cannot.
	Resolve *ResolveForm
}

type EditForm struct {
	Action, Text string
	From         int
}
type DeleteForm struct {
	Action string
	From   int
}
type VerdictView struct {
	Author, Label, Href, Short string
	Stale                      bool
}

// ResolveForm is the POST that changes the resolved state of a comment.
type ResolveForm struct {
	Action string
	// Value is the state the form sets.
	Value bool
	Label string
	// From is the window of the page the comment is on (CommentForm.From).
	From int
}

// CommentForm is the POST that adds a comment.
type CommentForm struct {
	ID     string
	Action string
	Title  string
	// Path, Line and Side designate the line; empty for a general comment.
	Path, Line, Side   string
	StartLine, ReplyTo string
	// Plain is a form of an issue comment: plain text, no verdict.
	Plain  bool
	Cancel string
	Focus  bool
	// From is the window of the page the form is on (see mrWindow), sent
	// back so that the answer returns to it; 0 for the first.
	From int
}

// MRCommit is a commit of the request.
type MRCommit struct {
	Short, Href, Subject, Author, When, WhenAt string
	// New is true for a commit pushed after the head the reviewer last read.
	New bool
	// Sig is the signature badge, nil for an unsigned or unchecked commit.
	Sig *SignatureBadge
}

// MergeBox is what the owner does with an open request.
type MergeBox struct {
	MergeAction string
	CloseAction string
	// ReadyAction takes a draft out of draft; empty when it is not one: a
	// draft shows it in place of the merge form.
	ReadyAction string
	ReadAction  string
	Head        string
	Modes       []ModeOption
	// HeadGone is true when the source branch no longer exists: there is
	// nothing to merge.
	HeadGone bool
	CLI      Clone
	// CanMarkRead is true when the head differs from the one last read.
	CanMarkRead bool
	// CanDeleteBranch offers to delete the source after the merge: it is not
	// the default branch. The server checks again, with the open requests.
	CanDeleteBranch bool
	// Mergeability is the live badge of the box and what the modes depend on.
	Mergeability MergeabilityView
	// Signed is true when a mode that creates a commit (squash, merge commit)
	// is offered; Fingerprint is then the one of the key that signs it, empty
	// when the server does not tell.
	Signed      bool
	Fingerprint string
	// Error is why the last merge was refused, shown in the box.
	Error string
	// AutoAction and CancelAutoAction post to the auto-merge routes.
	AutoAction, CancelAutoAction string
	// CanAutoMerge offers "Merge when ready": the repository allows
	// auto-merge and the server has CI to wait for.
	CanAutoMerge bool
	// AutoMerge is the pending auto-merge, nil when off.
	AutoMerge *AutoMergeView
}

// AutoMergeView is a pending auto-merge, ready to show.
type AutoMergeView struct {
	Mode, EnabledBy, When, WhenAt, HeadShort string
}

// ModeOption is a merge mode the repository allows. Disabled says that the
// mode is not possible for the current state of the request, Reason why.
type ModeOption struct {
	Value, Label string
	Disabled     bool
	Reason       string
}

// MergeabilityView is the mergeability of an open request, ready to show: the
// badge, and what the merge modes depend on (the flags are what the controls
// of the box read, live).
type MergeabilityView struct {
	State, Label, Detail string
	Paths                []string
	// PathsNote says that more paths conflict than are listed.
	PathsNote string
	// FFOk and MergeOk say whether fast-forward, squash and merge commit are
	// possible; the reasons say why not.
	FFOk, MergeOk         bool
	FFReason, MergeReason string
}

// SinceNav switches between the whole diff and the diff since the last read.
type SinceNav struct {
	AllHref, SinceHref string
	Files              int
	ReadShort          string
	HeadShort          string
	Current            bool
}

// MRPage is a merge request with its diff and comments.
type MRPage struct {
	Number         string
	Title          string
	State          string
	Open           bool
	Signed         bool
	NoteAuthor     string
	Source, Target string
	SourceHref     string
	TargetHref     string
	Author         string
	Opened         Person
	Updated        string
	UpdatedAt      string
	// Desc is the description rendered as Markdown (as a README: no raw
	// HTML, filtered URLs), "" when it is empty.
	Desc        template.HTML
	Head        string
	HeadShort   string
	MergedShort string
	MergedHref  string
	MergeMode   string
	// Deleting is the source branch that a merge just asked to delete: the
	// deletion follows the answer.
	Deleting string
	// MergeError is why a merge was refused when the page has no merge box
	// to show it in (the request is not open any more).
	MergeError string
	// Canonical is the address of the request, set when the page answers a
	// refused action from the address of that action: the script puts it in
	// the address bar, so that a reload does not post again.
	Canonical string

	Since     *SinceNav
	SinceNote string

	NFiles, Adds, Dels int
	Files              []DiffFile
	DiffView
	Note      string
	Invisible bool

	// General are the comments that are not on a line, Loose those on a line
	// that the diff shown does not have. With LooseInFiles (a window of files
	// loaded later) the loose comments of a listed file go under the file
	// instead (DiffFile.Loose).
	General, Loose []CommentView
	LooseInFiles   bool
	NComments      int
	NUnresolved    int
	GeneralForm    *CommentForm
	Verdicts       []VerdictView

	Commits     []MRCommit
	CommitsNote string
	CommitCount string
	Modes       []string
	ChecksBlock bool
	// Checks are the CI checks of the head of the request, ChecksSum their
	// state in the bar above the diff.
	Checks    ChecksSection
	ChecksSum ChecksSummary

	Box *MergeBox
	// Live is true when the page announces changes.
	Live bool
	// From is the first file of the window the page read the hunks of (?from=).
	From int
}

// CompareCommit is a commit of a branch that the target lacks.
type CompareCommit = CommitRow

// ComparePage is a branch compared with the default branch.
type ComparePage struct {
	Source, Target string
	SourceHref     string
	TargetHref     string
	Ahead          []CommitRow
	AheadText      string
	CLI            Clone
	// Note says what the page could not tell.
	Note string
	// The diff is the one of the single commit the branch is ahead by.
	NFiles, Adds, Dels int
	Files              []DiffFile
	DiffView
	Invisible bool
	HasDiff   bool
	// Existing is the open merge request for this pair, when there is one.
	Existing *Link
	// Create is the form to open one; nil when the reader cannot or there is
	// one already.
	Create *CreateForm
}

// CreateForm is the POST that opens a merge request.
type CreateForm struct {
	Action, Source, Target, Title string
}

// mrBase is the URL of the merge request pages of a repository.
func mrBase(repo string) string { return repoHref(repo, "merge-requests") }

func mrHref(repo string, n int64, sub ...string) string {
	return repoHref(repo, append([]string{"merge-requests", strconv.FormatInt(n, 10)}, sub...)...)
}

// mrNumber reads the number of the path: a positive decimal number, written
// without sign or leading zeros, else the page is not found.
func mrNumber(r *http.Request) (int64, error) {
	s := r.PathValue("n")
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 || strconv.FormatInt(n, 10) != s {
		return 0, errNotFound
	}
	return n, nil
}

// mrStateName is the state a page shows: "draft" for an open request whose
// title marks it so.
func mrStateName(m domain.MergeRequest) string {
	if m.IsOpen() && m.IsDraft() {
		return "draft"
	}
	return stateName(m.State)
}

func stateName(s domain.MRState) string {
	switch s {
	case domain.MRStateOpen:
		return "open"
	case domain.MRStateMerged:
		return "merged"
	case domain.MRStateClosed:
		return "closed"
	}
	return "unknown"
}

func modeName(m domain.MergeMode) string {
	switch m {
	case domain.MergeModeFastForward:
		return "fast-forward"
	case domain.MergeModeSquash:
		return "squash"
	case domain.MergeModeMerge:
		return "merge commit"
	}
	return ""
}

// mergeabilityView converts the mergeability of a request into the badge and
// the flags of the controls of the merge box. A request without result is
// being checked.
func mergeabilityView(m domain.Mergeability, target string) MergeabilityView {
	v := MergeabilityView{State: string(domain.MergeabilityChecking), Label: "Checking…"}
	v.FFOk, v.FFReason = m.Allows(domain.MergeModeFastForward)
	v.MergeOk, v.MergeReason = m.Allows(domain.MergeModeSquash)
	target = visibleText(target)
	switch m.State {
	case domain.MergeabilityUpToDate:
		v.State, v.Label = string(m.State), "Ready to merge"
	case domain.MergeabilityBehind:
		v.State, v.Label, v.Detail = string(m.State), "Behind "+target+" by "+behindText(m), "merges without conflicts"
	case domain.MergeabilityConflict:
		v.State, v.Label, v.Detail = string(m.State), "Conflicts with "+target, "rebase needed"
		v.Paths, v.PathsNote = conflictList(m)
	}
	return v
}

// behindText is "2 commits", "1 commit" or "1000+ commits".
func behindText(m domain.Mergeability) string {
	if m.Behind == 1 && !m.BehindCapped {
		return "1 commit"
	}
	n := itoa(m.Behind)
	if m.BehindCapped {
		n += "+"
	}
	return n + " commits"
}

// conflictList is the conflicting paths to show, and the note that says what
// the list lacks.
func conflictList(m domain.Mergeability) (paths []string, note string) {
	for _, p := range m.ConflictPaths {
		paths = append(paths, visibleText(p))
	}
	switch {
	case len(paths) == 0:
		note = "no common history."
	case m.ConflictPathsTruncated:
		note = "more files conflict than are listed."
	}
	return paths, note
}

// mrRow converts a merge request of a listing.
func (s *Server) mrRow(repo string, m domain.MergeRequest) MRRow {
	row := MRRow{
		Number: "#" + strconv.FormatInt(m.Number, 10), Href: mrHref(repo, m.Number),
		Title: visibleText(m.Title.String()), State: mrStateName(m),
		Branches: visibleText(m.Source) + " → " + visibleText(m.Target),
		Author:   s.who(m.Author),
	}
	ts := m.UpdatedAt
	if ts.IsZero() {
		ts = m.CreatedAt
	}
	row.When, row.WhenAt = whenOf(s.cfg.Now(), ts)
	return row
}

// mrList lists the merge requests: the open ones, then the merged and closed
// ones, a page at a time. The decided ones are paged with the page token of
// the use case in "page"; the open ones show on the first page only.
func (s *Server) mrList(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	token := r.URL.Query().Get("page")
	p := MRListPage{Default: visibleText(d.repo.DefaultBranch)}
	p.CLI = Clone{ID: "cli-new", Lines: []string{"ermite mr create " + shellWord(d.name()) + " --source <branch> --title <title>"}}
	base := mrBase(d.name())

	if token == "" {
		tok := ""
		for i := 0; i < maxOpenPages; i++ {
			res, err := s.cfg.MergeRequests.List(ctx, principal(ctx), d.repo.Name, ports.ListMergeRequestsParams{
				State: domain.MRStateOpen, PageSize: pageSize, PageToken: tok,
			})
			if err != nil {
				return nil, 0, err
			}
			for _, m := range res.MergeRequests {
				p.Open = append(p.Open, s.mrRow(d.name(), m))
			}
			if tok = res.NextPageToken; tok == "" {
				break
			}
		}
		if tok != "" {
			p.OpenNote = "more open merge requests than are listed here."
		}
	}

	tok := token
	for i := 0; i < maxMRPages; i++ {
		res, err := s.cfg.MergeRequests.List(ctx, principal(ctx), d.repo.Name, ports.ListMergeRequestsParams{
			PageSize: pageSize, PageToken: tok,
		})
		if err != nil {
			return nil, 0, err
		}
		for _, m := range res.MergeRequests {
			if m.State != domain.MRStateOpen {
				p.Done = append(p.Done, s.mrRow(d.name(), m))
			}
		}
		tok = res.NextPageToken
		if tok == "" || len(p.Done) >= mrPerPage {
			break
		}
	}
	if tok != "" {
		p.Older = base + "?page=" + url.QueryEscape(tok)
	}
	if token != "" {
		p.Newest = base
	}
	switch {
	case len(p.Open) > 0:
		p.Open[0].Cursor = true
	case len(p.Done) > 0:
		p.Done[0].Cursor = true
	}
	for _, b := range d.branches {
		if b.ShortName == d.repo.DefaultBranch || len(p.Branches) >= railBranches {
			continue
		}
		p.Branches = append(p.Branches, BranchLink{Name: visibleText(b.ShortName), Href: repoHref(d.name(), "compare", b.ShortName)})
	}

	l := s.repoLayout(ctx, d, "merge-requests")
	l.Title = "merge requests · " + d.name() + " · " + s.instance
	l.Crumbs[0].Current = false
	l.Crumbs = append(l.Crumbs, Crumb{Label: "merge requests", Current: true})
	if token == "" {
		p.Live = true
		l.Live, l.LiveURL = true, liveURL("repo:"+d.name(), "view=mrs")
	}
	l.HasRail = true
	return MRList(l, p), http.StatusOK, nil
}

// mrShow shows a merge request: the whole diff against the target.
func (s *Server) mrShow(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	return s.mrPage(ctx, r, false)
}

// mrSince shows the diff since the head the reviewer last marked read.
func (s *Server) mrSince(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	return s.mrPage(ctx, r, true)
}

func (s *Server) mrPage(ctx context.Context, r *http.Request, since bool) (templ.Component, int, error) {
	return s.mrPageView(ctx, r, since, false)
}

func (s *Server) mrPageView(ctx context.Context, r *http.Request, since, fragment bool) (templ.Component, int, error) {
	if since && !SignedIn(ctx) {
		return nil, 0, errNotFound
	}
	n, err := mrNumber(r)
	if err != nil {
		return nil, 0, err
	}
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	from, err := parseFrom(r)
	if err != nil {
		return nil, 0, err
	}
	res, err := s.cfg.MergeRequests.Get(ctx, principal(ctx), d.repo.Name, n, ports.GetMergeRequestOptions{
		SinceLastRead: since, Files: &ports.FileWindow{From: from, To: from + mrWindow},
		Load: fileParam(r), // a page without scripts: ?file=i reads that collapsed file in full
	})
	if err != nil {
		return nil, 0, err
	}
	if from > 0 && from >= len(res.Diff.Files) {
		return nil, 0, errNotFound
	}
	m := res.MergeRequest
	signed := SignedIn(ctx)
	open := m.State == domain.MRStateOpen
	head := m.HeadID.String()
	self := mrHref(d.name(), n)
	if since {
		self = mrHref(d.name(), n, "since")
	}

	p := MRPage{
		Number: "#" + strconv.FormatInt(n, 10), Title: visibleText(m.Title.String()),
		State: mrStateName(m), Open: open, Signed: signed, NoteAuthor: domain.AuthorName(principal(ctx)),
		Source: visibleText(m.Source), Target: visibleText(m.Target),
		Author: s.who(m.Author),
		Head:   head, HeadShort: short(head),
		NFiles: res.Stats.FilesChanged,
		Adds:   res.Stats.Additions,
		Dels:   res.Stats.Deletions,
		Files:  s.hl.files(ctx, res.Diff.Files),
	}
	contextGaps(p.Files, res.Diff.Files, repoHref(d.name(), "context", head))
	src := ""
	if !res.DiffBaseID.IsZero() {
		src = filesSrc(d.name(), n, since, res.DiffBaseID.String(), head)
	}
	collapseFiles(p.Files, res.Diff.Files, fileLinks{src: src, from: from, repo: d.name(), rev: head})
	if err := s.mrFacts(ctx, r, d, &p, res); err != nil {
		return nil, 0, err
	}

	p.Note = diffNote(res.Diff)
	countNotes(p.Files, res.Comments)
	if !res.DiffBaseID.IsZero() {
		lazyFiles(p.Files, from, filesSrc(d.name(), n, since, res.DiffBaseID.String(), head))
	}
	p.From = from

	s.mrReadPresentation(&p, d.name(), n, res, since, signed)

	// Comments: under their line, else in the list below the diff. The forms
	// are the owner's, on the whole diff of an open request.
	canWrite := signed && open && !since
	var on *lineRef
	if canWrite {
		on = parseOn(r.URL.Query().Get("on"))
	}
	s.placeComments(ctx, &p, d.name(), n, res.Comments, head, !since, canWrite, on)
	lines, invisible := diffLines(p.Files)
	p.Invisible = invisible
	p.DiffView = newDiffView(r, lines, withFrom(self+"?view=unified", from), withFrom(self+"?view=split", from))
	if signed && open {
		p.Box = s.mrBox(ctx, d, res)
	}

	if mergeErrorOf(ctx) != "" {
		p.Canonical = self
	}
	if p.Box == nil {
		p.MergeError = mergeErrorOf(ctx) // the request is not open any more
	}
	if canWrite {
		p.GeneralForm = &CommentForm{
			ID: "cf-general", Action: mrHref(d.name(), n, "comments"), Title: "comment on the merge request",
		}
	}

	if fragment {
		return MRStateFragment(p), http.StatusOK, nil
	}
	l := s.repoLayout(ctx, d, "merge-requests")
	l.Title = p.Number + " " + p.Title + " · " + d.name() + " · " + s.instance
	if head != "" {
		pinRef(&l, target{rev: head, name: m.Source})
		if !d.hasBranch(m.Source) {
			pinRef(&l, target{rev: head, name: short(head), detached: true})
		}
	}
	l.Crumbs[0].Current = false
	l.Crumbs = append(l.Crumbs,
		Crumb{Label: "merge requests", Href: mrBase(d.name())},
		Crumb{Label: p.Number, Href: mrHref(d.name(), n), Current: !since},
	)
	if since {
		l.Crumbs = append(l.Crumbs, Crumb{Label: "since", Current: true})
	}
	p.Live = true
	l.Live, l.LiveURL = true, liveURL("repo:"+d.name(), "view=mr&n="+strconv.FormatInt(n, 10)+"&ref="+url.QueryEscape(m.Source)+"&head="+url.QueryEscape(head)+"&target="+url.QueryEscape(m.Target))
	l.HasRail = true
	l.MR, l.Lazy = true, true
	return MergeRequest(l, p), http.StatusOK, nil
}

func (s *Server) mrBox(ctx context.Context, d *repoData, res ports.MergeRequestDetail) *MergeBox {
	m := res.MergeRequest
	box := s.mergeBox(d, m.Number, m.Source, m.HeadID.String(), res.HeadUnread, mergeabilityView(res.Mergeability, m.Target))
	box.Error = mergeErrorOf(ctx)
	if m.IsDraft() {
		box.ReadyAction = mrHref(d.name(), m.Number, "ready")
	}
	box.AutoMerge = autoMergeView(s.cfg.Now(), m.AutoMerge)
	return box
}

// autoMergeView is the pending auto-merge ready to show, nil when off.
func autoMergeView(now time.Time, am *domain.AutoMerge) *AutoMergeView {
	if am == nil {
		return nil
	}
	v := &AutoMergeView{Mode: am.Mode.String(), EnabledBy: am.EnablerLabel, HeadShort: short(am.HeadID.String())}
	v.When, v.WhenAt = whenOf(now, am.EnabledAt)
	return v
}

// mrFacts formats the description, metadata, branch links and checks.
func (s *Server) mrFacts(ctx context.Context, r *http.Request, d *repoData, p *MRPage, res ports.MergeRequestDetail) error {
	m := res.MergeRequest
	head := m.HeadID.String()
	signed := SignedIn(ctx)
	open := m.State == domain.MRStateOpen
	now := s.cfg.Now()
	if desc := m.Description.String(); strings.TrimSpace(desc) != "" {
		// Relative links point into the source branch as it is now (its head
		// commit); the heading ids carry their own prefix, apart from the
		// ids of the diff.
		h, err := s.md.renderNote([]byte(desc), d.name(), head, "mr-desc-", refResolver{notes: noteIDs(res.Comments), numbers: s.numberResolver(ctx, d.name())})
		if err != nil {
			return err
		}
		p.Desc = h
	}
	for _, v := range res.Verdicts {
		p.Verdicts = append(p.Verdicts, VerdictView{Author: s.who(v.Author), Label: v.Verdict.String(), Stale: v.Stale, Href: "#c" + strconv.FormatInt(v.CommentID, 10), Short: short(v.CommitID.String())})
	}
	p.Opened.Name = p.Author
	p.Opened.When, p.Opened.WhenAt = whenOf(now, m.CreatedAt)
	if !m.CreatedAt.IsZero() {
		p.Opened.Abs = absolute(m.CreatedAt)
	}
	p.Updated, p.UpdatedAt = whenOf(now, m.UpdatedAt)
	for _, b := range d.branches {
		switch b.ShortName {
		case m.Source:
			p.SourceHref = repoHref(d.name(), "tree", b.ShortName)
		case m.Target:
			p.TargetHref = repoHref(d.name(), "tree", b.ShortName)
		}
	}
	if m.State == domain.MRStateMerged && !m.MergedCommitID.IsZero() {
		p.MergedShort = short(m.MergedCommitID.String())
		p.MergedHref = repoHref(d.name(), "commit", m.MergedCommitID.String())
		p.MergeMode = modeName(m.MergeMode)
	}
	if signed && m.State == domain.MRStateMerged && r.URL.Query().Get("deleting") == "1" && d.hasBranch(m.Source) {
		p.Deleting = visibleText(m.Source)
	}
	for _, mode := range d.repo.Settings.AllowedMergeModes {
		if name := modeName(mode); name != "" {
			p.Modes = append(p.Modes, name)
		}
	}
	p.ChecksBlock = true
	if head != "" {
		checks := s.checksFor(ctx, d.repo.Name, head)
		p.Checks = checksSection(d.repo.Repo, checks)
		p.ChecksSum = checksSummary(d.repo.Name, head, checks)
		if !open {
			p.Checks.Note = ""
		}
	}
	return nil
}

// mrReadPresentation maps usecase read tracking to commit rows and navigation.
func (s *Server) mrReadPresentation(p *MRPage, name string, n int64, res ports.MergeRequestDetail, since, signed bool) {
	now := s.cfg.Now()
	head := res.MergeRequest.HeadID.String()
	// The commits of the request, newest first.
	for i, c := range res.Commits {
		row := commitRow(name, c, now)
		p.Commits = append(p.Commits, MRCommit{
			Short: row.Short, Href: row.CHref, Subject: row.Subject, Author: row.Author, When: row.When, WhenAt: row.WhenAt,
			Sig: row.Sig, New: signed && i < res.NewCommits,
		})
	}
	lastRead := res.LastReadID.String()
	p.CommitCount = itoa(len(p.Commits))
	if res.CommitsTruncated {
		p.CommitCount += "+"
		p.CommitsNote = "the branch has more commits than the " + itoa(len(p.Commits)) + " listed."
	}

	// Since the last read: the navigation, and what the page says about it.
	if signed && lastRead != "" {
		p.Since = &SinceNav{
			AllHref: mrHref(name, n), SinceHref: mrHref(name, n, "since"),
			ReadShort: short(lastRead), HeadShort: short(head), Current: since,
		}
	}
	if since {
		switch {
		case lastRead == "":
			p.SinceNote = "nothing was marked read yet: this is the whole change."
		case res.LastReadLost:
			p.SinceNote = "the commit you last read (" + short(lastRead) + ") is gone, the branch was rewritten: this is the whole change, review it again."
		case len(p.Files) == 0:
			p.SinceNote = "nothing changed since " + short(lastRead) + "."
		default:
			p.SinceNote = "changes since " + short(lastRead) + ", the head you last read."
		}
	}
	if p.Since != nil {
		p.Since.Files = len(p.Files)
	}

}

// hasBranch tells whether the repository has the branch.
func (d *repoData) hasBranch(name string) bool {
	for _, b := range d.branches {
		if b.ShortName == name {
			return true
		}
	}
	return false
}

// mergeBox is what the owner can do with an open request.
func (s *Server) mergeBox(d *repoData, n int64, source, head string, canMarkRead bool, mg MergeabilityView) *MergeBox {
	b := &MergeBox{
		MergeAction: mrHref(d.name(), n, "merge"), CloseAction: mrHref(d.name(), n, "close"),
		ReadAction: mrHref(d.name(), n, "read"), Head: head, HeadGone: head == "",
		CanMarkRead:     canMarkRead,
		CanDeleteBranch: source != d.repo.DefaultBranch,
		Mergeability:    mg,
		AutoAction:      mrHref(d.name(), n, "auto-merge"), CancelAutoAction: mrHref(d.name(), n, "auto-merge/cancel"),
		CanAutoMerge: d.repo.Settings.AllowAutoMerge && s.cfg.Runs != nil,
	}
	var lines []string
	cmd := ""
	if head != "" {
		cmd = "ermite mr merge " + shellWord(d.name()) + " " + strconv.FormatInt(n, 10) + " --head " + head
	}
	for _, mode := range d.repo.Settings.AllowedMergeModes {
		switch mode {
		case domain.MergeModeFastForward:
			b.Modes = append(b.Modes, ModeOption{Value: "fast-forward", Label: "fast-forward", Disabled: !mg.FFOk, Reason: reasonIf(!mg.FFOk, mg.FFReason)})
			lines = append(lines, cmd)
		case domain.MergeModeSquash:
			b.Signed = true
			b.Modes = append(b.Modes, ModeOption{Value: "squash", Label: "squash (one signed commit)", Disabled: !mg.MergeOk, Reason: reasonIf(!mg.MergeOk, mg.MergeReason)})
			lines = append(lines, cmd+" --mode squash")
		case domain.MergeModeMerge:
			b.Signed = true
			b.Modes = append(b.Modes, ModeOption{Value: "merge", Label: "merge commit (signed)", Disabled: !mg.MergeOk, Reason: reasonIf(!mg.MergeOk, mg.MergeReason)})
			lines = append(lines, cmd+" --mode merge")
		}
	}
	if b.Signed && s.cfg.SigningKey != nil {
		b.Fingerprint = s.cfg.SigningKey.Info().Key.Fingerprint
	}
	if head != "" {
		b.CLI = Clone{ID: "cli-merge", Lines: lines}
	}
	return b
}

func reasonIf(cond bool, reason string) string {
	if cond {
		return reason
	}
	return ""
}

// firstModeOK is the index of the first mode that is possible, -1 for none.
func (b *MergeBox) firstModeOK() int {
	for i, m := range b.Modes {
		if !m.Disabled {
			return i
		}
	}
	return -1
}

// Checked is the mode the form has checked: the first that is possible, else
// the first (it is disabled, and a live update may enable it).
func (b *MergeBox) Checked() int { return max(b.firstModeOK(), 0) }

// CanMerge tells whether some mode is possible: the button is enabled.
func (b *MergeBox) CanMerge() bool { return b.firstModeOK() >= 0 }

// MergeTitle is why the button is disabled, empty when it is not.
func (b *MergeBox) MergeTitle() string {
	if b.CanMerge() {
		return ""
	}
	if len(b.Modes) == 0 {
		return "the repository allows no merge mode"
	}
	return b.Modes[0].Reason
}

// shellWord quotes s for a shell unless it is made of safe characters, which
// a repository name always is.
func shellWord(s string) string {
	for _, c := range []byte(s) {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '.' && c != '_' && c != '/' {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

// lineRef designates a line of the diff in the query of the comment form:
// "?on=new:12:path/to/file".
type lineRef struct {
	side  string
	line  int
	start int
	path  string
}

func parseOn(s string) *lineRef {
	side, rest, ok := strings.Cut(s, ":")
	if !ok || (side != "new" && side != "old") {
		return nil
	}
	num, path, ok := strings.Cut(rest, ":")
	start := 0
	if a, b, rangeOK := strings.Cut(num, "-"); rangeOK {
		var e error
		start, e = strconv.Atoi(a)
		end, e2 := strconv.Atoi(b)
		if e != nil || e2 != nil || start < 1 || start >= end {
			return nil
		}
		num = b
	}
	line, err := strconv.Atoi(num)
	if !ok || err != nil || line < 1 || path == "" {
		return nil
	}
	return &lineRef{side: side, line: line, start: start, path: path}
}

// placeComments puts every comment under its line when the diff shown has it,
// else in the list below the diff. The comments name lines of the diff against
// the target, so a page that shows another diff (since the last read) lists
// them all below, with their line. A reader who can write gets the line numbers
// as links to the form of their line; on designates the line whose form shows.
//
//hexcheck:ignore suspicious-business-logic-in-adapter places comments under the lines of the diff shown; display only
func (s *Server) placeComments(ctx context.Context, p *MRPage, repo string, n int64, comments []domain.Comment, head string, inline, canWrite bool, on *lineRef) {
	now := s.cfg.Now()
	self := mrHref(repo, n)
	resolveAction := func(id string) string { return mrHref(repo, n, "comments", id, "resolve") }

	type lineKey struct {
		path string
		side domain.DiffSide
		line int
	}
	index := map[lineKey]*DiffLine{}
	fileIDs := map[string]string{}
	fileAt := map[string]int{}
	for fi := range p.Files {
		f := &p.Files[fi]
		fileIDs[f.Key] = f.ID
		fileAt[f.Key] = fi
		linkable := canWrite && f.Key != "" && !strings.ContainsFunc(f.Key, invisible)
		// The form of a line shows on the page of the window of its file.
		on := self + "?on="
		if w := windowStart(fi); w > 0 {
			on = self + "?from=" + itoa(w) + "&on="
		}
		for hi := range f.Hunks {
			for li := range f.Hunks[hi].Lines {
				ln := &f.Hunks[hi].Lines[li]
				if ln.oldN > 0 && ln.Kind != "add" {
					index[lineKey{f.Key, domain.SideOld, ln.oldN}] = ln
					if linkable {
						ln.OldHref = on + "old:" + itoa(ln.oldN) + ":" + url.QueryEscape(f.Key) + "#" + f.ID
					}
				}
				if ln.newN > 0 && ln.Kind != "del" {
					index[lineKey{f.Key, domain.SideNew, ln.newN}] = ln
					if linkable {
						ln.NewHref = on + "new:" + itoa(ln.newN) + ":" + url.QueryEscape(f.Key) + "#" + f.ID
					}
				}
			}
		}
	}
	find := func(path string, side domain.DiffSide, line int) *DiffLine {
		if !inline {
			return nil
		}
		return index[lineKey{path, side, line}]
	}

	exists := refResolver{notes: noteIDs(comments), numbers: s.numberResolver(ctx, repo)}
	children := map[int64][]domain.Comment{}
	for _, c := range comments {
		if c.ReplyTo != 0 && !c.Deleted {
			children[c.ReplyTo] = append(children[c.ReplyTo], c)
		}
		if !c.Deleted {
			p.NComments++
		}
		if c.ReplyTo == 0 && !c.Deleted && !c.Resolved {
			p.NUnresolved++
		}
	}
	var view func(domain.Comment, bool) CommentView
	view = func(c domain.Comment, replyable bool) CommentView {
		cid := strconv.FormatInt(c.ID, 10)
		body, err := s.md.renderNote([]byte(c.Text.String()), repo, head, "c"+cid+"-", exists)
		if err != nil {
			body = ""
		}
		bodySplit, err := s.md.renderNote([]byte(c.Text.String()), repo, head, "c"+cid+"s-", exists)
		if err != nil {
			bodySplit = ""
		}
		v := CommentView{BodySplit: bodySplit, ID: cid, Author: s.who(c.Author), Body: body, Resolved: c.Resolved, Deleted: c.Deleted, Edited: !c.EditedAt.IsZero(), Verdict: c.Verdict.String(), Outdated: !c.CommitID.IsZero() && c.CommitID.String() != head}
		v.When, v.WhenAt = whenOf(now, c.CreatedAt)
		if !c.CreatedAt.IsZero() {
			v.Abs = absolute(c.CreatedAt)
		}
		if c.StartLine > 0 {
			v.Range = "lines " + itoa(c.StartLine) + "–" + itoa(c.Line)
		}
		if canWrite && !c.Deleted {
			from := windowStart(fileAt[c.Path])
			root := cid
			if c.ReplyTo != 0 {
				root = strconv.FormatInt(c.ReplyTo, 10)
			}
			if replyable {
				v.Quote = "> " + strings.ReplaceAll(c.Text.String(), "\n", "\n> ") + "\n\n^" + cid + " "
			}
			if c.ReplyTo == 0 {
				v.Reply = &CommentForm{ID: "reply-" + cid, Action: mrHref(repo, n, "comments"), Title: "Reply", ReplyTo: root, From: from}
			}
			if c.Author == p.NoteAuthor {
				v.Edit = &EditForm{Action: mrHref(repo, n, "comments", cid, "edit"), Text: c.Text.String(), From: from}
				v.Delete = &DeleteForm{Action: mrHref(repo, n, "comments", cid, "delete"), From: from}
			}
		}
		for _, reply := range children[c.ID] {
			v.Replies = append(v.Replies, view(reply, replyable))
		}
		return v
	}
	for _, c := range comments {
		if c.ReplyTo != 0 || c.Deleted && c.Verdict == domain.VerdictNone && len(children[c.ID]) == 0 {
			continue
		}
		cid := strconv.FormatInt(c.ID, 10)
		v := view(c, canWrite && !c.Deleted)
		general := c.Path == ""
		fi, listed := fileAt[c.Path]
		if !general && listed && p.Files[fi].Lazy {
			// Its file comes with its window, and the comment with it.
			continue
		}
		var ln *DiffLine
		if !general {
			ln = find(c.Path, c.Side, c.Line)
			if ln == nil {
				side := "new"
				if c.Side == domain.SideOld {
					side = "old"
				}
				v.Where = visibleText(c.Path) + ":" + itoa(c.Line) + " (" + side + ")"
			}
		}
		if canWrite && !c.Deleted {
			v.Resolve = &ResolveForm{Action: resolveAction(cid), Value: !c.Resolved, Label: "Resolve"}
			if !general && listed {
				v.Resolve.From = windowStart(fi)
			}
			if c.Resolved {
				v.Resolve.Label = "Unresolve"
			}
		}
		switch {
		case ln != nil:
			ln.Notes = append(ln.Notes, v)
		case general:
			p.General = append(p.General, v)
		case p.LooseInFiles && listed:
			p.Files[fi].Loose = append(p.Files[fi].Loose, v)
		default:
			p.Loose = append(p.Loose, v)
		}
	}

	if on != nil {
		side := domain.SideNew
		if on.side == "old" {
			side = domain.SideOld
		}
		start := on.start
		if start == 0 {
			start = on.line
		}
		for key, ln := range index {
			if key.path == on.path && key.side == side && key.line >= start && key.line <= on.line {
				ln.Selected = true
			}
		}
		// The title names the whole range a shift + click selected: a.go:21-149.
		lines := itoa(on.line)
		if start < on.line {
			lines = itoa(start) + "-" + lines
		}
		if ln := find(on.path, side, on.line); ln != nil {
			ln.Form = &CommentForm{
				StartLine: optionalLine(on.start), ID: "cf-line", Action: mrHref(repo, n, "comments"), Path: on.path, Line: itoa(on.line), Side: on.side,
				Title:  "comment on " + visibleText(on.path) + ":" + lines + " (" + on.side + ")",
				Cancel: withFrom(self, windowStart(fileAt[on.path])) + "#" + fileIDs[on.path], Focus: true,
				From: windowStart(fileAt[on.path]),
			}
		}
	}
}

// userText renders the parts of a plain text: the text escaped, the http(s)
// addresses as links. Its white space is kept by the style of the container.
func userText(parts []TextPart) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		var b strings.Builder
		for _, part := range parts {
			if part.Href != "" {
				b.WriteString(`<a href="` + html.EscapeString(part.Href) + `" rel="noopener nofollow ugc">` + html.EscapeString(part.Text) + `</a>`)
			} else {
				b.WriteString(html.EscapeString(part.Text))
			}
		}
		_, err := io.WriteString(w, b.String())
		return err
	})
}

func noteIDs(comments []domain.Comment) func(int64) bool {
	ids := make(map[int64]bool, len(comments))
	for _, c := range comments {
		ids[c.ID] = true
	}
	return func(n int64) bool { return ids[n] }
}
func optionalLine(n int) string {
	if n == 0 {
		return ""
	}
	return itoa(n)
}
