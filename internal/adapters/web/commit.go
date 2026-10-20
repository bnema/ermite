// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/a-h/templ"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// bothViewsLines is the largest diff, in lines, that is sent in both the
// unified and the split layout so that the toggle needs no round trip. A
// larger one is sent in the layout asked for only, and the toggle is a link.
const bothViewsLines = 4000

// Person is an author or a committer with the date of the action.
type Person struct {
	Name, When, WhenAt, Abs string
}

// CommitPage is a commit with its diff.
type CommitPage struct {
	SHA, Short, Subject, Body string
	Author                    Person
	// Committer is nil when it is the author, at the same time.
	Committer *Person
	Parents   []Link
	Merge     bool
	Root      bool
	Refs      []string
	// NFiles, Adds and Dels are the totals of the commit, including the files
	// that the diff omits.
	NFiles, Adds, Dels int
	Files              []DiffFile
	// Note says the diff is incomplete.
	Note string
	DiffView
	// Invisible is true when some line holds an invisible character, shown as a
	// marker.
	Invisible bool
	// Checks is the combined state of the CI checks of the commit.
	Checks ChecksSummary
	// Sig is the signature badge, nil when the commit was not checked.
	Sig *SignatureBadge
	// Verification is what is known of the signing key, for the details.
	Verification domain.CommitVerification
}

// DiffView is how a page lays its diff out: the layouts in the page and the
// way to switch between them. The commit page and the merge request pages
// share it.
type DiffView struct {
	// View is the layout shown: "unified" or "split". Pinned is true when the
	// URL chose it, which then wins over the choice remembered by the browser.
	View   string
	Pinned bool
	// Both is true when both layouts are in the page, and the toggle works in
	// the browser; otherwise only View is sent.
	Both                   bool
	ShowUnified, ShowSplit bool
	UnifiedHref, SplitHref string
}

// newDiffView reads ?view= and decides which layouts a diff of that many
// lines sends; the links switch to the other layout.
func newDiffView(r *http.Request, lines int, unifiedHref, splitHref string) DiffView {
	v := DiffView{View: "unified", UnifiedHref: unifiedHref, SplitHref: splitHref}
	switch r.URL.Query().Get("view") {
	case "split":
		v.View, v.Pinned = "split", true
	case "unified":
		v.Pinned = true
	}
	v.Both = lines <= bothViewsLines
	v.ShowUnified, v.ShowSplit = v.Both || v.View == "unified", v.Both || v.View == "split"
	return v
}

// diffLines counts the lines of the hunks of files, and tells whether some
// line holds a marker.
func diffLines(files []DiffFile) (lines int, invisible bool) {
	for _, f := range files {
		for _, h := range f.Hunks {
			lines += len(h.Lines)
			invisible = invisible || slices.ContainsFunc(h.Lines, hasMarker)
		}
	}
	return lines, invisible
}

// commit shows a commit: its message, its totals, the changed files and the
// diff against the first parent. ?view=split selects the split layout.
func (s *Server) commit(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	sha := r.PathValue("sha")
	if !isCommitID(sha) {
		return nil, 0, errNotFound
	}
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	res, err := s.commitDiff(ctx, r, d.repo.Name, sha)
	if err != nil {
		return nil, 0, err
	}
	c := res.Commit
	cid := c.ID.String()
	now := s.cfg.Now()

	p := CommitPage{
		SHA: cid, Short: short(cid),
		Subject: visibleText(c.Summary), Body: commitBody(c),
		Author: person(c.Author, now),
		Merge:  len(c.Parents) > 1,
		Root:   len(c.Parents) == 0,
		NFiles: res.Stats.FilesChanged,
		Adds:   res.Stats.Additions,
		Dels:   res.Stats.Deletions,
		Files:  s.hl.files(ctx, res.Diff.Files),

		Sig:          signatureBadge(c.Verification, true),
		Verification: c.Verification,
	}
	contextGaps(p.Files, res.Diff.Files, repoHref(d.name(), "context", cid))
	collapseFiles(p.Files, res.Diff.Files, commitLinks(d.name(), cid))
	if cm := person(c.Committer, now); cm != p.Author || c.Committer.Email != c.Author.Email {
		p.Committer = &cm
	}
	for _, pid := range c.Parents {
		id := pid.String()
		p.Parents = append(p.Parents, Link{Label: short(id), Href: repoHref(d.name(), "commit", id)})
	}
	for _, b := range d.branches {
		if b.CommitID.String() == cid {
			p.Refs = append(p.Refs, b.ShortName)
		}
	}
	for _, t := range d.tags {
		if t.CommitID.String() == cid {
			p.Refs = append(p.Refs, "tag: "+t.ShortName)
		}
	}
	p.Note = diffNote(res.Diff, len(p.Files), p.NFiles)
	p.Checks = checksSummary(d.repo.Name, cid, s.checksFor(ctx, d.repo.Name, cid))

	lines, invisible := diffLines(p.Files)
	p.Invisible = invisible
	base := repoHref(d.name(), "commit", cid)
	p.DiffView = newDiffView(r, lines, base+"?view=unified", base+"?view=split")

	l := s.repoLayout(ctx, d, "commits")
	l.Title = p.Short + " " + p.Subject + " · " + d.name() + " · " + s.instance
	pinRef(&l, target{rev: cid, name: p.Short, detached: true})
	history := repoHref(d.name())
	if d.head != nil {
		history = repoHref(d.name(), "commits", d.head.ShortName)
	}
	l.Crumbs = []Crumb{
		{Label: d.name(), Href: repoHref(d.name())},
		{Label: "commit", Href: history},
		{Label: p.Short, Current: true},
	}
	l.HasRail = true
	l.Lazy = true
	return Commit(l, p), http.StatusOK, nil
}

// person converts a signature.
func person(s domain.Signature, now time.Time) Person {
	p := Person{Name: visibleText(s.Name)}
	p.When, p.WhenAt = whenOf(now, s.Time)
	if !s.Time.IsZero() {
		p.Abs = absolute(s.Time)
	}
	return p
}

// commitBody is the message without its first line, for display.
func commitBody(c domain.Commit) string {
	msg := strings.ReplaceAll(c.Message, "\r\n", "\n")
	_, body, _ := strings.Cut(msg, "\n")
	return messageText(strings.TrimSpace(body))
}

// diffNote says what is missing from an incomplete diff, "" for a complete one:
// files past the limit of the listing, files too large for the browser at all.
// Collapsed files say so on their own line and load on their own.
func diffNote(d domain.Diff, shown, total int) string {
	gone := 0
	for _, f := range d.Files {
		if f.Omitted == domain.OmitUnavailable {
			gone++
		}
	}
	var parts []string
	if d.Truncated && shown < total {
		parts = append(parts, plural(total-shown, "file is", "files are")+" not listed")
	}
	if gone > 0 {
		parts = append(parts, plural(gone, "file is", "files are")+" too large to show")
	}
	if len(parts) == 0 {
		return ""
	}
	return "this diff is incomplete: " + strings.Join(parts, ", ") + ". the totals above count everything; clone the repository to read the rest."
}

// commitWindow is how many files of a commit get their hunks read with the
// page; the files past it are collapsed, and load on their own.
const commitWindow = 300

// commitDiff reads a commit for its page (and the compare page): every file
// listed, the hunks of the first commitWindow files read, generated files
// collapsed. Files past the window are collapsed too. With ?file=i (the link
// of a page without scripts) file i is read in full and replaces its entry.
func (s *Server) commitDiff(ctx context.Context, r *http.Request, repo domain.RepoName, rev string) (domain.CommitDetail, error) {
	res, err := s.getCommit(ctx, repo, rev, ports.GetCommitOptions{Files: &ports.FileWindow{From: 0, To: commitWindow}})
	if err != nil {
		return res, err
	}
	for i := commitWindow; i < len(res.Diff.Files); i++ {
		if f := &res.Diff.Files[i]; !f.Binary && f.Omitted == domain.OmitNone && f.Additions+f.Deletions > 0 {
			f.Omitted = domain.OmitBudget
		}
	}
	if i, ok := fileParam(r, len(res.Diff.Files)); ok && res.Diff.Files[i].Omitted.Collapsed() {
		one, err := s.getCommit(ctx, repo, rev, ports.GetCommitOptions{Files: &ports.FileWindow{From: i, To: i + 1, Full: true}})
		if err != nil {
			return res, err
		}
		if i < len(one.Diff.Files) {
			res.Diff.Files[i] = one.Diff.Files[i]
		}
	}
	return res, nil
}

// commitLinks are the addresses a commit page loads its collapsed files from.
func commitLinks(repo, rev string) fileLinks {
	return fileLinks{src: repoHref(repo, "commit", rev, "files"), repo: repo, rev: rev}
}

// commitFiles is GET /<repo>/commit/<sha>/files?one=1&from=i[&view=&layouts=]:
// the one file i of the commit with its hunks, in full, as the one
// details.dfile the page swaps for its placeholder.
func (s *Server) commitFiles(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	sha := r.PathValue("sha")
	i, err := parseOne(r)
	if err != nil || !isCommitID(sha) {
		return nil, 0, errNotFound
	}
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	res, err := s.getCommit(ctx, d.repo.Name, sha, ports.GetCommitOptions{Files: &ports.FileWindow{From: i, To: i + 1, Full: true}})
	if err != nil {
		return nil, 0, err
	}
	if i >= len(res.Diff.Files) {
		return nil, 0, errNotFound
	}
	cid := res.Commit.ID.String()
	raw := res.Diff.Files[i : i+1]
	files := s.hl.files(ctx, raw)
	files[0].ID = "f" + itoa(i+1)
	contextGaps(files, raw, repoHref(d.name(), "context", cid))
	collapseFiles(files, raw, commitLinks(d.name(), cid))
	lines, _ := diffLines(files)
	return MRFiles(MRFilesPage{Files: files, DiffView: fragmentView(r, lines)}), http.StatusOK, nil
}
