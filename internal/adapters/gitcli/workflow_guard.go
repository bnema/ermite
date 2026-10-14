// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"path/filepath"
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

// ChangedWorkflow tells whether a push creates or changes a file under
// .github/workflows/. For each update that does not delete its ref, the
// directory .github/workflows of the old commit (none for a new ref) is
// compared with the one of the new commit: the same tree id is the same
// content, whatever it holds, so nothing is read; a different one, or one that
// appears, is a change, and the first top-level entry that differs names it. A
// new ref is thus compared with nothing: any workflow it carries counts, as on
// GitHub. A directory that disappears is not a change (as a deleted ref is
// not).
//
// What counts as the directory is exactly what the workflow loader
// (CI.workflowFiles, on both git readers) lists: .github and workflows both
// entries of mode 040000 and type tree. A path through a gitlink (160000), a
// symbolic link (120000, which the loader does not follow) or a file is no
// workflows, so the same tree id that is no workflows as a gitlink and becomes
// workflows as a directory is a change, which the object id alone would not
// tell (`cat-file` mode and type of "<commit>:.github/workflows").
//
// The comparison never walks the trees recursively: the trees of a push are the
// pusher's, and a recursive diff of trees that share subtrees fans out into
// more paths than the push has bytes (git builds the whole diff before it
// prints a line, so reading it as a stream would not help). The shallow diff
// that names the path reads one directory, as large as its tree object, as a
// stream, up to its first path, and git is stopped and reaped then; a path
// longer than maxWorkflowPath fails closed.
//
// Both ends are peeled first (`cat-file --batch-check` on `<oid>^{}`): a
// commit or a tree is compared, a blob (a tag may point to one) holds no
// workflows; a missing or malformed object fails closed. Git reads the objects
// of a push still in quarantine through the environment of the hook
// (ports.PushObjects), accepted only inside the repository's objects directory.
//
//hexcheck:ignore suspicious-business-logic-in-adapter reads the object ids and the path git prints for the compared trees; the rule that refuses is the use case's
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
	trees, err := g.batchCheck(ctx, dir, extra, kept, ":"+strings.TrimSuffix(workflowsDir, "/"), "%(objectmode) %(objectname) %(objecttype)")
	if err != nil {
		return "", err
	}
	for i := range kept {
		if types[2*i+1] == "blob" {
			continue // a blob holds no workflow, whatever it replaces
		}
		oldW, newW := workflowsOf(types[2*i], trees[2*i]), workflowsOf(types[2*i+1], trees[2*i+1])
		if oldW == newW || newW == "" {
			continue // the same directory, or none left
		}
		return g.firstChanged(ctx, dir, extra, oldW, newW)
	}
	return "", nil
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
