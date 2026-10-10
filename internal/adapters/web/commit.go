// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/a-h/templ"

	pb "bnema.dev/ermite-api/gen/ermite"
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
	// View is the layout shown: "unified" or "split". Pinned is true when the
	// URL chose it, which then wins over the choice remembered by the browser.
	View   string
	Pinned bool
	// Both is true when both layouts are in the page, and the toggle works in
	// the browser; otherwise only View is sent.
	Both                   bool
	ShowUnified, ShowSplit bool
	UnifiedHref, SplitHref string
	// Invisible is true when some line holds an invisible character, shown as a
	// marker.
	Invisible bool
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
	res, err := s.cfg.Git.GetCommit(ctx, connect.NewRequest(&pb.GetCommitRequest{Repo: d.name(), Revision: sha}))
	if err != nil {
		return nil, 0, err
	}
	c := res.Msg.GetCommit()
	if c == nil {
		return nil, 0, errNotFound
	}
	stats := res.Msg.GetStats()
	now := s.cfg.Now()

	p := CommitPage{
		SHA: c.GetId(), Short: short(c.GetId()),
		Subject: visibleText(c.GetSummary()), Body: commitBody(c),
		Author: person(c.GetAuthor(), now),
		Merge:  len(c.GetParentIds()) > 1,
		Root:   len(c.GetParentIds()) == 0,
		NFiles: int(stats.GetFilesChanged()),
		Adds:   int(stats.GetAdditions()),
		Dels:   int(stats.GetDeletions()),
		Files:  diffFiles(res.Msg.GetDiff().GetFiles()),
		View:   "unified",
	}
	if cm := person(c.GetCommitter(), now); cm != p.Author || c.GetCommitter().GetEmail() != c.GetAuthor().GetEmail() {
		p.Committer = &cm
	}
	for _, id := range c.GetParentIds() {
		p.Parents = append(p.Parents, Link{Label: short(id), Href: repoHref(d.name(), "commit", id)})
	}
	for _, b := range d.branches {
		if b.GetCommitId() == c.GetId() {
			p.Refs = append(p.Refs, b.GetShortName())
		}
	}
	for _, t := range d.tags {
		if t.GetCommitId() == c.GetId() {
			p.Refs = append(p.Refs, "tag: "+t.GetShortName())
		}
	}
	p.Note = diffNote(res.Msg.GetDiff(), len(p.Files), p.NFiles)

	lines := 0
	for _, f := range p.Files {
		for _, h := range f.Hunks {
			lines += len(h.Lines)
			p.Invisible = p.Invisible || slices.ContainsFunc(h.Lines, hasMarker)
		}
	}
	p.Both = lines <= bothViewsLines
	p.ShowUnified, p.ShowSplit = p.Both || p.View == "unified", p.Both || p.View == "split"
	switch r.URL.Query().Get("view") {
	case "split":
		p.View, p.Pinned = "split", true
	case "unified":
		p.Pinned = true
	}
	base := repoHref(d.name(), "commit", c.GetId())
	p.UnifiedHref, p.SplitHref = base+"?view=unified", base+"?view=split"

	l := s.repoLayout(ctx, d, "commits")
	l.Title = p.Short + " " + p.Subject + " · " + d.name() + " · " + s.instance
	l.SHA, l.RefPath = c.GetId(), ""
	history := repoHref(d.name())
	if d.head != nil {
		history = repoHref(d.name(), "commits", d.head.GetShortName())
	}
	l.Crumbs = []Crumb{
		{Label: d.name(), Href: repoHref(d.name())},
		{Label: "commit", Href: history},
		{Label: p.Short, Current: true},
	}
	l.Status = Status{
		Mode: "commit", Context: p.Short + " · " + p.totals(),
		Hints: "d split · u unified · x collapse all · ? help",
	}
	l.HasRail, l.RailSum = true, plural(len(p.Files), "changed file", "changed files")
	return Commit(l, p), http.StatusOK, nil
}

// totals is "3 files · +12 -4".
func (p CommitPage) totals() string {
	return plural(p.NFiles, "file", "files") + " · +" + itoa(p.Adds) + " -" + itoa(p.Dels)
}

// person converts a signature.
func person(s *pb.Signature, now time.Time) Person {
	p := Person{Name: visibleText(s.GetName())}
	if t := s.GetTime(); t.IsValid() {
		p.When, p.WhenAt, p.Abs = ago(now, t.AsTime()), rfc3339(t.AsTime()), absolute(t.AsTime())
	}
	return p
}

// commitBody is the message without its first line, for display.
func commitBody(c *pb.Commit) string {
	msg := strings.ReplaceAll(c.GetMessage(), "\r\n", "\n")
	_, body, _ := strings.Cut(msg, "\n")
	return messageText(strings.TrimSpace(body))
}

// diffNote says what is missing from an incomplete diff, "" for a complete one.
func diffNote(d *pb.Diff, shown, total int) string {
	if !d.GetTruncated() {
		return ""
	}
	var parts []string
	if shown < total {
		parts = append(parts, plural(total-shown, "file is", "files are")+" not listed")
	}
	cut := 0
	for _, f := range d.GetFiles() {
		if f.GetTruncated() {
			cut++
		}
	}
	if cut > 0 {
		parts = append(parts, plural(cut, "file is", "files are")+" too large to show")
	}
	return "this diff is incomplete: " + strings.Join(parts, ", ") + ". the totals above count everything; clone the repository to read the rest."
}
