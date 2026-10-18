// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// maxRefs bounds the refs one page reads. A repository with more has its
// lists cut, and the pages say so.
const maxRefs = 1000

// repoData is what the pages of one repository start from: its metadata and
// its refs, read once per request.
type repoData struct {
	repo     ports.RepoView
	branches []domain.Ref
	tags     []domain.Ref
	// cut is true when the repository has more refs than maxRefs.
	cut bool
	// head is the branch the repository opens on: its default branch, or the
	// first one when the default does not exist. nil for an empty repository.
	head *domain.Ref
}

// repoView reads a repository as the caller of the request may see it, with
// the Git state of its summary. A missing or private repository is
// domain.ErrNotFound.
func (s *Server) repoView(ctx context.Context, name domain.RepoName) (ports.RepoView, error) {
	repo, err := s.repoRecord(ctx, name)
	if err != nil {
		return ports.RepoView{}, err
	}
	return s.cfg.Repos.View(ctx, repo)
}

// repoRecord reads the repository record only, without Git: its name,
// visibility and settings (the defaults when it has none).
//
//hexcheck:ignore suspicious-business-logic-in-adapter reads the repository from the use case; filling in the default settings for display decides no rule
func (s *Server) repoRecord(ctx context.Context, name domain.RepoName) (domain.Repo, error) {
	repo, err := s.cfg.Repos.Get(ctx, principal(ctx), name)
	if err != nil {
		return domain.Repo{}, err
	}
	if repo.Settings.IsZero() {
		repo.Settings = domain.DefaultRepoSettings()
	}
	return repo, nil
}

// loadRepo reads a repository and its refs. A missing, private or invalidly
// named repository is a domain.ErrNotFound / invalid name error, which the
// caller turns into the 404 page.
func (s *Server) loadRepo(ctx context.Context, rawName string) (*repoData, error) {
	name, err := domain.ParseRepoName(rawName)
	if err != nil {
		return nil, err
	}
	view, err := s.repoView(ctx, name)
	if err != nil {
		return nil, err
	}
	d := &repoData{repo: view}
	// Each page of ListRefs runs the whole for-each-ref again: the refs are
	// asked in one page of up to maxRefs (the use case lowers it to its cap,
	// and the token then leads to the rest).
	token, n := "", 0
	for {
		page, err := s.cfg.Browse.ListRefs(ctx, principal(ctx), name, ports.ListRefsParams{
			Kinds:    []domain.RefKind{domain.RefKindBranch, domain.RefKindTag},
			PageSize: maxRefs, PageToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, r := range page.Refs {
			switch r.Kind {
			case domain.RefKindBranch:
				d.branches = append(d.branches, r)
			case domain.RefKindTag:
				d.tags = append(d.tags, r)
			}
			n++
		}
		if token = page.NextPageToken; token == "" {
			break
		}
		if n >= maxRefs {
			d.cut = true
			break
		}
	}
	for i := range d.branches {
		if d.branches[i].ShortName == d.repo.DefaultBranch {
			d.head = &d.branches[i]
		}
	}
	if d.head == nil && len(d.branches) > 0 {
		d.head = &d.branches[0]
	}
	return d, nil
}

// allRefs is the branches then the tags.
func (d *repoData) allRefs() []domain.Ref {
	return append(append([]domain.Ref(nil), d.branches...), d.tags...)
}

// name is the repository name.
func (d *repoData) name() string { return d.repo.Name.String() }

// openMRCount is the number of open merge requests shown on the tab, empty
// when there are none or they cannot be counted (the tab still works).
func (s *Server) openMRCount(ctx context.Context, name domain.RepoName) string {
	n, err := s.cfg.MergeRequests.CountOpen(ctx, principal(ctx), name)
	if err != nil || n == 0 {
		return ""
	}
	return itoa(n)
}

// layout is the Layout of the pages of a repository: the sections, the ref
// switcher on the head ref and the ":" targets. The caller sets the title,
// the crumbs, the status and the rail.
func (s *Server) repoLayout(ctx context.Context, d *repoData, section string) Layout {
	l := s.base()
	l.Repo = d.name()
	def := d.repo.DefaultBranch
	if d.head != nil {
		def = d.head.ShortName
		l.Ref, l.RefPath = def, def
		l.SHA, l.Short = d.head.CommitID.String(), short(d.head.CommitID.String())
		l.setFinder(l.SHA, def)
	}
	mk := func(key, label, href, id string) Section {
		return Section{Key: key, Label: label, Href: href, Current: section == id}
	}
	l.Sections = []Section{
		mk("s", "Summary", repoHref(d.name()), "summary"),
		mk("t", "Files", repoHref(d.name(), "tree", def), "tree"),
		mk("c", "Commits", repoHref(d.name(), "commits", def), "commits"),
		mk("r", "Branches & tags", repoHref(d.name(), "refs"), "refs"),
	}
	if s.cfg.MergeRequests != nil {
		mrs := mk("m", "Merge requests", mrBase(d.name()), "merge-requests")
		mrs.Count = s.openMRCount(ctx, d.repo.Name)
		l.Sections = append(l.Sections, mrs)
	}
	l.Sections = s.withActions(l.Sections, d.name(), section)
	l.Crumbs = []Crumb{{Label: d.name(), Href: repoHref(d.name()), Current: section == "summary"}}

	l.Cmd = append(l.Cmd, CmdItem{Kind: "go", Label: "home", Href: "/"})
	for _, sec := range l.Sections {
		l.Cmd = append(l.Cmd, CmdItem{Kind: "section", Label: strings.ToLower(sec.Label), Href: sec.Href})
	}
	if l.Ref != "" {
		for _, b := range d.branches {
			it := RefItem{Name: b.ShortName, Href: repoHref(d.name(), "tree", b.ShortName), Current: b.ShortName == d.head.ShortName}
			l.Heads = append(l.Heads, it)
			l.Cmd = append(l.Cmd, CmdItem{Kind: "branch", Label: it.Name, Href: it.Href})
		}
		for _, t := range d.tags {
			it := RefItem{Name: t.ShortName, Href: repoHref(d.name(), "tree", t.ShortName)}
			l.Tags = append(l.Tags, it)
			l.Cmd = append(l.Cmd, CmdItem{Kind: "tag", Label: it.Name, Href: it.Href})
		}
	}
	// Other repositories, when the list is reachable; the page does not
	// depend on it.
	if repos, err := s.listRepos(ctx); err == nil {
		l.Cmd = append(l.Cmd, repoCmds(repos, d.name())...)
	}
	return l
}

// withActions adds the Actions tab when the server runs CI.
func (s *Server) withActions(sections []Section, repo, current string) []Section {
	if s.cfg.Runs == nil {
		return sections
	}
	return append(sections, Section{Key: "a", Label: "Actions", Href: actionsBase(repo), Current: current == "actions"})
}

// cloneURL is the HTTPS clone URL of a repository.
func (s *Server) cloneURL(name string) string {
	return s.cfg.PublicURL + "/" + name + ".git"
}

// refCommit is what a ref shows: the commit it designates.
type refCommit struct {
	short, subject, when, whenAt string
	at                           int64 // unix time, to sort tags
}

// commitsOf reads the head commit of each ref, a few at a time. A ref whose
// commit cannot be read (a tag on a tree, say) has no entry.
func (s *Server) commitsOf(ctx context.Context, repo domain.RepoName, refs []domain.Ref) map[string]refCommit {
	const workers = 8
	var (
		mu  sync.Mutex
		out = make(map[string]refCommit, len(refs))
		sem = make(chan struct{}, workers)
		wg  sync.WaitGroup
	)
	now := s.cfg.Now()
	for _, r := range refs {
		if r.CommitID.IsZero() {
			continue
		}
		id := r.CommitID.String()
		mu.Lock()
		_, done := out[id]
		if !done {
			out[id] = refCommit{} // reserved: one lookup per commit
		}
		mu.Unlock()
		if done {
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			// By id, not by name: a branch and a tag may share a name.
			res, err := s.commitList(ctx, repo, id, "", 1, "")
			var rc refCommit
			if err == nil && len(res.Commits) == 1 {
				row := commitRow(repo.String(), res.Commits[0], now)
				rc.short, rc.subject, rc.when, rc.whenAt = row.Short, row.Subject, row.When, row.WhenAt
				rc.at = unixSeconds(res.Commits[0].Committer.Time)
			}
			mu.Lock()
			out[id] = rc
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// whenOf is the relative and the machine form of a time; both are "" when
// the time is unknown (the zero time).
func whenOf(now time.Time, t time.Time) (when, at string) {
	if t.IsZero() {
		return "", ""
	}
	return ago(now, t), rfc3339(t)
}

// unixSeconds is the Unix time of t in seconds, 0 for the zero time.
func unixSeconds(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// commitRow is the row every list of commits starts from: the short id, the
// subject, the author and the date of the committer, all fit to display. The
// caller adds what is specific to its list.
func commitRow(repo string, c domain.Commit, now time.Time) CommitRow {
	row := CommitRow{
		Short: short(c.ID.String()), Subject: visibleText(c.Summary),
		CHref:  repoHref(repo, "commit", c.ID.String()),
		Author: visibleText(c.Author.Name), Merge: len(c.Parents) > 1,
		Sig: signatureBadge(c.Verification, false),
	}
	row.When, row.WhenAt = whenOf(now, c.Committer.Time)
	return row
}

// namesAt maps commit ids to the names of the refs that designate them.
func namesAt(refs []domain.Ref) map[string][]string {
	m := map[string][]string{}
	for _, r := range refs {
		m[r.CommitID.String()] = append(m[r.CommitID.String()], r.ShortName)
	}
	for _, v := range m {
		sort.Strings(v)
	}
	return m
}

// emptyState is the page of a repository without a branch.
func (s *Server) emptyState(ctx context.Context, d *repoData) (Layout, StatePage) {
	l := s.repoLayout(ctx, d, "summary")
	l.Title = d.name() + " · " + s.instance
	l.Sections = nil
	l.Cmd = slices.DeleteFunc(l.Cmd, func(c CmdItem) bool { return c.Kind == "section" })
	branch := d.repo.DefaultBranch
	return l, StatePage{
		Code: "empty", Title: d.name() + " is empty",
		Lines: []string{"no commits yet. push a branch to create one."},
		Clone: &Clone{ID: "clone-" + d.name(), Lines: []string{
			"git remote add origin " + s.cloneURL(d.name()),
			"git push -u origin " + branch,
		}},
		Back: []Link{{Label: "home", Href: "/"}},
	}
}
