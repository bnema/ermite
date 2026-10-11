// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"html"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/a-h/templ"

	pb "bnema.dev/ermite-api/gen/ermite"
)

// The merge request pages (ADR 0017). They read through MergeRequestService
// and GitService as the visitor of the request: anonymous sees the merge
// requests of public repositories, the signed-in owner those of every
// repository, and only the owner gets the forms (mr_post.go handles them).
//
// Routes:
//
//	GET  /<repo>/merge-requests              the list, open ones first
//	GET  /<repo>/merge-requests/<n>          the request with its diff
//	GET  /<repo>/merge-requests/<n>/since    the diff since the last read
//	GET  /<repo>/compare/<branch>            a branch against the default branch
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
	// mrPerPage is how many decided merge requests a page of the list shows,
	// at least; pages of the API are read whole.
	mrPerPage = 50
	// maxMRPages bounds the API pages one list page reads.
	maxMRPages = 20
	// maxOpenPages bounds the pages read for the open merge requests, which
	// the first list page shows in full.
	maxOpenPages = 5
	// maxComparePages bounds how many pages of history the compare page reads
	// to find where the branch left the target.
	maxComparePages = 5
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
	ID       string
	Author   string
	When     string
	WhenAt   string
	Abs      string
	Parts    []TextPart
	Resolved bool
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

// ResolveForm is the POST that changes the resolved state of a comment.
type ResolveForm struct {
	Action string
	// Value is the state the form sets.
	Value bool
	Label string
}

// CommentForm is the POST that adds a comment.
type CommentForm struct {
	ID     string
	Action string
	Title  string
	// Path, Line and Side designate the line; empty for a general comment.
	Path, Line, Side string
	Cancel           string
	Focus            bool
}

// MRCommit is a commit of the request.
type MRCommit struct {
	Short, Href, Subject, Author, When, WhenAt string
	// New is true for a commit pushed after the head the reviewer last read.
	New bool
}

// MergeBox is what the owner does with an open request.
type MergeBox struct {
	MergeAction string
	CloseAction string
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
}

// ModeOption is a merge mode the repository allows.
type ModeOption struct{ Value, Label string }

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
	Source, Target string
	SourceHref     string
	TargetHref     string
	Author         string
	Opened         Person
	Updated        string
	UpdatedAt      string
	Desc           []TextPart
	Head           string
	HeadShort      string
	MergedShort    string
	MergedHref     string
	MergeMode      string

	Since     *SinceNav
	SinceNote string

	NFiles, Adds, Dels int
	Files              []DiffFile
	DiffView
	Note      string
	Invisible bool

	// General are the comments that are not on a line, Loose those on a line
	// that the diff shown does not have.
	General, Loose []CommentView
	NComments      int
	NUnresolved    int
	GeneralForm    *CommentForm

	Commits     []MRCommit
	CommitsNote string
	Modes       []string
	ChecksBlock bool

	Box *MergeBox
	// Live is true when the page announces changes.
	Live bool
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

func stateName(s pb.MergeRequestState) string {
	switch s {
	case pb.MergeRequestState_MERGE_REQUEST_STATE_OPEN:
		return "open"
	case pb.MergeRequestState_MERGE_REQUEST_STATE_MERGED:
		return "merged"
	case pb.MergeRequestState_MERGE_REQUEST_STATE_CLOSED:
		return "closed"
	}
	return "unknown"
}

func modeName(m pb.MergeMode) string {
	switch m {
	case pb.MergeMode_MERGE_MODE_FAST_FORWARD:
		return "fast-forward"
	case pb.MergeMode_MERGE_MODE_SQUASH:
		return "squash"
	}
	return ""
}

// mrRow converts a merge request of a listing.
func (s *Server) mrRow(repo string, m *pb.MergeRequest) MRRow {
	row := MRRow{
		Number: "!" + strconv.FormatInt(m.GetNumber(), 10), Href: mrHref(repo, m.GetNumber()),
		Title: visibleText(m.GetTitle()), State: stateName(m.GetState()),
		Branches: visibleText(m.GetSourceBranch()) + " → " + visibleText(m.GetTargetBranch()),
		Author:   visibleText(m.GetAuthor()),
	}
	ts := m.GetUpdatedAt()
	if !ts.IsValid() {
		ts = m.GetCreatedAt()
	}
	row.When, row.WhenAt = whenOf(s.cfg.Now(), ts)
	return row
}

// mrList lists the merge requests: the open ones, then the merged and closed
// ones, a page at a time. The decided ones are paged with the page token of
// the API in "page"; the open ones show on the first page only.
func (s *Server) mrList(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	token := r.URL.Query().Get("page")
	p := MRListPage{Default: visibleText(d.repo.GetDefaultBranch())}
	p.CLI = Clone{ID: "cli-new", Lines: []string{"ermite mr create " + shellWord(d.name()) + " --source <branch> --title <title>"}}
	base := mrBase(d.name())

	if token == "" {
		tok := ""
		for i := 0; i < maxOpenPages; i++ {
			res, err := s.cfg.MergeRequests.ListMergeRequests(ctx, connect.NewRequest(&pb.ListMergeRequestsRequest{
				Repo: d.name(), State: pb.MergeRequestState_MERGE_REQUEST_STATE_OPEN, PageSize: pageSize, PageToken: tok,
			}))
			if err != nil {
				return nil, 0, err
			}
			for _, m := range res.Msg.GetMergeRequests() {
				p.Open = append(p.Open, s.mrRow(d.name(), m))
			}
			if tok = res.Msg.GetNextPageToken(); tok == "" {
				break
			}
		}
		if tok != "" {
			p.OpenNote = "more open merge requests than are listed here."
		}
	}

	tok := token
	for i := 0; i < maxMRPages; i++ {
		res, err := s.cfg.MergeRequests.ListMergeRequests(ctx, connect.NewRequest(&pb.ListMergeRequestsRequest{
			Repo: d.name(), PageSize: pageSize, PageToken: tok,
		}))
		if err != nil {
			return nil, 0, err
		}
		for _, m := range res.Msg.GetMergeRequests() {
			if m.GetState() != pb.MergeRequestState_MERGE_REQUEST_STATE_OPEN {
				p.Done = append(p.Done, s.mrRow(d.name(), m))
			}
		}
		tok = res.Msg.GetNextPageToken()
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
		if b.GetShortName() == d.repo.GetDefaultBranch() || len(p.Branches) >= railBranches {
			continue
		}
		p.Branches = append(p.Branches, BranchLink{Name: visibleText(b.GetShortName()), Href: repoHref(d.name(), "compare", b.GetShortName())})
	}

	l := s.repoLayout(ctx, d, "merge-requests")
	l.Title = "merge requests · " + d.name() + " · " + s.instance
	l.Crumbs[0].Current = false
	l.Crumbs = append(l.Crumbs, Crumb{Label: "merge requests", Current: true})
	if token == "" {
		p.Live = true
		l.Live, l.LiveURL = true, liveURL("repo:"+d.name(), "view=mrs")
	}
	rows := len(p.Open) + len(p.Done)
	l.Status = Status{
		Mode: "merge requests", Context: itoa(len(p.Open)) + " open · " + itoa(len(p.Done)) + " decided",
		Hints: "j/k move · enter open · / filter · ? help", Cursor: true, Count: rows,
	}
	l.HasRail, l.RailSum = true, "states · open one"
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
	n, err := mrNumber(r)
	if err != nil {
		return nil, 0, err
	}
	if since && !SignedIn(ctx) {
		// The read position is the owner's: there is nothing to show others.
		return nil, 0, errNotFound
	}
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	res, err := s.cfg.MergeRequests.GetMergeRequest(ctx, connect.NewRequest(&pb.GetMergeRequestRequest{
		Repo: d.name(), Number: n, SinceLastRead: since,
	}))
	if err != nil {
		return nil, 0, err
	}
	m := res.Msg.GetMergeRequest()
	if m == nil {
		return nil, 0, errNotFound
	}
	now := s.cfg.Now()
	signed := SignedIn(ctx)
	open := m.GetState() == pb.MergeRequestState_MERGE_REQUEST_STATE_OPEN
	head := m.GetHeadId()
	self := mrHref(d.name(), n)
	if since {
		self = mrHref(d.name(), n, "since")
	}

	p := MRPage{
		Number: "!" + strconv.FormatInt(n, 10), Title: visibleText(m.GetTitle()),
		State: stateName(m.GetState()), Open: open, Signed: signed,
		Source: visibleText(m.GetSourceBranch()), Target: visibleText(m.GetTargetBranch()),
		Author: visibleText(m.GetAuthor()),
		Desc:   plainParts(m.GetDescription()),
		Head:   head, HeadShort: short(head),
		NFiles: int(res.Msg.GetStats().GetFilesChanged()),
		Adds:   int(res.Msg.GetStats().GetAdditions()),
		Dels:   int(res.Msg.GetStats().GetDeletions()),
		Files:  diffFiles(res.Msg.GetDiff().GetFiles()),
	}
	p.Opened.Name = p.Author
	p.Opened.When, p.Opened.WhenAt = whenOf(now, m.GetCreatedAt())
	if t := m.GetCreatedAt(); t.IsValid() {
		p.Opened.Abs = absolute(t.AsTime())
	}
	p.Updated, p.UpdatedAt = whenOf(now, m.GetUpdatedAt())
	for _, b := range d.branches {
		switch b.GetShortName() {
		case m.GetSourceBranch():
			p.SourceHref = repoHref(d.name(), "tree", b.GetShortName())
		case m.GetTargetBranch():
			p.TargetHref = repoHref(d.name(), "tree", b.GetShortName())
		}
	}
	if m.GetState() == pb.MergeRequestState_MERGE_REQUEST_STATE_MERGED && m.GetMergedCommitId() != "" {
		p.MergedShort = short(m.GetMergedCommitId())
		p.MergedHref = repoHref(d.name(), "commit", m.GetMergedCommitId())
		p.MergeMode = modeName(m.GetMergeMode())
	}
	if st := d.repo.GetSettings(); st != nil {
		for _, mode := range st.GetAllowedMergeModes() {
			if name := modeName(mode); name != "" {
				p.Modes = append(p.Modes, name)
			}
		}
		p.ChecksBlock = st.GetFailingChecksBlock()
	}
	p.Note = diffNote(res.Msg.GetDiff(), len(p.Files), p.NFiles)

	// The commits of the request, newest first.
	for _, c := range res.Msg.GetCommits() {
		row := commitRow(d.name(), c, now)
		p.Commits = append(p.Commits, MRCommit{
			Short: row.Short, Href: row.CHref, Subject: row.Subject, Author: row.Author, When: row.When, WhenAt: row.WhenAt,
		})
	}
	lastRead := res.Msg.GetLastReadId()
	if signed && lastRead != "" {
		// The commits are newest first: those before the last read head are new.
		// A head that is not among them (the branch was rewritten) marks none.
		for i, c := range res.Msg.GetCommits() {
			if c.GetId() != lastRead {
				continue
			}
			for j := 0; j < i; j++ {
				p.Commits[j].New = true
			}
		}
	}
	if res.Msg.GetCommitsTruncated() {
		p.CommitsNote = "the branch has more commits than the " + itoa(len(p.Commits)) + " listed."
	}

	// Since the last read: the navigation, and what the page says about it.
	if !signed {
		lastRead = "" // the read position is the owner's
	}
	if signed && lastRead != "" {
		p.Since = &SinceNav{
			AllHref: mrHref(d.name(), n), SinceHref: mrHref(d.name(), n, "since"),
			ReadShort: short(lastRead), HeadShort: short(head), Current: since,
		}
	}
	if since {
		switch {
		case lastRead == "":
			p.SinceNote = "nothing was marked read yet: this is the whole change."
		case res.Msg.GetLastReadLost():
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

	// Comments: under their line, else in the list below the diff. The forms
	// are the owner's, on the whole diff of an open request.
	canWrite := signed && open && !since
	var on *lineRef
	if canWrite {
		on = parseOn(r.URL.Query().Get("on"))
	}
	s.placeComments(&p, d.name(), n, res.Msg.GetComments(), head, !since, canWrite, on)
	lines, invisible := diffLines(p.Files)
	p.Invisible = invisible
	p.DiffView = newDiffView(r, lines, self+"?view=unified", self+"?view=split")
	if signed && open {
		p.Box = s.mergeBox(d, n, m.GetSourceBranch(), head, lastRead)
	}
	if canWrite {
		p.GeneralForm = &CommentForm{
			ID: "cf-general", Action: mrHref(d.name(), n, "comments"), Title: "comment on the merge request",
		}
	}

	l := s.repoLayout(ctx, d, "merge-requests")
	l.Title = p.Number + " " + p.Title + " · " + d.name() + " · " + s.instance
	if head != "" {
		pinRef(&l, target{rev: head, name: m.GetSourceBranch()})
		if !d.hasBranch(m.GetSourceBranch()) {
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
	l.Live, l.LiveURL = true, liveURL("repo:"+d.name(), "view=mr&n="+strconv.FormatInt(n, 10)+"&ref="+url.QueryEscape(m.GetSourceBranch()))
	l.Status = Status{
		Mode: "merge request", Context: p.Number + " · " + p.Source + " → " + p.Target + " · " + p.State,
		Hints: "n next note · d split · u unified · x collapse all · ? help",
	}
	l.HasRail, l.RailSum = true, "merge · checks · commits · files"
	return MergeRequest(l, p), http.StatusOK, nil
}

// hasBranch tells whether the repository has the branch.
func (d *repoData) hasBranch(name string) bool {
	for _, b := range d.branches {
		if b.GetShortName() == name {
			return true
		}
	}
	return false
}

// mergeBox is what the owner can do with an open request.
func (s *Server) mergeBox(d *repoData, n int64, source, head, lastRead string) *MergeBox {
	b := &MergeBox{
		MergeAction: mrHref(d.name(), n, "merge"), CloseAction: mrHref(d.name(), n, "close"),
		ReadAction: mrHref(d.name(), n, "read"), Head: head, HeadGone: head == "",
		CanMarkRead:     head != "" && head != lastRead,
		CanDeleteBranch: source != d.repo.GetDefaultBranch(),
	}
	ff, sq := false, false
	if st := d.repo.GetSettings(); st != nil {
		for _, mode := range st.GetAllowedMergeModes() {
			switch mode {
			case pb.MergeMode_MERGE_MODE_FAST_FORWARD:
				ff = true
				b.Modes = append(b.Modes, ModeOption{Value: "fast-forward", Label: "fast-forward"})
			case pb.MergeMode_MERGE_MODE_SQUASH:
				sq = true
				b.Modes = append(b.Modes, ModeOption{Value: "squash", Label: "squash (one signed commit)"})
			}
		}
	}
	if head != "" {
		cmd := "ermite mr merge " + shellWord(d.name()) + " " + strconv.FormatInt(n, 10) + " --head " + head
		b.CLI = Clone{ID: "cli-merge"}
		if ff {
			b.CLI.Lines = append(b.CLI.Lines, cmd)
		}
		if sq {
			b.CLI.Lines = append(b.CLI.Lines, cmd+" --squash")
		}
	}
	return b
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
	side string
	line int
	path string
}

func parseOn(s string) *lineRef {
	side, rest, ok := strings.Cut(s, ":")
	if !ok || (side != "new" && side != "old") {
		return nil
	}
	num, path, ok := strings.Cut(rest, ":")
	line, err := strconv.Atoi(num)
	if !ok || err != nil || line < 1 || path == "" {
		return nil
	}
	return &lineRef{side: side, line: line, path: path}
}

// placeComments puts every comment under its line when the diff shown has it,
// else in the list below the diff. The comments name lines of the diff against
// the target, so a page that shows another diff (since the last read) lists
// them all below, with their line. A reader who can write gets the line numbers
// as links to the form of their line; on designates the line whose form shows.
func (s *Server) placeComments(p *MRPage, repo string, n int64, comments []*pb.Comment, head string, inline, canWrite bool, on *lineRef) {
	now := s.cfg.Now()
	self := mrHref(repo, n)
	resolveAction := func(id string) string { return mrHref(repo, n, "comments", id, "resolve") }

	type lineKey struct {
		path string
		side pb.DiffSide
		line int
	}
	index := map[lineKey]*DiffLine{}
	fileIDs := map[string]string{}
	for fi := range p.Files {
		f := &p.Files[fi]
		fileIDs[f.Key] = f.ID
		linkable := canWrite && f.Key != "" && !strings.ContainsFunc(f.Key, invisible)
		for hi := range f.Hunks {
			for li := range f.Hunks[hi].Lines {
				ln := &f.Hunks[hi].Lines[li]
				if ln.oldN > 0 && ln.Kind != "add" {
					index[lineKey{f.Key, pb.DiffSide_DIFF_SIDE_OLD, ln.oldN}] = ln
					if linkable {
						ln.OldHref = self + "?on=old:" + itoa(ln.oldN) + ":" + url.QueryEscape(f.Key) + "#" + f.ID
					}
				}
				if ln.newN > 0 && ln.Kind != "del" {
					index[lineKey{f.Key, pb.DiffSide_DIFF_SIDE_NEW, ln.newN}] = ln
					if linkable {
						ln.NewHref = self + "?on=new:" + itoa(ln.newN) + ":" + url.QueryEscape(f.Key) + "#" + f.ID
					}
				}
			}
		}
	}
	find := func(path string, side pb.DiffSide, line int) *DiffLine {
		if !inline {
			return nil
		}
		return index[lineKey{path, side, line}]
	}

	for _, c := range comments {
		v := CommentView{
			ID: c.GetId(), Author: visibleText(c.GetAuthor()),
			Parts: plainParts(c.GetText()), Resolved: c.GetResolved(),
			Outdated: c.GetCommitId() != "" && c.GetCommitId() != head,
		}
		v.When, v.WhenAt = whenOf(now, c.GetCreatedAt())
		if t := c.GetCreatedAt(); t.IsValid() {
			v.Abs = absolute(t.AsTime())
		}
		p.NComments++
		if !c.GetResolved() {
			p.NUnresolved++
		}
		general := c.GetPath() == ""
		var ln *DiffLine
		if !general {
			ln = find(c.GetPath(), c.GetSide(), int(c.GetLine()))
			if ln == nil {
				side := "new"
				if c.GetSide() == pb.DiffSide_DIFF_SIDE_OLD {
					side = "old"
				}
				v.Where = visibleText(c.GetPath()) + ":" + itoa(int(c.GetLine())) + " (" + side + ")"
			}
		}
		if canWrite {
			v.Resolve = &ResolveForm{Action: resolveAction(c.GetId()), Value: !c.GetResolved(), Label: "[resolve]"}
			if c.GetResolved() {
				v.Resolve.Label = "[unresolve]"
			}
		}
		switch {
		case ln != nil:
			ln.Notes = append(ln.Notes, v)
		case general:
			p.General = append(p.General, v)
		default:
			p.Loose = append(p.Loose, v)
		}
	}

	if on != nil {
		side := pb.DiffSide_DIFF_SIDE_NEW
		if on.side == "old" {
			side = pb.DiffSide_DIFF_SIDE_OLD
		}
		if ln := find(on.path, side, on.line); ln != nil {
			ln.Form = &CommentForm{
				ID: "cf-line", Action: mrHref(repo, n, "comments"), Path: on.path, Line: itoa(on.line), Side: on.side,
				Title:  "comment on " + visibleText(on.path) + ":" + itoa(on.line) + " (" + on.side + ")",
				Cancel: self + "#" + fileIDs[on.path], Focus: true,
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
