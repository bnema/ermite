// SPDX-License-Identifier: AGPL-3.0-or-later

package ermitegit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	git "bnema.dev/ermite-git"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

const (
	headsPrefix = "refs/heads/"
	tagsPrefix  = "refs/tags/"
)

func domainID(id git.ObjectID) (domain.ObjectID, error) { return domain.ParseObjectID(id.String()) }

// listRefs lists HEAD, then the branches, then the tags, like gitcli: a tag
// follows to its commit when it leads to one; HEAD takes the ids of its
// branch or tag, and only the name of an unborn branch.
//
//hexcheck:ignore suspicious-business-logic-in-adapter converts ermite-git values into domain values; the limits and the orders come from domain
func listRefs(r *git.Repo) ([]domain.Ref, error) {
	var refs []domain.Ref
	byName := make(map[string]domain.Ref)
	for _, prefix := range [...]string{headsPrefix, tagsPrefix} {
		kind := domain.RefKindBranch
		if prefix == tagsPrefix {
			kind = domain.RefKindTag
		}
		for ref, err := range r.Refs(prefix) {
			if err != nil {
				return nil, fmt.Errorf("list refs: %w", err)
			}
			out := domain.Ref{Name: ref.Name, ShortName: ref.Name[len(prefix):], Kind: kind}
			var err2 error
			if out.ID, err2 = domainID(ref.ID); err2 != nil {
				return nil, err2
			}
			commit, err := r.PeelRef(ref)
			switch {
			case err == nil:
				if out.CommitID, err2 = domainID(commit); err2 != nil {
					return nil, err2
				}
			case errors.Is(err, git.ErrWrongType):
				// A tag of a tree or a blob has no commit.
			default:
				return nil, fmt.Errorf("follow %s: %w", ref.Name, err)
			}
			refs = append(refs, out)
			byName[out.Name] = out
		}
	}
	head := domain.Ref{Name: "HEAD", Kind: domain.RefKindHead}
	target, _, err := r.Head()
	if err != nil {
		return nil, fmt.Errorf("read HEAD: %w", err)
	}
	if b, ok := byName[target]; ok && target != "" {
		head.ShortName, head.ID, head.CommitID = b.ShortName, b.ID, b.CommitID
	} else if short, ok := cutPrefix(target, headsPrefix); ok {
		head.ShortName = short // unborn branch
	}
	return append([]domain.Ref{head}, refs...), nil
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return s, false
}

// resolved is a revision resolved to a commit.
type resolved struct {
	commit git.ObjectID
	ref    string
}

func (r resolved) domain() (domain.ResolvedRevision, error) {
	id, err := domainID(r.commit)
	return domain.ResolvedRevision{CommitID: id, Ref: r.ref}, err
}

// peelRef reads a ref and peels it to a commit. ok is false when the ref does
// not exist or does not lead to a commit.
func peelRef(r *git.Repo, name string) (_ git.ObjectID, ok bool, _ error) {
	id, err := r.ResolveRef(name)
	if errors.Is(err, git.ErrNotFound) {
		return git.ObjectID{}, false, nil
	}
	if err != nil {
		return git.ObjectID{}, false, err
	}
	return peelID(r, id)
}

func peelID(r *git.Repo, id git.ObjectID) (_ git.ObjectID, ok bool, _ error) {
	commit, err := r.PeelToCommit(id)
	if errors.Is(err, git.ErrNotFound) || errors.Is(err, git.ErrWrongType) {
		return git.ObjectID{}, false, nil
	}
	if err != nil {
		return git.ObjectID{}, false, err
	}
	return commit, true, nil
}

// resolveRevision tries the candidates of gitcli in the same order, which
// the port documents: HEAD is HEAD; a full object id is an object before it
// is a ref; then the branch, then the tag; then a full ref name and an
// abbreviated id as written. It does not use Repo.Resolve, whose order is
// git's own (tags before branches, no ref for a missing full id).
//
//hexcheck:ignore suspicious-business-logic-in-adapter tries the candidates of the port in its documented order, like gitcli; the rule is the port contract
func resolveRevision(r *git.Repo, rev domain.Revision) (resolved, error) {
	s := rev.String()
	if rev.IsHead() || s == "HEAD" {
		if c, ok, err := peelRef(r, "HEAD"); err != nil || ok {
			return resolved{commit: c}, err
		}
		return resolved{}, notFound(rev)
	}
	if rev.IsObjectID() {
		id, err := git.ParseID(s)
		if err != nil {
			return resolved{}, err
		}
		if c, ok, err := peelID(r, id); err != nil || ok {
			return resolved{commit: c}, err
		}
	}
	for _, name := range [...]string{headsPrefix + s, tagsPrefix + s} {
		if c, ok, err := peelRef(r, name); err != nil || ok {
			return resolved{commit: c, ref: name}, err
		}
	}
	switch {
	case len(s) > 5 && s[:5] == "refs/":
		if c, ok, err := peelRef(r, s); err != nil || ok {
			return resolved{commit: c, ref: s}, err
		}
	case rev.IsAbbrevID():
		id, err := r.ResolveCommitPrefix(s)
		if err == nil {
			// The prefix may name an annotated tag: git peels it.
			id, err = r.PeelToCommit(id)
		}
		switch {
		case err == nil:
			return resolved{commit: id}, nil
		case errors.Is(err, git.ErrNotFound), errors.Is(err, git.ErrAmbiguous), errors.Is(err, git.ErrWrongType):
		default:
			return resolved{}, err
		}
	}
	return resolved{}, notFound(rev)
}

func notFound(rev domain.Revision) error {
	return fmt.Errorf("revision %q: %w", rev, domain.ErrNotFound)
}

// canonMode is the mode git reports for a tree entry: legacy permission bits
// are normalized (tree-walk.c canon_mode).
func canonMode(m git.Mode) uint32 {
	switch {
	case m.IsRegular():
		if m&0o100 != 0 {
			return 0o100755
		}
		return 0o100644
	case m.IsSymlink():
		return 0o120000
	case m.IsTree():
		return 0o040000
	}
	return 0o160000
}

// commitTree resolves rev and reads the root tree of the commit.
func commitTree(r *git.Repo, rev domain.Revision) (domain.ObjectID, git.ObjectID, error) {
	res, err := resolveRevision(r, rev)
	if err != nil {
		return domain.ObjectID{}, git.ObjectID{}, err
	}
	var c git.Commit
	if err := r.ReadCommit(res.commit, &c); err != nil {
		return domain.ObjectID{}, git.ObjectID{}, err
	}
	id, err := domainID(res.commit)
	return id, c.Tree, err
}

// lookup finds the entry at a path of a commit tree. A path that does not
// exist, and a submodule (whose commit is not in the repository), are
// domain.ErrNotFound like in gitcli.
func lookup(r *git.Repo, tree git.ObjectID, path domain.TreePath) (git.Entry, error) {
	e, err := r.LookupPath(tree, path.String())
	if errors.Is(err, git.ErrNotFound) {
		return git.Entry{}, fmt.Errorf("path %q: %w", path, domain.ErrNotFound)
	}
	if err != nil {
		return git.Entry{}, err
	}
	if e.Mode.IsGitlink() {
		return git.Entry{}, fmt.Errorf("path %q: %w", path, domain.ErrNotFound)
	}
	return e, nil
}

// getTree lists a directory.
//
//hexcheck:ignore suspicious-business-logic-in-adapter converts ermite-git values into domain values; the limits and the orders come from domain
func getTree(ctx context.Context, r *git.Repo, rev domain.Revision, path domain.TreePath, includeLast bool) (domain.Tree, error) {
	commit, root, err := commitTree(r, rev)
	if err != nil {
		return domain.Tree{}, err
	}
	dir, err := lookup(r, root, path)
	if err != nil {
		return domain.Tree{}, err
	}
	if !dir.Mode.IsTree() {
		return domain.Tree{}, domain.ErrNotDirectory
	}
	treeID, err := domainID(dir.ID)
	if err != nil {
		return domain.Tree{}, err
	}
	tree := domain.Tree{CommitID: commit, ID: treeID}
	// The ids, the names and the paths of all the entries share one backing
	// string, cut into substrings.
	type span struct {
		name, path, id [2]int
		mode           uint32
		kind           domain.EntryKind
	}
	var (
		spans []span
		buf   []byte
		bad   error
	)
	prefix := path.String()
	err = r.ReadTree(dir.ID, func(e git.Entry) bool {
		mode := canonMode(e.Mode)
		kind := domain.EntryKindOf(mode)
		if kind == "" {
			bad = fmt.Errorf("unknown mode %o of %q", uint32(e.Mode), e.Name)
			return false
		}
		var sp span
		sp.mode, sp.kind = mode, kind
		sp.id[0] = len(buf)
		buf = e.ID.AppendHex(buf)
		sp.id[1] = len(buf)
		sp.name[0] = len(buf)
		buf = append(buf, e.Name...)
		sp.name[1] = len(buf)
		if prefix == "" {
			sp.path = sp.name
		} else {
			sp.path[0] = len(buf)
			buf = append(buf, prefix...)
			buf = append(buf, '/')
			buf = append(buf, e.Name...)
			sp.path[1] = len(buf)
		}
		spans = append(spans, sp)
		return true
	})
	if err == nil {
		err = bad
	}
	if err != nil {
		return domain.Tree{}, fmt.Errorf("list tree %q: %w", path, err)
	}
	text := string(buf)
	tree.Entries = make([]domain.TreeEntry, len(spans))
	for i, sp := range spans {
		id, err := domain.ParseObjectID(text[sp.id[0]:sp.id[1]])
		if err != nil {
			return domain.Tree{}, err
		}
		tree.Entries[i] = domain.TreeEntry{
			Name: text[sp.name[0]:sp.name[1]], Path: text[sp.path[0]:sp.path[1]],
			Kind: sp.kind, Mode: sp.mode, ID: id,
		}
	}
	domain.SortTreeEntries(tree.Entries)
	tree.BoundTree()
	// The size of a file costs a header read: only for the entries that stay.
	for i, e := range tree.Entries {
		if e.Kind == domain.EntryDirectory || e.Kind == domain.EntrySubmodule {
			continue
		}
		id, err := git.ParseID(e.ID.String())
		if err != nil {
			return domain.Tree{}, err
		}
		_, size, err := r.ObjectHeader(id)
		if err != nil {
			return domain.Tree{}, fmt.Errorf("size of %q: %w", e.Path, err)
		}
		tree.Entries[i].Size = size
	}
	if includeLast {
		if err := fillLastCommits(ctx, r, commit, path, &tree); err != nil {
			return domain.Tree{}, fmt.Errorf("last commits of %q: %w", path, err)
		}
	}
	return tree, nil
}

// fillLastCommits sets the last commit of the first MaxLastCommitEntries
// entries with one walk.
//
//hexcheck:ignore suspicious-business-logic-in-adapter converts ermite-git values into domain values; the limits and the orders come from domain
func fillLastCommits(ctx context.Context, r *git.Repo, commit domain.ObjectID, dir domain.TreePath, tree *domain.Tree) error {
	n := len(tree.Entries)
	if n > domain.MaxLastCommitEntries {
		n = domain.MaxLastCommitEntries
		tree.LastCommitsOmitted = true
	}
	if n == 0 {
		return nil
	}
	start, err := git.ParseID(commit.String())
	if err != nil {
		return err
	}
	names := make([]string, n)
	for i := range n {
		names[i] = tree.Entries[i].Name
	}
	ids, err := r.LastCommits(ctx, start, dir.String(), names)
	if err != nil {
		return err
	}
	var c git.Commit
	seen := make(map[git.ObjectID]domain.Commit, 8)
	for i, id := range ids {
		if id.IsZero() {
			return fmt.Errorf("no commit touches %q: %w", tree.Entries[i].Path, domain.ErrNotFound)
		}
		dc, ok := seen[id]
		if !ok {
			if err := r.ReadCommit(id, &c); err != nil {
				return err
			}
			if dc, err = convertCommit(&c); err != nil {
				return err
			}
			seen[id] = dc
		}
		cp := dc
		tree.Entries[i].LastCommit = &cp
	}
	return nil
}

// convertCommit builds a domain commit. A commit in another encoding than
// UTF-8 is converted by git and not by the library: errFallback.
//
// The ids, the names, the emails and the message share one backing string, cut
// into substrings: one allocation for the text of a commit instead of one per
// field.
//
//hexcheck:ignore suspicious-business-logic-in-adapter converts ermite-git values into domain values; the limits and the orders come from domain
func convertCommit(c *git.Commit) (domain.Commit, error) {
	if len(c.Encoding) > 0 && !isUTF8Label(c.Encoding) {
		return domain.Commit{}, errFallback
	}
	buf := make([]byte, 0, 40*(1+len(c.Parents))+len(c.Author.Name)+len(c.Author.Email)+
		len(c.Committer.Name)+len(c.Committer.Email)+len(c.Message))
	buf = c.ID.AppendHex(buf)
	for _, p := range c.Parents {
		buf = p.AppendHex(buf)
	}
	end := [5]int{}
	for i, f := range [...][]byte{c.Author.Name, c.Author.Email, c.Committer.Name, c.Committer.Email, c.Message} {
		buf = append(buf, f...)
		end[i] = len(buf)
	}
	text := string(buf)

	id, err := domain.ParseObjectID(text[:40])
	if err != nil {
		return domain.Commit{}, err
	}
	out := domain.Commit{ID: id}
	if len(c.Parents) > 0 {
		out.Parents = make([]domain.ObjectID, len(c.Parents))
		for i := range c.Parents {
			if out.Parents[i], err = domain.ParseObjectID(text[40*(i+1) : 40*(i+2)]); err != nil {
				return domain.Commit{}, err
			}
		}
	}
	at := 40 * (1 + len(c.Parents))
	out.Author = domain.Signature{
		Name: domain.ValidGitText(text[at:end[0]]), Email: domain.ValidGitText(text[end[0]:end[1]]),
		Time: time.Unix(c.Author.Time, 0).UTC(),
	}
	out.Committer = domain.Signature{
		Name: domain.ValidGitText(text[end[1]:end[2]]), Email: domain.ValidGitText(text[end[2]:end[3]]),
		Time: time.Unix(c.Committer.Time, 0).UTC(),
	}
	out.Message, out.Summary = domain.SplitCommitMessage(text[end[3]:])
	return out, nil
}

func isUTF8Label(b []byte) bool {
	return bytes.EqualFold(b, []byte("utf-8")) || bytes.EqualFold(b, []byte("utf8"))
}

// listCommits returns a page of history, one commit more than the page to
// know whether the history goes on.
func listCommits(ctx context.Context, r *git.Repo, q ports.CommitQuery) (ports.CommitPage, error) {
	res, err := resolveRevision(r, q.Revision)
	if err != nil {
		return ports.CommitPage{}, err
	}
	head, err := res.domain()
	if err != nil {
		return ports.CommitPage{}, err
	}
	page := ports.CommitPage{Head: head.CommitID, Commits: make([]domain.Commit, 0, min(q.Limit, 64))}
	opts := git.WalkOptions{Skip: q.Skip, Limit: q.Limit + 1, Path: q.Path.String()}
	for c, err := range r.Walk(ctx, res.commit, opts) {
		if err != nil {
			return ports.CommitPage{}, err
		}
		if len(page.Commits) == q.Limit {
			page.HasMore = true
			break
		}
		dc, err := convertCommit(c)
		if err != nil {
			return ports.CommitPage{}, err
		}
		page.Commits = append(page.Commits, dc)
	}
	if len(page.Commits) == 0 {
		page.Commits = nil // like gitcli: an empty page has no slice
	}
	return page, nil
}

// openBlob describes the file at path and opens its content. The first
// BinarySniffLen bytes are read to decide Binary and MediaType, then given
// back to the reader.
//
//hexcheck:ignore suspicious-business-logic-in-adapter converts ermite-git values into domain values; the limits and the orders come from domain
func openBlob(ctx context.Context, r *git.Repo, rev domain.Revision, path domain.TreePath) (domain.BlobInfo, io.ReadCloser, error) {
	if path.IsRoot() {
		return domain.BlobInfo{}, nil, domain.ErrNotFile
	}
	_, root, err := commitTree(r, rev)
	if err != nil {
		return domain.BlobInfo{}, nil, err
	}
	e, err := lookup(r, root, path)
	if err != nil {
		return domain.BlobInfo{}, nil, err
	}
	if e.Mode.IsTree() {
		return domain.BlobInfo{}, nil, domain.ErrNotFile
	}
	id, err := domainID(e.ID)
	if err != nil {
		return domain.BlobInfo{}, nil, err
	}
	size, rc, err := r.OpenBlob(e.ID)
	if err != nil {
		return domain.BlobInfo{}, nil, err
	}
	info := domain.BlobInfo{ID: id, Path: path.String(), Size: size, Mode: canonMode(e.Mode)}
	head := make([]byte, min(size, int64(domain.BinarySniffLen)))
	if _, err := io.ReadFull(rc, head); err != nil {
		_ = rc.Close()
		return domain.BlobInfo{}, nil, fmt.Errorf("read blob: %w", err)
	}
	info.SniffContent(head)
	return info, &blobReader{ctx: ctx, head: head, rc: rc}, nil
}

// blobReader replays the bytes read to sniff the content, then the rest of
// the blob. It stops when the context ends, as a gitcli stream does.
type blobReader struct {
	ctx  context.Context
	head []byte
	rc   io.ReadCloser
}

func (b *blobReader) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	if len(b.head) > 0 {
		n := copy(p, b.head)
		b.head = b.head[n:]
		return n, nil
	}
	return b.rc.Read(p)
}

func (b *blobReader) Close() error { return b.rc.Close() }
