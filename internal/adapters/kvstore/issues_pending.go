// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	kv "bnema.dev/ermite-kv"
	"github.com/bnema/zerowrap"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Key layout of the public issue form.
const (
	pendingPrefix = "ispend/" // ispend/<repo>/<id>
	banPrefix     = "isban/"  // isban/<hex of the hash>
	issueKeyKey   = "meta/issuekey"
	issueKeyLen   = 32
)

// PendingIssues is the ermite-kv backed ports.PendingIssueStore. It shares the
// database of the Store it comes from.
type PendingIssues struct {
	db    *kv.DB
	locks *numberLocks
}

var _ ports.PendingIssueStore = (*PendingIssues)(nil)

// PendingIssues returns the pending issue store over the same database. It is
// valid until the Store is closed.
func (s *Store) PendingIssues() *PendingIssues { return &PendingIssues{db: s.db, locks: s.locks} }

func pendingRepoPrefix(repo domain.RepoName) string { return pendingPrefix + repo.String() + "/" }
func pendingKey(repo domain.RepoName, id domain.PendingIssueID) []byte {
	return []byte(pendingRepoPrefix(repo) + id.String())
}

// pendingKeys lists every key of the snapshot that holds a pending issue of
// the repository.
func pendingKeys(ctx context.Context, snap *kv.Snapshot, repo domain.RepoName) [][]byte {
	var keys [][]byte
	for k := range snap.Prefix(ctx, []byte(pendingRepoPrefix(repo))) {
		keys = append(keys, slices.Clone(k))
	}
	return keys
}

// Put stores the pending issue under a new key. The repository must exist (see
// guardRepoKey): domain.ErrNotFound otherwise.
func (s *PendingIssues) Put(ctx context.Context, p domain.PendingIssue) error {
	val, err := encodePending(p)
	if err != nil {
		return err
	}
	var b kv.Batch
	if err := guardRepoKey(ctx, s.db, &b, p.Repo); err != nil {
		return err
	}
	b.RequireAbsent(pendingKey(p.Repo, p.ID))
	b.Put(pendingKey(p.Repo, p.ID), val)
	if _, err := s.db.Apply(ctx, &b); err != nil {
		if errors.Is(err, kv.ErrConflict) {
			return domain.ErrConflict
		}
		return fmt.Errorf("store pending issue of %q: %w", p.Repo, err)
	}
	return nil
}

// Get reads ispend/<repo>/<id>.
func (s *PendingIssues) Get(ctx context.Context, repo domain.RepoName, id domain.PendingIssueID) (domain.PendingIssue, error) {
	item, ok := s.db.Get(ctx, pendingKey(repo, id))
	if !ok {
		return domain.PendingIssue{}, domain.ErrNotFound
	}
	return decodePending(repo, id, item)
}

// List reads the pending issues of the repository, or of all, sorted by
// creation time then repository then id. There are at most
// domain.MaxPendingIssues of them (a little more under a race), so they are
// read and sorted. The page token is the position of the last item returned.
func (s *PendingIssues) List(ctx context.Context, repo domain.RepoName, page ports.ListPendingParams) (ports.PendingIssuePage, error) {
	prefix := pendingPrefix
	if !repo.IsZero() {
		prefix = pendingRepoPrefix(repo)
	}
	var after pendingCursor
	if page.PageToken != "" {
		var err error
		if after, err = parsePendingCursor(page.PageToken); err != nil {
			return ports.PendingIssuePage{}, err
		}
	}
	var all []domain.PendingIssue
	for key, item := range s.db.Snapshot(ctx).Prefix(ctx, []byte(prefix)) {
		if err := ctx.Err(); err != nil {
			return ports.PendingIssuePage{}, err
		}
		r, id, err := splitPendingKey(string(key))
		if err != nil {
			return ports.PendingIssuePage{}, err
		}
		p, err := decodePending(r, id, item)
		if err != nil {
			return ports.PendingIssuePage{}, fmt.Errorf("decode %s: %w", key, err)
		}
		all = append(all, p)
	}
	slices.SortFunc(all, comparePending)
	if page.PageToken != "" {
		all = slices.DeleteFunc(all, func(p domain.PendingIssue) bool { return comparePending(p, after.item()) <= 0 })
	}
	var out ports.PendingIssuePage
	if page.PageSize > 0 && len(all) > page.PageSize {
		all = all[:page.PageSize]
		out.NextPageToken = encodePendingCursor(all[len(all)-1])
	}
	out.Issues = all
	return out, nil
}

func comparePending(a, b domain.PendingIssue) int {
	if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
		return c
	}
	if c := strings.Compare(a.Repo.String(), b.Repo.String()); c != 0 {
		return c
	}
	return strings.Compare(a.ID.String(), b.ID.String())
}

type pendingCursor struct {
	nanos int64
	repo  domain.RepoName
	id    domain.PendingIssueID
}

func (c pendingCursor) item() domain.PendingIssue {
	return domain.PendingIssue{Repo: c.repo, ID: c.id, CreatedAt: time.Unix(0, c.nanos)}
}

func encodePendingCursor(p domain.PendingIssue) string {
	return strconv.FormatInt(p.CreatedAt.UnixNano(), 10) + "/" + p.Repo.String() + "/" + p.ID.String()
}

func parsePendingCursor(s string) (pendingCursor, error) {
	bad := fmt.Errorf("%w: page token", domain.ErrInvalid)
	parts := strings.Split(s, "/")
	if len(parts) != 3 {
		return pendingCursor{}, bad
	}
	n, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return pendingCursor{}, bad
	}
	repo, err := domain.ParseRepoName(parts[1])
	if err != nil {
		return pendingCursor{}, bad
	}
	id, err := domain.ParsePendingIssueID(parts[2])
	if err != nil {
		return pendingCursor{}, bad
	}
	return pendingCursor{nanos: n, repo: repo, id: id}, nil
}

func splitPendingKey(key string) (domain.RepoName, domain.PendingIssueID, error) {
	rest := strings.TrimPrefix(key, pendingPrefix)
	repoName, idStr, ok := strings.Cut(rest, "/")
	if !ok {
		return domain.RepoName{}, "", fmt.Errorf("pending issue key %q", key)
	}
	repo, err := domain.ParseRepoName(repoName)
	if err != nil {
		return domain.RepoName{}, "", fmt.Errorf("pending issue key %q: %w", key, err)
	}
	id, err := domain.ParsePendingIssueID(idStr)
	if err != nil {
		return domain.RepoName{}, "", fmt.Errorf("pending issue key %q: %w", key, err)
	}
	return repo, id, nil
}

// Count counts the keys under ispend/, stopping after MaxPendingIssues+1.
func (s *PendingIssues) Count(ctx context.Context) (int, error) {
	n := 0
	for range s.db.Snapshot(ctx).Prefix(ctx, []byte(pendingPrefix)) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		n++
		if n > domain.MaxPendingIssues {
			break
		}
	}
	return n, nil
}

// Approve numbers the pending issue from mrseq/<repo> (see createNumbered),
// writes the open issue and its index key and deletes the pending record, in
// one batch that requires the pending record to be unchanged.
func (s *PendingIssues) Approve(ctx context.Context, repo domain.RepoName, id domain.PendingIssueID, now time.Time) (domain.Issue, error) {
	var number int64
	err := createNumbered(ctx, s.db, s.locks, repo, func(b *kv.Batch, n int64) error {
		key := pendingKey(repo, id)
		item, ok := s.db.Get(ctx, key)
		if !ok {
			return domain.ErrNotFound
		}
		p, err := decodePending(repo, id, item)
		if err != nil {
			return err
		}
		issue := domain.ApprovedIssue(p, now)
		issue.Repo, issue.Number = repo, n
		val, err := encodeIssue(issue)
		if err != nil {
			return err
		}
		b.RequireVersion(key, item.Version)
		b.Delete(key)
		b.RequireAbsent(issueKey(repo, n))
		b.Put(issueKey(repo, n), val)
		indexOpenIssue(b, issue)
		number = n
		return nil
	})
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.Issue{}, domain.ErrNotFound
		}
		return domain.Issue{}, fmt.Errorf("approve pending issue of %q: %w", repo, err)
	}
	return (&Issues{db: s.db, locks: s.locks}).Get(ctx, repo, number)
}

// Delete removes the pending record, or returns domain.ErrNotFound.
func (s *PendingIssues) Delete(ctx context.Context, repo domain.RepoName, id domain.PendingIssueID) error {
	key := pendingKey(repo, id)
	item, ok := s.db.Get(ctx, key)
	if !ok {
		return domain.ErrNotFound
	}
	var b kv.Batch
	b.RequireVersion(key, item.Version)
	b.Delete(key)
	if _, err := s.db.Apply(ctx, &b); err != nil {
		if errors.Is(err, kv.ErrConflict) {
			return domain.ErrNotFound // gone or approved meanwhile
		}
		return fmt.Errorf("delete pending issue of %q: %w", repo, err)
	}
	return nil
}

func encodePending(p domain.PendingIssue) ([]byte, error) {
	if p.Repo.IsZero() {
		return nil, fmt.Errorf("%w: empty name", domain.ErrInvalidName)
	}
	if _, err := domain.ParsePendingIssueID(p.ID.String()); err != nil {
		return nil, err
	}
	if !utf8.ValidString(p.Author) {
		return nil, errors.New("pending issue author is not valid UTF-8")
	}
	if len(p.IPHash) != domain.IPHashLen {
		return nil, fmt.Errorf("%w: ip hash is %d bytes, want %d", domain.ErrInvalid, len(p.IPHash), domain.IPHashLen)
	}
	return mustMarshal(&storagepb.PendingIssue{
		Title: p.Title.String(), Body: p.Body.String(), Author: p.Author,
		CreatedAt: encodeTime(p.CreatedAt), IpHash: p.IPHash,
	}), nil
}

func decodePending(repo domain.RepoName, id domain.PendingIssueID, item kv.Item) (domain.PendingIssue, error) {
	var pb storagepb.PendingIssue
	if err := proto.Unmarshal(item.Value, &pb); err != nil {
		return domain.PendingIssue{}, fmt.Errorf("decode pending issue: %w", err)
	}
	title, err := domain.ParseIssueTitle(pb.GetTitle())
	if err != nil {
		return domain.PendingIssue{}, fmt.Errorf("decode pending issue %s of %q: %w", id, repo, err)
	}
	body, err := domain.ParseIssueBody(pb.GetBody())
	if err != nil {
		return domain.PendingIssue{}, fmt.Errorf("decode pending issue %s of %q: %w", id, repo, err)
	}
	return domain.PendingIssue{
		ID: id, Repo: repo, Title: title, Body: body, Author: pb.GetAuthor(),
		CreatedAt: decodeTime(pb.GetCreatedAt()), IPHash: slices.Clone(pb.GetIpHash()), Version: item.Version,
	}, nil
}

// IssueKeys is the ermite-kv backed ports.IssueKeyStore.
type IssueKeys struct {
	db *kv.DB
}

var _ ports.IssueKeyStore = (*IssueKeys)(nil)

// IssueKeys returns the issue key store over the same database. It is valid
// until the Store is closed.
func (s *Store) IssueKeys() *IssueKeys { return &IssueKeys{db: s.db} }

// Key reads meta/issuekey and generates it when absent. The key is never
// logged.
func (k *IssueKeys) Key(ctx context.Context) ([]byte, error) {
	for range updateAttempts {
		if item, ok := k.db.Get(ctx, []byte(issueKeyKey)); ok {
			var m storagepb.IssueKey
			if err := proto.Unmarshal(item.Value, &m); err != nil {
				return nil, fmt.Errorf("decode issue key: %w", err)
			}
			if len(m.GetKey()) != issueKeyLen {
				return nil, fmt.Errorf("decode issue key: %d bytes, want %d", len(m.GetKey()), issueKeyLen)
			}
			return append([]byte(nil), m.GetKey()...), nil
		}
		key := make([]byte, issueKeyLen)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate issue key: %w", err)
		}
		var b kv.Batch
		b.RequireAbsent([]byte(issueKeyKey))
		b.Put([]byte(issueKeyKey), mustMarshal(&storagepb.IssueKey{Key: key}))
		_, err := k.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue // written meanwhile: read it
		}
		if err != nil {
			return nil, fmt.Errorf("store issue key: %w", err)
		}
		log := zerowrap.FromCtx(ctx)
		log.Info().Msg("issue key generated")
		return key, nil
	}
	return nil, fmt.Errorf("store issue key: %w", domain.ErrConflict)
}

// IssueBans is the ermite-kv backed ports.IssueBanStore.
type IssueBans struct {
	db *kv.DB
}

var _ ports.IssueBanStore = (*IssueBans)(nil)

// IssueBans returns the ban store over the same database. It is valid until
// the Store is closed.
func (s *Store) IssueBans() *IssueBans { return &IssueBans{db: s.db} }

func banKey(hash []byte) []byte { return []byte(banPrefix + hex.EncodeToString(hash)) }

// Ban writes isban/<hex hash> with its expiry.
func (s *IssueBans) Ban(ctx context.Context, hash []byte, until time.Time) error {
	if len(hash) != domain.IPHashLen {
		return fmt.Errorf("%w: ban hash is %d bytes, want %d", domain.ErrInvalid, len(hash), domain.IPHashLen)
	}
	var b kv.Batch
	b.Put(banKey(hash), mustMarshal(&storagepb.IssueBan{Until: encodeTime(until)}))
	if _, err := s.db.Apply(ctx, &b); err != nil {
		return fmt.Errorf("store ban: %w", err)
	}
	return nil
}

// Banned reads the ban; one that expired is not a ban and is deleted (if it
// has not been replaced meanwhile).
func (s *IssueBans) Banned(ctx context.Context, hash []byte, now time.Time) (bool, error) {
	key := banKey(hash)
	item, ok := s.db.Get(ctx, key)
	if !ok {
		return false, nil
	}
	var pb storagepb.IssueBan
	if err := proto.Unmarshal(item.Value, &pb); err != nil {
		return false, fmt.Errorf("decode ban: %w", err)
	}
	if now.Before(decodeTime(pb.GetUntil())) {
		return true, nil
	}
	var b kv.Batch
	b.RequireVersion(key, item.Version)
	b.Delete(key)
	if _, err := s.db.Apply(ctx, &b); err != nil && !errors.Is(err, kv.ErrConflict) {
		return false, fmt.Errorf("delete expired ban: %w", err)
	}
	return false, nil
}
