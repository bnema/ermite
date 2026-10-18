// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// CountCommits runs `git rev-list --count`, from the commit graph when the
// repository has one.
//
//hexcheck:ignore suspicious-business-logic-in-adapter reads the count git prints
func (g *Git) CountCommits(ctx context.Context, repo domain.RepoName, head domain.ObjectID) (int, error) {
	if head.IsZero() {
		return 0, fmt.Errorf("count commits: %w: no head", domain.ErrInvalid)
	}
	dir, err := g.repoDir(repo)
	if err != nil {
		return 0, err
	}
	out, err := g.run(ctx, readArgs(dir, "rev-list", "--count", "--end-of-options", head.String())...)
	if err != nil {
		if exitCode(err) == 128 {
			return 0, fmt.Errorf("count commits of %q: %w", repo, domain.ErrNotFound)
		}
		return 0, fmt.Errorf("count commits of %q: %w", repo, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("count commits of %q: git printed %q", repo, strings.TrimSpace(out))
	}
	return n, nil
}

// CommitTimes runs `git rev-list --since-as-filter --format=%ct`. The filter
// looks at every reachable commit: --since stops the walk at the first older
// one, and misses newer commits behind it when dates are out of order.
//
//hexcheck:ignore suspicious-business-logic-in-adapter reads the dates git prints; the bound comes from the port
func (g *Git) CommitTimes(ctx context.Context, repo domain.RepoName, head domain.ObjectID, since time.Time) ([]time.Time, bool, error) {
	if head.IsZero() {
		return nil, false, fmt.Errorf("commit times: %w: no head", domain.ErrInvalid)
	}
	dir, err := g.repoDir(repo)
	if err != nil {
		return nil, false, err
	}
	args := readArgs(dir, "rev-list", "--no-commit-header", "--format=%ct",
		"--since-as-filter=@"+strconv.FormatInt(since.Unix(), 10), "--end-of-options", head.String())
	var (
		times     []time.Time
		truncated bool
	)
	err = g.pipe(ctx, args, func(r *bufio.Reader) error {
		for {
			line, err := r.ReadString('\n')
			if errors.Is(err, io.EOF) && line == "" {
				return nil
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			sec, perr := strconv.ParseInt(strings.TrimSpace(line), 10, 64)
			if perr != nil {
				return fmt.Errorf("git rev-list: unexpected line %q", truncateText(line))
			}
			if len(times) == ports.MaxCommitTimes {
				truncated = true
				return errStop
			}
			times = append(times, time.Unix(sec, 0).UTC())
		}
	})
	if err != nil {
		if exitCode(err) == 128 {
			return nil, false, fmt.Errorf("commit times of %q: %w", repo, domain.ErrNotFound)
		}
		return nil, false, fmt.Errorf("commit times of %q: %w", repo, err)
	}
	return times, truncated, nil
}

// LatestTag runs `git for-each-ref --sort=-creatordate --count=1` on the tags.
//
//hexcheck:ignore suspicious-business-logic-in-adapter reads the line git prints
func (g *Git) LatestTag(ctx context.Context, repo domain.RepoName) (domain.TagInfo, bool, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return domain.TagInfo{}, false, err
	}
	out, err := g.run(ctx, readArgs(dir, "for-each-ref", "--sort=-creatordate", "--count=1",
		"--format=%(refname:strip=2)%00%(creatordate:unix)%00%(objecttype)%00%(objectname)%00%(*objecttype)%00%(*objectname)", tagsRefPrefix)...)
	if err != nil {
		return domain.TagInfo{}, false, fmt.Errorf("latest tag of %q: %w", repo, err)
	}
	line := strings.TrimSuffix(out, "\n")
	if line == "" {
		return domain.TagInfo{}, false, nil
	}
	f := strings.Split(line, "\x00")
	if len(f) != 6 || f[0] == "" {
		return domain.TagInfo{}, false, fmt.Errorf("latest tag of %q: unexpected line %q", repo, truncateText(line))
	}
	sec, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return domain.TagInfo{}, false, fmt.Errorf("latest tag of %q: unexpected line %q", repo, truncateText(line))
	}
	tag := domain.TagInfo{Name: domain.ValidGitText(f[0]), At: time.Unix(sec, 0).UTC()}
	// A lightweight tag names its commit; an annotated one is peeled to the
	// end of its chain of tags.
	commit := ""
	switch {
	case f[2] == "commit":
		commit = f[3]
	case f[4] == "commit":
		commit = f[5]
	}
	if commit != "" {
		if tag.CommitID, err = domain.ParseObjectID(commit); err != nil {
			return domain.TagInfo{}, false, fmt.Errorf("latest tag of %q: %w", repo, err)
		}
	}
	return tag, true, nil
}
