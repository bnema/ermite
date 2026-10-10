// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"errors"
	"fmt"
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

// Update replaces the stored repository. It returns domain.ErrNotFound if
// there is none and domain.ErrConflict if another write changed it since it
// was read.
//
//hexcheck:ignore suspicious-business-logic-in-adapter port method name only
func (s *Store) Update(ctx context.Context, repo domain.Repo) error {
	val, err := encodeRepo(repo)
	if err != nil {
		return err
	}
	key := repoKey(repo.Name)
	item, ok := s.db.Get(ctx, key)
	if !ok {
		return domain.ErrNotFound
	}
	var b kv.Batch
	b.RequireVersion(key, item.Version)
	b.Put(key, val)
	if _, err := s.db.Apply(ctx, &b); err != nil {
		if errors.Is(err, kv.ErrConflict) {
			return domain.ErrConflict
		}
		return fmt.Errorf("update repository %q: %w", repo.Name, err)
	}
	return nil
}

// Delete removes the repository; a missing one is not an error.
func (s *Store) Delete(ctx context.Context, name domain.RepoName) error {
	var b kv.Batch
	b.Delete(repoKey(name))
	if _, err := s.db.Apply(ctx, &b); err != nil {
		return fmt.Errorf("delete repository %q: %w", name, err)
	}
	return nil
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
	pb := &storagepb.Repo{
		Name:        r.Name.String(),
		Description: r.Description,
		Visibility:  vis,
		CreatedAt:   encodeTime(r.CreatedAt),
		PushedAt:    encodeTime(r.PushedAt),
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
		Visibility:  decodeVisibility(pb.GetVisibility()),
		CreatedAt:   decodeTime(pb.GetCreatedAt()),
		PushedAt:    decodeTime(pb.GetPushedAt()),
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
