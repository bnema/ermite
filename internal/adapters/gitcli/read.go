// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

var _ ports.RepoReader = (*Git)(nil)

const (
	headsRefPrefix = "refs/heads/"
	tagsRefPrefix  = "refs/tags/"
	// maxRefLine bounds a line of for-each-ref.
	maxRefLine = 16 * 1024
)

// notFound reports whether err means a missing object.
func notFound(err error) bool { return errors.Is(err, errObjectMissing) }

// ListRefs lists HEAD, the branches and the tags with `git for-each-ref`.
// An annotated tag is followed to its commit; a tag of a tag is followed with
// one `cat-file` request each.
func (g *Git) ListRefs(ctx context.Context, repo domain.RepoName) ([]domain.Ref, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return nil, err
	}
	type raw struct {
		name, id, typ, derefID, derefType string
	}
	var raws []raw
	args := readArgs(dir, "for-each-ref",
		"--format=%(refname)%00%(objectname)%00%(objecttype)%00%(*objectname)%00%(*objecttype)",
		headsRefPrefix, tagsRefPrefix)
	err = g.pipe(ctx, args, func(r *bufio.Reader) error {
		lr := newLineReader(r, maxRefLine)
		for {
			line, _, err := lr.next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			f := strings.Split(string(line), "\x00")
			if len(f) != 5 {
				return fmt.Errorf("git for-each-ref: unexpected line %q", truncateText(string(line)))
			}
			raws = append(raws, raw{f[0], f[1], f[2], f[3], f[4]})
		}
	})
	if err != nil {
		return nil, fmt.Errorf("list refs of %q: %w", repo, err)
	}

	refs := make([]domain.Ref, 0, len(raws)+1)
	byName := make(map[string]domain.Ref, len(raws))
	var nested []int // indices in refs of tags that point to a tag
	for _, rw := range raws {
		ref := domain.Ref{Name: rw.name}
		switch {
		case strings.HasPrefix(rw.name, headsRefPrefix):
			ref.Kind, ref.ShortName = domain.RefKindBranch, strings.TrimPrefix(rw.name, headsRefPrefix)
		case strings.HasPrefix(rw.name, tagsRefPrefix):
			ref.Kind, ref.ShortName = domain.RefKindTag, strings.TrimPrefix(rw.name, tagsRefPrefix)
		default:
			continue
		}
		if ref.ID, err = domain.ParseObjectID(rw.id); err != nil {
			return nil, fmt.Errorf("list refs of %q: %w", repo, err)
		}
		switch {
		case rw.typ == "commit":
			ref.CommitID = ref.ID
		case rw.typ == "tag" && rw.derefType == "commit":
			if ref.CommitID, err = domain.ParseObjectID(rw.derefID); err != nil {
				return nil, fmt.Errorf("list refs of %q: %w", repo, err)
			}
		case rw.typ == "tag" && rw.derefType == "tag":
			nested = append(nested, len(refs))
		}
		refs = append(refs, ref)
		byName[ref.Name] = ref
	}
	if len(nested) > 0 {
		err := g.cat.do(ctx, dir, func(c *catConn) error {
			for _, i := range nested {
				h, err := c.info(refs[i].Name + "^{commit}")
				if notFound(err) {
					continue
				}
				if err != nil {
					return err
				}
				if refs[i].CommitID, err = domain.ParseObjectID(h.ID); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("follow tags of %q: %w", repo, err)
		}
		for _, i := range nested {
			byName[refs[i].Name] = refs[i]
		}
	}

	head := domain.Ref{Name: "HEAD", Kind: domain.RefKindHead}
	target, err := readHead(dir)
	if err != nil {
		return nil, fmt.Errorf("read HEAD of %q: %w", repo, err)
	}
	if b, ok := byName[target]; ok && target != "" {
		head.ShortName, head.ID, head.CommitID = b.ShortName, b.ID, b.CommitID
	} else if short, ok := strings.CutPrefix(target, headsRefPrefix); ok {
		head.ShortName = short // unborn branch
	}
	return append([]domain.Ref{head}, refs...), nil
}

// readHead returns the ref HEAD of the bare repository in dir points to, or
// "" if HEAD is detached. It reads the HEAD file (the repositories use the
// files ref backend), which is what `git symbolic-ref HEAD` would answer
// without starting a process. A missing file is domain.ErrNotFound.
func readHead(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "HEAD"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", domain.ErrNotFound
		}
		return "", fmt.Errorf("read HEAD: %w", err)
	}
	ref, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "ref: ")
	if !ok {
		return "", nil // a commit id: detached
	}
	return ref, nil
}

// resolve resolves the domain candidates on an open cat-file connection.
func resolve(c *catConn, rev domain.Revision) (domain.ResolvedRevision, error) {
	for _, cand := range rev.Candidates() {
		id, ok, err := lookupRevisionCandidate(c, cand)
		if err != nil {
			return domain.ResolvedRevision{}, err
		}
		if !ok {
			continue
		}
		return domain.ResolvedRevision{CommitID: id, Ref: cand.ResolvedRef()}, nil
	}
	return domain.ResolvedRevision{}, revisionNotFound(rev)
}

func revisionNotFound(rev domain.Revision) error {
	return fmt.Errorf("revision %q: %w", rev, domain.ErrNotFound)
}

// lookupRevisionCandidate asks cat-file to peel a candidate to a commit.
func lookupRevisionCandidate(c *catConn, candidate domain.RevisionCandidate) (domain.ObjectID, bool, error) {
	h, err := c.info(candidate.Name + "^{commit}")
	if notFound(err) {
		return domain.ObjectID{}, false, nil
	}
	if err != nil {
		return domain.ObjectID{}, false, err
	}
	if h.Type != "commit" {
		return domain.ObjectID{}, false, nil
	}
	id, err := domain.ParseObjectID(h.ID)
	if err != nil {
		return domain.ObjectID{}, false, c.fail(err)
	}
	return id, true, nil
}

// ResolveRevision resolves a revision with `cat-file`.
func (g *Git) ResolveRevision(ctx context.Context, repo domain.RepoName, rev domain.Revision) (domain.ResolvedRevision, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return domain.ResolvedRevision{}, err
	}
	var res domain.ResolvedRevision
	err = g.cat.do(ctx, dir, func(c *catConn) (err error) {
		res, err = resolve(c, rev)
		return err
	})
	if err != nil {
		return domain.ResolvedRevision{}, fmt.Errorf("resolve %q in %q: %w", rev, repo, err)
	}
	return res, nil
}

// ResolveRefPath tries the leading segments of path as a revision (branch,
// tag, full ref name, HEAD; a single segment may also be a commit id), longest
// first, with up to one `cat-file` request per candidate on the repository's
// process. At most domain.MaxRefPathTries segments are tried.
func (g *Git) ResolveRefPath(ctx context.Context, repo domain.RepoName, path string) (domain.RefPath, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return domain.RefPath{}, err
	}
	var out domain.RefPath
	err = g.cat.do(ctx, dir, func(c *catConn) (err error) {
		out, err = domain.SplitRefPath(path, func(rev domain.Revision) (domain.ResolvedRevision, error) {
			return resolve(c, rev)
		})
		return err
	})
	if err != nil {
		return domain.RefPath{}, fmt.Errorf("resolve path %q in %q: %w", path, repo, err)
	}
	return out, nil
}

// object is what lookup finds at a path of a revision.
type object struct {
	commit domain.ObjectID
	info   catInfo
}

// lookup resolves rev and reads the header of the object at path, in one
// request series on the repository's process. A missing path is
// domain.ErrNotFound.
//
//hexcheck:ignore suspicious-business-logic-in-adapter translates a root path to Git tree syntax and a non-root path to Git colon syntax, then reads the cat-file header
func (g *Git) lookup(ctx context.Context, dir string, rev domain.Revision, path domain.TreePath) (object, error) {
	var obj object
	err := g.cat.do(ctx, dir, func(c *catConn) error {
		res, err := resolve(c, rev)
		if err != nil {
			return err
		}
		spec := res.CommitID.String() + "^{tree}"
		if !path.IsRoot() {
			spec = res.CommitID.String() + ":" + path.String()
		}
		h, err := c.info(spec)
		if notFound(err) {
			return fmt.Errorf("path %q: %w", path, domain.ErrNotFound)
		}
		if err != nil {
			return err
		}
		obj = object{commit: res.CommitID, info: h}
		return nil
	})
	return obj, err
}

// ListPaths lists the files and symbolic links of a revision with
// `git ls-tree -r -z`, in its order, up to the bounds of the port.
//
//hexcheck:ignore suspicious-business-logic-in-adapter parses git output into domain values; limits and rules come from the port
func (g *Git) ListPaths(ctx context.Context, repo domain.RepoName, rev domain.Revision, maxPaths, maxBytes int) (domain.PathList, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return domain.PathList{}, err
	}
	obj, err := g.lookup(ctx, dir, rev, domain.TreePath{})
	if err != nil {
		return domain.PathList{}, fmt.Errorf("paths of %q: %w", repo, err)
	}
	treeID, err := domain.ParseObjectID(obj.info.ID)
	if err != nil {
		return domain.PathList{}, fmt.Errorf("paths of %q: %w", repo, err)
	}
	list := domain.PathList{CommitID: obj.commit, TreeID: treeID}
	err = g.pipe(ctx, readArgs(dir, "ls-tree", "-r", "-z", treeID.String()), func(r *bufio.Reader) error {
		for {
			tok, err := readToken(r)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			meta, path, ok := strings.Cut(tok, "\t")
			if !ok {
				return fmt.Errorf("malformed ls-tree entry %q", tok)
			}
			// meta is "<mode> <type> <id>": a submodule is a commit.
			_, rest, _ := strings.Cut(meta, " ")
			if typ, _, _ := strings.Cut(rest, " "); typ == "commit" {
				continue
			}
			// A copy: path would keep the whole "<mode> <type> <id>\t" entry alive.
			if !list.Add(strings.Clone(path), maxPaths, maxBytes) {
				return errStop
			}
		}
	})
	if err != nil {
		return domain.PathList{}, fmt.Errorf("paths of %q: %w", repo, err)
	}
	return list, nil
}

var _ ports.TreeFileLister = (*Git)(nil)

// ListFileSizes lists the regular files of a commit with their sizes, with one
// `git ls-tree -r -l -z`, in tree order. Symbolic links and submodules are
// skipped. At most limit files are returned; Truncated tells there were more.
func (g *Git) ListFileSizes(ctx context.Context, repo domain.RepoName, commit domain.ObjectID, limit int) (domain.FileSizes, error) {
	if limit < 1 || commit.IsZero() {
		return domain.FileSizes{}, fmt.Errorf("file sizes: %w: limit %d", domain.ErrInvalid, limit)
	}
	dir, err := g.repoDir(repo)
	if err != nil {
		return domain.FileSizes{}, err
	}
	var out domain.FileSizes
	err = g.pipe(ctx, readArgs(dir, "ls-tree", "-r", "-l", "-z", "--end-of-options", commit.String()), func(r *bufio.Reader) error {
		for {
			tok, err := readToken(r)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			e, err := parseTreeEntry(tok, domain.TreePath{})
			if err != nil {
				return err
			}
			if e.Mode != 0o100644 && e.Mode != 0o100755 {
				continue
			}
			if len(out.Files) == limit {
				out.Truncated = true
				return errStop
			}
			// A copy: the name would keep the whole entry alive.
			out.Files = append(out.Files, domain.FileSize{Path: strings.Clone(e.Path), Size: e.Size})
		}
	})
	if err != nil {
		return domain.FileSizes{}, fmt.Errorf("file sizes of %q at %s: %w", repo, commit, err)
	}
	return out, nil
}

// GetTree lists a directory with `git ls-tree -z -l`.
//
//hexcheck:ignore suspicious-business-logic-in-adapter parses git output into domain values; limits and rules come from the port
func (g *Git) GetTree(ctx context.Context, repo domain.RepoName, rev domain.Revision, path domain.TreePath, includeLast bool) (domain.Tree, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return domain.Tree{}, err
	}
	obj, err := g.lookup(ctx, dir, rev, path)
	if err != nil {
		return domain.Tree{}, fmt.Errorf("tree %q of %q: %w", path, repo, err)
	}
	if obj.info.Type != "tree" {
		return domain.Tree{}, fmt.Errorf("tree %q of %q: %w", path, repo, domain.ErrNotDirectory)
	}
	if !path.IsRoot() && obj.info.Mode != 0o040000 {
		// A submodule entry whose commit id names a tree the repository has:
		// no directory of this repository, as for ermite-git.
		return domain.Tree{}, fmt.Errorf("tree %q of %q: %w", path, repo, domain.ErrNotFound)
	}
	treeID, err := domain.ParseObjectID(obj.info.ID)
	if err != nil {
		return domain.Tree{}, err
	}
	tree := domain.Tree{CommitID: obj.commit, ID: treeID}

	err = g.pipe(ctx, readArgs(dir, "ls-tree", "-z", "-l", treeID.String()), func(r *bufio.Reader) error {
		for {
			tok, err := readToken(r)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			e, err := parseTreeEntry(tok, path)
			if err != nil {
				return err
			}
			tree.Entries = append(tree.Entries, e)
		}
	})
	if err != nil {
		return domain.Tree{}, fmt.Errorf("list tree %q of %q: %w", path, repo, err)
	}
	domain.SortTreeEntries(tree.Entries)
	tree.BoundTree()
	if includeLast {
		if err := g.fillLastCommits(ctx, dir, &tree); err != nil {
			return domain.Tree{}, fmt.Errorf("last commits of %q in %q: %w", path, repo, err)
		}
	}
	return tree, nil
}

// parseTreeEntry parses "<mode> <type> <id> <size>\t<name>" of ls-tree -l.
func parseTreeEntry(rec string, parent domain.TreePath) (domain.TreeEntry, error) {
	meta, name, ok := strings.Cut(rec, "\t")
	f := strings.Fields(meta)
	if !ok || len(f) != 4 || name == "" {
		return domain.TreeEntry{}, fmt.Errorf("git ls-tree: unexpected entry %q", truncateText(rec))
	}
	mode, err := strconv.ParseUint(f[0], 8, 32)
	if err != nil {
		return domain.TreeEntry{}, fmt.Errorf("git ls-tree: mode of %q: %w", truncateText(rec), err)
	}
	id, err := domain.ParseObjectID(f[2])
	if err != nil {
		return domain.TreeEntry{}, fmt.Errorf("git ls-tree: %w", err)
	}
	var size int64
	if f[3] != "-" {
		if size, err = strconv.ParseInt(f[3], 10, 64); err != nil {
			return domain.TreeEntry{}, fmt.Errorf("git ls-tree: size of %q: %w", truncateText(rec), err)
		}
	}
	kind := domain.EntryKindOf(uint32(mode))
	if kind == "" {
		return domain.TreeEntry{}, fmt.Errorf("git ls-tree: unknown mode %s of %q", f[0], name)
	}
	return domain.TreeEntry{
		Name: name, Path: parent.Join(name), Kind: kind, Mode: uint32(mode), ID: id, Size: size,
	}, nil
}

// fillLastCommits sets the last commit of the first MaxLastCommitEntries
// entries with `git log -1 <commit> -- <path>`, run by a small pool of
// workers: min(4, max(1, MaxExec/2)) at most. Git answers each from the
// commit-graph and its changed-path filters when they exist. Every command
// takes a slot of the exec semaphore on its own (the workers hold none), so a
// large directory cannot use more than its share of the processes the server
// may run. Each worker writes its own entry, so the result does not depend on
// the scheduling; the first error stops the others and is returned. Entries
// beyond the bound are left without one and the tree says so.
//
//hexcheck:ignore suspicious-business-logic-in-adapter fills the tree with git output; the bound comes from the port
func (g *Git) fillLastCommits(ctx context.Context, dir string, tree *domain.Tree) error {
	n := len(tree.Entries)
	if n > domain.MaxLastCommitEntries {
		n = domain.MaxLastCommitEntries
		tree.LastCommitsOmitted = true
	}
	workers := min(4, max(1, cap(g.exec)/2), n)
	if workers == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	next := make(chan int)
	for range workers {
		wg.Go(func() {
			for i := range next {
				c, err := g.lastCommit(ctx, dir, tree.CommitID, tree.Entries[i].Path)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
						cancel()
					}
					mu.Unlock()
					continue
				}
				tree.Entries[i].LastCommit = &c
			}
		})
	}
feed:
	for i := range n {
		select {
		case next <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// lastCommit returns the newest commit reachable from head that changed path;
// an empty path means head itself.
func (g *Git) lastCommit(ctx context.Context, dir string, head domain.ObjectID, path string) (domain.Commit, error) {
	var c domain.Commit
	found := false
	args := readArgs(dir, "log", "-z", "-n", "1", "--format="+commitFormat, "--end-of-options", head.String())
	if path != "" {
		args = append(args, "--", path)
	}
	err := g.pipe(ctx, args, func(r *bufio.Reader) (err error) {
		c, err = readCommit(r)
		if errors.Is(err, io.EOF) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		return domain.Commit{}, err
	}
	if !found {
		return domain.Commit{}, fmt.Errorf("no commit touches %q: %w", path, domain.ErrNotFound)
	}
	return c, nil
}

// commitFormat is the `git log` format of a commit: NUL separated fields,
// the message last. With -z the commits are NUL terminated, so a commit is
// nine tokens.
const commitFormat = "%H%x00%P%x00%an%x00%ae%x00%at%x00%cn%x00%ce%x00%ct%x00%B"

// readCommit reads one commit in commitFormat. It returns io.EOF at the end
// of the output.
func readCommit(r *bufio.Reader) (domain.Commit, error) {
	var f [9]string
	for i := range f {
		tok, err := readToken(r)
		if err != nil {
			if i > 0 && errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return domain.Commit{}, err
		}
		f[i] = tok
	}
	id, err := domain.ParseObjectID(f[0])
	if err != nil {
		return domain.Commit{}, fmt.Errorf("git log: %w", err)
	}
	c := domain.Commit{ID: id}
	for p := range strings.FieldsSeq(f[1]) {
		pid, err := domain.ParseObjectID(p)
		if err != nil {
			return domain.Commit{}, fmt.Errorf("git log: parent of %s: %w", id, err)
		}
		c.Parents = append(c.Parents, pid)
	}
	if c.Author, err = signature(f[2], f[3], f[4]); err != nil {
		return domain.Commit{}, fmt.Errorf("git log: author of %s: %w", id, err)
	}
	if c.Committer, err = signature(f[5], f[6], f[7]); err != nil {
		return domain.Commit{}, fmt.Errorf("git log: committer of %s: %w", id, err)
	}
	c.Message, c.Summary = domain.SplitCommitMessage(f[8])
	return c, nil
}

func signature(name, email, unix string) (domain.Signature, error) {
	sec, err := strconv.ParseInt(unix, 10, 64)
	if err != nil {
		return domain.Signature{}, err
	}
	return domain.Signature{
		Name:  domain.ValidGitText(name),
		Email: domain.ValidGitText(email),
		Time:  time.Unix(sec, 0).UTC(),
	}, nil
}

// ListCommits pages through `git log`, which stops after Skip+Limit+1 commits.
//
//hexcheck:ignore suspicious-business-logic-in-adapter parses git output into domain values; limits and rules come from the port
func (g *Git) ListCommits(ctx context.Context, repo domain.RepoName, q ports.CommitQuery) (ports.CommitPage, error) {
	if err := q.Validate(); err != nil {
		return ports.CommitPage{}, err
	}
	dir, err := g.repoDir(repo)
	if err != nil {
		return ports.CommitPage{}, err
	}
	var head domain.ResolvedRevision
	err = g.cat.do(ctx, dir, func(c *catConn) (err error) {
		head, err = resolve(c, q.Revision)
		return err
	})
	if err != nil {
		return ports.CommitPage{}, fmt.Errorf("history of %q: %w", repo, err)
	}
	page := ports.CommitPage{Head: head.CommitID}
	args := readArgs(dir, "log", "-z", "--skip="+strconv.Itoa(q.Skip), "-n", strconv.Itoa(q.Limit+1),
		"--format="+commitFormat, "--end-of-options", head.CommitID.String())
	if !q.Path.IsRoot() {
		args = append(args, "--", q.Path.String())
	}
	err = g.pipe(ctx, args, func(r *bufio.Reader) error {
		for {
			c, err := readCommit(r)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if len(page.Commits) == q.Limit {
				page.HasMore = true
				return errStop
			}
			page.Commits = append(page.Commits, c)
		}
	})
	if err != nil {
		return ports.CommitPage{}, fmt.Errorf("history of %q: %w", repo, err)
	}
	return page, nil
}
