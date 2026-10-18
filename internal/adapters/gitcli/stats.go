// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"bnema.dev/ermite/internal/domain"
)

// CountCommits runs `git rev-list --count`, from the commit graph when the
// repository has one. A bound uses --since-as-filter, which looks at every
// reachable commit: --since stops the walk at the first older one, and
// misses newer commits behind it when dates are out of order.
//
//hexcheck:ignore suspicious-business-logic-in-adapter reads the count git prints
func (g *Git) CountCommits(ctx context.Context, repo domain.RepoName, head domain.ObjectID, since time.Time) (int, error) {
	if head.IsZero() {
		return 0, fmt.Errorf("count commits: %w: no head", domain.ErrInvalid)
	}
	dir, err := g.repoDir(repo)
	if err != nil {
		return 0, err
	}
	args := []string{"rev-list", "--count"}
	if !since.IsZero() {
		args = append(args, "--since-as-filter=@"+strconv.FormatInt(since.Unix(), 10))
	}
	out, err := g.run(ctx, readArgs(dir, append(args, "--end-of-options", head.String())...)...)
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
