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

// TagDistance reads at most limit+1 commits from head with `git rev-list
// --max-count`, which also tells a missing head, then streams the tags of
// `git for-each-ref --sort=-creatordate` against them, keeping the tag of the
// nearest commit met: the memory is bounded by the limit, not by the number
// of tags.
func (g *Git) TagDistance(ctx context.Context, repo domain.RepoName, head domain.ObjectID, limit int) (domain.TagDistance, bool, error) {
	if head.IsZero() || limit < 1 {
		return domain.TagDistance{}, false, fmt.Errorf("tag distance: %w: head %s, limit %d", domain.ErrInvalid, head, limit)
	}
	dir, err := g.repoDir(repo)
	if err != nil {
		return domain.TagDistance{}, false, err
	}
	pos := make(map[domain.ObjectID]int, min(limit+1, 1024))
	args := readArgs(dir, "rev-list", "--max-count="+strconv.Itoa(limit+1), "--end-of-options", head.String())
	err = g.pipe(ctx, args, func(r *bufio.Reader) error {
		return eachLine(r, func(line string) error {
			id, perr := domain.ParseObjectID(line)
			if perr != nil {
				return fmt.Errorf("git rev-list: unexpected line %q", truncateText(line))
			}
			pos[id] = len(pos)
			return nil
		})
	})
	if err != nil {
		if exitCode(err) == 128 {
			return domain.TagDistance{}, false, fmt.Errorf("tag distance in %q: %w", repo, domain.ErrNotFound)
		}
		return domain.TagDistance{}, false, fmt.Errorf("tag distance in %q: %w", repo, err)
	}

	var (
		anyTag bool
		best   = -1
		tag    domain.TagInfo
	)
	args = readArgs(dir, "for-each-ref", "--sort=-creatordate", tagFormat, tagsRefPrefix)
	err = g.pipe(ctx, args, func(r *bufio.Reader) error {
		read := 0
		return eachLine(r, func(line string) error {
			if read++; read%tagCheckEvery == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			t, err := parseTagLine(line)
			if err != nil || t.CommitID.IsZero() {
				return err // a tag of a tree or a blob leads to no commit
			}
			anyTag = true
			// The first of a commit is the most recent: a later one only wins
			// when it is nearer.
			if p, ok := pos[t.CommitID]; ok && (best < 0 || p < best) {
				best, tag = p, t
			}
			return nil
		})
	})
	if err != nil {
		return domain.TagDistance{}, false, fmt.Errorf("tags of %q: %w", repo, err)
	}
	switch {
	case !anyTag:
		return domain.TagDistance{}, false, nil
	case best < 0:
		return domain.TagDistance{Commits: min(len(pos), limit), More: len(pos) > limit}, true, nil
	}
	return domain.TagDistance{Commits: best, Tag: tag}, true, nil
}

// tagCheckEvery is how many tags a scan reads between two looks at its
// context.
const tagCheckEvery = 256

// eachLine calls fn with each line of r, without its newline.
func eachLine(r *bufio.Reader, fn func(string) error) error {
	for {
		line, err := r.ReadString('\n')
		if errors.Is(err, io.EOF) && line == "" {
			return nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if ferr := fn(strings.TrimSuffix(line, "\n")); ferr != nil {
			return ferr
		}
	}
}

// tagFormat is the for-each-ref format parseTagLine reads.
const tagFormat = "--format=%(refname:strip=2)%00%(creatordate:unix)%00%(objecttype)%00%(objectname)%00%(*objecttype)%00%(*objectname)"

// parseTagLine reads a line of tagFormat. A lightweight tag names its commit;
// an annotated one is peeled to the end of its chain of tags. CommitID is zero
// for a tag of a tree or a blob, and so is At for a lightweight one, which
// git gives no date.
func parseTagLine(line string) (domain.TagInfo, error) {
	f := strings.Split(line, "\x00")
	if len(f) != 6 || f[0] == "" {
		return domain.TagInfo{}, fmt.Errorf("unexpected line %q", truncateText(line))
	}
	tag := domain.TagInfo{Name: domain.ValidGitText(f[0])}
	commit := ""
	switch {
	case f[2] == "commit":
		commit = f[3]
	case f[4] == "commit":
		commit = f[5]
	}
	if f[1] == "" && commit == "" && f[2] != "tag" {
		return tag, nil // a lightweight tag of a tree or a blob
	}
	sec, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return domain.TagInfo{}, fmt.Errorf("unexpected line %q", truncateText(line))
	}
	tag.At = time.Unix(sec, 0).UTC()
	if commit != "" {
		if tag.CommitID, err = domain.ParseObjectID(commit); err != nil {
			return domain.TagInfo{}, err
		}
	}
	return tag, nil
}

// LatestTag runs `git for-each-ref --sort=-creatordate --count=1` on the tags.
func (g *Git) LatestTag(ctx context.Context, repo domain.RepoName) (domain.TagInfo, bool, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return domain.TagInfo{}, false, err
	}
	out, err := g.run(ctx, readArgs(dir, "for-each-ref", "--sort=-creatordate", "--count=1", tagFormat, tagsRefPrefix)...)
	if err != nil {
		return domain.TagInfo{}, false, fmt.Errorf("latest tag of %q: %w", repo, err)
	}
	line := strings.TrimSuffix(out, "\n")
	if line == "" {
		return domain.TagInfo{}, false, nil
	}
	tag, err := parseTagLine(line)
	if err != nil {
		return domain.TagInfo{}, false, fmt.Errorf("latest tag of %q: %w", repo, err)
	}
	return tag, true, nil
}
