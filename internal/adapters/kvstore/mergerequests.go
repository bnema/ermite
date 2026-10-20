// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	kv "bnema.dev/ermite-kv"
	"github.com/bnema/zerowrap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Key layout of the merge requests. Repository names hold no '/', so a prefix
// ending in "/" never matches another repository; numbers and comment ids are
// zero-padded so that keys sort numerically.
const (
	mrPrefix      = "mr/"      // mr/<repo>/<number:010d>
	commentPrefix = "mrc/"     // mrc/<repo>/<number:010d>/<id:010d>
	readPrefix    = "mrread/"  // mrread/<repo>/<number:010d>
	seqPrefix     = "mrseq/"   // mrseq/<repo>
	mergePrefix   = "mrmerge/" // mrmerge/<repo>/<number:010d>
	// The indexes of the open merge requests by source branch and by target
	// branch, which a push looks up (the values are not read). They are written in
	// the same batch as the merge request, so they are never out of step with it,
	// and built once, together, for the data that predates them (BuildOpenIndex,
	// mrOpenIndexMarker).
	mrOpenPrefix      = "mropen/"     // mropen/<repo>/<escaped source>/<number:010d>
	mrTargetPrefix    = "mrtarget/"   // mrtarget/<repo>/<escaped target>/<number:010d>
	mrOpenIndexMarker = "meta/mropen" // present once the indexes cover every merge request
	numberFormat      = "%010d"
	createTries       = 10
)

// MergeRequests is the ermite-kv backed ports.MergeRequestStore. It shares the
// database of the Store it comes from.
type MergeRequests struct {
	db    *kv.DB
	locks *numberLocks
}

var _ ports.MergeRequestStore = (*MergeRequests)(nil)

// MergeRequests returns the merge request store over the same database. It is
// valid until the Store is closed.
func (s *Store) MergeRequests() *MergeRequests { return &MergeRequests{db: s.db, locks: s.locks} }

func mrRepoPrefix(repo domain.RepoName) string { return mrPrefix + repo.String() + "/" }
func mrKey(repo domain.RepoName, n int64) []byte {
	return []byte(mrRepoPrefix(repo) + fmt.Sprintf(numberFormat, n))
}
func commentsPrefix(repo domain.RepoName, n int64) string {
	return commentPrefix + repo.String() + "/" + fmt.Sprintf(numberFormat, n) + "/"
}
func commentKey(repo domain.RepoName, n, id int64) []byte {
	return []byte(commentsPrefix(repo, n) + fmt.Sprintf(numberFormat, id))
}
func readKey(repo domain.RepoName, n int64) []byte {
	return []byte(readPrefix + repo.String() + "/" + fmt.Sprintf(numberFormat, n))
}
func mergeKey(repo domain.RepoName, n int64) []byte {
	return []byte(mergePrefix + repo.String() + "/" + fmt.Sprintf(numberFormat, n))
}
func seqKey(repo domain.RepoName) []byte { return []byte(seqPrefix + repo.String()) }

// mrOpenRepoPrefix is where the open merge requests of a repository are indexed
// by source branch.
func mrOpenRepoPrefix(repo domain.RepoName) string { return mrOpenPrefix + repo.String() + "/" }

// indexBranchPrefix is where the open merge requests of one branch are indexed
// under prefix (mrOpenPrefix or mrTargetPrefix): the branch is escaped, so it
// holds no '/' and the prefix matches that branch only.
func indexBranchPrefix(prefix string, repo domain.RepoName, branch string) string {
	return prefix + repo.String() + "/" + url.PathEscape(branch) + "/"
}

func indexKey(prefix string, repo domain.RepoName, branch string, n int64) []byte {
	return []byte(indexBranchPrefix(prefix, repo, branch) + fmt.Sprintf(numberFormat, n))
}

// indexOpen adds to b the writes that keep the indexes of the open merge
// requests (by source and by target) in step with a write of mr over old (nil
// when old is not known, as for a new request or a comment: the keys of mr are
// then put or deleted, whatever they were). Deleting a key that is not there is
// not an error.
//
//hexcheck:ignore suspicious-business-logic-in-adapter maintains source and target indexes to match the stored merge request open state and branch names
func indexOpen(b *kv.Batch, old *domain.MergeRequest, mr domain.MergeRequest) {
	if old != nil && old.IsOpen() {
		if !mr.IsOpen() || old.Source != mr.Source {
			b.Delete(indexKey(mrOpenPrefix, old.Repo, old.Source, old.Number))
		}
		if !mr.IsOpen() || old.Target != mr.Target {
			b.Delete(indexKey(mrTargetPrefix, old.Repo, old.Target, old.Number))
		}
	}
	switch {
	case mr.IsOpen():
		b.Put(indexKey(mrOpenPrefix, mr.Repo, mr.Source, mr.Number), []byte{1})
		b.Put(indexKey(mrTargetPrefix, mr.Repo, mr.Target, mr.Number), []byte{1})
	case old == nil:
		b.Delete(indexKey(mrOpenPrefix, mr.Repo, mr.Source, mr.Number))
		b.Delete(indexKey(mrTargetPrefix, mr.Repo, mr.Target, mr.Number))
	}
}

// BuildOpenIndex builds the indexes of the open merge requests (by source and
// by target) when the data does not have them yet (it predates them): one pass
// over the merge requests, in batches, then the marker. It runs when the store is opened and is idempotent: a pass
// cut short by a crash is simply done again, writing the same keys.
//
//hexcheck:ignore suspicious-business-logic-in-adapter one-time storage backfill of an index: a storage concern, no policy
func (s *Store) BuildOpenIndex(ctx context.Context) error {
	if _, ok := s.db.Get(ctx, []byte(mrOpenIndexMarker)); ok {
		return nil
	}
	var b kv.Batch
	n := 0
	flush := func() error {
		if b.Len() == 0 {
			return nil
		}
		if _, err := s.db.Apply(ctx, &b); err != nil {
			return fmt.Errorf("build the index of open merge requests: %w", err)
		}
		b.Reset()
		return nil
	}
	for key, item := range s.db.Snapshot(ctx).Prefix(ctx, []byte(mrPrefix)) {
		repoName, _, ok := strings.Cut(strings.TrimPrefix(string(key), mrPrefix), "/")
		if !ok {
			continue
		}
		repo, err := domain.ParseRepoName(repoName)
		if err != nil {
			continue
		}
		mr, err := decodeMergeRequest(repo, item)
		if err != nil {
			return fmt.Errorf("build the index of open merge requests: decode %s: %w", key, err)
		}
		if !mr.IsOpen() {
			continue
		}
		b.Put(indexKey(mrOpenPrefix, mr.Repo, mr.Source, mr.Number), []byte{1})
		b.Put(indexKey(mrTargetPrefix, mr.Repo, mr.Target, mr.Number), []byte{1})
		if n++; n%(deleteChunk/2) == 0 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	b.Put([]byte(mrOpenIndexMarker), []byte{1})
	if _, err := s.db.Apply(ctx, &b); err != nil {
		return fmt.Errorf("build the index of open merge requests: %w", err)
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Int("open", n).Msg("index of open merge requests built")
	return nil
}

// mergeRequestKeys lists every key of the snapshot that belongs to the
// repository's merge requests.
func mergeRequestKeys(ctx context.Context, snap *kv.Snapshot, repo domain.RepoName) [][]byte {
	var keys [][]byte
	for _, p := range []string{
		mrRepoPrefix(repo), commentPrefix + repo.String() + "/", readPrefix + repo.String() + "/",
		mrOpenRepoPrefix(repo), mrTargetPrefix + repo.String() + "/",
		mergePrefix + repo.String() + "/",
	} {
		for k := range snap.Prefix(ctx, []byte(p)) {
			keys = append(keys, slices.Clone(k))
		}
	}
	if _, ok := snap.Get(ctx, seqKey(repo)); ok {
		keys = append(keys, seqKey(repo))
	}
	return keys
}

// guardRepo is guardRepoKey on the database of the merge requests; see there.
func (m *MergeRequests) guardRepo(ctx context.Context, b *kv.Batch, repo domain.RepoName) error {
	return guardRepoKey(ctx, m.db, b, repo)
}

// Create takes the next number of the repository from mrseq/<repo> and stores
// the merge request under it, in one batch that requires the sequence to be
// unchanged since it was read: two concurrent creations never share a number.
// The batch also requires the repository to be unchanged since it was read and
// rewrites it (see guardRepo), so that a repository deleted meanwhile is not
// left with a stray merge request (a push recording pushed_at makes it retry). A missing repository gives
// domain.ErrNotFound.
func (m *MergeRequests) Create(ctx context.Context, mr domain.MergeRequest) (domain.MergeRequest, error) {
	err := createNumbered(ctx, m.db, m.locks, mr.Repo, func(b *kv.Batch, n int64) error {
		mr.Number = n
		val, err := encodeMergeRequest(mr)
		if err != nil {
			return err
		}
		b.RequireAbsent(mrKey(mr.Repo, n))
		b.Put(mrKey(mr.Repo, n), val)
		indexOpen(b, nil, mr)
		return nil
	})
	if err != nil {
		return domain.MergeRequest{}, fmt.Errorf("create merge request of %q: %w", mr.Repo, err)
	}
	return m.Get(ctx, mr.Repo, mr.Number)
}

// createNumbered takes the next number of the repository from mrseq/<repo>,
// shared by the merge requests and the issues, and lets add put the record
// under it into a batch that requires the sequence to be unchanged since it was
// read (absent for the first number) and the repository to be unchanged (see
// guardRepoKey): two concurrent creations, of either kind, never share a
// number. It retries when the batch conflicts; a missing repository gives
// domain.ErrNotFound.
//
// Creations in one repository are serialized in the process (numberLocks), so
// a burst of merge requests and issues never exhausts the retries; the version
// checks still guard against any other writer.
func createNumbered(ctx context.Context, db *kv.DB, locks *numberLocks, repo domain.RepoName, add func(b *kv.Batch, number int64) error) error {
	unlock := locks.lock(repo)
	defer unlock()
	for range createTries {
		repoItem, repoOK := db.Get(ctx, repoKey(repo))
		if !repoOK {
			return domain.ErrNotFound
		}
		var last int64
		item, ok := db.Get(ctx, seqKey(repo))
		if ok {
			var seq storagepb.Sequence
			if err := proto.Unmarshal(item.Value, &seq); err != nil {
				return fmt.Errorf("decode sequence of %q: %w", repo, err)
			}
			last = seq.GetLast()
		}
		number := last + 1
		var b kv.Batch
		b.RequireVersion(repoKey(repo), repoItem.Version)
		b.Put(repoKey(repo), repoItem.Value) // see guardRepoKey
		if ok {
			b.RequireVersion(seqKey(repo), item.Version)
		} else {
			b.RequireAbsent(seqKey(repo))
		}
		b.Put(seqKey(repo), mustMarshal(&storagepb.Sequence{Last: number}))
		if err := add(&b, number); err != nil {
			return err
		}
		_, err := db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue
		}
		return err
	}
	return domain.ErrConflict
}

// numberLockStripes is the size of the table of numbering locks.
const numberLockStripes = 64

// numberLocks is a fixed table of mutexes, one stripe per hash of the
// repository name, for createNumbered: the memory is bounded whatever the
// number of repositories, and two repositories sharing a stripe only wait for
// each other. One table is shared by every store of a Store, so the merge
// requests and the issues of a repository are numbered one at a time.
type numberLocks [numberLockStripes]sync.Mutex

// lock locks the numbering of repo and returns the unlock function.
func (l *numberLocks) lock(repo domain.RepoName) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(repo.String()))
	mu := &l[h.Sum32()%numberLockStripes]
	mu.Lock()
	return mu.Unlock
}

// Get reads mr/<repo>/<number>.
func (m *MergeRequests) Get(ctx context.Context, repo domain.RepoName, number int64) (domain.MergeRequest, error) {
	if number < 1 {
		return domain.MergeRequest{}, domain.ErrNotFound
	}
	item, ok := m.db.Get(ctx, mrKey(repo, number))
	if !ok {
		return domain.MergeRequest{}, domain.ErrNotFound
	}
	return decodeMergeRequest(repo, item)
}

// List returns the merge requests of the repository, highest number first.
func (m *MergeRequests) List(ctx context.Context, repo domain.RepoName) ([]domain.MergeRequest, error) {
	var out []domain.MergeRequest
	for key, item := range m.db.Snapshot(ctx).Prefix(ctx, []byte(mrRepoPrefix(repo))) {
		mr, err := decodeMergeRequest(repo, item)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		out = append(out, mr)
	}
	slices.Reverse(out)
	return out, nil
}

// scanIndex reads the numbers under the prefixes of an index, in key order. It
// reads keys only: no merge request is decoded. With a limit above 0 it stops as
// soon as it has seen limit+1 keys and returns the first limit with truncated
// set, so the work does not grow with the number of open requests on a branch.
// The context is checked before every prefix and every key (the scan of ermite-kv
// itself ignores it): on cancellation it stops and returns what it has with the
// error of the context.
func (m *MergeRequests) scanIndex(ctx context.Context, prefixes []string, limit int) (numbers []int64, truncated bool, err error) {
	snap := m.db.Snapshot(ctx)
	for _, prefix := range prefixes {
		if err := ctx.Err(); err != nil {
			return numbers, true, err
		}
		for key := range snap.Prefix(ctx, []byte(prefix)) {
			if err := ctx.Err(); err != nil {
				return numbers, true, err
			}
			if limit > 0 && len(numbers) == limit {
				return numbers, true, nil // the limit+1th key: more exist
			}
			n, perr := strconv.ParseInt(string(key[strings.LastIndexByte(string(key), '/')+1:]), 10, 64)
			if perr != nil {
				return numbers, false, fmt.Errorf("index key %q: %w", key, perr)
			}
			numbers = append(numbers, n)
		}
	}
	return numbers, false, nil
}

func (m *MergeRequests) byBranch(ctx context.Context, index string, repo domain.RepoName, branches []string, limit int) ([]int64, bool, error) {
	prefixes := make([]string, 0, len(branches))
	for _, b := range branches {
		prefixes = append(prefixes, indexBranchPrefix(index, repo, b))
	}
	return m.scanIndex(ctx, prefixes, limit)
}

// OpenBySource returns the numbers of the open merge requests of the repository
// whose source branch is one of branches, from the index: a scan of the keys of
// those branches only, whatever the number of merge requests the repository
// ever had, stopping after limit of them (truncated).
func (m *MergeRequests) OpenBySource(ctx context.Context, repo domain.RepoName, branches []string, limit int) ([]int64, bool, error) {
	return m.byBranch(ctx, mrOpenPrefix, repo, branches, limit)
}

// OpenByTarget is OpenBySource for the target branch.
func (m *MergeRequests) OpenByTarget(ctx context.Context, repo domain.RepoName, branches []string, limit int) ([]int64, bool, error) {
	return m.byBranch(ctx, mrTargetPrefix, repo, branches, limit)
}

// ListOpen returns the open merge requests of the repository, highest number
// first, through the index: only the open ones are read.
//
//hexcheck:ignore suspicious-business-logic-in-adapter reads the open merge requests through their index: a storage concern, no policy
func (m *MergeRequests) ListOpen(ctx context.Context, repo domain.RepoName) ([]domain.MergeRequest, error) {
	numbers, _, err := m.scanIndex(ctx, []string{mrOpenRepoPrefix(repo)}, 0)
	if err != nil {
		return nil, err
	}
	slices.Sort(numbers)
	var out []domain.MergeRequest
	for _, n := range slices.Backward(numbers) {
		mr, err := m.Get(ctx, repo, n)
		if errors.Is(err, domain.ErrNotFound) {
			continue // closed or deleted since the snapshot of the index
		}
		if err != nil {
			return nil, err
		}
		if mr.IsOpen() {
			out = append(out, mr)
		}
	}
	return out, nil
}

// ListOpenAll returns the open merge requests of every repository, repository
// by repository and highest number first, through the mropen/ index: the
// requests it names are read, no other.
//
//hexcheck:ignore suspicious-business-logic-in-adapter reads the open merge requests of all repositories through their index: a storage concern, no policy
func (m *MergeRequests) ListOpenAll(ctx context.Context) ([]domain.MergeRequest, error) {
	type ref struct {
		repo domain.RepoName
		n    int64
	}
	var refs []ref
	for key := range m.db.Snapshot(ctx).Prefix(ctx, []byte(mrOpenPrefix)) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rest := strings.TrimPrefix(string(key), mrOpenPrefix)
		repoName, _, ok := strings.Cut(rest, "/")
		if !ok {
			return nil, fmt.Errorf("index key %q", key)
		}
		repo, err := domain.ParseRepoName(repoName)
		if err != nil {
			return nil, fmt.Errorf("index key %q: %w", key, err)
		}
		n, err := strconv.ParseInt(rest[strings.LastIndexByte(rest, '/')+1:], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("index key %q: %w", key, err)
		}
		refs = append(refs, ref{repo, n})
	}
	var out []domain.MergeRequest
	for _, r := range refs {
		mr, err := m.Get(ctx, r.repo, r.n)
		if errors.Is(err, domain.ErrNotFound) {
			continue // closed or deleted since the snapshot of the index
		}
		if err != nil {
			return nil, err
		}
		if mr.IsOpen() {
			out = append(out, mr)
		}
	}
	return out, nil
}

// CountOpen counts the open merge requests of the repository from the keys of
// the index: no request is decoded.
func (m *MergeRequests) CountOpen(ctx context.Context, repo domain.RepoName) (int, error) {
	numbers, _, err := m.scanIndex(ctx, []string{mrOpenRepoPrefix(repo)}, 0)
	return len(numbers), err
}

// Update replaces the merge request with a version precondition.
//
//hexcheck:ignore suspicious-business-logic-in-adapter replaces a versioned record and maintains open-request indexes and the derived mergeability record
func (m *MergeRequests) Update(ctx context.Context, mr domain.MergeRequest) error {
	val, err := encodeMergeRequest(mr)
	if err != nil {
		return err
	}
	key := mrKey(mr.Repo, mr.Number)
	cur, ok := m.db.Get(ctx, key)
	if !ok {
		return domain.ErrNotFound
	}
	if mr.Version == 0 {
		return fmt.Errorf("update merge request %d of %q: read it first: %w", mr.Number, mr.Repo, domain.ErrConflict)
	}
	var b kv.Batch
	b.RequireVersion(key, mr.Version)
	b.Put(key, val)
	// The stored request is the one mr was read at (the version is required
	// above): its index key moves with the write.
	if old, err := decodeMergeRequest(mr.Repo, cur); err == nil {
		indexOpen(&b, &old, mr)
	} else {
		indexOpen(&b, nil, mr)
	}
	if !mr.IsOpen() {
		b.Delete(mergeKey(mr.Repo, mr.Number))
	}
	if _, err := m.db.Apply(ctx, &b); err != nil {
		if errors.Is(err, kv.ErrConflict) {
			return domain.ErrConflict
		}
		return fmt.Errorf("update merge request %d of %q: %w", mr.Number, mr.Repo, err)
	}
	return nil
}

// ListComments returns the comments of the merge request, oldest first.
func (m *MergeRequests) ListComments(ctx context.Context, repo domain.RepoName, number int64) ([]domain.Comment, error) {
	var out []domain.Comment
	for key, item := range m.db.Snapshot(ctx).Prefix(ctx, []byte(commentsPrefix(repo, number))) {
		c, err := decodeComment(item)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		out = append(out, c)
	}
	return out, nil
}

// GetComment reads one comment.
func (m *MergeRequests) GetComment(ctx context.Context, repo domain.RepoName, number, id int64) (domain.Comment, error) {
	if number < 1 || id < 1 {
		return domain.Comment{}, domain.ErrNotFound
	}
	item, ok := m.db.Get(ctx, commentKey(repo, number, id))
	if !ok {
		return domain.Comment{}, domain.ErrNotFound
	}
	return decodeComment(item)
}

// SaveComment writes the comment and the merge request in one batch, each with
// its version precondition (absent for a new comment), and requires the
// repository to exist (see guardRepo): domain.ErrNotFound otherwise.
func (m *MergeRequests) SaveComment(ctx context.Context, mr domain.MergeRequest, c domain.Comment) error {
	mrVal, err := encodeMergeRequest(mr)
	if err != nil {
		return err
	}
	cVal, err := encodeComment(c)
	if err != nil {
		return err
	}
	if mr.Version == 0 {
		return fmt.Errorf("save comment on %d of %q: read the merge request first: %w", mr.Number, mr.Repo, domain.ErrConflict)
	}
	var b kv.Batch
	if err := m.guardRepo(ctx, &b, mr.Repo); err != nil {
		return err
	}
	b.RequireVersion(mrKey(mr.Repo, mr.Number), mr.Version)
	b.Put(mrKey(mr.Repo, mr.Number), mrVal)
	indexOpen(&b, nil, mr)
	if c.Version == 0 {
		b.RequireAbsent(commentKey(mr.Repo, mr.Number, c.ID))
	} else {
		b.RequireVersion(commentKey(mr.Repo, mr.Number, c.ID), c.Version)
	}
	b.Put(commentKey(mr.Repo, mr.Number, c.ID), cVal)
	if _, err := m.db.Apply(ctx, &b); err != nil {
		if errors.Is(err, kv.ErrConflict) {
			return domain.ErrConflict
		}
		return fmt.Errorf("save comment on %d of %q: %w", mr.Number, mr.Repo, err)
	}
	return nil
}

// GetRead reads mrread/<repo>/<number>: the id of a commit, or nothing.
func (m *MergeRequests) GetRead(ctx context.Context, repo domain.RepoName, number int64) (domain.ObjectID, error) {
	item, ok := m.db.Get(ctx, readKey(repo, number))
	if !ok {
		return domain.ObjectID{}, nil
	}
	id, err := domain.ParseObjectID(string(item.Value))
	if err != nil {
		return domain.ObjectID{}, fmt.Errorf("decode read position of %d in %q: %w", number, repo, err)
	}
	return id, nil
}

// SetRead writes mrread/<repo>/<number>. The repository must exist (see
// guardRepo): domain.ErrNotFound otherwise.
func (m *MergeRequests) SetRead(ctx context.Context, repo domain.RepoName, number int64, head domain.ObjectID) error {
	if head.IsZero() {
		return fmt.Errorf("%w: empty read position", domain.ErrInvalidObjectID)
	}
	for range createTries {
		var b kv.Batch
		if err := m.guardRepo(ctx, &b, repo); err != nil {
			return err
		}
		b.Put(readKey(repo, number), []byte(head.String()))
		_, err := m.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue
		}
		if err != nil {
			return fmt.Errorf("record read position of %d in %q: %w", number, repo, err)
		}
		return nil
	}
	return fmt.Errorf("record read position of %d in %q: %w", number, repo, domain.ErrConflict)
}

// GetMergeability returns the stored mergeability, or domain.ErrNotFound.
func (m *MergeRequests) GetMergeability(ctx context.Context, repo domain.RepoName, number int64) (domain.Mergeability, error) {
	item, ok := m.db.Get(ctx, mergeKey(repo, number))
	if !ok {
		return domain.Mergeability{}, domain.ErrNotFound
	}
	return decodeMergeability(repo, number, item)
}

// PutMergeability stores the mergeability in a batch that requires the merge
// request to be unchanged since it was read here and open, and the record to
// be absent (Version 0) or unchanged.
//
//hexcheck:ignore suspicious-business-logic-in-adapter version preconditions of a derived record: a storage concern, no policy
func (m *MergeRequests) PutMergeability(ctx context.Context, mg domain.Mergeability) error {
	val, err := encodeMergeability(mg)
	if err != nil {
		return err
	}
	for range createTries {
		mrItem, ok := m.db.Get(ctx, mrKey(mg.Repo, mg.Number))
		if !ok {
			return domain.ErrNotFound
		}
		mr, err := decodeMergeRequest(mg.Repo, mrItem)
		if err != nil {
			return err
		}
		if !mr.IsOpen() {
			return domain.ErrNotFound
		}
		key := mergeKey(mg.Repo, mg.Number)
		var b kv.Batch
		b.RequireVersion(mrKey(mg.Repo, mg.Number), mrItem.Version)
		if mg.Version == 0 {
			b.RequireAbsent(key)
		} else {
			b.RequireVersion(key, mg.Version)
		}
		b.Put(key, val)
		_, err = m.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			// Either the record changed (the caller's conflict) or the merge
			// request did (retry against it).
			if cur, ok := m.db.Get(ctx, mrKey(mg.Repo, mg.Number)); ok && cur.Version != mrItem.Version {
				continue
			}
			return domain.ErrConflict
		}
		if err != nil {
			return fmt.Errorf("store mergeability of %d in %q: %w", mg.Number, mg.Repo, err)
		}
		return nil
	}
	return fmt.Errorf("store mergeability of %d in %q: %w", mg.Number, mg.Repo, domain.ErrConflict)
}

// DeleteMergeability removes the stored mergeability; absent is fine.
func (m *MergeRequests) DeleteMergeability(ctx context.Context, repo domain.RepoName, number int64) error {
	var b kv.Batch
	b.Delete(mergeKey(repo, number))
	if _, err := m.db.Apply(ctx, &b); err != nil {
		return fmt.Errorf("delete mergeability of %d in %q: %w", number, repo, err)
	}
	return nil
}

func encodeMergeability(m domain.Mergeability) ([]byte, error) {
	var state storagepb.MergeabilityState
	switch m.State {
	case domain.MergeabilityUpToDate:
		state = storagepb.MergeabilityState_MERGEABILITY_STATE_UP_TO_DATE
	case domain.MergeabilityBehind:
		state = storagepb.MergeabilityState_MERGEABILITY_STATE_BEHIND
	case domain.MergeabilityConflict:
		state = storagepb.MergeabilityState_MERGEABILITY_STATE_CONFLICT
	default:
		return nil, fmt.Errorf("%w: mergeability state %q is not stored", domain.ErrInvalid, m.State)
	}
	return proto.Marshal(&storagepb.Mergeability{
		State:                  state,
		TargetId:               m.TargetID.String(),
		HeadId:                 m.HeadID.String(),
		Behind:                 int32(m.Behind), //nolint:gosec // capped at MaxBehindCount
		BehindCapped:           m.BehindCapped,
		ConflictPaths:          m.ConflictPaths,
		ConflictPathsTruncated: m.ConflictPathsTruncated,
		CheckedAt:              timestamppb.New(m.CheckedAt),
	})
}

func decodeMergeability(repo domain.RepoName, number int64, item kv.Item) (domain.Mergeability, error) {
	var pb storagepb.Mergeability
	if err := proto.Unmarshal(item.Value, &pb); err != nil {
		return domain.Mergeability{}, fmt.Errorf("decode mergeability of %d in %q: %w", number, repo, err)
	}
	state, ok := mergeabilityStates[pb.GetState()]
	if !ok {
		return domain.Mergeability{}, fmt.Errorf("decode mergeability of %d in %q: unknown state %d: %w", number, repo, pb.GetState(), domain.ErrNotFound)
	}
	target, err := domain.ParseObjectID(pb.GetTargetId())
	if err != nil {
		return domain.Mergeability{}, fmt.Errorf("decode mergeability of %d in %q: %w", number, repo, err)
	}
	head, err := domain.ParseObjectID(pb.GetHeadId())
	if err != nil {
		return domain.Mergeability{}, fmt.Errorf("decode mergeability of %d in %q: %w", number, repo, err)
	}
	return domain.Mergeability{
		Repo: repo, Number: number, State: state,
		TargetID: target, HeadID: head,
		Behind: int(pb.GetBehind()), BehindCapped: pb.GetBehindCapped(),
		ConflictPaths:          pb.GetConflictPaths(),
		ConflictPathsTruncated: pb.GetConflictPathsTruncated(),
		CheckedAt:              pb.GetCheckedAt().AsTime(),
		Version:                item.Version,
	}, nil
}

// mergeabilityStates maps the stored states to the domain ones; CHECKING is
// never stored.
var mergeabilityStates = map[storagepb.MergeabilityState]domain.MergeabilityState{
	storagepb.MergeabilityState_MERGEABILITY_STATE_UP_TO_DATE: domain.MergeabilityUpToDate,
	storagepb.MergeabilityState_MERGEABILITY_STATE_BEHIND:     domain.MergeabilityBehind,
	storagepb.MergeabilityState_MERGEABILITY_STATE_CONFLICT:   domain.MergeabilityConflict,
}

func encodeMergeRequest(mr domain.MergeRequest) ([]byte, error) {
	if mr.Repo.IsZero() {
		return nil, fmt.Errorf("%w: empty name", domain.ErrInvalidName)
	}
	if !utf8.ValidString(mr.Author) {
		return nil, errors.New("merge request author is not valid UTF-8")
	}
	state, err := encodeState(mr.State)
	if err != nil {
		return nil, err
	}
	mode, err := encodeMode(mr.MergeMode)
	if err != nil {
		return nil, err
	}
	var autoMerge *storagepb.AutoMerge
	if am := mr.AutoMerge; am != nil {
		amMode, err := encodeMode(am.Mode)
		if err != nil || amMode == storagepb.MergeMode_MERGE_MODE_UNSPECIFIED {
			return nil, fmt.Errorf("%w: merge mode %q in the auto-merge", domain.ErrInvalid, am.Mode)
		}
		autoMerge = &storagepb.AutoMerge{
			Mode: amMode, HeadId: am.HeadID.String(),
			EnablerKind: string(am.EnabledBy.Kind), EnablerTokenId: am.EnabledBy.TokenID,
			EnablerLabel: am.EnablerLabel, EnabledAt: encodeTime(am.EnabledAt),
		}
	}
	return mustMarshal(&storagepb.MergeRequest{
		Number:         mr.Number,
		Title:          mr.Title.String(),
		Description:    mr.Description.String(),
		SourceBranch:   mr.Source,
		TargetBranch:   mr.Target,
		State:          state,
		Author:         mr.Author,
		CreatedAt:      encodeTime(mr.CreatedAt),
		UpdatedAt:      encodeTime(mr.UpdatedAt),
		HeadId:         mr.HeadID.String(),
		BaseId:         mr.BaseID.String(),
		MergedCommitId: mr.MergedCommitID.String(),
		MergeMode:      mode,
		CommentSeq:     mr.CommentSeq,
		AutoMerge:      autoMerge,
		AutoMerged:     mr.AutoMerged,
	}), nil
}

func decodeMergeRequest(repo domain.RepoName, item kv.Item) (domain.MergeRequest, error) {
	var pb storagepb.MergeRequest
	if err := proto.Unmarshal(item.Value, &pb); err != nil {
		return domain.MergeRequest{}, fmt.Errorf("decode merge request: %w", err)
	}
	fail := func(err error) (domain.MergeRequest, error) {
		return domain.MergeRequest{}, fmt.Errorf("decode merge request %d of %q: %w", pb.GetNumber(), repo, err)
	}
	title, err := domain.ParseMRTitle(pb.GetTitle())
	if err != nil {
		return fail(err)
	}
	desc, err := domain.ParsePlainText(pb.GetDescription())
	if err != nil {
		return fail(err)
	}
	state, err := decodeState(pb.GetState())
	if err != nil {
		return fail(err)
	}
	mr := domain.MergeRequest{
		Repo: repo, Number: pb.GetNumber(), Title: title, Description: desc,
		Source: pb.GetSourceBranch(), Target: pb.GetTargetBranch(), State: state,
		Author:    pb.GetAuthor(),
		CreatedAt: decodeTime(pb.GetCreatedAt()), UpdatedAt: decodeTime(pb.GetUpdatedAt()),
		MergeMode:  decodeMode(pb.GetMergeMode()),
		CommentSeq: pb.GetCommentSeq(),
		AutoMerged: pb.GetAutoMerged(),
		Version:    item.Version,
	}
	if am := pb.GetAutoMerge(); am != nil {
		head, err := domain.ParseObjectID(am.GetHeadId())
		if err != nil {
			return fail(err)
		}
		mode := decodeMode(am.GetMode())
		if mode == "" {
			return fail(fmt.Errorf("unknown auto-merge mode %d", am.GetMode()))
		}
		mr.AutoMerge = &domain.AutoMerge{
			Mode: mode, HeadID: head,
			EnabledBy:    domain.CIPusher{Kind: domain.CIPusherKind(am.GetEnablerKind()), TokenID: am.GetEnablerTokenId()},
			EnablerLabel: am.GetEnablerLabel(), EnabledAt: decodeTime(am.GetEnabledAt()),
		}
	}
	for _, f := range []struct {
		dst *domain.ObjectID
		src string
	}{{&mr.HeadID, pb.GetHeadId()}, {&mr.BaseID, pb.GetBaseId()}, {&mr.MergedCommitID, pb.GetMergedCommitId()}} {
		if f.src == "" {
			continue
		}
		if *f.dst, err = domain.ParseObjectID(f.src); err != nil {
			return fail(err)
		}
	}
	return mr, nil
}

func encodeComment(c domain.Comment) ([]byte, error) {
	side, err := encodeSide(c.Side)
	if err != nil {
		return nil, err
	}
	return mustMarshal(&storagepb.Comment{
		ReplyTo:   c.ReplyTo,
		StartLine: int32(c.StartLine), //nolint:gosec // validated against the diff
		Verdict:   storagepb.Verdict(c.Verdict),
		EditedAt:  encodeTime(c.EditedAt),
		Deleted:   c.Deleted,
		Id:        c.ID,
		Author:    c.Author,
		Path:      c.Path,
		Line:      int32(c.Line), //nolint:gosec // validated against the diff, far below 2^31
		Side:      side,
		CommitId:  c.CommitID.String(),
		Text:      c.Text.String(),
		Resolved:  c.Resolved,
		CreatedAt: encodeTime(c.CreatedAt),
	}), nil
}

func decodeComment(item kv.Item) (domain.Comment, error) {
	var pb storagepb.Comment
	if err := proto.Unmarshal(item.Value, &pb); err != nil {
		return domain.Comment{}, fmt.Errorf("decode comment: %w", err)
	}
	text, err := domain.ParsePlainText(pb.GetText())
	if err != nil {
		return domain.Comment{}, fmt.Errorf("decode comment %d: %w", pb.GetId(), err)
	}
	c := domain.Comment{
		ReplyTo: pb.GetReplyTo(), StartLine: int(pb.GetStartLine()), Verdict: domain.Verdict(pb.GetVerdict()), EditedAt: decodeTime(pb.GetEditedAt()), Deleted: pb.GetDeleted(),
		ID: pb.GetId(), Author: pb.GetAuthor(), Path: pb.GetPath(), Line: int(pb.GetLine()),
		Side: decodeSide(pb.GetSide()), Text: text, Resolved: pb.GetResolved(),
		CreatedAt: decodeTime(pb.GetCreatedAt()), Version: item.Version,
	}
	if pb.GetCommitId() != "" {
		if c.CommitID, err = domain.ParseObjectID(pb.GetCommitId()); err != nil {
			return domain.Comment{}, fmt.Errorf("decode comment %d: %w", c.ID, err)
		}
	}
	return c, nil
}

func encodeState(s domain.MRState) (storagepb.MergeRequestState, error) {
	switch s {
	case domain.MRStateOpen:
		return storagepb.MergeRequestState_MERGE_REQUEST_STATE_OPEN, nil
	case domain.MRStateMerged:
		return storagepb.MergeRequestState_MERGE_REQUEST_STATE_MERGED, nil
	case domain.MRStateClosed:
		return storagepb.MergeRequestState_MERGE_REQUEST_STATE_CLOSED, nil
	}
	return 0, fmt.Errorf("%w: merge request state %q", domain.ErrInvalid, s)
}

func decodeState(s storagepb.MergeRequestState) (domain.MRState, error) {
	switch s {
	case storagepb.MergeRequestState_MERGE_REQUEST_STATE_OPEN:
		return domain.MRStateOpen, nil
	case storagepb.MergeRequestState_MERGE_REQUEST_STATE_MERGED:
		return domain.MRStateMerged, nil
	case storagepb.MergeRequestState_MERGE_REQUEST_STATE_CLOSED:
		return domain.MRStateClosed, nil
	}
	return "", fmt.Errorf("unknown merge request state %d", s)
}

func encodeMode(m domain.MergeMode) (storagepb.MergeMode, error) {
	switch m {
	case "":
		return storagepb.MergeMode_MERGE_MODE_UNSPECIFIED, nil
	case domain.MergeModeFastForward:
		return storagepb.MergeMode_MERGE_MODE_FAST_FORWARD, nil
	case domain.MergeModeSquash:
		return storagepb.MergeMode_MERGE_MODE_SQUASH, nil
	case domain.MergeModeMerge:
		return storagepb.MergeMode_MERGE_MODE_MERGE, nil
	}
	return 0, fmt.Errorf("%w: merge mode %q", domain.ErrInvalid, m)
}

// decodeMode maps an unknown mode to none: it only labels a merged request.
func decodeMode(m storagepb.MergeMode) domain.MergeMode {
	switch m {
	case storagepb.MergeMode_MERGE_MODE_FAST_FORWARD:
		return domain.MergeModeFastForward
	case storagepb.MergeMode_MERGE_MODE_SQUASH:
		return domain.MergeModeSquash
	case storagepb.MergeMode_MERGE_MODE_MERGE:
		return domain.MergeModeMerge
	}
	return ""
}

func encodeSide(s domain.DiffSide) (storagepb.DiffSide, error) {
	switch s {
	case domain.SideNone:
		return storagepb.DiffSide_DIFF_SIDE_UNSPECIFIED, nil
	case domain.SideOld:
		return storagepb.DiffSide_DIFF_SIDE_OLD, nil
	case domain.SideNew:
		return storagepb.DiffSide_DIFF_SIDE_NEW, nil
	}
	return 0, fmt.Errorf("%w: diff side %q", domain.ErrInvalid, s)
}

func decodeSide(s storagepb.DiffSide) domain.DiffSide {
	switch s {
	case storagepb.DiffSide_DIFF_SIDE_OLD:
		return domain.SideOld
	case storagepb.DiffSide_DIFF_SIDE_NEW:
		return domain.SideNew
	}
	return domain.SideNone
}

func encodeSettings(s domain.RepoSettings) (*storagepb.RepoSettings, error) {
	pb := &storagepb.RepoSettings{FailingChecksBlock: s.FailingChecksBlock, MergeModesV2: true, AllowAutoMerge: s.AllowAutoMerge}
	for _, m := range s.AllowedMergeModes {
		mode, err := encodeMode(m)
		if err != nil || mode == storagepb.MergeMode_MERGE_MODE_UNSPECIFIED {
			return nil, fmt.Errorf("%w: merge mode %q in the settings", domain.ErrInvalid, m)
		}
		pb.AllowedMergeModes = append(pb.AllowedMergeModes, mode)
	}
	issues, err := encodeIssuesMode(s.Issues)
	if err != nil {
		return nil, err
	}
	pb.Issues = issues
	return pb, nil
}

// decodeSettings returns the defaults for a repository stored before merge
// requests existed, and for settings left with no mode that this build knows.
func decodeSettings(pb *storagepb.RepoSettings) domain.RepoSettings {
	if pb == nil {
		return domain.DefaultRepoSettings()
	}
	var modes []domain.MergeMode
	for _, m := range pb.GetAllowedMergeModes() {
		if mode := decodeMode(m); mode != "" {
			modes = append(modes, mode)
		}
	}
	if len(modes) == 0 {
		return domain.DefaultRepoSettings()
	}
	normalized, err := domain.NormalizeMergeModes(modes)
	if err != nil {
		return domain.DefaultRepoSettings()
	}
	// Settings stored before merge commits: exactly {fast-forward, squash} was
	// the old default, "everything allowed".
	if !pb.GetMergeModesV2() && slices.Equal(normalized, []domain.MergeMode{domain.MergeModeFastForward, domain.MergeModeSquash}) {
		normalized = domain.MergeModes()
	}
	return domain.RepoSettings{AllowedMergeModes: normalized, FailingChecksBlock: pb.GetFailingChecksBlock(), Issues: decodeIssuesMode(pb.GetIssues()), AllowAutoMerge: pb.GetAllowAutoMerge()}
}
