// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	kv "bnema.dev/ermite-kv"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Deps is the ermite-kv backed ports.DepsStore. It shares the database of the
// Store it comes from (Store already has Get, List and Mutate for repositories).
type Deps struct {
	db *kv.DB
}

var _ ports.DepsStore = (*Deps)(nil)

// Deps returns the Bernard store over the same database. It is valid until the
// Store is closed.
func (s *Store) Deps() *Deps { return &Deps{db: s.db} }

// Key layout of the Bernard records:
//
//	dpend/<repo>                                          whole RepoDeps record
//	dign/<repo>/<eco>/<dir>/<name>@<version>              ignore marker (empty value)
//	dseen/<eco>/<name>@<version>@<sha>                    first-seen time, global
//
// Every free-form component (eco, dir, name, version, sha) goes through
// escapeDepsPart, so a "/" or "@" inside a module path, directory or version
// cannot move a boundary and make two refs share a key. Repository names
// cannot contain "/" so <repo> needs no escaping.
const (
	depsPendPrefix = "dpend/"
	depsIgnPrefix  = "dign/"
	depsSeenPrefix = "dseen/"
)

func depsPendKey(repo domain.RepoName) []byte { return []byte(depsPendPrefix + repo.String()) }

func depsIgnoreRepoPrefix(repo domain.RepoName) string {
	return depsIgnPrefix + repo.String() + "/"
}

func depsIgnoreKey(repo domain.RepoName, ref domain.DepRef) []byte {
	return []byte(depsIgnoreRepoPrefix(repo) +
		escapeDepsPart(string(ref.Ecosystem)) + "/" +
		escapeDepsPart(ref.Directory) + "/" +
		escapeDepsPart(ref.Name) + "@" + escapeDepsPart(ref.Version))
}

func depsSeenKey(eco domain.DepsEcosystem, name, version, sha string) []byte {
	return []byte(depsSeenPrefix + escapeDepsPart(string(eco)) + "/" +
		escapeDepsPart(name) + "@" + escapeDepsPart(version) + "@" + escapeDepsPart(sha))
}

// escapeDepsPart percent-encodes every byte but [A-Za-z0-9._~-], so the output
// holds neither "/" nor "@" nor "%" except as an escape. The empty string (the
// repository root directory, an action without sha) is written as a lone "%",
// which no escaped non-empty string can equal.
func escapeDepsPart(s string) string {
	if s == "" {
		return "%"
	}
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '~', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}

// depsKeys lists the per-repository Bernard keys, for Delete. dseen/ is global
// and stays.
func depsKeys(ctx context.Context, snap *kv.Snapshot, repo domain.RepoName) [][]byte {
	var keys [][]byte
	if _, ok := snap.Get(ctx, depsPendKey(repo)); ok {
		keys = append(keys, depsPendKey(repo))
	}
	for key := range snap.Prefix(ctx, []byte(depsIgnoreRepoPrefix(repo))) {
		keys = append(keys, key)
	}
	return keys
}

// Get returns the record of repo, or RepoDeps{Repo: repo} when absent.
func (d *Deps) Get(ctx context.Context, repo domain.RepoName) (domain.RepoDeps, error) {
	item, ok := d.db.Get(ctx, depsPendKey(repo))
	if !ok {
		return domain.RepoDeps{Repo: repo}, nil
	}
	return decodeRepoDeps(item.Value)
}

// List returns every record ordered by repository.
func (d *Deps) List(ctx context.Context) ([]domain.RepoDeps, error) {
	var out []domain.RepoDeps
	for key, item := range d.db.Snapshot(ctx).Prefix(ctx, []byte(depsPendPrefix)) {
		rd, err := decodeRepoDeps(item.Value)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		out = append(out, rd)
	}
	return out, nil
}

// Mutate applies fn under the version it read (or the absence of the record,
// which it creates). changed=false writes nothing.
func (d *Deps) Mutate(ctx context.Context, repo domain.RepoName, fn func(*domain.RepoDeps) (bool, error)) (domain.RepoDeps, error) {
	key := depsPendKey(repo)
	for range repoMutationTries {
		if err := ctx.Err(); err != nil {
			return domain.RepoDeps{}, err
		}
		item, ok := d.db.Get(ctx, key)
		rd := domain.RepoDeps{Repo: repo}
		if ok {
			var err error
			if rd, err = decodeRepoDeps(item.Value); err != nil {
				return domain.RepoDeps{}, err
			}
		}
		changed, err := fn(&rd)
		if err != nil {
			return domain.RepoDeps{}, err
		}
		if rd.Repo != repo {
			return domain.RepoDeps{}, fmt.Errorf("mutate deps repository name: %w", domain.ErrInvalidName)
		}
		if !changed {
			if ok {
				return decodeRepoDeps(item.Value)
			}
			return domain.RepoDeps{Repo: repo}, nil
		}
		val, err := encodeRepoDeps(rd)
		if err != nil {
			return domain.RepoDeps{}, err
		}
		var b kv.Batch
		if ok {
			b.RequireVersion(key, item.Version)
		} else {
			b.RequireAbsent(key)
		}
		b.Put(key, val)
		if _, err := d.db.Apply(ctx, &b); err != nil {
			if errors.Is(err, kv.ErrConflict) {
				continue
			}
			return domain.RepoDeps{}, fmt.Errorf("mutate deps of %q: %w", repo, err)
		}
		return decodeRepoDeps(val)
	}
	return domain.RepoDeps{}, domain.ErrConflict
}

// Ignore records refs as ignored for repo; writing an existing marker again
// changes nothing.
func (d *Deps) Ignore(ctx context.Context, repo domain.RepoName, refs []domain.DepRef) error {
	if len(refs) == 0 {
		return nil
	}
	var b kv.Batch
	for _, ref := range refs {
		b.Put(depsIgnoreKey(repo, ref), nil)
	}
	if _, err := d.db.Apply(ctx, &b); err != nil {
		return fmt.Errorf("ignore dependencies of %q: %w", repo, err)
	}
	return nil
}

// IsIgnored reports whether ref is ignored for repo.
func (d *Deps) IsIgnored(ctx context.Context, repo domain.RepoName, ref domain.DepRef) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	_, ok := d.db.Get(ctx, depsIgnoreKey(repo, ref))
	return ok, nil
}

// FirstSeen returns the recorded first-seen time, recording at if none exists.
func (d *Deps) FirstSeen(ctx context.Context, eco domain.DepsEcosystem, name, version, sha string, at time.Time) (time.Time, error) {
	key := depsSeenKey(eco, name, version, sha)
	val := mustMarshal(&storagepb.DepFirstSeen{At: encodeTime(at)})
	for range repoMutationTries {
		if err := ctx.Err(); err != nil {
			return time.Time{}, err
		}
		if item, ok := d.db.Get(ctx, key); ok {
			return decodeFirstSeen(item.Value)
		}
		var b kv.Batch
		b.RequireAbsent(key)
		b.Put(key, val)
		_, err := d.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue // lost the race: re-read the winner's time
		}
		if err != nil {
			return time.Time{}, fmt.Errorf("record first seen of %s: %w", name, err)
		}
		return decodeFirstSeen(val)
	}
	return time.Time{}, domain.ErrConflict
}

func decodeFirstSeen(val []byte) (time.Time, error) {
	var pb storagepb.DepFirstSeen
	if err := proto.Unmarshal(val, &pb); err != nil {
		return time.Time{}, fmt.Errorf("decode first seen: %w", err)
	}
	return decodeTime(pb.GetAt()), nil
}

func encodeDepRef(r domain.DepRef) *storagepb.DepRef {
	return &storagepb.DepRef{Ecosystem: string(r.Ecosystem), Directory: r.Directory, Name: r.Name, Version: r.Version}
}

func decodeDepRef(pb *storagepb.DepRef) domain.DepRef {
	return domain.DepRef{Ecosystem: domain.DepsEcosystem(pb.GetEcosystem()), Directory: pb.GetDirectory(), Name: pb.GetName(), Version: pb.GetVersion()}
}

func encodeDepUpdate(u domain.DepUpdate) *storagepb.DepUpdate {
	return &storagepb.DepUpdate{Ref: encodeDepRef(u.DepRef), Current: u.Current, Kind: int32(u.Kind), New: u.New}
}

func decodeDepUpdate(u *storagepb.DepUpdate) domain.DepUpdate {
	return domain.DepUpdate{DepRef: decodeDepRef(u.GetRef()), Current: u.GetCurrent(), Kind: domain.DepKind(u.GetKind()), New: u.GetNew()}
}

func encodeRepoDeps(rd domain.RepoDeps) ([]byte, error) {
	if rd.Repo.IsZero() {
		return nil, fmt.Errorf("%w: empty name", domain.ErrInvalidName)
	}
	pb := &storagepb.RepoDeps{
		Repo:         rd.Repo.String(),
		CheckedAt:    encodeTime(rd.CheckedAt),
		SnoozedUntil: encodeTime(rd.SnoozedUntil),
		State:        int32(rd.State),
		Error:        rd.Error,
		RunId:        rd.RunID,
		Mr:           rd.MR,
	}
	for _, u := range rd.Updates {
		pb.Updates = append(pb.Updates, encodeDepUpdate(u))
	}
	for _, u := range rd.RunUpdates {
		pb.RunUpdates = append(pb.RunUpdates, encodeDepUpdate(u))
	}
	for _, r := range rd.MRVersions {
		pb.MrVersions = append(pb.MrVersions, encodeDepRef(r))
	}
	val, err := proto.MarshalOptions{Deterministic: true}.Marshal(pb)
	if err != nil {
		return nil, fmt.Errorf("%w: deps record is not valid UTF-8", domain.ErrInvalid)
	}
	return val, nil
}

func decodeRepoDeps(val []byte) (domain.RepoDeps, error) {
	var pb storagepb.RepoDeps
	if err := proto.Unmarshal(val, &pb); err != nil {
		return domain.RepoDeps{}, fmt.Errorf("decode deps record: %w", err)
	}
	repo, err := domain.ParseRepoName(pb.GetRepo())
	if err != nil {
		return domain.RepoDeps{}, fmt.Errorf("decode deps record: %w", err)
	}
	rd := domain.RepoDeps{
		Repo:         repo,
		CheckedAt:    decodeTime(pb.GetCheckedAt()),
		SnoozedUntil: decodeTime(pb.GetSnoozedUntil()),
		State:        domain.DepsState(pb.GetState()),
		Error:        pb.GetError(),
		RunID:        pb.GetRunId(),
		MR:           pb.GetMr(),
	}
	for _, u := range pb.GetUpdates() {
		rd.Updates = append(rd.Updates, decodeDepUpdate(u))
	}
	for _, u := range pb.GetRunUpdates() {
		rd.RunUpdates = append(rd.RunUpdates, decodeDepUpdate(u))
	}
	for _, r := range pb.GetMrVersions() {
		rd.MRVersions = append(rd.MRVersions, decodeDepRef(r))
	}
	return rd, nil
}
