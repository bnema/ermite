// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"bnema.dev/ermite/internal/domain"
)

// MergeTree runs `git merge-tree --write-tree -z --name-only`, which merges in
// the object database and touches no ref or index. It prints the tree of the
// result, then on a conflict (exit 1) the NUL-separated conflicted paths.
// Commits with no common history do not merge: not clean, without paths.
//
//hexcheck:ignore suspicious-business-logic-in-adapter maps git exit codes and output to the clean/conflict answer of the port
func (g *Git) MergeTree(ctx context.Context, repo domain.RepoName, base, head domain.ObjectID) (domain.MergeTreeResult, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return domain.MergeTreeResult{}, err
	}
	if _, err := g.MergeBase(ctx, repo, base, head); errors.Is(err, domain.ErrNotFound) {
		return domain.MergeTreeResult{}, nil
	} else if err != nil {
		return domain.MergeTreeResult{}, err
	}
	out, err := g.runCapture(ctx, nil, readArgs(dir, "merge-tree", "--write-tree", "-z", "--name-only", "--no-messages", "--end-of-options", base.String(), head.String())...)
	conflict := exitCode(err) == 1
	if err != nil && !conflict {
		return domain.MergeTreeResult{}, fmt.Errorf("merge %s into %s in %q: %w", head, base, repo, err)
	}
	fields := strings.Split(out, "\x00")
	tree, perr := parseID(fields[0])
	if perr != nil && !conflict {
		return domain.MergeTreeResult{}, perr
	}
	if !conflict {
		return domain.MergeTreeResult{Tree: tree, Clean: true}, nil
	}
	res := domain.MergeTreeResult{}
	for _, p := range fields[1:] {
		if p == "" {
			break
		}
		if len(res.ConflictPaths) == domain.MaxConflictPaths {
			res.Truncated = true
			break
		}
		res.ConflictPaths = append(res.ConflictPaths, p)
	}
	return res, nil
}

// CountBehind runs `git rev-list --count --max-count=max+1 <target> ^<head>`.
//
//hexcheck:ignore suspicious-business-logic-in-adapter applies the cap of the port to the count git printed
func (g *Git) CountBehind(ctx context.Context, repo domain.RepoName, head, target domain.ObjectID, max int) (int, bool, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return 0, false, err
	}
	if max < 0 {
		max = 0
	}
	out, err := g.run(ctx, readArgs(dir, "rev-list", "--count", "--max-count="+strconv.Itoa(max+1), target.String(), "^"+head.String(), "--")...)
	if err != nil {
		return 0, false, fmt.Errorf("count behind %s in %q: %w", target, repo, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, false, fmt.Errorf("git printed %q: %w", strings.TrimSpace(out), err)
	}
	if n > max {
		return max, true, nil
	}
	return n, false, nil
}
