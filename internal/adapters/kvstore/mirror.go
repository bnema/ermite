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

var _ ports.MirrorStore = (*Store)(nil)
var _ ports.MirrorKeyStore = (*Store)(nil)

const mirrorConfigKey = "mirror/config"
const mirrorRepoPrefix = "mirror/repo/"
const mirrorStatePrefix = "mirror/state/"
const mirrorSnapshotPrefix = "mirror/snapshot/"

func mirrorKeys(ctx context.Context, snap *kv.Snapshot, repo domain.RepoName) [][]byte {
	var keys [][]byte
	for _, prefix := range []string{mirrorRepoPrefix, mirrorStatePrefix, mirrorSnapshotPrefix} {
		key := []byte(prefix + repo.String())
		if _, ok := snap.Get(ctx, key); ok {
			keys = append(keys, key)
		}
	}
	return keys
}

func (s *Store) readMirror(ctx context.Context, key string, pb proto.Message) error {
	item, ok := s.db.Get(ctx, []byte(key))
	if !ok {
		return domain.ErrNotFound
	}
	if err := proto.Unmarshal(item.Value, pb); err != nil {
		return fmt.Errorf("decode mirror record: %w", err)
	}
	return nil
}

func (s *Store) writeMirror(ctx context.Context, key string, repo domain.RepoName, pb proto.Message) error {
	val := mustMarshal(pb)
	for range updateAttempts {
		var b kv.Batch
		if key != mirrorConfigKey {
			if err := guardRepoKey(ctx, s.db, &b, repo); err != nil {
				return err
			}
		}
		b.Put([]byte(key), val)
		_, err := s.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue
		}
		if err != nil {
			return fmt.Errorf("store mirror record: %w", err)
		}
		return nil
	}
	return domain.ErrConflict
}

func (s *Store) GetGlobal(ctx context.Context) (domain.MirrorGlobalConfig, error) {
	var pb storagepb.MirrorGlobal
	err := s.readMirror(ctx, mirrorConfigKey, &pb)
	if errors.Is(err, domain.ErrNotFound) {
		err = nil
	}
	return domain.MirrorGlobalConfig{Enabled: pb.Enabled, URLTemplate: pb.UrlTemplate, Cron: pb.Cron, AllBranches: pb.AllBranches, UpdatedAt: decodeTime(pb.UpdatedAt)}, err
}
func (s *Store) PutGlobal(ctx context.Context, g domain.MirrorGlobalConfig) error {
	return s.writeMirror(ctx, mirrorConfigKey, domain.RepoName{}, &storagepb.MirrorGlobal{Enabled: g.Enabled, UrlTemplate: g.URLTemplate, Cron: g.Cron, AllBranches: g.AllBranches, UpdatedAt: encodeTime(g.UpdatedAt)})
}
func decodeMirrorRepo(repo domain.RepoName, pb *storagepb.MirrorRepo) domain.MirrorRepoConfig {
	return domain.MirrorRepoConfig{Repo: repo, URL: pb.Url, Cron: pb.Cron, AllBranches: pb.AllBranches, Excluded: pb.Excluded, UpdatedAt: decodeTime(pb.UpdatedAt)}
}
func (s *Store) GetRepoConfig(ctx context.Context, repo domain.RepoName) (domain.MirrorRepoConfig, error) {
	var pb storagepb.MirrorRepo
	err := s.readMirror(ctx, mirrorRepoPrefix+repo.String(), &pb)
	return decodeMirrorRepo(repo, &pb), err
}
func (s *Store) PutRepoConfig(ctx context.Context, c domain.MirrorRepoConfig) error {
	return s.writeMirror(ctx, mirrorRepoPrefix+c.Repo.String(), c.Repo, &storagepb.MirrorRepo{Url: c.URL, Cron: c.Cron, AllBranches: c.AllBranches, Excluded: c.Excluded, UpdatedAt: encodeTime(c.UpdatedAt)})
}
func (s *Store) DeleteRepoConfig(ctx context.Context, repo domain.RepoName) error {
	var b kv.Batch
	b.Delete([]byte(mirrorRepoPrefix + repo.String()))
	_, err := s.db.Apply(ctx, &b)
	if err != nil {
		return fmt.Errorf("delete mirror config: %w", err)
	}
	return nil
}
func (s *Store) ListRepoConfigs(ctx context.Context) ([]domain.MirrorRepoConfig, error) {
	var out []domain.MirrorRepoConfig
	for key, item := range s.db.Snapshot(ctx).Prefix(ctx, []byte(mirrorRepoPrefix)) {
		repo, err := domain.ParseRepoName(strings.TrimPrefix(string(key), mirrorRepoPrefix))
		if err != nil {
			return nil, err
		}
		var pb storagepb.MirrorRepo
		if err := proto.Unmarshal(item.Value, &pb); err != nil {
			return nil, fmt.Errorf("decode mirror config: %w", err)
		}
		out = append(out, decodeMirrorRepo(repo, &pb))
	}
	return out, nil
}
func decodeMirrorStatus(repo domain.RepoName, pb *storagepb.MirrorState) domain.MirrorStatus {
	return domain.MirrorStatus{Repo: repo, State: domain.MirrorState(pb.State), Class: domain.MirrorFailureClass(pb.Class), Reason: pb.Reason, Detail: pb.Detail, ConsecutiveFailures: int(pb.ConsecutiveFailures), LastAttemptAt: decodeTime(pb.LastAttemptAt), LastSuccessAt: decodeTime(pb.LastSuccessAt), NextRetryAt: decodeTime(pb.NextRetryAt), PausedAt: decodeTime(pb.PausedAt), UpdatedAt: decodeTime(pb.UpdatedAt)}
}
func (s *Store) GetStatus(ctx context.Context, repo domain.RepoName) (domain.MirrorStatus, error) {
	var pb storagepb.MirrorState
	err := s.readMirror(ctx, mirrorStatePrefix+repo.String(), &pb)
	return decodeMirrorStatus(repo, &pb), err
}
func (s *Store) PutStatus(ctx context.Context, c domain.MirrorStatus) error {
	return s.writeMirror(ctx, mirrorStatePrefix+c.Repo.String(), c.Repo, &storagepb.MirrorState{State: string(c.State), Class: string(c.Class), Reason: c.Reason, Detail: c.Detail, ConsecutiveFailures: int32(c.ConsecutiveFailures), LastAttemptAt: encodeTime(c.LastAttemptAt), LastSuccessAt: encodeTime(c.LastSuccessAt), NextRetryAt: encodeTime(c.NextRetryAt), PausedAt: encodeTime(c.PausedAt), UpdatedAt: encodeTime(c.UpdatedAt)})
}
func (s *Store) ListStatus(ctx context.Context) ([]domain.MirrorStatus, error) {
	var out []domain.MirrorStatus
	for key, item := range s.db.Snapshot(ctx).Prefix(ctx, []byte(mirrorStatePrefix)) {
		repo, err := domain.ParseRepoName(strings.TrimPrefix(string(key), mirrorStatePrefix))
		if err != nil {
			return nil, err
		}
		var pb storagepb.MirrorState
		if err := proto.Unmarshal(item.Value, &pb); err != nil {
			return nil, fmt.Errorf("decode mirror state: %w", err)
		}
		out = append(out, decodeMirrorStatus(repo, &pb))
	}
	return out, nil
}
func (s *Store) GetSnapshot(ctx context.Context, repo domain.RepoName) (domain.MirrorSnapshot, error) {
	var pb storagepb.MirrorSnapshot
	err := s.readMirror(ctx, mirrorSnapshotPrefix+repo.String(), &pb)
	out := domain.MirrorSnapshot{URL: pb.Url, AllBranches: pb.AllBranches, At: decodeTime(pb.At)}
	for _, ref := range pb.Refs {
		id, parseErr := domain.ParseObjectID(ref.Id)
		if parseErr != nil {
			return domain.MirrorSnapshot{}, fmt.Errorf("decode mirror snapshot ref: %w", parseErr)
		}
		out.Refs = append(out.Refs, domain.MirrorRef{Name: ref.Name, ID: id})
	}
	return out, err
}
func (s *Store) PutSnapshot(ctx context.Context, repo domain.RepoName, c domain.MirrorSnapshot) error {
	pb := &storagepb.MirrorSnapshot{Url: c.URL, AllBranches: c.AllBranches, At: encodeTime(c.At)}
	for _, ref := range c.Refs {
		pb.Refs = append(pb.Refs, &storagepb.MirrorSnapshotRef{Name: ref.Name, Id: ref.ID.String()})
	}
	return s.writeMirror(ctx, mirrorSnapshotPrefix+repo.String(), repo, pb)
}
func (s *Store) GetMirrorKey(ctx context.Context) ([]byte, time.Time, error) {
	var pb storagepb.MirrorKey
	err := s.readMirror(ctx, domain.MirrorKeyPath, &pb)
	return pb.Sealed, decodeTime(pb.CreatedAt), err
}
func (s *Store) PutMirrorKey(ctx context.Context, sealed []byte, at time.Time, ifAbsent bool) error {
	var b kv.Batch
	key := []byte(domain.MirrorKeyPath)
	if ifAbsent {
		b.RequireAbsent(key)
	}
	b.Put(key, mustMarshal(&storagepb.MirrorKey{Sealed: sealed, CreatedAt: encodeTime(at)}))
	_, err := s.db.Apply(ctx, &b)
	if ifAbsent && errors.Is(err, kv.ErrConflict) {
		return domain.ErrAlreadyExists
	}
	if err != nil {
		return fmt.Errorf("store mirror key: %w", err)
	}
	return nil
}

// RawMirrorKey exposes the stored protobuf for encryption-at-rest tests.
func (s *Store) RawMirrorKey(ctx context.Context) ([]byte, bool) {
	item, ok := s.db.Get(ctx, []byte(domain.MirrorKeyPath))
	return item.Value, ok
}
