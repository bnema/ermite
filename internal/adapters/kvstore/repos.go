// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	kv "bnema.dev/ermite-kv"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
)

const repoPrefix = "repo/"

func repoKey(name domain.RepoName) []byte { return []byte(repoPrefix + name.String()) }

// Create stores repo with an "absent" precondition: a repository with that
// name, or a concurrent Create that commits first, yields domain.ErrAlreadyExists.
func (s *Store) Create(ctx context.Context, repo domain.Repo) error {
	val, err := encodeRepo(repo)
	if err != nil {
		return err
	}
	key := repoKey(repo.Name)
	var b kv.Batch
	b.RequireAbsent(key)
	b.Put(key, val)
	if _, err := s.db.Apply(ctx, &b); err != nil {
		if errors.Is(err, kv.ErrConflict) {
			return domain.ErrAlreadyExists
		}
		return fmt.Errorf("create repository %q: %w", repo.Name, err)
	}
	return nil
}

// Get returns the repository or domain.ErrNotFound.
func (s *Store) Get(ctx context.Context, name domain.RepoName) (domain.Repo, error) {
	item, ok := s.db.Get(ctx, repoKey(name))
	if !ok {
		return domain.Repo{}, domain.ErrNotFound
	}
	return decodeRepo(item.Value)
}

// List returns every repository ordered by name.
func (s *Store) List(ctx context.Context) ([]domain.Repo, error) {
	var repos []domain.Repo
	for key, item := range s.db.Snapshot(ctx).Prefix(ctx, []byte(repoPrefix)) {
		repo, err := decodeRepo(item.Value)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		repos = append(repos, repo)
	}
	return repos, nil
}

const repoMutationTries = 8

// Mutate applies a retry-safe mutation under the version it read.
func (s *Store) Mutate(ctx context.Context, name domain.RepoName, fn func(*domain.Repo) (bool, error)) (domain.Repo, error) {
	key := repoKey(name)
	for range repoMutationTries {
		if err := ctx.Err(); err != nil {
			return domain.Repo{}, err
		}
		item, ok := s.db.Get(ctx, key)
		if !ok {
			return domain.Repo{}, domain.ErrNotFound
		}
		repo, err := decodeRepo(item.Value)
		if err != nil {
			return domain.Repo{}, err
		}
		changed, err := fn(&repo)
		if err != nil {
			return domain.Repo{}, err
		}
		if repo.Name != name {
			return domain.Repo{}, fmt.Errorf("mutate repository name: %w", domain.ErrInvalidName)
		}
		if !changed {
			return decodeRepo(item.Value)
		}
		val, err := encodeRepo(repo)
		if err != nil {
			return domain.Repo{}, err
		}
		var b kv.Batch
		b.RequireVersion(key, item.Version)
		b.Put(key, val)
		if _, err := s.db.Apply(ctx, &b); err != nil {
			if errors.Is(err, kv.ErrConflict) {
				continue
			}
			return domain.Repo{}, fmt.Errorf("mutate repository %q: %w", name, err)
		}
		return decodeRepo(val)
	}
	return domain.Repo{}, domain.ErrConflict
}

// guardRepoKey makes the batch depend on the repository: it requires the
// repository key to be at the version just read and rewrites it, which gives it
// a new version. Every write that adds a key under the repository (a merge
// request, a comment, a read position, a CI run, job or check) goes through it,
// so that
//
//   - such a write fails with domain.ErrNotFound once the repository is deleted,
//     and leaves no key that a repository created again under the name would
//     inherit;
//   - Store.Delete, which requires the version of the repository key it listed
//     the keys at, notices a write that got in after the listing and lists again.
//
// It returns domain.ErrNotFound when the repository does not exist.
func guardRepoKey(ctx context.Context, db *kv.DB, b *kv.Batch, repo domain.RepoName) error {
	item, ok := db.Get(ctx, repoKey(repo))
	if !ok {
		return domain.ErrNotFound
	}
	b.RequireVersion(repoKey(repo), item.Version)
	b.Put(repoKey(repo), item.Value)
	return nil
}

// deleteChunk is how many merge request keys one batch deletes before the
// last one, which also removes the repository.
const deleteChunk = 1000

// deleteTries bounds the listings of Delete: a listing is repeated when a merge
// request write got in after it.
const deleteTries = 32

// Delete removes the repository and everything keyed to it: its merge
// requests, their comments and read positions, and its number sequence, and
// its CI runs, jobs, checks, run numbers and artifacts. The
// Git data is moved to the trash by the caller; the merge requests are not
// kept anywhere, like the repository metadata, so a repository created again
// under the same name starts clean, with its numbers starting at 1. A missing
// repository is not an error.
//
// The merge request keys go first, in chunks, and the repository key goes with
// the last chunk: a delete that fails half way leaves the repository, which
// the caller can delete again. The last batch requires the repository key to
// be at the version the keys were listed at. Every write of a merge request key
// rewrites the repository key (see guardRepoKey), so a write that
// got in after the listing makes the batch fail and the keys are listed again,
// and a write after the batch finds no repository: no key outlives the
// repository to attach itself to a repository created again.
func (s *Store) Delete(ctx context.Context, name domain.RepoName) error {
	for range deleteTries {
		snap := s.db.Snapshot(ctx)
		repoItem, repoOK := snap.Get(ctx, repoKey(name))
		keys := append(mergeRequestKeys(ctx, snap, name), issueKeys(ctx, snap, name)...)
		keys = append(keys, pendingKeys(ctx, snap, name)...)
		keys = append(keys, ciKeys(ctx, snap, name)...)
		keys = append(keys, settingsKeys(ctx, snap, name)...)
		for len(keys) > deleteChunk {
			var b kv.Batch
			for _, k := range keys[:deleteChunk] {
				b.Delete(k)
			}
			if _, err := s.db.Apply(ctx, &b); err != nil {
				return fmt.Errorf("delete merge requests of %q: %w", name, err)
			}
			keys = keys[deleteChunk:]
		}
		var b kv.Batch
		if repoOK {
			b.RequireVersion(repoKey(name), repoItem.Version)
		} else {
			b.RequireAbsent(repoKey(name))
		}
		for _, k := range keys {
			b.Delete(k)
		}
		b.Delete(repoKey(name))
		_, err := s.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue
		}
		if err != nil {
			return fmt.Errorf("delete repository %q: %w", name, err)
		}
		return nil
	}
	return fmt.Errorf("delete repository %q: %w", name, domain.ErrConflict)
}

func encodeRepo(r domain.Repo) ([]byte, error) {
	if r.Name.IsZero() {
		return nil, fmt.Errorf("%w: empty name", domain.ErrInvalidName)
	}
	if !utf8.ValidString(r.Description) {
		return nil, errors.New("repository description is not valid UTF-8")
	}
	vis, err := encodeVisibility(r.Visibility)
	if err != nil {
		return nil, err
	}
	settings := r.Settings
	if settings.IsZero() {
		// Settings are always stored explicitly, so that a later change of
		// the defaults never alters a stored repository.
		settings = domain.DefaultRepoSettings()
	}
	pbSettings, err := encodeSettings(settings)
	if err != nil {
		return nil, err
	}
	pb := &storagepb.Repo{
		Name:        r.Name.String(),
		Description: r.Description,
		Topics:      r.Topics,
		Visibility:  vis,
		CreatedAt:   encodeTime(r.CreatedAt),
		PushedAt:    encodeTime(r.PushedAt),
		ActiveAt:    encodeTime(r.ActiveAt),
		Settings:    pbSettings,
	}
	return mustMarshal(pb), nil
}

func decodeRepo(val []byte) (domain.Repo, error) {
	var pb storagepb.Repo
	if err := proto.Unmarshal(val, &pb); err != nil {
		return domain.Repo{}, fmt.Errorf("decode repository: %w", err)
	}
	name, err := domain.ParseRepoName(pb.GetName())
	if err != nil {
		return domain.Repo{}, fmt.Errorf("decode repository: %w", err)
	}
	return domain.Repo{
		Name:        name,
		Description: pb.GetDescription(),
		Topics:      slices.Clone(pb.GetTopics()),
		Visibility:  decodeVisibility(pb.GetVisibility()),
		CreatedAt:   decodeTime(pb.GetCreatedAt()),
		PushedAt:    decodeTime(pb.GetPushedAt()),
		ActiveAt:    decodeTime(pb.GetActiveAt()),
		Settings:    decodeSettings(pb.GetSettings()),
	}, nil
}

func encodeVisibility(v domain.Visibility) (storagepb.Visibility, error) {
	switch v {
	case domain.VisibilityPublic:
		return storagepb.Visibility_VISIBILITY_PUBLIC, nil
	case domain.VisibilityPrivate:
		return storagepb.Visibility_VISIBILITY_PRIVATE, nil
	}
	return storagepb.Visibility_VISIBILITY_UNSPECIFIED, fmt.Errorf("%w: %q", domain.ErrInvalidVisibility, v)
}

// decodeVisibility maps anything but public to private, including the
// unspecified value and values from a newer schema: the safe side.
func decodeVisibility(v storagepb.Visibility) domain.Visibility {
	if v == storagepb.Visibility_VISIBILITY_PUBLIC {
		return domain.VisibilityPublic
	}
	return domain.VisibilityPrivate
}

func encodeTime(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func decodeTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}
