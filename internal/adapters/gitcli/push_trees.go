// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

var _ ports.PushTreeReader = (*Git)(nil)

const (
	maxDiffPath    = 4096
	maxRevListLine = 8192
)

// PushSubtrees peels push endpoints, walks new commits, and reads entries at path.
func (g *Git) PushSubtrees(ctx context.Context, repo domain.RepoName, updates []domain.RefUpdate, objects ports.PushObjects, path string, bounds domain.PushWalkBounds) (domain.PushSubtrees, error) {
	out := domain.PushSubtrees{Ends: map[string]domain.PushedEnd{}, Subtrees: map[string]domain.Subtree{}}
	dir, err := g.repoDir(repo)
	if err != nil {
		return out, err
	}
	extra, err := quarantineEnv(dir, objects)
	if err != nil {
		return out, err
	}
	var kept []domain.RefUpdate
	for _, up := range updates {
		if up.New != domain.ZeroOID {
			kept = append(kept, up)
		}
	}
	if len(kept) == 0 {
		return out, nil
	}
	return g.readPushSubtrees(ctx, dir, extra, kept, path, bounds, out)
}

// readPushSubtrees batches the endpoint and walked-object cat-file queries.
func (g *Git) readPushSubtrees(ctx context.Context, dir string, extra []string, kept []domain.RefUpdate, path string, bounds domain.PushWalkBounds, out domain.PushSubtrees) (domain.PushSubtrees, error) {
	types, err := g.batchCheck(ctx, dir, extra, kept, "^{}", "%(objecttype)")
	if err != nil {
		return out, err
	}
	tips := pushCommitTips(kept, types, out.Ends)
	if len(tips) != 0 {
		out.Commits, err = g.newCommits(ctx, dir, extra, tips, bounds)
		if err != nil {
			return out, err
		}
	}
	ids := commitAndParentIDs(out.Commits)
	ws, err := g.subtreesAt(ctx, dir, extra, ids, path)
	if err != nil {
		return out, err
	}
	for id, w := range ws {
		out.Subtrees[id] = w
	}
	ids = pushEndSubtreeIDs(kept, out.Ends)
	ws, err = g.subtreesAt(ctx, dir, extra, ids, path)
	if err != nil {
		return out, err
	}
	for id, w := range ws {
		out.Subtrees[id] = w
	}
	return out, nil
}

// pushCommitTips translates peeled endpoint answers and deduplicates rev-list tips.
func pushCommitTips(kept []domain.RefUpdate, types []string, ends map[string]domain.PushedEnd) []string {
	var tips []string
	seen := map[string]bool{}
	for i, up := range kept {
		for j, id := range []string{up.Old, up.New} {
			if id != domain.ZeroOID {
				ends[id] = domain.PushedEnd{Type: domain.ObjectType(types[2*i+j])}
			}
		}
		if types[2*i+1] == "commit" && !seen[up.New] {
			seen[up.New] = true
			tips = append(tips, up.New)
		}
	}
	return tips
}

// commitAndParentIDs deduplicates the object names for a cat-file batch.
func commitAndParentIDs(commits []domain.PushedCommit) []string {
	var ids []string
	asked := map[string]bool{}
	for _, c := range commits {
		for _, id := range append([]domain.ObjectID{c.ID}, c.Parents...) {
			key := id.String()
			if !asked[key] {
				asked[key] = true
				ids = append(ids, key)
			}
		}
	}
	return ids
}

// pushEndSubtreeIDs omits blobs (not tree-ish) and includes HEAD only for creates.
func pushEndSubtreeIDs(kept []domain.RefUpdate, ends map[string]domain.PushedEnd) []string {
	var ids []string
	for _, up := range kept {
		for _, id := range []string{up.Old, up.New} {
			if id != domain.ZeroOID && ends[id].Type != domain.ObjectBlob {
				ids = append(ids, id)
			}
		}
	}
	// HEAD is needed only for a new tree-ish ref. Keep it in the end batch.
	for _, up := range kept {
		if up.Old == domain.ZeroOID && ends[up.New].Type != domain.ObjectBlob {
			ids = append(ids, domain.HeadKey)
			break
		}
	}
	return ids
}

// newCommits lists the commits reachable from the tips and from no ref
// (`rev-list --parents <tips> --not --all`), at most bounds.Commits: more
// fails closed.
func (g *Git) newCommits(ctx context.Context, dir string, extra []string, tips []string, bounds domain.PushWalkBounds) ([]domain.PushedCommit, error) {
	if err := revListLimitsError(bounds); err != nil {
		return nil, err
	}
	args := readArgs(dir, append(append([]string{"rev-list", "--parents", "--max-count=" + strconv.Itoa(bounds.Commits+1)}, tips...), "--not", "--all")...)
	var out []domain.PushedCommit
	err := g.pipeWith(ctx, extra, nil, args, func(r *bufio.Reader) error {
		var err error
		out, err = parseRevList(r, bounds.Commits, bounds.IDs)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("commits of a push: %w", err)
	}
	return out, nil
}

// revListLimitsError checks the numeric range of rev-list and parser limits.
func revListLimitsError(bounds domain.PushWalkBounds) error {
	if bounds.Commits < 0 || bounds.IDs < 0 || bounds.Commits == int(^uint(0)>>1) {
		return fmt.Errorf("%w: invalid push walk bounds", domain.ErrInvalid)
	}
	return nil
}

// parseRevList reads the lines of `rev-list --parents` ("<commit> <parent>..."):
// at most maxCommits lines and maxIDs ids in all (a commit and each of its
// parents count, repeats too). A line over maxRevListLine, a limit crossed or an
// id that is not one is an error, and it is raised as soon as the line that
// crosses it is read: nothing past it is read or kept.
func parseRevList(r *bufio.Reader, maxCommits, maxIDs int) ([]domain.PushedCommit, error) {
	var out []domain.PushedCommit
	ids := 0
	for {
		line, err := r.ReadSlice('\n')
		switch {
		case errors.Is(err, bufio.ErrBufferFull) || len(line) > maxRevListLine:
			return nil, fmt.Errorf("%w: a pushed commit has too many parents", domain.ErrInvalid)
		case errors.Is(err, io.EOF) && len(line) == 0:
			return out, nil
		case err != nil && !errors.Is(err, io.EOF):
			return nil, err
		}
		f := strings.Fields(string(line))
		if len(f) == 0 {
			continue
		}
		if len(out) == maxCommits {
			return nil, fmt.Errorf("%w: the push brings more than %d commits", domain.ErrInvalid, maxCommits)
		}
		if ids += len(f); ids > maxIDs {
			return nil, fmt.Errorf("%w: the push brings commits with more than %d ids (commits and parents) in all", domain.ErrInvalid, maxIDs)
		}
		for _, id := range f {
			if _, err := domain.ParseObjectID(id); err != nil {
				return nil, fmt.Errorf("%w: unexpected rev-list output", domain.ErrInvalid)
			}
		}
		id, _ := domain.ParseObjectID(f[0])
		c := domain.PushedCommit{ID: id}
		for _, p := range f[1:] {
			id, _ := domain.ParseObjectID(p)
			c.Parents = append(c.Parents, id)
		}
		out = append(out, c)
	}
}

// subtreesAt reads the mode, id and type of entries without traversing them.
func (g *Git) subtreesAt(ctx context.Context, dir string, extra []string, ids []string, path string) (map[string]domain.Subtree, error) {
	out := make(map[string]domain.Subtree, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	specs := make([]string, len(ids))
	for i, id := range ids {
		specs[i] = id + ":" + path
	}
	args := readArgs(dir, "cat-file", "--batch-check=%(objectmode) %(objectname) %(objecttype)")
	err := g.pipeWith(ctx, extra, strings.NewReader(strings.Join(specs, "\n")+"\n"), args, func(r *bufio.Reader) error {
		for _, key := range ids {
			line, err := r.ReadSlice('\n')
			if err != nil {
				return fmt.Errorf("read object answers: %w", err)
			}
			f := strings.Fields(string(line))
			if len(f) != 3 {
				continue
			} // missing entry
			mode, err := strconv.ParseUint(f[0], 8, 32)
			if err != nil {
				continue
			}
			id, err := domain.ParseObjectID(f[1])
			if err != nil {
				return err
			}
			kind := domain.EntryKindOf(uint32(mode))
			if kind == domain.EntryDirectory && (f[0] != "040000" || f[2] != "tree") {
				kind = ""
			}
			out[key] = domain.Subtree{Kind: kind, ID: id}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("subtrees of a push: %w", err)
	}
	return out, nil
}

// FirstDifferentEntry streams the first differing top-level entry of two trees.
func (g *Git) FirstDifferentEntry(ctx context.Context, repo domain.RepoName, objects ports.PushObjects, oldID, newID domain.ObjectID) (string, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return "", err
	}
	extra, err := quarantineEnv(dir, objects)
	if err != nil {
		return "", err
	}
	from := oldID.String()
	if from == "" {
		from = emptyTree
	}
	to := newID.String()
	args := readArgs(dir, "diff-tree", "--name-only", "-z", "--no-renames", "--no-commit-id", "--end-of-options", from, to)
	var name string
	err = g.pipeWith(ctx, extra, nil, args, func(r *bufio.Reader) error {
		n, found, err := firstPath(r)
		name = n
		if err != nil || !found {
			return err // no name: git's exit status is checked, so a failure refuses
		}
		return errStop // the first name is all that is needed: stop git
	})
	if err != nil {
		return "", fmt.Errorf("different tree entries %s..%s: %w", from, to, err)
	}
	return name, nil
}

// firstPath reads the first NUL-terminated path of r; found is false at the end
// of the output without one. A path longer than maxDiffPath is an error.
func firstPath(r *bufio.Reader) (path string, found bool, err error) {
	var b []byte
	for {
		c, err := r.ReadByte()
		switch {
		case err != nil:
			return "", false, nil // the end of the output; a failing exit status is reported by pipe
		case c == 0:
			return string(b), true, nil
		case len(b) >= maxDiffPath:
			return "", false, fmt.Errorf("%w: a changed path is longer than %d bytes", domain.ErrInvalid, maxDiffPath)
		}
		b = append(b, c)
	}
}

// batchCheck asks `cat-file --batch-check=<format>` about <oid><suffix> for the
// old (unless the ref is created) and the new end of each update and returns
// the answers in that order, two per update, "" for the old end of a created
// ref. With suffix "^{}" an object that is missing, malformed or neither a
// commit, a tree nor a blob is an error (the push is refused); with any other
// suffix an answer "<input> missing" is kept as the answer.
func (g *Git) batchCheck(ctx context.Context, dir string, extra []string, ups []domain.RefUpdate, suffix, format string) ([]string, error) {
	var in strings.Builder
	want := 0
	for _, up := range ups {
		for _, oid := range []string{up.Old, up.New} {
			if oid == domain.ZeroOID {
				continue
			}
			if _, err := domain.ParseObjectID(oid); err != nil {
				return nil, fmt.Errorf("%w: %v", domain.ErrInvalid, err)
			}
			in.WriteString(oid + suffix + "\n")
			want++
		}
	}
	got := make([]string, 0, want)
	args := readArgs(dir, "cat-file", "--batch-check="+format)
	err := g.pipeWith(ctx, extra, strings.NewReader(in.String()), args, func(r *bufio.Reader) error {
		for range want {
			line, err := r.ReadSlice('\n')
			if err != nil {
				if errors.Is(err, bufio.ErrBufferFull) {
					return fmt.Errorf("%w: unexpected cat-file output", domain.ErrInvalid)
				}
				return fmt.Errorf("read object answers: %w", err)
			}
			got = append(got, strings.TrimSuffix(string(line), "\n"))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("objects of a push: %w", err)
	}
	out := make([]string, 0, 2*len(ups))
	i := 0
	for _, up := range ups {
		for _, oid := range []string{up.Old, up.New} {
			if oid == domain.ZeroOID {
				out = append(out, "")
				continue
			}
			if suffix == "^{}" {
				switch got[i] {
				case "commit", "tree", "blob":
				default: // "<input> missing", or a type that cannot be compared
					return nil, fmt.Errorf("%w: object %s of the push is missing or of an unexpected type", domain.ErrInvalid, oid)
				}
			}
			out = append(out, got[i])
			i++
		}
	}
	return out, nil
}

// quarantineEnv returns the environment that makes git read the objects of a
// push in quarantine. Each directory must exist and, once symbolic links are
// resolved, be inside the objects directory of the repository dir (itself
// resolved): a hook request cannot send git to read anything else. The resolved
// paths are the ones given to git.
func quarantineEnv(repoDir string, o ports.PushObjects) ([]string, error) {
	if o.Dir == "" && o.Alternates == "" {
		return nil, nil
	}
	root, err := filepath.EvalSymlinks(filepath.Join(repoDir, "objects"))
	if err != nil {
		return nil, fmt.Errorf("resolve the objects directory of %s: %w", repoDir, err)
	}
	inside := func(p string) (string, bool) {
		if !filepath.IsAbs(p) {
			return "", false
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			return "", false
		}
		rel, err := filepath.Rel(root, real)
		return real, err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	dir, ok := inside(o.Dir)
	if o.Dir == "" || !ok {
		return nil, fmt.Errorf("%w: the objects directory of the push is outside the repository", domain.ErrInvalid)
	}
	env := []string{"GIT_OBJECT_DIRECTORY=" + dir}
	if o.Alternates != "" {
		var alts []string
		for _, a := range filepath.SplitList(o.Alternates) {
			real, ok := inside(a)
			if !ok {
				return nil, fmt.Errorf("%w: an alternate objects directory of the push is outside the repository", domain.ErrInvalid)
			}
			alts = append(alts, real)
		}
		env = append(env, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+strings.Join(alts, string(filepath.ListSeparator)))
	}
	return env, nil
}
