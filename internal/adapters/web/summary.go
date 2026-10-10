// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/a-h/templ"
	"github.com/bnema/zerowrap"

	pb "bnema.dev/ermite-api/gen/ermite"
)

// maxReadmeBytes is the largest README that is rendered in the page; a
// larger one is replaced by a note.
const maxReadmeBytes = 512 << 10

// recentCommits is how many commits the rail of the summary lists.
const recentCommits = 5

// readmeNames are the README file names in order of preference (compared
// lowercase): Markdown first.
var readmeNames = []string{"readme.md", "readme.markdown", "readme", "readme.txt"}

// summary is the page of a repository: description, README, and in the rail
// the clone URL, the facts, the recent commits and the README outline.
func (s *Server) summary(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	if d.head == nil {
		l, st := s.emptyState(ctx, d)
		return State(l, st), http.StatusOK, nil
	}
	ref := d.head.GetShortName()
	now := s.cfg.Now()

	p := SummaryPage{
		Desc:    d.repo.GetDescription(),
		AllHref: repoHref(d.name(), "commits", ref),
		Clone:   Clone{ID: "clone-" + d.name(), Lines: []string{"git clone " + s.cloneURL(d.name())}},
	}

	commits, err := s.cfg.Git.ListCommits(ctx, connect.NewRequest(&pb.ListCommitsRequest{
		Repo: d.name(), Revision: d.head.GetCommitId(), PageSize: recentCommits,
	}))
	if err != nil {
		return nil, 0, err
	}
	branchesAt, tagsAt := namesAt(d.branches), namesAt(d.tags)
	for i, c := range commits.Msg.GetCommits() {
		row := commitRow(d.name(), c, now)
		row.Branches, row.Tags, row.Cursor = branchesAt[c.GetId()], tagsAt[c.GetId()], i == 0
		p.Recent = append(p.Recent, row)
	}

	p.Facts = s.facts(ctx, d, now)

	if rd, err := s.readme(ctx, d, ref); err != nil {
		return nil, 0, err
	} else if rd != nil {
		p.Readme, p.Outline = rd.readme, rd.outline
	}

	l := s.repoLayout(ctx, d, "summary")
	l.Title = d.name() + " · " + s.instance
	l.Live, l.LiveURL = true, liveURL("repo:"+d.name(), "view=summary")
	l.Status = Status{
		Mode: "summary", Context: plural(len(commits.Msg.GetCommits()), "recent commit", "recent commits"),
		Hints: "j/k move · enter open · b ref · y permalink · ? help", Cursor: true, Count: len(p.Recent),
	}
	l.HasRail, l.Read, l.RailSum = true, true, "clone · facts · recent commits · outline"
	return Summary(l, p), http.StatusOK, nil
}

// maxTagDates is the most tags whose commit dates are read to name the
// latest one; with more, the fact shows the count only.
const maxTagDates = 20

// facts are the key/value rows of the rail.
func (s *Server) facts(ctx context.Context, d *repoData, now time.Time) []Fact {
	fs := []Fact{
		lastPushFact(d.repo, now),
		{Key: "branch", Val: d.head.GetShortName() + " · " + plural(len(d.branches), "branch", "branches")},
	}
	if n := len(d.tags); n > 0 {
		val := plural(n, "tag", "tags")
		if n <= maxTagDates {
			if name, c := latestTag(d.tags, s.commitsOf(ctx, d.name(), d.tags)); name != "" {
				val = name + " · " + c.when + " · " + val
			}
		}
		fs = append(fs, Fact{Key: "tag", Val: val})
	}
	if c := d.repo.GetCreatedAt(); c.IsValid() {
		fs = append(fs, Fact{Key: "created", Val: absolute(c.AsTime()), Time: rfc3339(c.AsTime())})
	}
	return fs
}

// lastPushFact is the "last push" fact of a repository; the live updates
// render it again.
func lastPushFact(r *pb.Repo, now time.Time) Fact {
	last := Fact{Key: "last push", Val: "never", ID: "last-push-" + r.GetName()}
	if t := r.GetPushedAt(); t.IsValid() && t.GetSeconds() != 0 {
		last.Val = ago(now, t.AsTime()) + " · " + absolute(t.AsTime())
		last.Time = rfc3339(t.AsTime())
	}
	return last
}

// latestTag is the tag whose commit is the newest.
func latestTag(tags []*pb.Ref, commits map[string]refCommit) (string, refCommit) {
	var name string
	var best refCommit
	for _, t := range tags {
		c, ok := commits[t.GetCommitId()]
		if ok && c.short != "" && (name == "" || c.at > best.at) {
			name, best = t.GetShortName(), c
		}
	}
	return name, best
}

// renderedReadme is the README of a repository, ready for the page.
type renderedReadme struct {
	readme  *Readme
	outline []Heading
}

// readme finds the README at the root of the tree of the head commit, reads
// it and renders it. It is nil when the repository has none. Rendering
// problems degrade to plain text; only API failures are errors.
func (s *Server) readme(ctx context.Context, d *repoData, ref string) (*renderedReadme, error) {
	tree, err := s.cfg.Git.GetTree(ctx, connect.NewRequest(&pb.GetTreeRequest{Repo: d.name(), Revision: d.head.GetCommitId()}))
	if err != nil {
		return nil, err
	}
	return s.readmeIn(ctx, d.name(), d.head.GetCommitId(), ref, "", tree.Msg.GetEntries())
}

// readmeIn picks the README among the entries of the directory dir ("" for
// the root) of the commit rev, reads it and renders it. Links in it are
// relative to dir, on the ref ref. It is nil when the directory has none.
func (s *Server) readmeIn(ctx context.Context, repo, rev, ref, dir string, entries []*pb.TreeEntry) (*renderedReadme, error) {
	var best *pb.TreeEntry
	rank := len(readmeNames)
	for _, e := range entries {
		if e.GetKind() != pb.EntryKind_ENTRY_KIND_FILE && e.GetKind() != pb.EntryKind_ENTRY_KIND_EXECUTABLE {
			continue
		}
		for i, n := range readmeNames {
			if i < rank && strings.EqualFold(e.GetName(), n) {
				best, rank = e, i
			}
		}
	}
	if best == nil {
		return nil, nil
	}
	rd := &Readme{Name: best.GetName()}
	if best.GetSize() > maxReadmeBytes {
		rd.Note = best.GetName() + " is too large to show here (" + humanSize(best.GetSize()) + ")."
		return &renderedReadme{readme: rd}, nil
	}
	body, binary, err := s.readBlob(ctx, repo, rev, best.GetPath(), maxReadmeBytes)
	if err != nil {
		return nil, err
	}
	if binary {
		rd.Note = best.GetName() + " is not text."
		return &renderedReadme{readme: rd}, nil
	}
	name := strings.ToLower(best.GetName())
	if strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".markdown") {
		h, outline, err := s.md.render(body, repo, ref, dir)
		if err == nil {
			rd.HTML = h
			return &renderedReadme{readme: rd, outline: outline}, nil
		}
		log := zerowrap.FromCtx(ctx)
		log.Warn().Err(err).Str("repo", repo).Msg("render README, showing plain text")
	}
	rd.HTML = template.HTML("<pre>" + html.EscapeString(string(body)) + "</pre>") // #nosec G203 -- escaped
	return &renderedReadme{readme: rd}, nil
}

// readBlob reads a file through the GetBlob stream, at most limit bytes. It
// reports a binary file without its content.
func (s *Server) readBlob(ctx context.Context, repo, rev, path string, limit int64) (body []byte, binary bool, err error) {
	stream, err := s.cfg.Git.GetBlob(ctx, connect.NewRequest(&pb.GetBlobRequest{Repo: repo, Revision: rev, Path: path}))
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = stream.Close() }()
	for stream.Receive() {
		m := stream.Msg()
		if m.GetInfo().GetBinary() {
			return nil, true, nil
		}
		body = append(body, m.GetChunk()...)
		if int64(len(body)) > limit {
			return body[:limit], false, nil
		}
	}
	if err := stream.Err(); err != nil {
		return nil, false, err
	}
	return body, false, nil
}

// humanSize formats a size in bytes.
func humanSize(n int64) string {
	switch {
	case n < 1024:
		return itoa(int(n)) + " B"
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}
