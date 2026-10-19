// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli

import (
	"bufio"
	"context"
	"fmt"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

var _ ports.CIGit = (*Git)(nil)

// emptyTree is the id of the empty tree of a SHA-1 repository, what a diff
// from "nothing" starts at.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// ChangedPaths runs `git diff-tree -r --name-only -z` from base to head, with
// renames not detected (a rename lists both paths, as GitHub's push filters
// see it), and stops reading after max paths: truncated tells there were more.
// A zero base diffs against the empty tree.
func (g *Git) ChangedPaths(ctx context.Context, repo domain.RepoName, base, head domain.ObjectID, limit int) ([]string, bool, error) {
	if limit < 1 || head.IsZero() {
		return nil, false, fmt.Errorf("changed paths: %w: limit %d", domain.ErrInvalid, limit)
	}
	dir, err := g.repoDir(repo)
	if err != nil {
		return nil, false, err
	}
	from := emptyTree
	if !base.IsZero() {
		from = base.String()
	}
	args := readArgs(dir, "diff-tree", "-r", "--name-only", "-z", "--no-renames", "--no-commit-id", "--end-of-options", from, head.String())
	var (
		paths     []string
		truncated bool
	)
	err = g.pipe(ctx, args, func(r *bufio.Reader) error {
		for {
			name, err := r.ReadString(0)
			if err != nil {
				return nil // the end of the output; a failing exit status is reported by pipe
			}
			if len(paths) == limit {
				truncated = true
				return errStop
			}
			paths = append(paths, name[:len(name)-1])
		}
	})
	if err != nil {
		return nil, false, fmt.Errorf("changed paths %s..%s in %q: %w", from, head, repo, err)
	}
	return paths, truncated, nil
}

// CommitsOffBranches lists the commits reachable from head and from no branch
// (refs/heads/*), newest first, at most limit. It runs
// `git log --not --branches --not <head>`: tags do not count, so the commit of
// another tag that no branch has is listed.
func (g *Git) CommitsOffBranches(ctx context.Context, repo domain.RepoName, head domain.ObjectID, limit int) ([]domain.Commit, bool, error) {
	if head.IsZero() {
		return nil, false, fmt.Errorf("commits off branches: %w: no head", domain.ErrInvalid)
	}
	// "--not --branches" excludes every branch, the second "--not" turns the
	// negation off again for the head that follows.
	return g.logCommits(ctx, repo, head, limit, []string{"--not", "--branches", "--not"}, nil)
}
