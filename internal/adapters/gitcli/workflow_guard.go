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

var _ ports.WorkflowGuard = (*Git)(nil)

// workflowsDir is where workflow files live, as a literal pathspec directory.
const workflowsDir = ".github/workflows/"

// maxWorkflowPath bounds the one path the guard reads from git: a longer one
// fails closed (the push is refused), whatever the trees hold.
const maxWorkflowPath = 4096

// Bounds of the walk over the commits a push brings: more than that, or a
// commit with more parents than a line of rev-list holds, fails closed (the
// push is refused), whatever the commits hold. maxPushedObjects counts every
// commit and every parent listed, repeats included (so it bounds the distinct
// ids too), and is enforced as the lines are read, before anything more is
// kept: a few thousand commits with two hundred parents each are refused after
// the first lines, not after they have been held.
const (
	maxPushedCommits = 10000
	maxPushedObjects = 4 * maxPushedCommits
	maxRevListLine   = 8192
)

// ChangedWorkflow tells whether a push changes the workflows of a repository.
// It returns the path of the first change, "" for none. Deleting a ref changes
// nothing, and a workflows directory that disappears is not a change.
//
// Two things are judged, as GitHub does for the token of a job (it refuses a
// push when the commits pushed change workflow files, not when a new branch is
// cut from a commit that has them):
//
//  1. Every commit the push brings (`rev-list <new> --not --all`, so the commits
//     no ref has yet) must leave .github/workflows as it found it: it is
//     compared with each of its parents (a root commit with nothing) and is a
//     change when it is present and differs from every one of them. A merge of
//     existing commits is thus no change; a side that brings a change is itself
//     a new commit, and judged as one.
//  2. The directory a ref ends with must not be one it did not have: for an
//     update the final .github/workflows must be the old one or absent, for a
//     new ref the one of the default branch (HEAD) or absent. This keeps a job
//     token from moving a ref onto existing commits that carry other workflows
//     (an older commit, a commit of a branch with different ones).
//
// Presence is the directory the workflow loader lists (CI.workflowFiles, on both
// git readers): .github and workflows both entries of mode 040000 and type
// tree. A path through a gitlink (160000), a symbolic link (120000, which the
// loader does not follow) or a file is no workflows, so the same tree id that is
// no workflows as a gitlink and becomes workflows as a directory is a change,
// which the object id alone would not tell. Mode and id of
// "<commit>:.github/workflows" are what is compared: equal means the same
// content, whatever it holds, so nothing is read.
//
// Nothing is walked recursively: the trees of a push are the pusher's, and a
// recursive diff of trees that share subtrees fans out into more paths than the
// push has bytes (git builds the whole diff before it prints a line, so reading
// it as a stream would not help). The shallow diff that names the path reads one
// directory, as large as its tree object, as a stream, up to its first name, and
// git is stopped and reaped then; a name longer than maxWorkflowPath fails
// closed.
//
// The ends of each update are peeled first (`cat-file --batch-check` on
// `<oid>^{}`): a commit or a tree is compared, a blob (a tag may point to one)
// holds no workflows; a missing or malformed object fails closed. Git reads the
// objects of a push still in quarantine through the environment of the hook
// (ports.PushObjects), accepted only inside the repository's objects directory.
//
//hexcheck:ignore suspicious-business-logic-in-adapter business rule for which pushed commits and ref changes alter workflows; pending move to domain/usecase
func (g *Git) ChangedWorkflow(ctx context.Context, repo domain.RepoName, updates []domain.RefUpdate, objects ports.PushObjects) (string, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return "", err
	}
	extra, err := quarantineEnv(dir, objects)
	if err != nil {
		return "", err
	}
	kept := make([]domain.RefUpdate, 0, len(updates))
	for _, up := range updates {
		if !up.IsDelete() {
			kept = append(kept, up)
		}
	}
	if len(kept) == 0 {
		return "", nil
	}
	types, err := g.batchCheck(ctx, dir, extra, kept, "^{}", "%(objecttype)")
	if err != nil {
		return "", err
	}
	if p, err := g.changedByNewCommits(ctx, dir, extra, kept, types); p != "" || err != nil {
		return p, err
	}
	trees, err := g.batchCheck(ctx, dir, extra, kept, ":"+strings.TrimSuffix(workflowsDir, "/"), "%(objectmode) %(objectname) %(objecttype)")
	if err != nil {
		return "", err
	}
	headW, headRead := "", false
	for i, up := range kept {
		if types[2*i+1] == "blob" {
			continue // a blob holds no workflow, whatever it replaces
		}
		base := workflowsOf(types[2*i], trees[2*i])
		if up.IsCreate() {
			if !headRead {
				w, err := g.workflowsAt(ctx, dir, extra, []string{"HEAD:" + strings.TrimSuffix(workflowsDir, "/")})
				if err != nil {
					return "", err
				}
				headW, headRead = w["HEAD:"+strings.TrimSuffix(workflowsDir, "/")], true
			}
			base = headW // a new ref may end with the workflows of the default branch
		}
		newW := workflowsOf(types[2*i+1], trees[2*i+1])
		if base == newW || newW == "" {
			continue // the same directory, or none left
		}
		return g.firstChanged(ctx, dir, extra, base, newW)
	}
	return "", nil
}

// changedByNewCommits judges the commits the updates bring (rule 1 of
// ChangedWorkflow).
func (g *Git) changedByNewCommits(ctx context.Context, dir string, extra []string, ups []domain.RefUpdate, types []string) (string, error) {
	var tips []string
	seen := map[string]bool{}
	for i, up := range ups {
		if types[2*i+1] == "commit" && !seen[up.New] {
			seen[up.New] = true
			tips = append(tips, up.New)
		}
	}
	if len(tips) == 0 {
		return "", nil
	}
	commits, err := g.newCommits(ctx, dir, extra, tips)
	if err != nil {
		return "", err
	}
	const suffix = ":.github/workflows"
	var specs []string
	asked := map[string]bool{}
	for _, c := range commits {
		for _, id := range append([]string{c.id}, c.parents...) {
			if !asked[id] {
				asked[id] = true
				specs = append(specs, id+suffix)
			}
		}
	}
	ws, err := g.workflowsAt(ctx, dir, extra, specs)
	if err != nil {
		return "", err
	}
	for _, c := range commits {
		cur := ws[c.id+suffix]
		if cur == "" {
			continue // none: nothing new is active
		}
		first, same := "", false
		for i, p := range c.parents {
			if i == 0 {
				first = ws[p+suffix]
			}
			if ws[p+suffix] == cur {
				same = true
			}
		}
		if !same {
			return g.firstChanged(ctx, dir, extra, first, cur)
		}
	}
	return "", nil
}

type pushedCommit struct {
	id      string
	parents []string
}

// newCommits lists the commits reachable from the tips and from no ref
// (`rev-list --parents <tips> --not --all`), at most maxPushedCommits: more
// fails closed.
func (g *Git) newCommits(ctx context.Context, dir string, extra []string, tips []string) ([]pushedCommit, error) {
	args := readArgs(dir, append(append([]string{"rev-list", "--parents", "--max-count=" + strconv.Itoa(maxPushedCommits+1)}, tips...), "--not", "--all")...)
	var out []pushedCommit
	err := g.pipeWith(ctx, extra, nil, args, func(r *bufio.Reader) error {
		var err error
		out, err = parseRevList(r, maxPushedCommits, maxPushedObjects)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("commits of a push: %w", err)
	}
	return out, nil
}

// parseRevList reads the lines of `rev-list --parents` ("<commit> <parent>..."):
// at most maxCommits lines and maxIDs ids in all (a commit and each of its
// parents count, repeats too). A line over maxRevListLine, a limit crossed or an
// id that is not one is an error, and it is raised as soon as the line that
// crosses it is read: nothing past it is read or kept.
func parseRevList(r *bufio.Reader, maxCommits, maxIDs int) ([]pushedCommit, error) {
	var out []pushedCommit
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
		out = append(out, pushedCommit{id: f[0], parents: f[1:]})
	}
}

// workflowsAt returns, for each spec ("<rev>:.github/workflows"), the id of the
// directory it names, or "" when there is none (see workflowsOf).
func (g *Git) workflowsAt(ctx context.Context, dir string, extra []string, specs []string) (map[string]string, error) {
	out := make(map[string]string, len(specs))
	if len(specs) == 0 {
		return out, nil
	}
	args := readArgs(dir, "cat-file", "--batch-check=%(objectmode) %(objectname) %(objecttype)")
	err := g.pipeWith(ctx, extra, strings.NewReader(strings.Join(specs, "\n")+"\n"), args, func(r *bufio.Reader) error {
		for _, spec := range specs {
			line, err := r.ReadSlice('\n')
			if err != nil {
				return fmt.Errorf("read object answers: %w", err)
			}
			out[spec] = workflowsOf("", strings.TrimSuffix(string(line), "\n"))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("workflows of a push: %w", err)
	}
	return out, nil
}

// workflowsOf is the id of the tree that is the .github/workflows of a commit
// or a tree, "" when there is none: the answer for "<it>:.github/workflows" must
// be a tree of mode 040000, and the object peeled to a tree-ish, not a blob.
func workflowsOf(objType, answer string) string {
	if objType == "blob" {
		return ""
	}
	mode, rest, _ := strings.Cut(answer, " ")
	id, typ, _ := strings.Cut(rest, " ")
	if mode != "040000" || typ != "tree" {
		return ""
	}
	return id
}

// firstChanged names the first top-level entry of .github/workflows that
// differs between two trees (oldID may be "": none, the empty tree).
func (g *Git) firstChanged(ctx context.Context, dir string, extra []string, oldID, newID string) (string, error) {
	from := oldID
	if from == "" {
		from = emptyTree
	}
	to := newID
	args := readArgs(dir, "diff-tree", "--name-only", "-z", "--no-renames", "--no-commit-id", "--end-of-options", from, to)
	var name string
	err := g.pipeWith(ctx, extra, nil, args, func(r *bufio.Reader) error {
		n, found, err := firstPath(r)
		name = n
		if err != nil || !found {
			return err // no name: git's exit status is checked, so a failure refuses
		}
		return errStop // the first name is all that is needed: stop git
	})
	if err != nil {
		return "", fmt.Errorf("changed workflows %s..%s: %w", from, to, err)
	}
	if name == "" {
		return strings.TrimSuffix(workflowsDir, "/"), nil
	}
	return workflowsDir + name, nil
}

// firstPath reads the first NUL-terminated path of r; found is false at the end
// of the output without one. A path longer than maxWorkflowPath is an error.
func firstPath(r *bufio.Reader) (path string, found bool, err error) {
	var b []byte
	for {
		c, err := r.ReadByte()
		switch {
		case err != nil:
			return "", false, nil // the end of the output; a failing exit status is reported by pipe
		case c == 0:
			return string(b), true, nil
		case len(b) >= maxWorkflowPath:
			return "", false, fmt.Errorf("%w: a changed path is longer than %d bytes", domain.ErrInvalid, maxWorkflowPath)
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
