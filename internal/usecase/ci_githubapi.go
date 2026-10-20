// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The REST subset (ADR 0025, CI-2): what the actions that run on ermite call
// on github.api_url. The access rule is the forge's: a repository is read by
// anyone when public, and with repo:read when private, which the principal of a
// job repository token holds on the repository of its run (and the archive
// tokens on theirs). The holder of an archive token reads none of it but its
// own archive.

var _ ports.GitHubAPIService = (*CI)(nil)

// githubRepo looks the repository up as GitHub names it, owner/repo. Another
// owner, a name that is not one and a repository the caller cannot read are all
// not found.
func (c *CI) githubRepo(ctx context.Context, p domain.Principal, owner, name string) (domain.Repo, error) {
	if !domain.OwnerMatches(c.cfg.Owner, owner) {
		return domain.Repo{}, domain.ErrNotFound
	}
	rn, err := domain.ParseRepoName(strings.ToLower(name))
	if err != nil {
		return domain.Repo{}, domain.ErrNotFound
	}
	return c.readableRepo(ctx, p, rn)
}

// GetRepo returns GET /repos/{owner}/{repo}.
//
// The object is the repository of the push event (repoObj: real values where
// ermite has them, GitHub's for a repository without the feature elsewhere)
// with the REST differences: dates are ISO 8601 strings, and the permissions of
// the caller are given, as GitHub does for an authenticated request.
func (c *CI) GetRepo(ctx context.Context, p domain.Principal, owner, name string) ([]byte, error) {
	repo, err := c.githubRepo(ctx, p, owner, name)
	if err != nil {
		return nil, err
	}
	branch, err := c.cfg.Repos.HeadBranch(ctx, repo.Name)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if branch == "" {
		branch = "main"
	}
	u := c.urls(repo)
	created, pushed := repo.CreatedAt, repo.PushedAt
	obj := c.repoObj(repo, u, branch, created)
	obj["created_at"] = created.UTC().Format(timeLayout)
	if pushed.IsZero() {
		pushed = created
	}
	obj["pushed_at"] = pushed.UTC().Format(timeLayout)
	obj["updated_at"] = pushed.UTC().Format(timeLayout)
	delete(obj, "master_branch") // push event only
	delete(obj, "stargazers")
	delete(obj, "watchers")
	delete(obj, "forks")
	delete(obj, "open_issues")
	for k, v := range map[string]any{
		"network_count": 0, "subscribers_count": 0, "allow_squash_merge": repo.Settings.Allows(domain.MergeModeSquash),
		"allow_merge_commit": false, "allow_rebase_merge": repo.Settings.Allows(domain.MergeModeFastForward),
		"allow_auto_merge": repo.EffectiveSettings().AllowAutoMerge, "delete_branch_on_merge": false, "allow_update_branch": false,
		"use_squash_pr_title_as_default": false, "has_pull_requests": true,
	} {
		obj[k] = v
	}
	if !p.IsAnonymous() {
		write := canWrite(p, repo.Name)
		obj["permissions"] = map[string]bool{"admin": canAdmin(p, repo.Name), "maintain": write, "push": write, "triage": write, "pull": true}
	}
	return json.Marshal(obj)
}

// timeLayout is the ISO 8601 form of GitHub's REST dates.
const timeLayout = "2006-01-02T15:04:05Z"

// HashAlgorithm answers GET /repos/{owner}/{repo}/hash-algorithm: checkout asks
// it before it runs git init. ermite repositories are SHA-1.
func (c *CI) HashAlgorithm(ctx context.Context, p domain.Principal, owner, name string) (string, error) {
	if _, err := c.githubRepo(ctx, p, owner, name); err != nil {
		return "", err
	}
	return "sha1", nil
}

// archiveTarget finds the repository and the commit of an archive request and
// checks the access to it. A reader of the repository (the job's own token, any
// principal with repo:read, anyone for a public one) gets any ref. The holder of
// an archive token (domain.RepoTokenPrincipal) gets its commit of its
// repository and nothing else: the ref may be anything that resolves to exactly
// that commit, and every other answer is not found.
func (c *CI) archiveTarget(ctx context.Context, p domain.Principal, owner, name, ref string) (domain.Repo, domain.ObjectID, error) {
	var repo domain.Repo
	grantRepo, grantSha, granted := p.ArchiveGrant()
	if granted {
		if !domain.OwnerMatches(c.cfg.Owner, owner) {
			return domain.Repo{}, domain.ObjectID{}, domain.ErrNotFound
		}
		rn, err := domain.ParseRepoName(strings.ToLower(name))
		if err != nil || rn != grantRepo {
			return domain.Repo{}, domain.ObjectID{}, domain.ErrNotFound
		}
		if repo, err = lookupRepo(ctx, c.cfg.Meta, rn); err != nil {
			return domain.Repo{}, domain.ObjectID{}, err
		}
	} else {
		var err error
		if repo, err = c.githubRepo(ctx, p, owner, name); err != nil {
			return domain.Repo{}, domain.ObjectID{}, err
		}
	}
	rev, err := domain.ParseRevision(ref)
	if err != nil {
		return domain.Repo{}, domain.ObjectID{}, domain.ErrNotFound
	}
	res, err := c.cfg.Reader.ResolveRevision(ctx, repo.Name, rev)
	if err != nil {
		return domain.Repo{}, domain.ObjectID{}, err
	}
	if granted && res.CommitID.String() != grantSha {
		return domain.Repo{}, domain.ObjectID{}, domain.ErrNotFound
	}
	return repo, res.CommitID, nil
}

func (c *CI) archiveDir(repo domain.Repo, commit domain.ObjectID) string {
	return c.cfg.Owner + "-" + repo.Name.String() + "-" + commit.String()[:7]
}

// TarballName answers a HEAD of the archive: the access and the ref are
// checked as for the archive, and the name of the file is returned, without
// running git archive.
func (c *CI) TarballName(ctx context.Context, p domain.Principal, owner, name, ref string) (string, error) {
	repo, commit, err := c.archiveTarget(ctx, p, owner, name, ref)
	if err != nil {
		return "", err
	}
	return c.archiveDir(repo, commit) + ".tar.gz", nil
}

// Tarball streams the archive of a ref as `git archive` makes it, in one
// response (GitHub answers 302 to codeload: ermite serves it itself). The files
// are in a directory <owner>-<repo>-<7 first characters of the commit>, as
// GitHub names it.
func (c *CI) Tarball(ctx context.Context, p domain.Principal, owner, name, ref string, begin func(string) io.Writer) error {
	repo, commit, err := c.archiveTarget(ctx, p, owner, name, ref)
	if err != nil {
		return err
	}
	dir := c.archiveDir(repo, commit)
	return c.cfg.Archive.Archive(ctx, repo.Name, commit, dir, &lazyWriter{begin: func() io.Writer { return begin(dir + ".tar.gz") }})
}

// lazyWriter calls begin at the first write, so that nothing is sent before git
// produced a byte.
type lazyWriter struct {
	begin func() io.Writer
	w     io.Writer
}

func (l *lazyWriter) Write(b []byte) (int, error) {
	if l.w == nil {
		l.w = l.begin()
	}
	return l.w.Write(b)
}
