// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"slices"

	"connectrpc.com/connect"
	"github.com/a-h/templ"

	pb "bnema.dev/ermite-api/gen/ermite"
)

// compareDepth bounds the commits read on each side to find where a branch
// left its target.
const compareDepth = maxComparePages * pageSize

// compare shows a branch against the default branch of the repository: the
// commits it is ahead by and, for the owner, the form that opens a merge
// request.
//
// The API has no "diff between two branches" call outside a merge request, so
// the page shows the diff only when it is exact without one: the branch is
// ahead by a single commit that sits on the tip of the target (that commit's
// own diff is then the whole change). Otherwise it lists the commits, which
// link to their own diffs, and says the whole diff comes with the merge
// request.
func (s *Server) compare(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	src, rest, ok := resolveRef(d.branches, r.PathValue("rest"))
	if !ok || rest != "" || d.head == nil {
		return nil, 0, errNotFound
	}
	tgt := d.head
	name := d.name()

	p := ComparePage{
		Source: visibleText(src.GetShortName()), Target: visibleText(tgt.GetShortName()),
		SourceHref: repoHref(name, "tree", src.GetShortName()), TargetHref: repoHref(name, "tree", tgt.GetShortName()),
		CLI: Clone{ID: "cli-create", Lines: []string{
			"ermite mr create " + shellWord(name) + " --source " + shellWord(src.GetShortName()) + " --title <title>",
		}},
	}

	same := src.GetShortName() == tgt.GetShortName()
	var ahead []*pb.Commit
	exact, behind := false, false
	if same {
		p.Note = "this is the default branch: there is nothing to compare it with."
		p.AheadText = "0 commits"
	} else {
		ahead, exact, behind, err = s.commitsAhead(ctx, name, src.GetCommitId(), tgt.GetCommitId())
		if err != nil {
			return nil, 0, err
		}
		now := s.cfg.Now()
		for _, c := range ahead {
			p.Ahead = append(p.Ahead, commitRow(name, c, now))
		}
		p.AheadText = plural(len(ahead), "commit", "commits")
		if !exact {
			p.AheadText += " or more"
		}
		switch {
		case !exact:
			p.Note = "the branch left " + p.Target + " too long ago to tell where: only the newest commits are listed."
		case len(ahead) == 0:
			p.Note = "nothing to merge: " + p.Target + " already has every commit of " + p.Source + "."
		case behind:
			p.Note = p.Target + " has moved on since the branch started: a merge request can only be merged after " +
				p.Source + " is rebased on " + p.Target + " and pushed again."
		}
	}

	// The exact diff: one commit on top of the tip of the target.
	if exact && !behind && len(ahead) == 1 {
		res, err := s.cfg.Git.GetCommit(ctx, connect.NewRequest(&pb.GetCommitRequest{Repo: name, Revision: ahead[0].GetId()}))
		if err != nil {
			return nil, 0, err
		}
		p.HasDiff = true
		p.Files = diffFiles(res.Msg.GetDiff().GetFiles())
		p.NFiles, p.Adds, p.Dels = int(res.Msg.GetStats().GetFilesChanged()), int(res.Msg.GetStats().GetAdditions()), int(res.Msg.GetStats().GetDeletions())
		if n := diffNote(res.Msg.GetDiff(), len(p.Files), p.NFiles); n != "" {
			p.Note = n
		}
		lines, invisible := diffLines(p.Files)
		p.Invisible = invisible
		self := repoHref(name, "compare", src.GetShortName())
		p.DiffView = newDiffView(r, lines, self+"?view=unified", self+"?view=split")
	} else if len(ahead) > 1 && exact {
		if p.Note == "" {
			p.Note = "the whole diff of the branch is on its merge request; each commit links to its own."
		}
	}

	// An open request for the same pair, or the form to open one.
	var existing *pb.MergeRequest
	if !same {
		existing, err = s.openRequestFor(ctx, name, src.GetShortName(), tgt.GetShortName())
		if err != nil {
			return nil, 0, err
		}
	}
	switch {
	case existing != nil:
		p.Existing = &Link{Label: "!" + itoa(int(existing.GetNumber())) + " " + visibleText(existing.GetTitle()), Href: mrHref(name, existing.GetNumber())}
	case SignedIn(ctx) && !same && len(ahead) > 0:
		title := ""
		if len(ahead) == 1 {
			title = ahead[0].GetSummary()
		} else {
			title = ahead[len(ahead)-1].GetSummary()
		}
		p.Create = &CreateForm{
			Action: mrBase(name), Source: src.GetShortName(), Target: tgt.GetShortName(),
			Title: cutRunes(title, 120),
		}
	}

	l := s.repoLayout(ctx, d, "merge-requests")
	l.Title = "compare " + p.Source + " · " + name + " · " + s.instance
	pinRef(&l, target{rev: src.GetCommitId(), name: src.GetShortName()})
	l.Crumbs[0].Current = false
	l.Crumbs = append(l.Crumbs,
		Crumb{Label: "merge requests", Href: mrBase(name)},
		Crumb{Label: "compare", Current: false},
		Crumb{Label: p.Source, Current: true},
	)
	l.HasRail = true
	return Compare(l, p), http.StatusOK, nil
}

// commitsAhead lists the commits of the source that the target lacks, newest
// first. exact is false when the walk gave up before finding where the source
// meets the target. behind is true when the target has commits the source
// lacks (the tip of the target is not an ancestor of the source).
func (s *Server) commitsAhead(ctx context.Context, repo, srcTip, tgtTip string) (ahead []*pb.Commit, exact, behind bool, err error) {
	if srcTip == tgtTip {
		return nil, true, false, nil
	}
	onTarget := map[string]bool{}
	tokenT := ""
	for len(onTarget) < compareDepth {
		res, err := s.cfg.Git.ListCommits(ctx, connect.NewRequest(&pb.ListCommitsRequest{
			Repo: repo, Revision: tgtTip, PageSize: pageSize, PageToken: tokenT,
		}))
		if err != nil {
			return nil, false, false, err
		}
		for _, c := range res.Msg.GetCommits() {
			onTarget[c.GetId()] = true
		}
		if tokenT = res.Msg.GetNextPageToken(); tokenT == "" {
			break
		}
	}
	token := ""
	for n := 0; n < compareDepth; {
		res, err := s.cfg.Git.ListCommits(ctx, connect.NewRequest(&pb.ListCommitsRequest{
			Repo: repo, Revision: srcTip, PageSize: pageSize, PageToken: token,
		}))
		if err != nil {
			return nil, false, false, err
		}
		for _, c := range res.Msg.GetCommits() {
			n++
			if onTarget[c.GetId()] {
				// The first commit of the source that the target has: where the
				// branch left it. The target moved on when this is not its tip.
				return ahead, true, c.GetId() != tgtTip, nil
			}
			ahead = append(ahead, c)
		}
		if token = res.Msg.GetNextPageToken(); token == "" {
			// The whole history of the source is ahead: no common history.
			return ahead, true, true, nil
		}
	}
	return ahead, false, !slices.ContainsFunc(ahead, func(c *pb.Commit) bool { return c.GetId() == tgtTip }), nil
}

// openRequestFor finds the open merge request with this source and target.
func (s *Server) openRequestFor(ctx context.Context, repo, source, target string) (*pb.MergeRequest, error) {
	token := ""
	for range maxOpenPages {
		res, err := s.cfg.MergeRequests.ListMergeRequests(ctx, connect.NewRequest(&pb.ListMergeRequestsRequest{
			Repo: repo, State: pb.MergeRequestState_MERGE_REQUEST_STATE_OPEN, PageSize: pageSize, PageToken: token,
		}))
		if err != nil {
			return nil, err
		}
		for _, m := range res.Msg.GetMergeRequests() {
			if m.GetSourceBranch() == source && m.GetTargetBranch() == target {
				return m, nil
			}
		}
		if token = res.Msg.GetNextPageToken(); token == "" {
			break
		}
	}
	return nil, nil
}

// cutRunes shortens s to at most n characters.
func cutRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
