// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"bnema.dev/ermite/internal/domain"
)

// MergeTree runs `git merge-tree --write-tree -z --name-only`, which merges in
// the object database and touches no ref or index. It prints the tree of the
// result, then on a conflict (exit 1) the NUL-separated conflicted paths.
// Commits with no common history do not merge: not clean, without paths.
func (g *Git) MergeTree(ctx context.Context, repo domain.RepoName, base, head domain.ObjectID) (domain.MergeTreeResult, error) {
	shared, err := g.sharesHistory(ctx, repo, base, head)
	if err != nil || !shared {
		return domain.MergeTreeResult{}, err
	}
	out, conflict, err := g.mergeTreeOutput(ctx, repo, base, head)
	if err != nil {
		return domain.MergeTreeResult{}, err
	}
	return parseMergeTree(out, conflict)
}

// mergeTreeOutput runs merge-tree and returns what it printed; conflict is its
// exit status 1.
func (g *Git) mergeTreeOutput(ctx context.Context, repo domain.RepoName, base, head domain.ObjectID) (out string, conflict bool, err error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return "", false, err
	}
	out, err = g.runCapture(ctx, nil, readArgs(dir, "merge-tree", "--write-tree", "-z", "--name-only", "--no-messages", "--end-of-options", base.String(), head.String())...)
	conflict = exitCode(err) == 1
	if err != nil && !conflict {
		return "", false, fmt.Errorf("merge %s into %s in %q: %w", head, base, repo, err)
	}
	return out, conflict, nil
}

// sharesHistory reports whether the commits have a merge base.
func (g *Git) sharesHistory(ctx context.Context, repo domain.RepoName, a, b domain.ObjectID) (bool, error) {
	_, err := g.MergeBase(ctx, repo, a, b)
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// parseMergeTree reads the output of merge-tree: the tree, then on a conflict
// the NUL-separated paths, all of them.
func parseMergeTree(out string, conflict bool) (domain.MergeTreeResult, error) {
	fields := strings.Split(out, "\x00")
	tree, err := parseID(fields[0])
	if !conflict {
		return domain.MergeTreeResult{Tree: tree, Clean: true}, err
	}
	paths := fields[1:]
	if i := slices.Index(paths, ""); i >= 0 {
		paths = paths[:i]
	}
	return domain.MergeTreeResult{ConflictPaths: paths}, nil
}

// CountBehind runs `git rev-list --count --max-count=<limit> <target> ^<head>`
// and returns the count git printed.
func (g *Git) CountBehind(ctx context.Context, repo domain.RepoName, head, target domain.ObjectID, limit int) (int, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return 0, err
	}
	out, err := g.run(ctx, readArgs(dir, "rev-list", "--count", "--max-count="+strconv.Itoa(limit), target.String(), "^"+head.String(), "--")...)
	if err != nil {
		return 0, fmt.Errorf("count behind %s in %q: %w", target, repo, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("git printed %q: %w", strings.TrimSpace(out), err)
	}
	return n, nil
}
