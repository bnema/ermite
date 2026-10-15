// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

var _ ports.PullGit = (*Git)(nil)

// TestMerge runs `git merge-tree --write-tree`, which merges in the object
// database and touches no ref or index: it prints the tree of the result and
// exits 0, or exits 1 when the merge conflicts. Commits with no common history
// do not merge (GitHub makes no test merge for them either).
//
//hexcheck:ignore suspicious-business-logic-in-adapter maps git exit codes to the clean/conflict answer of the port
func (g *Git) TestMerge(ctx context.Context, repo domain.RepoName, base, head domain.ObjectID) (domain.ObjectID, bool, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return domain.ObjectID{}, false, err
	}
	if _, err := g.MergeBase(ctx, repo, base, head); errors.Is(err, domain.ErrNotFound) {
		return domain.ObjectID{}, false, nil
	} else if err != nil {
		return domain.ObjectID{}, false, err
	}
	out, err := g.run(ctx, readArgs(dir, "merge-tree", "--write-tree", "--no-messages", "--end-of-options", base.String(), head.String())...)
	switch {
	case exitCode(err) == 1:
		return domain.ObjectID{}, false, nil
	case err != nil:
		return domain.ObjectID{}, false, fmt.Errorf("test merge %s into %s in %q: %w", head, base, repo, err)
	}
	line, _, _ := strings.Cut(out, "\n")
	tree, err := parseID(line)
	if err != nil {
		return domain.ObjectID{}, false, err
	}
	return tree, true, nil
}

// SetPullRef runs `git update-ref` on a ref that domain.IsPullRef accepts, or
// `update-ref -d` for a zero id (deleting a ref that is not there is fine).
//
//hexcheck:ignore suspicious-business-logic-in-adapter checks the ref name with the domain rule before handing it to git
func (g *Git) SetPullRef(ctx context.Context, repo domain.RepoName, ref string, id domain.ObjectID) error {
	if !domain.IsPullRef(ref) {
		return fmt.Errorf("pull ref %q of %q: %w", ref, repo, domain.ErrInvalid)
	}
	dir, err := g.repoDir(repo)
	if err != nil {
		return err
	}
	args := []string{"--git-dir=" + dir, "update-ref"}
	if id.IsZero() {
		args = append(args, "-d", ref)
	} else {
		args = append(args, ref, id.String())
	}
	if _, err := g.run(ctx, args...); err != nil {
		return fmt.Errorf("update pull ref %q of %q: %w", ref, repo, err)
	}
	return nil
}

// PullRef reads a ref that domain.IsPullRef accepts with `git rev-parse`.
//
//hexcheck:ignore suspicious-business-logic-in-adapter checks the ref name with the domain rule before handing it to git
func (g *Git) PullRef(ctx context.Context, repo domain.RepoName, ref string) (domain.ObjectID, error) {
	if !domain.IsPullRef(ref) {
		return domain.ObjectID{}, fmt.Errorf("pull ref %q of %q: %w", ref, repo, domain.ErrNotFound)
	}
	dir, err := g.repoDir(repo)
	if err != nil {
		return domain.ObjectID{}, err
	}
	out, err := g.run(ctx, readArgs(dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")...)
	if exitCode(err) == 1 {
		return domain.ObjectID{}, fmt.Errorf("pull ref %q of %q: %w", ref, repo, domain.ErrNotFound)
	}
	if err != nil {
		return domain.ObjectID{}, fmt.Errorf("read pull ref %q of %q: %w", ref, repo, err)
	}
	return parseID(out)
}
