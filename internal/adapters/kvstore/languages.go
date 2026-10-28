// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"errors"
	"fmt"

	kv "bnema.dev/ermite-kv"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Languages is the ermite-kv backed ports.LanguageStore. It shares the
// database of the Store it comes from. The record lives under lang/<repo>,
// apart from the repository record: it is derived data that a background
// writer rewrites.
type Languages struct {
	db *kv.DB
}

var _ ports.LanguageStore = (*Languages)(nil)

// Languages returns the language store over the same database. It is valid
// until the Store is closed.
func (s *Store) Languages() *Languages { return &Languages{db: s.db} }

const languagesPrefix = "lang/"

func languagesKey(repo domain.RepoName) []byte { return []byte(languagesPrefix + repo.String()) }

// languageKeys lists the language key of repo, for Delete.
func languageKeys(ctx context.Context, snap *kv.Snapshot, repo domain.RepoName) [][]byte {
	if _, ok := snap.Get(ctx, languagesKey(repo)); ok {
		return [][]byte{languagesKey(repo)}
	}
	return nil
}

// Get returns the record of repo; ok is false when there is none.
func (l *Languages) Get(ctx context.Context, repo domain.RepoName) (domain.RepoLanguages, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.RepoLanguages{}, false, err
	}
	item, ok := l.db.Get(ctx, languagesKey(repo))
	if !ok {
		return domain.RepoLanguages{}, false, nil
	}
	rec, err := decodeRepoLanguages(item.Value)
	if err != nil {
		return domain.RepoLanguages{}, false, err
	}
	return rec, true, nil
}

// Put stores rec. It returns domain.ErrNotFound, and writes nothing, when the
// repository does not exist or was created again (its created_at is not
// rec.RepoCreatedAt).
func (l *Languages) Put(ctx context.Context, rec domain.RepoLanguages) error {
	val, err := encodeRepoLanguages(rec)
	if err != nil {
		return err
	}
	for range repoMutationTries {
		if err := ctx.Err(); err != nil {
			return err
		}
		item, ok := l.db.Get(ctx, repoKey(rec.Repo))
		if !ok {
			return domain.ErrNotFound
		}
		repo, err := decodeRepo(item.Value)
		if err != nil {
			return err
		}
		if !repo.CreatedAt.Equal(rec.RepoCreatedAt) {
			return domain.ErrNotFound
		}
		var b kv.Batch
		if err := guardRepoKey(ctx, l.db, &b, rec.Repo); err != nil {
			return err
		}
		b.Put(languagesKey(rec.Repo), val)
		if _, err := l.db.Apply(ctx, &b); err != nil {
			if errors.Is(err, kv.ErrConflict) {
				continue
			}
			return fmt.Errorf("store languages of %q: %w", rec.Repo, err)
		}
		return nil
	}
	return domain.ErrConflict
}

// Delete removes the record of repo; a missing record is not an error.
func (l *Languages) Delete(ctx context.Context, repo domain.RepoName) error {
	var b kv.Batch
	b.Delete(languagesKey(repo))
	if _, err := l.db.Apply(ctx, &b); err != nil {
		return fmt.Errorf("delete languages of %q: %w", repo, err)
	}
	return nil
}

func encodeRepoLanguages(r domain.RepoLanguages) ([]byte, error) {
	if r.Repo.IsZero() {
		return nil, fmt.Errorf("%w: empty name", domain.ErrInvalidName)
	}
	pb := &storagepb.RepoLanguages{
		Repo:          r.Repo.String(),
		RepoCreatedAt: encodeTime(r.RepoCreatedAt),
		Commit:        r.Commit.String(),
		Version:       uint32(r.Version),
		ComputedAt:    encodeTime(r.ComputedAt),
		Truncated:     r.Truncated,
	}
	for _, l := range r.Languages {
		pb.Languages = append(pb.Languages, &storagepb.LanguageBytes{Name: l.Name, Bytes: l.Bytes})
	}
	val, err := proto.MarshalOptions{Deterministic: true}.Marshal(pb)
	if err != nil {
		return nil, fmt.Errorf("%w: language record is not valid UTF-8", domain.ErrInvalid)
	}
	return val, nil
}

func decodeRepoLanguages(val []byte) (domain.RepoLanguages, error) {
	var pb storagepb.RepoLanguages
	if err := proto.Unmarshal(val, &pb); err != nil {
		return domain.RepoLanguages{}, fmt.Errorf("decode languages record: %w", err)
	}
	repo, err := domain.ParseRepoName(pb.GetRepo())
	if err != nil {
		return domain.RepoLanguages{}, fmt.Errorf("decode languages record: %w", err)
	}
	var commit domain.ObjectID
	if pb.GetCommit() != "" {
		if commit, err = domain.ParseObjectID(pb.GetCommit()); err != nil {
			return domain.RepoLanguages{}, fmt.Errorf("decode languages record: %w", err)
		}
	}
	rec := domain.RepoLanguages{
		Repo:          repo,
		RepoCreatedAt: decodeTime(pb.GetRepoCreatedAt()),
		Commit:        commit,
		Version:       int(pb.GetVersion()),
		ComputedAt:    decodeTime(pb.GetComputedAt()),
		Truncated:     pb.GetTruncated(),
	}
	for _, l := range pb.GetLanguages() {
		rec.Languages = append(rec.Languages, domain.LanguageBytes{Name: l.GetName(), Bytes: l.GetBytes()})
	}
	return rec, nil
}
