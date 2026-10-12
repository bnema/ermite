// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"html"
	"html/template"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
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
	ref := d.head.ShortName
	now := s.cfg.Now()

	p := SummaryPage{
		AllHref: repoHref(d.name(), "commits", ref),
		Clone:   Clone{ID: "clone-" + d.name(), Lines: []string{"git clone " + s.cloneURL(d.name())}},
	}

	commits, err := s.commitList(ctx, d.repo.Name, d.head.CommitID.String(), "", recentCommits, "")
	if err != nil {
		return nil, 0, err
	}
	branchesAt, tagsAt := namesAt(d.branches), namesAt(d.tags)
	for i, c := range commits.Commits {
		id := c.ID.String()
		row := commitRow(d.name(), c, now)
		row.Branches, row.Tags, row.Cursor = branchesAt[id], tagsAt[id], i == 0
		p.Recent = append(p.Recent, row)
	}

	p.Facts = s.facts(d, now)
	p.Latest = s.latest(ctx, d)

	if rd, err := s.readme(ctx, d, ref); err != nil {
		return nil, 0, err
	} else if rd != nil {
		p.Readme, p.Outline = rd.readme, rd.outline
	}

	l := s.repoLayout(ctx, d, "summary")
	l.Title = d.name() + " · " + s.instance
	l.Live, l.LiveURL = true, liveURL("repo:"+d.name(), "view=summary")
	l.HasRail = true
	l.Lede = d.repo.Description
	return Summary(l, p), http.StatusOK, nil
}

// maxTagDates is the most tags whose commit dates are read to name the
// latest one; with more, the rail shows the count only.
const maxTagDates = 20

// latest is the newest tag of the repository for the rail, nil without tags.
func (s *Server) latest(ctx context.Context, d *repoData) *LatestTag {
	n := len(d.tags)
	if n == 0 {
		return nil
	}
	lt := &LatestTag{Count: n, RefsHref: repoHref(d.name(), "refs")}
	if n <= maxTagDates {
		if name, c := latestTag(d.tags, s.commitsOf(ctx, d.repo.Name, d.tags)); name != "" {
			lt.Name, lt.Href = name, repoHref(d.name(), "tree", name)
			lt.Subject, lt.When, lt.WhenAt = c.subject, c.when, c.whenAt
		}
	}
	return lt
}

// facts are the key/value rows of the rail.
func (s *Server) facts(d *repoData, now time.Time) []Fact {
	fs := []Fact{
		lastPushFact(d.repo, now),
		{Key: "branch", Val: d.head.ShortName + " · " + plural(len(d.branches), "branch", "branches")},
	}
	if c := d.repo.CreatedAt; !c.IsZero() {
		fs = append(fs, Fact{Key: "created", Val: absolute(c), Time: rfc3339(c)})
	}
	return fs
}

// lastPushFact is the "last push" fact of a repository; the live updates
// render it again.
func lastPushFact(r ports.RepoView, now time.Time) Fact {
	last := Fact{Key: "last push", Val: "never", ID: "last-push-" + r.Name.String()}
	if t := r.PushedAt; !t.IsZero() {
		last.Val = ago(now, t) + " · " + absolute(t)
		last.Time = rfc3339(t)
	}
	return last
}

// latestTag is the tag whose commit is the newest.
func latestTag(tags []domain.Ref, commits map[string]refCommit) (string, refCommit) {
	var name string
	var best refCommit
	for _, t := range tags {
		c, ok := commits[t.CommitID.String()]
		if ok && c.short != "" && (name == "" || c.at > best.at) {
			name, best = t.ShortName, c
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
// problems degrade to plain text; only use case failures are errors.
func (s *Server) readme(ctx context.Context, d *repoData, ref string) (*renderedReadme, error) {
	rev := d.head.CommitID.String()
	tree, err := s.getTree(ctx, d.repo.Name, rev, "", false)
	if err != nil {
		return nil, err
	}
	return s.readmeIn(ctx, d.repo.Name, rev, ref, "", tree.Entries)
}

// readmeIn picks the README among the entries of the directory dir ("" for
// the root) of the commit rev, reads it and renders it. Links in it are
// relative to dir, on the ref ref. It is nil when the directory has none.
//
//hexcheck:ignore suspicious-business-logic-in-adapter picks the README among the entries of a directory to show; display only
func (s *Server) readmeIn(ctx context.Context, repo domain.RepoName, rev, ref, dir string, entries []domain.TreeEntry) (*renderedReadme, error) {
	var best *domain.TreeEntry
	rank := len(readmeNames)
	for i := range entries {
		e := &entries[i]
		if e.Kind != domain.EntryFile && e.Kind != domain.EntryExecutable {
			continue
		}
		for i, n := range readmeNames {
			if i < rank && strings.EqualFold(e.Name, n) {
				best, rank = e, i
			}
		}
	}
	if best == nil {
		return nil, nil
	}
	rd := &Readme{Name: best.Name}
	if best.Size > maxReadmeBytes {
		rd.Note = best.Name + " is too large to show here (" + humanSize(best.Size) + ")."
		return &renderedReadme{readme: rd}, nil
	}
	body, binary, err := s.readBlob(ctx, repo, rev, best.Path, maxReadmeBytes)
	if err != nil {
		return nil, err
	}
	if binary {
		rd.Note = best.Name + " is not text."
		return &renderedReadme{readme: rd}, nil
	}
	name := strings.ToLower(best.Name)
	if strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".markdown") {
		h, outline, err := s.md.render(body, repo.String(), ref, dir)
		if err == nil {
			rd.HTML = h
			return &renderedReadme{readme: rd, outline: outline}, nil
		}
		log := zerowrap.FromCtx(ctx)
		log.Warn().Err(err).Str("repo", repo.String()).Msg("render README, showing plain text")
	}
	rd.HTML = template.HTML("<pre>" + html.EscapeString(string(body)) + "</pre>") // #nosec G203 -- escaped
	return &renderedReadme{readme: rd}, nil
}

// readBlob reads a file, at most limit bytes. It reports a binary file
// without its content.
func (s *Server) readBlob(ctx context.Context, repo domain.RepoName, rev, path string, limit int64) (body []byte, binary bool, err error) {
	info, rc, err := s.openBlobAt(ctx, repo, rev, path)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rc.Close() }()
	if info.Binary {
		return nil, true, nil
	}
	body, err = io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) > limit {
		return body[:limit], false, nil
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
