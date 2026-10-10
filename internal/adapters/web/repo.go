// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"slices"
	"sort"
	"sync"

	"connectrpc.com/connect"

	pb "bnema.dev/ermite-api/gen/ermite"
)

// maxRefs bounds the refs one page reads. A repository with more has its
// lists cut, and the pages say so.
const maxRefs = 1000

// repoData is what the pages of one repository start from: its metadata and
// its refs, read once per request.
type repoData struct {
	repo     *pb.Repo
	branches []*pb.Ref
	tags     []*pb.Ref
	// cut is true when the repository has more refs than maxRefs.
	cut bool
	// head is the branch the repository opens on: its default branch, or the
	// first one when the default does not exist. nil for an empty repository.
	head *pb.Ref
}

// loadRepo reads a repository and its refs. A missing, private or invalidly
// named repository is a not_found / invalid_argument error of the API, which
// the caller turns into the 404 page.
func (s *Server) loadRepo(ctx context.Context, name string) (*repoData, error) {
	res, err := s.cfg.Repos.GetRepo(ctx, connect.NewRequest(&pb.GetRepoRequest{Repo: name}))
	if err != nil {
		return nil, err
	}
	d := &repoData{repo: res.Msg.GetRepo()}
	token, n := "", 0
	for {
		page, err := s.cfg.Git.ListRefs(ctx, connect.NewRequest(&pb.ListRefsRequest{
			Repo: name, PageSize: pageSize, PageToken: token,
			Kinds: []pb.RefKind{pb.RefKind_REF_KIND_BRANCH, pb.RefKind_REF_KIND_TAG},
		}))
		if err != nil {
			return nil, err
		}
		for _, r := range page.Msg.GetRefs() {
			switch r.GetKind() {
			case pb.RefKind_REF_KIND_BRANCH:
				d.branches = append(d.branches, r)
			case pb.RefKind_REF_KIND_TAG:
				d.tags = append(d.tags, r)
			}
			n++
		}
		if token = page.Msg.GetNextPageToken(); token == "" {
			break
		}
		if n >= maxRefs {
			d.cut = true
			break
		}
	}
	for _, b := range d.branches {
		if b.GetShortName() == d.repo.GetDefaultBranch() {
			d.head = b
		}
	}
	if d.head == nil && len(d.branches) > 0 {
		d.head = d.branches[0]
	}
	return d, nil
}

// name is the repository name.
func (d *repoData) name() string { return d.repo.GetName() }

// layout is the Layout of the pages of a repository: the sections, the ref
// switcher on the head ref and the ":" targets. The caller sets the title,
// the crumbs, the status and the rail.
func (s *Server) repoLayout(ctx context.Context, d *repoData, section string) Layout {
	l := s.base()
	l.Repo = d.name()
	def := d.repo.GetDefaultBranch()
	if d.head != nil {
		def = d.head.GetShortName()
		l.Ref, l.RefPath = def, def
		l.SHA, l.Short = d.head.GetCommitId(), short(d.head.GetCommitId())
	}
	mk := func(key, post, href, id string) Section {
		return Section{Key: key, Post: post, Href: href, Current: section == id}
	}
	l.Sections = []Section{
		mk("s", "ummary", repoHref(d.name()), "summary"),
		mk("t", "ree", repoHref(d.name(), "tree", def), "tree"),
		mk("c", "ommits", repoHref(d.name(), "commits", def), "commits"),
		mk("r", "efs", repoHref(d.name(), "refs"), "refs"),
	}
	l.Crumbs = []Crumb{{Label: d.name(), Href: repoHref(d.name()), Current: section == "summary"}}

	l.Cmd = append(l.Cmd, CmdItem{Kind: "go", Label: "home", Href: "/"})
	for _, sec := range l.Sections {
		l.Cmd = append(l.Cmd, CmdItem{Kind: "section", Label: sec.Key + sec.Post, Href: sec.Href})
	}
	if l.Ref != "" {
		for _, b := range d.branches {
			it := RefItem{Name: b.GetShortName(), Href: repoHref(d.name(), "tree", b.GetShortName()), Current: b == d.head}
			l.Heads = append(l.Heads, it)
			l.Cmd = append(l.Cmd, CmdItem{Kind: "branch", Label: it.Name, Href: it.Href})
		}
		for _, t := range d.tags {
			it := RefItem{Name: t.GetShortName(), Href: repoHref(d.name(), "tree", t.GetShortName())}
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
func (s *Server) commitsOf(ctx context.Context, repo string, refs []*pb.Ref) map[string]refCommit {
	const workers = 8
	var (
		mu  sync.Mutex
		out = make(map[string]refCommit, len(refs))
		sem = make(chan struct{}, workers)
		wg  sync.WaitGroup
	)
	now := s.cfg.Now()
	for _, r := range refs {
		id := r.GetCommitId()
		if id == "" {
			continue
		}
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
			res, err := s.cfg.Git.ListCommits(ctx, connect.NewRequest(&pb.ListCommitsRequest{Repo: repo, Revision: id, PageSize: 1}))
			var rc refCommit
			if err == nil && len(res.Msg.GetCommits()) == 1 {
				c := res.Msg.GetCommits()[0]
				rc.short, rc.subject = short(c.GetId()), c.GetSummary()
				if t := c.GetCommitter().GetTime(); t.IsValid() {
					rc.at = t.GetSeconds()
					rc.when, rc.whenAt = ago(now, t.AsTime()), rfc3339(t.AsTime())
				}
			}
			mu.Lock()
			out[id] = rc
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// namesAt maps commit ids to the names of the refs that designate them.
func namesAt(refs []*pb.Ref) map[string][]string {
	m := map[string][]string{}
	for _, r := range refs {
		m[r.GetCommitId()] = append(m[r.GetCommitId()], r.GetShortName())
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
	l.Status = Status{Mode: "empty", Context: "empty repository · 0 commits", Hints: "g h home · ? help"}
	branch := d.repo.GetDefaultBranch()
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
