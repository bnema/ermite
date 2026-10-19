// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	kv "bnema.dev/ermite-kv"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Key layout of the issues, like that of the merge requests. The numbers come
// from the sequence of the merge requests (mrseq/<repo>): an issue and a merge
// request never share one.
const (
	issuePrefix        = "is/"     // is/<repo>/<number:010d>
	issueCommentPrefix = "isc/"    // isc/<repo>/<number:010d>/<id:010d>
	issueOpenPrefix    = "isopen/" // isopen/<repo>/<number:010d>, written with the issue
)

// Issues is the ermite-kv backed ports.IssueStore. It shares the database of
// the Store it comes from.
type Issues struct {
	db    *kv.DB
	locks *numberLocks
}

var _ ports.IssueStore = (*Issues)(nil)

// Issues returns the issue store over the same database. It is valid until the
// Store is closed.
func (s *Store) Issues() *Issues { return &Issues{db: s.db, locks: s.locks} }

func issueRepoPrefix(repo domain.RepoName) string { return issuePrefix + repo.String() + "/" }
func issueKey(repo domain.RepoName, n int64) []byte {
	return []byte(issueRepoPrefix(repo) + fmt.Sprintf(numberFormat, n))
}
func issueCommentsPrefix(repo domain.RepoName, n int64) string {
	return issueCommentPrefix + repo.String() + "/" + fmt.Sprintf(numberFormat, n) + "/"
}
func issueCommentKey(repo domain.RepoName, n, id int64) []byte {
	return []byte(issueCommentsPrefix(repo, n) + fmt.Sprintf(numberFormat, id))
}
func issueOpenRepoPrefix(repo domain.RepoName) string { return issueOpenPrefix + repo.String() + "/" }
func issueOpenKey(repo domain.RepoName, n int64) []byte {
	return []byte(issueOpenRepoPrefix(repo) + fmt.Sprintf(numberFormat, n))
}

// indexOpenIssue adds to b the write that keeps the index of the open issues in
// step with the issue: the key is put when it is open, deleted otherwise
// (deleting a key that is not there is not an error).
func indexOpenIssue(b *kv.Batch, issue domain.Issue) {
	if issue.IsOpen() {
		b.Put(issueOpenKey(issue.Repo, issue.Number), []byte{1})
		return
	}
	b.Delete(issueOpenKey(issue.Repo, issue.Number))
}

// issueKeys lists every key of the snapshot that belongs to the repository's
// issues.
func issueKeys(ctx context.Context, snap *kv.Snapshot, repo domain.RepoName) [][]byte {
	var keys [][]byte
	for _, p := range []string{
		issueRepoPrefix(repo), issueCommentPrefix + repo.String() + "/", issueOpenRepoPrefix(repo),
	} {
		for k := range snap.Prefix(ctx, []byte(p)) {
			keys = append(keys, slices.Clone(k))
		}
	}
	return keys
}

// Create takes the next number of the repository from mrseq/<repo>, the
// sequence of the merge requests, and stores the issue under it; see
// createNumbered. A missing repository gives domain.ErrNotFound.
func (i *Issues) Create(ctx context.Context, issue domain.Issue) (domain.Issue, error) {
	err := createNumbered(ctx, i.db, i.locks, issue.Repo, func(b *kv.Batch, n int64) error {
		issue.Number = n
		val, err := encodeIssue(issue)
		if err != nil {
			return err
		}
		b.RequireAbsent(issueKey(issue.Repo, n))
		b.Put(issueKey(issue.Repo, n), val)
		indexOpenIssue(b, issue)
		return nil
	})
	if err != nil {
		return domain.Issue{}, fmt.Errorf("create issue of %q: %w", issue.Repo, err)
	}
	return i.Get(ctx, issue.Repo, issue.Number)
}

// Get reads is/<repo>/<number>.
func (i *Issues) Get(ctx context.Context, repo domain.RepoName, number int64) (domain.Issue, error) {
	if number < 1 {
		return domain.Issue{}, domain.ErrNotFound
	}
	item, ok := i.db.Get(ctx, issueKey(repo, number))
	if !ok {
		return domain.Issue{}, domain.ErrNotFound
	}
	return decodeIssue(repo, item)
}

// List returns a page of the issues of the repository, highest number first. The
// page token is the last number of the previous page.
func (i *Issues) List(ctx context.Context, repo domain.RepoName, params ports.ListIssuesParams) (ports.IssuePage, error) {
	before := int64(0) // 0: from the highest
	if params.PageToken != "" {
		n, err := strconv.ParseInt(params.PageToken, 10, 64)
		if err != nil || n < 1 {
			return ports.IssuePage{}, fmt.Errorf("%w: page token", domain.ErrInvalid)
		}
		before = n
	}
	// The keys sort by number: walk [first, cursor) ascending, keep the last
	// PageSize+1 matching records undecoded (the extra one tells whether
	// another page follows), then decode only those.
	start := []byte(issueRepoPrefix(repo))
	end := append(slices.Clone(start[:len(start)-1]), '/'+1) // the prefix ends in '/'
	if before > 0 {
		end = issueKey(repo, before)
	}
	keep := params.PageSize + 1
	var window []kv.Item
	for _, item := range i.db.Snapshot(ctx).Range(ctx, start, end) {
		if params.State != "" {
			var pb storagepb.Issue
			if err := proto.Unmarshal(item.Value, &pb); err != nil {
				return ports.IssuePage{}, fmt.Errorf("decode issue: %w", err)
			}
			st, err := decodeIssueState(pb.GetState())
			if err != nil {
				return ports.IssuePage{}, fmt.Errorf("decode issue of %q: %w", repo, err)
			}
			if st != params.State {
				continue
			}
		}
		window = append(window, item)
		if params.PageSize > 0 && len(window) >= 2*keep {
			window = append(window[:0], window[len(window)-keep:]...)
		}
	}
	if params.PageSize > 0 && len(window) > keep {
		window = window[len(window)-keep:]
	}
	all := make([]domain.Issue, 0, len(window))
	for _, item := range slices.Backward(window) {
		issue, err := decodeIssue(repo, item)
		if err != nil {
			return ports.IssuePage{}, err
		}
		all = append(all, issue)
	}
	var page ports.IssuePage
	if params.PageSize > 0 && len(all) > params.PageSize {
		all = all[:params.PageSize]
		page.NextPageToken = strconv.FormatInt(all[len(all)-1].Number, 10)
	}
	page.Issues = all
	return page, nil
}

// ListOpenAll returns the open issues of every repository through the isopen/
// index: the issues it names are read, no other.
//
//hexcheck:ignore suspicious-business-logic-in-adapter reads the open issues of all repositories through their index: a storage concern, no policy
func (i *Issues) ListOpenAll(ctx context.Context) ([]domain.Issue, error) {
	type ref struct {
		repo domain.RepoName
		n    int64
	}
	var refs []ref
	for key := range i.db.Snapshot(ctx).Prefix(ctx, []byte(issueOpenPrefix)) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		repoName, num, ok := strings.Cut(strings.TrimPrefix(string(key), issueOpenPrefix), "/")
		if !ok {
			return nil, fmt.Errorf("index key %q", key)
		}
		repo, err := domain.ParseRepoName(repoName)
		if err != nil {
			return nil, fmt.Errorf("index key %q: %w", key, err)
		}
		n, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("index key %q: %w", key, err)
		}
		refs = append(refs, ref{repo, n})
	}
	var out []domain.Issue
	for _, r := range refs {
		issue, err := i.Get(ctx, r.repo, r.n)
		if errors.Is(err, domain.ErrNotFound) {
			continue // closed or deleted since the snapshot of the index
		}
		if err != nil {
			return nil, err
		}
		if issue.IsOpen() {
			out = append(out, issue)
		}
	}
	return out, nil
}

// Update replaces the issue with a version precondition and moves its index key
// in the same batch. The repository must exist (see guardRepoKey), so that a
// concurrent delete of the repository leaves no index key behind.
func (i *Issues) Update(ctx context.Context, issue domain.Issue) error {
	val, err := encodeIssue(issue)
	if err != nil {
		return err
	}
	key := issueKey(issue.Repo, issue.Number)
	if _, ok := i.db.Get(ctx, key); !ok {
		return domain.ErrNotFound
	}
	if issue.Version == 0 {
		return fmt.Errorf("update issue %d of %q: read it first: %w", issue.Number, issue.Repo, domain.ErrConflict)
	}
	var b kv.Batch
	if err := guardRepoKey(ctx, i.db, &b, issue.Repo); err != nil {
		return err
	}
	b.RequireVersion(key, issue.Version)
	b.Put(key, val)
	indexOpenIssue(&b, issue)
	if _, err := i.db.Apply(ctx, &b); err != nil {
		if errors.Is(err, kv.ErrConflict) {
			return domain.ErrConflict
		}
		return fmt.Errorf("update issue %d of %q: %w", issue.Number, issue.Repo, err)
	}
	return nil
}

// ListComments returns the comments of the issue, oldest first.
func (i *Issues) ListComments(ctx context.Context, repo domain.RepoName, number int64) ([]domain.IssueComment, error) {
	var out []domain.IssueComment
	for key, item := range i.db.Snapshot(ctx).Prefix(ctx, []byte(issueCommentsPrefix(repo, number))) {
		c, err := decodeIssueComment(item)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		out = append(out, c)
	}
	return out, nil
}

// SaveComment writes the comment and the issue in one batch, each with its
// version precondition (absent for a new comment), and requires the repository
// to exist (see guardRepoKey): domain.ErrNotFound otherwise.
func (i *Issues) SaveComment(ctx context.Context, issue domain.Issue, c domain.IssueComment) error {
	issueVal, err := encodeIssue(issue)
	if err != nil {
		return err
	}
	cVal, err := encodeIssueComment(c)
	if err != nil {
		return err
	}
	if issue.Version == 0 {
		return fmt.Errorf("save comment on %d of %q: read the issue first: %w", issue.Number, issue.Repo, domain.ErrConflict)
	}
	var b kv.Batch
	if err := guardRepoKey(ctx, i.db, &b, issue.Repo); err != nil {
		return err
	}
	b.RequireVersion(issueKey(issue.Repo, issue.Number), issue.Version)
	b.Put(issueKey(issue.Repo, issue.Number), issueVal)
	indexOpenIssue(&b, issue)
	ckey := issueCommentKey(issue.Repo, issue.Number, c.ID)
	if c.Version == 0 {
		b.RequireAbsent(ckey)
	} else {
		b.RequireVersion(ckey, c.Version)
	}
	b.Put(ckey, cVal)
	if _, err := i.db.Apply(ctx, &b); err != nil {
		if errors.Is(err, kv.ErrConflict) {
			return domain.ErrConflict
		}
		return fmt.Errorf("save comment on %d of %q: %w", issue.Number, issue.Repo, err)
	}
	return nil
}

func encodeIssue(issue domain.Issue) ([]byte, error) {
	if issue.Repo.IsZero() {
		return nil, fmt.Errorf("%w: empty name", domain.ErrInvalidName)
	}
	if !utf8.ValidString(issue.Author) {
		return nil, errors.New("issue author is not valid UTF-8")
	}
	state, err := encodeIssueState(issue.State)
	if err != nil {
		return nil, err
	}
	return mustMarshal(&storagepb.Issue{
		Number:     issue.Number,
		Title:      issue.Title.String(),
		Body:       issue.Body.String(),
		State:      state,
		Author:     issue.Author,
		CreatedAt:  encodeTime(issue.CreatedAt),
		UpdatedAt:  encodeTime(issue.UpdatedAt),
		CommentSeq: issue.CommentSeq,
		External:   issue.External,
	}), nil
}

func decodeIssue(repo domain.RepoName, item kv.Item) (domain.Issue, error) {
	var pb storagepb.Issue
	if err := proto.Unmarshal(item.Value, &pb); err != nil {
		return domain.Issue{}, fmt.Errorf("decode issue: %w", err)
	}
	fail := func(err error) (domain.Issue, error) {
		return domain.Issue{}, fmt.Errorf("decode issue %d of %q: %w", pb.GetNumber(), repo, err)
	}
	title, err := domain.ParseIssueTitle(pb.GetTitle())
	if err != nil {
		return fail(err)
	}
	body, err := domain.ParseIssueBody(pb.GetBody())
	if err != nil {
		return fail(err)
	}
	state, err := decodeIssueState(pb.GetState())
	if err != nil {
		return fail(err)
	}
	return domain.Issue{
		Repo: repo, Number: pb.GetNumber(), Title: title, Body: body, State: state,
		Author:    pb.GetAuthor(),
		CreatedAt: decodeTime(pb.GetCreatedAt()), UpdatedAt: decodeTime(pb.GetUpdatedAt()),
		CommentSeq: pb.GetCommentSeq(),
		External:   pb.GetExternal(),
		Version:    item.Version,
	}, nil
}

func encodeIssueComment(c domain.IssueComment) ([]byte, error) {
	if !utf8.ValidString(c.Author) {
		return nil, errors.New("issue comment author is not valid UTF-8")
	}
	return mustMarshal(&storagepb.IssueComment{
		Id:        c.ID,
		Author:    c.Author,
		Text:      c.Text.String(),
		CreatedAt: encodeTime(c.CreatedAt),
	}), nil
}

func decodeIssueComment(item kv.Item) (domain.IssueComment, error) {
	var pb storagepb.IssueComment
	if err := proto.Unmarshal(item.Value, &pb); err != nil {
		return domain.IssueComment{}, fmt.Errorf("decode issue comment: %w", err)
	}
	text, err := domain.ParseIssueBody(pb.GetText())
	if err != nil {
		return domain.IssueComment{}, fmt.Errorf("decode issue comment %d: %w", pb.GetId(), err)
	}
	return domain.IssueComment{
		ID: pb.GetId(), Author: pb.GetAuthor(), Text: text,
		CreatedAt: decodeTime(pb.GetCreatedAt()), Version: item.Version,
	}, nil
}

func encodeIssueState(s domain.IssueState) (storagepb.IssueState, error) {
	switch s {
	case domain.IssueStateOpen:
		return storagepb.IssueState_ISSUE_STATE_OPEN, nil
	case domain.IssueStateClosed:
		return storagepb.IssueState_ISSUE_STATE_CLOSED, nil
	}
	return 0, fmt.Errorf("%w: issue state %q", domain.ErrInvalid, s)
}

func decodeIssueState(s storagepb.IssueState) (domain.IssueState, error) {
	switch s {
	case storagepb.IssueState_ISSUE_STATE_OPEN:
		return domain.IssueStateOpen, nil
	case storagepb.IssueState_ISSUE_STATE_CLOSED:
		return domain.IssueStateClosed, nil
	case storagepb.IssueState_ISSUE_STATE_PENDING:
		return "", errors.New("stored issue is pending: a numbered issue is open or closed")
	}
	return "", fmt.Errorf("unknown issue state %d", s)
}

func encodeIssuesMode(m domain.IssuesMode) (storagepb.IssuesMode, error) {
	switch m {
	case "":
		return storagepb.IssuesMode_ISSUES_MODE_UNSPECIFIED, nil
	case domain.IssuesOff:
		return storagepb.IssuesMode_ISSUES_MODE_OFF, nil
	case domain.IssuesOwner:
		return storagepb.IssuesMode_ISSUES_MODE_OWNER, nil
	case domain.IssuesPublic:
		return storagepb.IssuesMode_ISSUES_MODE_PUBLIC, nil
	}
	return 0, fmt.Errorf("%w: issues mode %q in the settings", domain.ErrInvalid, m)
}

// decodeIssuesMode reads an unspecified or unknown mode as off: the safe side.
func decodeIssuesMode(m storagepb.IssuesMode) domain.IssuesMode {
	switch m {
	case storagepb.IssuesMode_ISSUES_MODE_OWNER:
		return domain.IssuesOwner
	case storagepb.IssuesMode_ISSUES_MODE_PUBLIC:
		return domain.IssuesPublic
	}
	return domain.IssuesOff
}
