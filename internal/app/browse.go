// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Browse are the read use cases over the Git data of a repository: refs,
// trees, blobs, history and commits. Every method applies the access rule
// first: a repository the principal may not read, or that does not exist,
// answers domain.ErrNotFound before the Git reader is touched.
type Browse struct {
	meta   ports.RepoMetaStore
	reader ports.RepoReader
	// history keeps the answers about a commit designated by its full id (a
	// tree with the last commit of its entries, the latest commit of a path):
	// they never change, and computing them costs a history walk. Nil
	// disables it.
	history *historyCache
}

// NewBrowse builds the read use cases over their ports, with a history cache
// of the default bounds.
func NewBrowse(meta ports.RepoMetaStore, reader ports.RepoReader) *Browse {
	return &Browse{
		meta: meta, reader: reader,
		history: newHistoryCache(DefaultHistoryCacheEntries, DefaultHistoryCacheBytes),
	}
}

// WithHistoryCache replaces the bounds of the history cache: at most
// maxEntries answers and about maxBytes of memory, least recently used first
// out. A bound that is not positive disables the cache. Call it before the
// first request.
func (b *Browse) WithHistoryCache(maxEntries int, maxBytes int64) *Browse {
	b.history = newHistoryCache(maxEntries, maxBytes)
	return b
}

// authorize returns the repository if p may read it. The caller uses it to
// tell a repository from a later one created under the same name.
func (b *Browse) authorize(ctx context.Context, p domain.Principal, name domain.RepoName) (domain.Repo, error) {
	repo, err := lookupRepo(ctx, b.meta, name)
	if err != nil {
		return domain.Repo{}, err
	}
	if !canRead(p, repo) {
		return domain.Repo{}, domain.ErrNotFound
	}
	return repo, nil
}

// repoEpoch identifies one life of a repository name: its creation time in
// nanoseconds, 0 if unknown.
func repoEpoch(r domain.Repo) int64 {
	if r.CreatedAt.IsZero() {
		return 0
	}
	return r.CreatedAt.UnixNano()
}

// ListRefsParams selects the refs to list.
type ListRefsParams struct {
	// Kinds restricts the result; empty means every kind.
	Kinds []domain.RefKind
	// PageSize is 0 for the default, and lowered to MaxRefPageSize.
	PageSize int
	// PageToken is the NextPageToken of the previous page, or empty.
	PageToken string
}

// RefPage is a page of refs.
type RefPage struct {
	Refs []domain.Ref
	// NextPageToken is empty on the last page.
	NextPageToken string
}

func kindRank(k domain.RefKind) int {
	switch k {
	case domain.RefKindHead:
		return 0
	case domain.RefKindBranch:
		return 1
	}
	return 2
}

// ListRefs lists HEAD, the branches and the tags, in that order, then by
// name. The page token names the last ref of the previous page, so refs that
// appear or disappear between two pages never shift the others.
func (b *Browse) ListRefs(ctx context.Context, p domain.Principal, repo domain.RepoName, params ListRefsParams) (RefPage, error) {
	size, err := pageSizeUpTo(params.PageSize, MaxRefPageSize)
	if err != nil {
		return RefPage{}, err
	}
	for _, k := range params.Kinds {
		if k != domain.RefKindHead && k != domain.RefKindBranch && k != domain.RefKindTag {
			return RefPage{}, fmt.Errorf("%w: %q", domain.ErrInvalidRefKind, k)
		}
	}
	after, err := decodeRefToken(params.PageToken)
	if err != nil {
		return RefPage{}, err
	}
	if _, err := b.authorize(ctx, p, repo); err != nil {
		return RefPage{}, err
	}
	all, err := b.reader.ListRefs(ctx, repo)
	if err != nil {
		return RefPage{}, err
	}

	var wanted []domain.Ref
	for _, r := range all {
		if len(params.Kinds) == 0 || slices.Contains(params.Kinds, r.Kind) {
			wanted = append(wanted, r)
		}
	}
	var page RefPage
	page.Refs, page.NextPageToken = pageAfter(wanted, size,
		func(r domain.Ref) bool { return after == nil || refAfter(r, *after) },
		encodeRefToken)
	return page, nil
}

// refAfter reports whether r sorts after the ref the token names.
func refAfter(r domain.Ref, last refCursor) bool {
	if kr, lr := kindRank(r.Kind), kindRank(last.kind); kr != lr {
		return kr > lr
	}
	return r.Name > last.name
}

// refCursor is the decoded page token of ListRefs.
type refCursor struct {
	kind domain.RefKind
	name string
}

const (
	refTokenPrefix    = "r1:"
	commitTokenPrefix = "c1:"
)

func encodeRefToken(last domain.Ref) string {
	return base64.RawURLEncoding.EncodeToString([]byte(refTokenPrefix + string(last.Kind) + ":" + last.Name))
}

// decodeRefToken returns nil for the empty token.
func decodeRefToken(tok string) (*refCursor, error) {
	if tok == "" {
		return nil, nil
	}
	raw, ok := decodeToken(tok, refTokenPrefix)
	if !ok {
		return nil, fmt.Errorf("%w: refs", domain.ErrInvalidPageToken)
	}
	kind, name, ok := strings.Cut(raw, ":")
	k := domain.RefKind(kind)
	if !ok || name == "" || (k != domain.RefKindHead && k != domain.RefKindBranch && k != domain.RefKindTag) {
		return nil, fmt.Errorf("%w: refs", domain.ErrInvalidPageToken)
	}
	return &refCursor{kind: k, name: name}, nil
}

// ResolveRevision turns a revision into the commit it designates.
func (b *Browse) ResolveRevision(ctx context.Context, p domain.Principal, repo domain.RepoName, rev domain.Revision) (domain.ResolvedRevision, error) {
	if _, err := b.authorize(ctx, p, repo); err != nil {
		return domain.ResolvedRevision{}, err
	}
	return b.reader.ResolveRevision(ctx, repo, rev)
}

// ResolveRefPath splits a path such as "feature/x/dir/file" into the longest
// leading ref and the path inside it.
func (b *Browse) ResolveRefPath(ctx context.Context, p domain.Principal, repo domain.RepoName, path string) (domain.RefPath, error) {
	if _, err := b.authorize(ctx, p, repo); err != nil {
		return domain.RefPath{}, err
	}
	return b.reader.ResolveRefPath(ctx, repo, path)
}

// GetTree lists a directory at a revision. With includeLastCommit, entries get
// their last commit, for at most domain.MaxLastCommitEntries of them.
func (b *Browse) GetTree(ctx context.Context, p domain.Principal, repo domain.RepoName, rev domain.Revision, path domain.TreePath, includeLastCommit bool) (domain.Tree, error) {
	r, err := b.authorize(ctx, p, repo)
	if err != nil {
		return domain.Tree{}, err
	}
	// The last commits are the costly part (a history walk per entry) and
	// depend only on the commit and the path: cache them once the revision is
	// a full commit id. A name or HEAD can move, so it is never a key.
	if !includeLastCommit || !rev.IsObjectID() {
		return b.reader.GetTree(ctx, repo, rev, path, includeLastCommit)
	}
	key := historyKey{repo: repo, created: repoEpoch(r), rev: rev.String(), path: path.String(), kind: kindTreeLast}
	if e, ok := b.history.get(key); ok {
		return cloneTree(e.tree), nil
	}
	tree, err := b.reader.GetTree(ctx, repo, rev, path, true)
	if err != nil {
		return domain.Tree{}, err
	}
	b.history.put(treeEntry(key, tree))
	return tree, nil
}

// OpenBlob returns the description of a file and a reader of its content. The
// caller closes the reader.
func (b *Browse) OpenBlob(ctx context.Context, p domain.Principal, repo domain.RepoName, rev domain.Revision, path domain.TreePath) (domain.BlobInfo, io.ReadCloser, error) {
	if _, err := b.authorize(ctx, p, repo); err != nil {
		return domain.BlobInfo{}, nil, err
	}
	return b.reader.OpenBlob(ctx, repo, rev, path)
}

// ListCommitsParams selects a page of history.
type ListCommitsParams struct {
	// Revision is where the history starts; the zero value is HEAD. Ignored
	// when PageToken is set: the token pins the commit the first page started
	// from, so every page walks the same history even if a branch moves.
	Revision domain.Revision
	// Path limits the history to the commits that changed it. It must be the
	// same on every page of a listing.
	Path domain.TreePath
	// PageSize is 0 for the default, and lowered to MaxPageSize.
	PageSize int
	// PageToken is the NextPageToken of the previous page, or empty.
	PageToken string
}

// CommitPage is a page of history, newest first.
type CommitPage struct {
	Commits []domain.Commit
	// NextPageToken is empty on the last page.
	NextPageToken string
}

// ListCommits returns a page of history. The page token holds the commit the
// listing started from, how many commits were already returned and a digest
// of the path; a token used with another path is invalid.
func (b *Browse) ListCommits(ctx context.Context, p domain.Principal, repo domain.RepoName, params ListCommitsParams) (CommitPage, error) {
	size, err := pageSize(params.PageSize)
	if err != nil {
		return CommitPage{}, err
	}
	q := ports.CommitQuery{Revision: params.Revision, Path: params.Path, Limit: size}
	if params.PageToken != "" {
		head, skip, err := decodeCommitToken(params.PageToken, params.Path)
		if err != nil {
			return CommitPage{}, err
		}
		q.Revision, q.Skip = domain.RevisionOf(head), skip
	}
	r, err := b.authorize(ctx, p, repo)
	if err != nil {
		return CommitPage{}, err
	}
	res, err := b.listCommits(ctx, r, q)
	if err != nil {
		return CommitPage{}, err
	}
	out := CommitPage{Commits: res.Commits}
	if res.HasMore {
		out.NextPageToken = encodeCommitToken(res.Head, q.Skip+len(res.Commits), params.Path)
	}
	return out, nil
}

// listCommits asks the reader, through the history cache for the first
// commit of a path at a full commit id (the "last change" of a file): that
// answer is immutable.
func (b *Browse) listCommits(ctx context.Context, r domain.Repo, q ports.CommitQuery) (ports.CommitPage, error) {
	repo := r.Name
	if q.Limit != 1 || q.Skip != 0 || !q.Revision.IsObjectID() {
		return b.reader.ListCommits(ctx, repo, q)
	}
	key := historyKey{repo: r.Name, created: repoEpoch(r), rev: q.Revision.String(), path: q.Path.String(), kind: kindPage, limit: q.Limit}
	if e, ok := b.history.get(key); ok {
		return clonePage(e.page), nil
	}
	res, err := b.reader.ListCommits(ctx, repo, q)
	if err != nil {
		return ports.CommitPage{}, err
	}
	b.history.put(pageEntry(key, res))
	return res, nil
}

func pathDigest(path domain.TreePath) string {
	sum := sha256.Sum256([]byte(path.String()))
	return hex.EncodeToString(sum[:6])
}

func encodeCommitToken(head domain.ObjectID, skip int, path domain.TreePath) string {
	raw := commitTokenPrefix + head.String() + ":" + strconv.Itoa(skip) + ":" + pathDigest(path)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCommitToken(tok string, path domain.TreePath) (domain.ObjectID, int, error) {
	bad := fmt.Errorf("%w: commits", domain.ErrInvalidPageToken)
	raw, ok := decodeToken(tok, commitTokenPrefix)
	if !ok {
		return domain.ObjectID{}, 0, bad
	}
	parts := strings.Split(raw, ":")
	if len(parts) != 3 {
		return domain.ObjectID{}, 0, bad
	}
	head, err := domain.ParseObjectID(parts[0])
	skip, err2 := strconv.Atoi(parts[1])
	if err != nil || err2 != nil || skip < 0 || skip > MaxCommitSkip || parts[2] != pathDigest(path) {
		return domain.ObjectID{}, 0, bad
	}
	return head, skip, nil
}

// GetCommit returns a commit with its statistics and its diff against the
// first parent, within the default diff limits.
func (b *Browse) GetCommit(ctx context.Context, p domain.Principal, repo domain.RepoName, rev domain.Revision) (domain.CommitDetail, error) {
	if _, err := b.authorize(ctx, p, repo); err != nil {
		return domain.CommitDetail{}, err
	}
	return b.reader.GetCommit(ctx, repo, rev, domain.DefaultDiffLimits())
}
