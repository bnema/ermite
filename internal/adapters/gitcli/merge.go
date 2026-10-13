// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

var _ ports.MergeGit = (*Git)(nil)

// exitCode returns the exit status of a failed git run, or -1 when err is not
// the exit of the process (it did not start, was cancelled, ...).
func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

// parseID parses the id git printed on a line.
func parseID(out string) (domain.ObjectID, error) {
	id, err := domain.ParseObjectID(strings.TrimSpace(out))
	if err != nil {
		return domain.ObjectID{}, fmt.Errorf("git printed %q: %w", strings.TrimSpace(out), err)
	}
	return id, nil
}

// BranchHead resolves refs/heads/<branch>^{commit} with `cat-file`.
//
//hexcheck:ignore suspicious-business-logic-in-adapter maps a branch name to a git object name
func (g *Git) BranchHead(ctx context.Context, repo domain.RepoName, branch string) (domain.ObjectID, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return domain.ObjectID{}, err
	}
	if _, err := domain.ParseBranchName(branch); err != nil {
		return domain.ObjectID{}, fmt.Errorf("branch %q of %q: %w", branch, repo, domain.ErrNotFound)
	}
	var id domain.ObjectID
	err = g.cat.do(ctx, dir, func(c *catConn) error {
		h, err := c.info(headsRefPrefix + branch + "^{commit}")
		if err != nil {
			return err
		}
		id, err = domain.ParseObjectID(h.ID)
		if err != nil {
			return c.fail(err)
		}
		return nil
	})
	if notFound(err) {
		return domain.ObjectID{}, fmt.Errorf("branch %q of %q: %w", branch, repo, domain.ErrNotFound)
	}
	if err != nil {
		return domain.ObjectID{}, fmt.Errorf("branch %q of %q: %w", branch, repo, err)
	}
	return id, nil
}

// HasCommit asks `cat-file` for the type of the object.
func (g *Git) HasCommit(ctx context.Context, repo domain.RepoName, id domain.ObjectID) (bool, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return false, err
	}
	found := false
	err = g.cat.do(ctx, dir, func(c *catConn) error {
		h, err := c.info(id.String())
		if notFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		found = h.Type == "commit"
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("look up commit %s in %q: %w", id, repo, err)
	}
	return found, nil
}

// MergeBase runs `git merge-base`, which exits 1 when there is none.
//
//hexcheck:ignore suspicious-business-logic-in-adapter maps git exit codes to the domain not found error
func (g *Git) MergeBase(ctx context.Context, repo domain.RepoName, a, b domain.ObjectID) (domain.ObjectID, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return domain.ObjectID{}, err
	}
	out, err := g.run(ctx, readArgs(dir, "merge-base", a.String(), b.String())...)
	switch {
	case exitCode(err) == 1:
		return domain.ObjectID{}, fmt.Errorf("%s and %s share no history: %w", a, b, domain.ErrNotFound)
	case err != nil:
		return domain.ObjectID{}, fmt.Errorf("merge base in %q: %w", repo, err)
	}
	return parseID(out)
}

// IsAncestor runs `git merge-base --is-ancestor`, which exits 1 for "no".
func (g *Git) IsAncestor(ctx context.Context, repo domain.RepoName, ancestor, descendant domain.ObjectID) (bool, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return false, err
	}
	_, err = g.run(ctx, readArgs(dir, "merge-base", "--is-ancestor", ancestor.String(), descendant.String())...)
	switch {
	case err == nil:
		return true, nil
	case exitCode(err) == 1:
		return false, nil
	}
	return false, fmt.Errorf("ancestry in %q: %w", repo, err)
}

// ListRange runs `git log <head> ^<exclude>`.
//
//hexcheck:ignore suspicious-business-logic-in-adapter parses git output into domain values; the limit comes from the port
func (g *Git) ListRange(ctx context.Context, repo domain.RepoName, exclude, head domain.ObjectID, limit int) ([]domain.Commit, bool, error) {
	var excluded []string
	if !exclude.IsZero() {
		excluded = []string{"^" + exclude.String()}
	}
	return g.logCommits(ctx, repo, head, limit, nil, excluded)
}

// logCommits runs `git log` for the commits reachable from head: revArgs go
// before the head (options such as --not), excluded after it. Newest first,
// at most limit; more says there were others.
//
//hexcheck:ignore suspicious-business-logic-in-adapter parses git output into domain values; the limit comes from the port
func (g *Git) logCommits(ctx context.Context, repo domain.RepoName, head domain.ObjectID, limit int, revArgs, excluded []string) ([]domain.Commit, bool, error) {
	if limit < 1 || limit > ports.MaxCommitLimit {
		return nil, false, fmt.Errorf("list range: %w: limit %d", domain.ErrInvalidPageSize, limit)
	}
	dir, err := g.repoDir(repo)
	if err != nil {
		return nil, false, err
	}
	args := readArgs(dir, "log", "-z", "-n", strconv.Itoa(limit+1), "--format="+commitFormat)
	args = append(args, revArgs...)
	args = append(args, "--end-of-options", head.String())
	args = append(args, excluded...)
	var (
		commits []domain.Commit
		more    bool
	)
	err = g.pipe(ctx, args, func(r *bufio.Reader) error {
		for {
			c, err := readCommit(r)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if len(commits) == limit {
				more = true
				return errStop
			}
			commits = append(commits, c)
		}
	})
	if err != nil {
		return nil, false, fmt.Errorf("commits of %s in %q: %w", head, repo, err)
	}
	return commits, more, nil
}

// DiffCommits reads the diff between two commits like GetCommit does.
//
//hexcheck:ignore suspicious-business-logic-in-adapter reads a git diff into the domain diff with the limits the port passes in
func (g *Git) DiffCommits(ctx context.Context, repo domain.RepoName, base, head domain.ObjectID, limits domain.DiffLimits) (domain.DiffStats, domain.Diff, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return domain.DiffStats{}, domain.Diff{}, err
	}
	stats, diff, err := g.diffTrees(ctx, dir, base.String(), head.String(), limits)
	if err != nil {
		return domain.DiffStats{}, domain.Diff{}, fmt.Errorf("diff %s..%s in %q: %w", base, head, repo, err)
	}
	return stats, diff, nil
}

// CommitTree runs `git rev-parse <commit>^{tree}`.
//
//hexcheck:ignore suspicious-business-logic-in-adapter parses the id git printed
func (g *Git) CommitTree(ctx context.Context, repo domain.RepoName, id domain.ObjectID) (domain.ObjectID, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return domain.ObjectID{}, err
	}
	out, err := g.run(ctx, readArgs(dir, "rev-parse", "--verify", id.String()+"^{tree}")...)
	if err != nil {
		return domain.ObjectID{}, fmt.Errorf("tree of %s in %q: %w", id, repo, err)
	}
	return parseID(out)
}

// WriteCommit runs `git hash-object -t commit -w --stdin`, which also checks
// that the object is a well-formed commit.
func (g *Git) WriteCommit(ctx context.Context, repo domain.RepoName, object []byte) (domain.ObjectID, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return domain.ObjectID{}, err
	}
	out, err := g.runInput(ctx, bytes.NewReader(object), "--git-dir="+dir, "hash-object", "-t", "commit", "-w", "--stdin")
	if err != nil {
		return domain.ObjectID{}, fmt.Errorf("write commit in %q: %w", repo, err)
	}
	return parseID(out)
}

// UpdateBranch runs `git update-ref refs/heads/<branch> <new> <old>`, which
// takes the ref lock and refuses when the ref is not at old. A failure is
// told from a moved branch by reading the branch again.
//
//hexcheck:ignore suspicious-business-logic-in-adapter maps a refused update-ref to the conflict error of the port
func (g *Git) UpdateBranch(ctx context.Context, repo domain.RepoName, branch string, old, newID domain.ObjectID) error {
	dir, err := g.repoDir(repo)
	if err != nil {
		return err
	}
	if _, err := domain.ParseBranchName(branch); err != nil {
		return fmt.Errorf("update branch of %q: %w", repo, err)
	}
	_, err = g.run(ctx, "--git-dir="+dir, "update-ref", headsRefPrefix+branch, newID.String(), old.String())
	if err == nil {
		return nil
	}
	if cur, herr := g.BranchHead(ctx, repo, branch); herr == nil && cur != old {
		return fmt.Errorf("branch %q of %q is at %s, not %s: %w", branch, repo, cur, old, domain.ErrConflict)
	}
	return fmt.Errorf("update branch %q of %q: %w", branch, repo, err)
}

// DeleteBranch runs `git update-ref -d refs/heads/<branch> <old>`, which takes
// the ref lock and refuses when the ref is not at old. A failure is told from
// a moved or deleted branch by reading the branch again.
//
//hexcheck:ignore suspicious-business-logic-in-adapter maps a refused update-ref to the conflict error of the port
func (g *Git) DeleteBranch(ctx context.Context, repo domain.RepoName, branch string, old domain.ObjectID) error {
	dir, err := g.repoDir(repo)
	if err != nil {
		return err
	}
	if _, err := domain.ParseBranchName(branch); err != nil {
		return fmt.Errorf("delete branch of %q: %w", repo, err)
	}
	_, err = g.run(ctx, "--git-dir="+dir, "update-ref", "-d", headsRefPrefix+branch, old.String())
	if err == nil {
		return nil
	}
	switch cur, herr := g.BranchHead(ctx, repo, branch); {
	case errors.Is(herr, domain.ErrNotFound):
		return fmt.Errorf("branch %q of %q is gone: %w", branch, repo, domain.ErrConflict)
	case herr == nil && cur != old:
		return fmt.Errorf("branch %q of %q is at %s, not %s: %w", branch, repo, cur, old, domain.ErrConflict)
	}
	return fmt.Errorf("delete branch %q of %q: %w", branch, repo, err)
}
