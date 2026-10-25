// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	kv "bnema.dev/ermite-kv"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The owner's activity journal:
//
//	journal/<time><seq>/<repo>      JournalEntry
//	journalseq                      Sequence: the last <seq> handed out
//
// <time> is the Unix time of the entry in nanoseconds and <seq> a sequence of
// the database, both 16 lowercase hexadecimal digits (big-endian), so that
// the keys sort by time, then by append order. The sequence is written in the
// batch of the entry: it never goes back, across restarts and processes, so
// two entries never share a key and entries of the same time keep their
// order whatever their repository. The entries are global: they are not
// deleted with their repository but expire (Prune); the inbox leaves out
// those of a repository that is gone or was created again since.
const (
	journalPrefix = "journal/"
	journalSeqKey = "journalseq"
)

// journalRepoAt is where the repository starts in a key: after the prefix,
// the time, the sequence and the slash.
const journalRepoAt = len(journalPrefix) + 16 + 16 + 1

// journalCheckEvery is how many keys a scan reads between two looks at its
// context: the snapshot iterators ignore it.
const journalCheckEvery = 1024

// Journal is the ermite-kv backed ports.JournalStore. It shares the database
// of the Store it comes from.
type Journal struct {
	db *kv.DB

	// mu serializes the appends of this instance, so that they do not race
	// for the sequence; another instance on the same database is a conflict
	// that Append retries.
	mu sync.Mutex
}

var _ ports.JournalStore = (*Journal)(nil)

// Journal returns the activity journal over the same database. It is valid
// until the Store is closed.
func (s *Store) Journal() *Journal { return &Journal{db: s.db} }

// journalTimeKey is the start of the keys of the entries at t and after.
func journalTimeKey(t time.Time) string {
	return journalPrefix + hex16(journalNanos(t))
}

func journalKey(t time.Time, seq uint64, repo domain.RepoName) []byte {
	return []byte(journalTimeKey(t) + hex16(seq) + "/" + repo.String())
}

// journalNanos is t in Unix nanoseconds, clamped to what UnixNano can
// represent: a time before 1970 is 0 and a time after 2262 is the largest
// value. The bounds are checked first because UnixNano overflows outside them.
func journalNanos(t time.Time) uint64 {
	switch {
	case !t.After(unixEpoch):
		return 0
	case t.After(maxUnixNano):
		return math.MaxInt64
	}
	return uint64(t.UnixNano())
}

var (
	unixEpoch   = time.Unix(0, 0)
	maxUnixNano = time.Unix(0, math.MaxInt64)
)

func hex16(n uint64) string {
	s := strconv.FormatUint(n, 16)
	const zeros = "0000000000000000"
	return zeros[:16-len(s)] + s
}

// Append stores the entry under the next sequence, in the batch that moves
// the sequence.
func (j *Journal) Append(ctx context.Context, e domain.JournalEntry) error {
	if e.Repo.IsZero() || e.At.IsZero() {
		return fmt.Errorf("append journal entry: %w: no repository or time", domain.ErrInvalid)
	}
	val, err := encodeJournalEntry(e)
	if err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	seqKey := []byte(journalSeqKey)
	for range updateAttempts {
		var last uint64
		item, ok := j.db.Get(ctx, seqKey)
		if ok {
			var seq storagepb.Sequence
			if err := proto.Unmarshal(item.Value, &seq); err != nil {
				return fmt.Errorf("append journal entry: decode %s: %w", journalSeqKey, err)
			}
			last = uint64(seq.GetLast())
		}
		var b kv.Batch
		if ok {
			b.RequireVersion(seqKey, item.Version)
		} else {
			b.RequireAbsent(seqKey)
		}
		b.Put(seqKey, mustMarshal(&storagepb.Sequence{Last: int64(last + 1)}))
		b.Put(journalKey(e.At, last+1, e.Repo), val)
		_, err := j.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue // another instance appended meanwhile
		}
		if err != nil {
			return fmt.Errorf("append journal entry: %w", err)
		}
		return nil
	}
	return fmt.Errorf("append journal entry: %w", domain.ErrConflict)
}

// ListSince reads the keys from since to before (the end of the journal when
// before is empty) once and keeps the newest limit accepted entries in a
// ring: the cost in memory does not grow with the range. The repository is
// read from the key. Without keep, the ring holds raw items, decoded at the
// end; with it, each entry of the repository is decoded to be weighed, and
// only the accepted ones stay.
func (j *Journal) ListSince(ctx context.Context, since time.Time, before string, repo domain.RepoName, keep func(domain.JournalEntry) bool, limit int) (ports.JournalList, error) {
	var out ports.JournalList
	if limit < 1 {
		return out, fmt.Errorf("list journal: %w: limit %d", domain.ErrInvalidPageSize, limit)
	}
	end, err := journalEndKey(before)
	if err != nil {
		return out, err
	}
	start := []byte(journalTimeKey(since))
	if err := ctx.Err(); err != nil {
		return out, fmt.Errorf("list journal: %w", err)
	}
	type slot struct {
		key     []byte
		val     []byte // nil once decoded
		entry   domain.JournalEntry
		decoded bool
	}
	ring := make([]slot, 0, min(limit, 64))
	next, accepted, read := 0, 0, 0
	want := repo.String()
	for key, item := range j.db.Snapshot(ctx).Range(ctx, start, end) {
		if read++; read%journalCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return out, fmt.Errorf("list journal: %w", err)
			}
		}
		if want != "" && (len(key) <= journalRepoAt || string(key[journalRepoAt:]) != want) {
			continue
		}
		s := slot{key: key, val: item.Value}
		if keep != nil {
			e, err := decodeJournalKey(key, item.Value)
			if err != nil {
				return ports.JournalList{}, fmt.Errorf("decode %s: %w", key, err)
			}
			if !keep(e) {
				continue
			}
			s = slot{key: key, entry: e, decoded: true}
		}
		accepted++
		if len(ring) < limit {
			ring = append(ring, s)
			continue
		}
		ring[next] = s
		next = (next + 1) % limit
	}
	out.Entries = make([]domain.JournalEntry, 0, len(ring))
	// The newest is just before next in the ring.
	for i := range len(ring) {
		s := ring[(next-1-i+2*len(ring))%len(ring)]
		if !s.decoded {
			e, err := decodeJournalKey(s.key, s.val)
			if err != nil {
				return ports.JournalList{}, fmt.Errorf("decode %s: %w", s.key, err)
			}
			s.entry = e
		}
		out.Entries = append(out.Entries, s.entry)
	}
	out.Truncated = accepted > len(ring)
	return out, nil
}

// journalEndKey is where a scan stops: before the entry with ID before, or
// at the end of the journal when it is empty.
func journalEndKey(before string) ([]byte, error) {
	if before == "" {
		return []byte(journalPrefix[:len(journalPrefix)-1] + "0"), nil // the byte after '/'
	}
	if !domain.ValidJournalID(before) {
		return nil, fmt.Errorf("list journal: %w: entry id %q", domain.ErrInvalidPageToken, before)
	}
	// The keys of the entry itself start with journal/<before>/: after it.
	return []byte(journalPrefix + before), nil
}

// Prune deletes the entries before the given time, deleteChunk keys per
// batch, as it reads them.
func (j *Journal) Prune(ctx context.Context, before time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("prune journal: %w", err)
	}
	done := 0
	var b kv.Batch
	flush := func() error {
		if b.Len() == 0 {
			return nil
		}
		n := b.Len()
		if _, err := j.db.Apply(ctx, &b); err != nil {
			return fmt.Errorf("prune journal: %w", err)
		}
		done += n
		b.Reset()
		return nil
	}
	read := 0
	for key := range j.db.Snapshot(ctx).Range(ctx, []byte(journalPrefix), []byte(journalTimeKey(before))) {
		if read++; read%journalCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return done, fmt.Errorf("prune journal: %w", err)
			}
		}
		b.Delete(key)
		if b.Len() == deleteChunk {
			if err := flush(); err != nil {
				return done, err
			}
		}
	}
	if err := flush(); err != nil {
		return done, err
	}
	return done, nil
}

func encodeJournalEntry(e domain.JournalEntry) ([]byte, error) {
	pb := &storagepb.JournalEntry{
		At: encodeTime(e.At), Repo: e.Repo.String(), Kind: string(e.Kind),
		Actor: domain.ValidGitText(e.Actor), Number: e.Number, Title: domain.ValidGitText(e.Title),
		Ref: domain.ValidGitText(e.Ref), Commits: int32(min(e.Commits, domain.MaxJournalCommits)), More: e.More,
		Summary: domain.ValidGitText(e.Summary),
	}
	if !e.Commit.IsZero() {
		pb.Commit = e.Commit.String()
	}
	val, err := proto.Marshal(pb)
	if err != nil {
		return nil, fmt.Errorf("encode journal entry: %w", err)
	}
	return val, nil
}

// decodeJournalKey decodes the entry stored under key, its ID taken from the
// key.
func decodeJournalKey(key, val []byte) (domain.JournalEntry, error) {
	e, err := decodeJournalEntry(val)
	if err != nil {
		return e, err
	}
	if len(key) < journalRepoAt-1 {
		return domain.JournalEntry{}, fmt.Errorf("short journal key")
	}
	e.ID = string(key[len(journalPrefix) : journalRepoAt-1])
	return e, nil
}

func decodeJournalEntry(val []byte) (domain.JournalEntry, error) {
	var pb storagepb.JournalEntry
	if err := proto.Unmarshal(val, &pb); err != nil {
		return domain.JournalEntry{}, err
	}
	repo, err := domain.ParseRepoName(pb.GetRepo())
	if err != nil {
		return domain.JournalEntry{}, err
	}
	e := domain.JournalEntry{
		At: decodeTime(pb.GetAt()), Repo: repo, Kind: domain.JournalKind(pb.GetKind()),
		Actor: pb.GetActor(), Number: pb.GetNumber(), Title: pb.GetTitle(), Ref: pb.GetRef(),
		Commits: int(pb.GetCommits()), More: pb.GetMore(), Summary: pb.GetSummary(),
	}
	if c := pb.GetCommit(); c != "" {
		if e.Commit, err = domain.ParseObjectID(c); err != nil {
			return domain.JournalEntry{}, err
		}
	}
	return e, nil
}
