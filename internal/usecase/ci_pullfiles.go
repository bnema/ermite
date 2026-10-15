// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"bnema.dev/ermite/internal/domain"
)

// GET /repos/{owner}/{repo}/pulls/{number}/files, the only REST call that
// dorny/paths-filter@v3 makes on a pull_request (src/main.ts:181-215, `octokit
// .rest.pulls.listFiles` with per_page 100 through paginate.iterator; it reads
// `filename`, `status`, and `previous_filename` for renames, and stops when the
// Link header has no rel="next"). It needs no GET /pulls/{n}: the number comes
// from the event payload.

const (
	// DefaultPullFilesPerPage and MaxPullFilesPerPage are GitHub's page sizes.
	DefaultPullFilesPerPage = 30
	MaxPullFilesPerPage     = 100
	// maxPullFiles is GitHub's cap: "responses include a maximum of 3000 files".
	maxPullFiles = 3000
)

// ListPullFiles implements ports.GitHubAPIService.
func (c *CI) ListPullFiles(ctx context.Context, p domain.Principal, owner, name string, number int64, page, perPage int) ([]byte, string, error) {
	repo, err := c.pullFilesRepo(ctx, p, owner, name)
	if err != nil {
		return nil, "", err
	}
	if c.cfg.MRs == nil || number < 1 {
		return nil, "", domain.ErrNotFound
	}
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = DefaultPullFilesPerPage
	}
	perPage = min(perPage, MaxPullFilesPerPage)
	mr, err := c.cfg.MRs.Get(ctx, repo.Name, number)
	if err != nil {
		return nil, "", err
	}
	files, err := c.pullFiles(ctx, repo.Name, mr)
	if err != nil {
		return nil, "", err
	}
	total := len(files)
	// Bounds by division: page * perPage overflows for a huge page, which would
	// make a negative slice index. A page past the end is empty.
	lo, hi := total, total
	if page-1 <= total/perPage {
		lo = min((page-1)*perPage, total)
		hi = min(lo+perPage, total)
	}
	u := c.urls(repo)
	out := make([]map[string]any, 0, hi-lo)
	for _, f := range files[lo:hi] {
		out = append(out, c.pullFileJSON(u, f))
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, "", err
	}
	return body, pullFilesLink(u.api+"/repos/"+u.full+"/pulls/"+strconv.FormatInt(number, 10)+"/files", page, perPage, total), nil
}

// pullFilesRepo is the access rule of the endpoint. The job token of the
// repository (which has repo:read only with contents: read) is judged by its
// pull-requests permission: with pull-requests: read it reads the merge requests
// of its own repository even when contents is none, as on GitHub; without it a
// private repository answers 403, a public one is read by anyone. A job token
// never reads another repository's merge requests. Every other principal is
// judged by its reach on the repository (githubRepo).
func (c *CI) pullFilesRepo(ctx context.Context, p domain.Principal, owner, name string) (domain.Repo, error) {
	if _, _, archive := p.ArchiveGrant(); archive {
		// An archive token downloads one archive and reads nothing else, not even
		// the merge requests of a public repository.
		return domain.Repo{}, domain.ErrNotFound
	}
	if !p.ViaJobToken() {
		return c.githubRepo(ctx, p, owner, name)
	}
	if !domain.OwnerMatches(c.cfg.Owner, owner) {
		return domain.Repo{}, domain.ErrNotFound
	}
	rn, err := domain.ParseRepoName(strings.ToLower(name))
	if err != nil {
		return domain.Repo{}, domain.ErrNotFound
	}
	repo, err := lookupRepo(ctx, c.cfg.Meta, rn)
	if err != nil {
		return domain.Repo{}, err
	}
	switch {
	case p.JobPermits(domain.PermPullRequests, domain.PermissionRead, rn):
		return repo, nil
	case repo.Visibility == domain.VisibilityPublic:
		return repo, nil
	case canRead(p, repo):
		return domain.Repo{}, fmt.Errorf("%w: the token lacks pull-requests: read", domain.ErrForbidden)
	}
	return domain.Repo{}, domain.ErrNotFound // another repository's token
}

// pullFiles lists the files a merge request changes: between the merge base of
// the target and the head, and the head, as its review shows them (GitHub's
// three-dot diff). A closed or merged request uses the base and head it stored.
func (c *CI) pullFiles(ctx context.Context, repo domain.RepoName, mr domain.MergeRequest) ([]domain.FileDiff, error) {
	head, base := mr.HeadID, mr.BaseID
	if mr.IsOpen() {
		var err error
		if head, err = c.cfg.Git.BranchHead(ctx, repo, mr.Source); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return nil, nil // the source branch is gone: no files
			}
			return nil, err
		}
		tip, err := c.cfg.Git.BranchHead(ctx, repo, mr.Target)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			base = domain.ObjectID{}
		case err != nil:
			return nil, err
		default:
			switch base, err = c.cfg.Git.MergeBase(ctx, repo, tip, head); {
			case errors.Is(err, domain.ErrNotFound):
				base = domain.ObjectID{}
			case err != nil:
				return nil, err
			}
		}
	}
	if head.IsZero() {
		return nil, nil
	}
	if ok, err := c.cfg.Git.HasCommit(ctx, repo, head); err != nil || !ok {
		return nil, err // the reviewed commits are gone
	}
	if base.IsZero() {
		return nil, nil
	}
	// The diff between two commits never changes: the pages of one request (and
	// the requests that follow while it is not pushed) share one computation.
	key := pullFilesKey{repo: repo, base: base, head: head}
	if files, ok := c.pullFileLists.get(key); ok {
		return files, nil
	}
	limits := domain.DiffLimits{MaxFiles: maxPullFiles, MaxFileLines: 2000, MaxTotalLines: 50000, MaxTotalBytes: 8 << 20}
	_, diff, err := c.cfg.Git.DiffCommits(ctx, repo, base, head, limits)
	if err != nil {
		return nil, err
	}
	c.pullFileLists.put(key, diff.Files)
	return diff.Files, nil
}

// pullFileJSON is one element of the answer, in GitHub's schema (the "diff-entry"
// of the REST description).
func (c *CI) pullFileJSON(u ghURLs, f domain.FileDiff) map[string]any {
	name, status := f.NewPath, "modified"
	switch f.Status {
	case domain.FileAdded:
		status = "added"
	case domain.FileDeleted:
		name, status = f.OldPath, "removed"
	case domain.FileRenamed:
		status = "renamed"
	case domain.FileCopied:
		status = "copied"
	case domain.FileTypeChanged:
		status = "changed"
	}
	sha := f.NewID.String()
	if f.NewID.IsZero() {
		sha = f.OldID.String()
	}
	esc := escapeURLPath(name)
	o := map[string]any{
		"sha": sha, "filename": name, "status": status,
		"additions": f.Additions, "deletions": f.Deletions, "changes": f.Additions + f.Deletions,
		"blob_url":     u.repo + "/blob/" + sha + "/" + esc,
		"raw_url":      u.repo + "/raw/" + sha + "/" + esc,
		"contents_url": u.api + "/repos/" + u.full + "/contents/" + esc,
	}
	if f.Status == domain.FileRenamed || f.Status == domain.FileCopied {
		o["previous_filename"] = f.OldPath
	}
	if patch, ok := filePatch(f); ok {
		o["patch"] = patch
	}
	return o
}

func escapeURLPath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// filePatch is the unified-diff hunks of a file, as GitHub's "patch" shows them
// (hunk headers and lines, no file headers). Absent for a binary file, a pure
// rename and a file too large for the diff limits, as on GitHub.
func filePatch(f domain.FileDiff) (string, bool) {
	if f.Binary || f.Truncated || len(f.Hunks) == 0 {
		return "", false
	}
	var b strings.Builder
	for i, h := range f.Hunks {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "@@ -%s +%s @@", hunkRange(h.OldStart, h.OldLines), hunkRange(h.NewStart, h.NewLines))
		if h.Header != "" {
			b.WriteString(" " + h.Header)
		}
		for _, l := range h.Lines {
			b.WriteByte('\n')
			switch l.Kind {
			case domain.LineAddition:
				b.WriteByte('+')
			case domain.LineDeletion:
				b.WriteByte('-')
			default:
				b.WriteByte(' ')
			}
			b.WriteString(l.Text)
			if l.NoNewlineAtEOF {
				b.WriteString("\n\\ No newline at end of file")
			}
		}
	}
	return b.String(), true
}

func hunkRange(start, n int) string {
	if n == 1 {
		return strconv.Itoa(start)
	}
	return strconv.Itoa(start) + "," + strconv.Itoa(n)
}

// pullFilesLink is the Link header of a page: rel next, last, first and prev as
// GitHub writes them; empty when everything fits on one page.
func pullFilesLink(base string, page, perPage, total int) string {
	last := max((total+perPage-1)/perPage, 1)
	if last == 1 {
		return ""
	}
	at := func(n int) string {
		return "<" + base + "?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(n) + ">"
	}
	var parts []string
	if page < last {
		parts = append(parts, at(page+1)+`; rel="next"`, at(last)+`; rel="last"`)
	}
	if page > 1 {
		parts = append(parts, at(1)+`; rel="first"`, at(min(page-1, last))+`; rel="prev"`)
	}
	return strings.Join(parts, ", ")
}
