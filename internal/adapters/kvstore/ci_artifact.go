// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"encoding/hex"
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

// Key layout of the CI artifacts (CI-4 D3). New prefixes only: an older build
// ignores them, so there is no schema change.
//
//	artifact/<repo>/<run:020d>/<id:020d>   CIArtifact
//	artifactid/<id:020d>                   index: "<repo>/<run:020d>"
//	artifactblk/<id:020d>/<hex(block id)>  CIArtifactBlock: a staged block
//	artifactuse/<repo>                     CIArtifactUsage: bytes charged
//	artifactseq                            Sequence: last artifact id
//
// Every write of the artifacts of a repository requires and rewrites its
// usage record (ports.CIArtifactWrite). The records of a repository go with it
// (ciKeys); those of a run that retention removed are deleted by the sweep of
// the artifacts, which refunds them.
const (
	artifactPrefix    = "artifact/"
	artifactIDPrefix  = "artifactid/"
	artifactBlkPrefix = "artifactblk/"
	artifactUsePrefix = "artifactuse/"
	artifactSeqKey    = "artifactseq"
)

func artifactRunPrefix(repo domain.RepoName, runID int64) string {
	return artifactPrefix + repo.String() + "/" + fmt.Sprintf(idFormat, runID) + "/"
}
func artifactKey(repo domain.RepoName, runID, id int64) []byte {
	return []byte(artifactRunPrefix(repo, runID) + fmt.Sprintf(idFormat, id))
}
func artifactIDKey(id int64) []byte { return []byte(artifactIDPrefix + fmt.Sprintf(idFormat, id)) }
func artifactBlocksPrefix(id int64) string {
	return artifactBlkPrefix + fmt.Sprintf(idFormat, id) + "/"
}
func artifactBlockKey(id int64, block []byte) []byte {
	return []byte(artifactBlocksPrefix(id) + hex.EncodeToString(block))
}
func artifactUseKey(repo domain.RepoName) []byte { return []byte(artifactUsePrefix + repo.String()) }

// CIArtifacts is the ermite-kv backed ports.CIArtifactStore. It shares the
// database of the Store it comes from.
type CIArtifacts struct {
	db *kv.DB
}

var _ ports.CIArtifactStore = (*CIArtifacts)(nil)

// Artifacts returns the artifact store over the same database. It is valid
// until the Store is closed.
func (s *Store) Artifacts() *CIArtifacts { return &CIArtifacts{db: s.db} }

// --- encoding ---

var artifactStates = []struct {
	d domain.CIArtifactState
	p storagepb.CIArtifactState
}{
	{domain.CIArtifactPending, storagepb.CIArtifactState_CI_ARTIFACT_STATE_PENDING},
	{domain.CIArtifactCommitted, storagepb.CIArtifactState_CI_ARTIFACT_STATE_COMMITTED},
	{domain.CIArtifactFinal, storagepb.CIArtifactState_CI_ARTIFACT_STATE_FINAL},
}

// encodeArtifact serializes an artifact for storage.
//
//hexcheck:ignore suspicious-business-logic-in-adapter validates artifact storage identifiers, UTF-8 and enum encoding before protobuf serialization
func encodeArtifact(a domain.CIArtifact) ([]byte, error) {
	if a.Repo.IsZero() || a.ID < 1 || a.RunID < 1 {
		return nil, fmt.Errorf("%w: artifact without id, run or repository", domain.ErrInvalid)
	}
	if !utf8.ValidString(a.Name) || !utf8.ValidString(a.MediaType) || !utf8.ValidString(a.JobID) {
		return nil, fmt.Errorf("%w: artifact strings must be valid UTF-8", domain.ErrInvalid)
	}
	pb := &storagepb.CIArtifact{
		Id: a.ID, Repo: a.Repo.String(), RunId: a.RunID, Attempt: a.Attempt, JobId: a.JobID,
		Name: a.Name, MediaType: a.MediaType, Size: a.Size, Digest: a.Digest,
		CreatedAt: encodeTime(a.CreatedAt), ExpiresAt: encodeTime(a.ExpiresAt),
		Charged: a.Charged, ListDigest: a.ListDigest,
	}
	if a.Assignment != (domain.TokenHash{}) {
		pb.Assignment = a.Assignment[:]
	}
	for _, s := range artifactStates {
		if s.d == a.State {
			pb.State = s.p
		}
	}
	if pb.State == storagepb.CIArtifactState_CI_ARTIFACT_STATE_UNSPECIFIED {
		return nil, fmt.Errorf("%w: artifact state %q", domain.ErrInvalid, a.State)
	}
	return mustMarshal(pb), nil
}

func decodeArtifact(item kv.Item) (domain.CIArtifact, error) {
	var pb storagepb.CIArtifact
	if err := proto.Unmarshal(item.Value, &pb); err != nil {
		return domain.CIArtifact{}, fmt.Errorf("decode artifact: %w", err)
	}
	repo, err := domain.ParseRepoName(pb.GetRepo())
	if err != nil {
		return domain.CIArtifact{}, fmt.Errorf("decode artifact: %w", err)
	}
	a := domain.CIArtifact{
		ID: pb.GetId(), Repo: repo, RunID: pb.GetRunId(), Attempt: pb.GetAttempt(), JobID: pb.GetJobId(),
		Name: pb.GetName(), MediaType: pb.GetMediaType(), Size: pb.GetSize(), Digest: pb.GetDigest(),
		CreatedAt: decodeTime(pb.GetCreatedAt()), ExpiresAt: decodeTime(pb.GetExpiresAt()),
		Charged: pb.GetCharged(), ListDigest: pb.GetListDigest(), Version: item.Version,
	}
	copy(a.Assignment[:], pb.GetAssignment())
	for _, s := range artifactStates {
		if s.p == pb.GetState() {
			a.State = s.d
		}
	}
	if a.State == "" {
		return domain.CIArtifact{}, fmt.Errorf("decode artifact %d: unknown state %v", a.ID, pb.GetState())
	}
	return a, nil
}

// --- reads ---

// artifactIDTries bounds the attempts of NextArtifactID: each conflict means
// another call took an id, and uploads begin many at once.
const artifactIDTries = 1000

// NextArtifactID takes the next id in its own batch.
func (c *CIArtifacts) NextArtifactID(ctx context.Context) (int64, error) {
	ci := &CI{db: c.db}
	for range artifactIDTries {
		last, item, ok, err := ci.readSeq(ctx, []byte(artifactSeqKey))
		if err != nil {
			return 0, err
		}
		var b kv.Batch
		stageSeq(&b, []byte(artifactSeqKey), item, ok, last+1)
		_, err = c.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("take an artifact id: %w", err)
		}
		return last + 1, nil
	}
	return 0, fmt.Errorf("take an artifact id: %w", domain.ErrConflict)
}

// ArtifactUsage reads artifactuse/<repo>.
func (c *CIArtifacts) ArtifactUsage(ctx context.Context, repo domain.RepoName) (int64, uint64, error) {
	item, ok := c.db.Get(ctx, artifactUseKey(repo))
	if !ok {
		return 0, 0, nil
	}
	var pb storagepb.CIArtifactUsage
	if err := proto.Unmarshal(item.Value, &pb); err != nil {
		return 0, 0, fmt.Errorf("decode artifact usage: %w", err)
	}
	return pb.GetUsed(), item.Version, nil
}

// GetArtifact resolves the id index, then reads the record.
func (c *CIArtifacts) GetArtifact(ctx context.Context, id int64) (domain.CIArtifact, error) {
	snap := c.db.Snapshot(ctx)
	idx, ok := snap.Get(ctx, artifactIDKey(id))
	if !ok || id < 1 {
		return domain.CIArtifact{}, domain.ErrNotFound
	}
	repo, runID, err := decodeArtifactIndex(idx.Value)
	if err != nil {
		return domain.CIArtifact{}, err
	}
	item, ok := snap.Get(ctx, artifactKey(repo, runID, id))
	if !ok {
		return domain.CIArtifact{}, domain.ErrNotFound
	}
	return decodeArtifact(item)
}

func encodeArtifactIndex(a domain.CIArtifact) []byte {
	return []byte(a.Repo.String() + "/" + fmt.Sprintf(idFormat, a.RunID))
}

func decodeArtifactIndex(val []byte) (domain.RepoName, int64, error) {
	name, run, ok := strings.Cut(string(val), "/")
	if !ok {
		return domain.RepoName{}, 0, fmt.Errorf("decode artifact index %q", val)
	}
	repo, err := domain.ParseRepoName(name)
	if err != nil {
		return domain.RepoName{}, 0, fmt.Errorf("decode artifact index: %w", err)
	}
	runID, err := strconv.ParseInt(run, 10, 64)
	if err != nil {
		return domain.RepoName{}, 0, fmt.Errorf("decode artifact index: %w", err)
	}
	return repo, runID, nil
}

// RunArtifacts lists artifact/<repo>/<run>/, which is in id order.
func (c *CIArtifacts) RunArtifacts(ctx context.Context, repo domain.RepoName, runID int64) ([]domain.CIArtifact, error) {
	return c.list(ctx, artifactRunPrefix(repo, runID))
}

// AllArtifacts lists artifact/.
func (c *CIArtifacts) AllArtifacts(ctx context.Context) ([]domain.CIArtifact, error) {
	return c.list(ctx, artifactPrefix)
}

func (c *CIArtifacts) list(ctx context.Context, prefix string) ([]domain.CIArtifact, error) {
	var out []domain.CIArtifact
	for key, item := range c.db.Snapshot(ctx).Prefix(ctx, []byte(prefix)) {
		a, err := decodeArtifact(item)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// ArtifactBlocks lists artifactblk/<id>/.
func (c *CIArtifacts) ArtifactBlocks(ctx context.Context, id int64) ([]ports.CIArtifactBlock, error) {
	prefix := artifactBlocksPrefix(id)
	var out []ports.CIArtifactBlock
	for key, item := range c.db.Snapshot(ctx).Prefix(ctx, []byte(prefix)) {
		block, err := hex.DecodeString(strings.TrimPrefix(string(key), prefix))
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		var pb storagepb.CIArtifactBlock
		if err := proto.Unmarshal(item.Value, &pb); err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		out = append(out, ports.CIArtifactBlock{ArtifactID: id, ID: block, Size: pb.GetSize(), File: pb.GetFile()})
	}
	return out, nil
}

// BlockArtifactIDs lists the artifact ids under artifactblk/.
func (c *CIArtifacts) BlockArtifactIDs(ctx context.Context) ([]int64, error) {
	var ids []int64
	for key := range c.db.Snapshot(ctx).Prefix(ctx, []byte(artifactBlkPrefix)) {
		raw, _, _ := strings.Cut(strings.TrimPrefix(string(key), artifactBlkPrefix), "/")
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		if len(ids) == 0 || ids[len(ids)-1] != id {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// --- writes ---

// ApplyArtifacts writes w in one batch.
func (c *CIArtifacts) ApplyArtifacts(ctx context.Context, w ports.CIArtifactWrite) error {
	var b kv.Batch
	guard := false
	if w.UsageVersion == 0 {
		b.RequireAbsent(artifactUseKey(w.Repo))
		guard = true
	} else {
		b.RequireVersion(artifactUseKey(w.Repo), w.UsageVersion)
	}
	b.Put(artifactUseKey(w.Repo), mustMarshal(&storagepb.CIArtifactUsage{Used: w.Used}))
	snap := c.db.Snapshot(ctx)
	for _, a := range w.Put {
		if a.Repo != w.Repo {
			return fmt.Errorf("%w: artifact %d of another repository", domain.ErrInvalid, a.ID)
		}
		val, err := encodeArtifact(a)
		if err != nil {
			return err
		}
		stage(&b, artifactKey(a.Repo, a.RunID, a.ID), val, a.Version)
		if a.Version == 0 {
			b.RequireAbsent(artifactIDKey(a.ID))
			b.Put(artifactIDKey(a.ID), encodeArtifactIndex(a))
			guard = true
		}
	}
	for _, a := range w.Delete {
		if a.Repo != w.Repo || a.Version == 0 {
			return fmt.Errorf("%w: delete of artifact %d", domain.ErrInvalid, a.ID)
		}
		key := artifactKey(a.Repo, a.RunID, a.ID)
		b.RequireVersion(key, a.Version)
		b.Delete(key)
		b.Delete(artifactIDKey(a.ID))
		for k := range snap.Prefix(ctx, []byte(artifactBlocksPrefix(a.ID))) {
			b.Delete(slices.Clone(k))
		}
	}
	for _, id := range w.DropBlocks {
		for k := range snap.Prefix(ctx, []byte(artifactBlocksPrefix(id))) {
			b.Delete(slices.Clone(k))
		}
	}
	for _, blk := range w.PutBlocks {
		if !utf8.ValidString(blk.File) {
			return fmt.Errorf("%w: block file name", domain.ErrInvalid)
		}
		b.Put(artifactBlockKey(blk.ArtifactID, blk.ID), mustMarshal(&storagepb.CIArtifactBlock{Size: blk.Size, File: blk.File}))
	}
	if guard {
		if err := guardRepoKey(ctx, c.db, &b, w.Repo); err != nil {
			return err
		}
	}
	_, err := c.db.Apply(ctx, &b)
	if errors.Is(err, kv.ErrConflict) {
		return domain.ErrConflict
	}
	if err != nil {
		return fmt.Errorf("write the artifacts of %q: %w", w.Repo, err)
	}
	return nil
}

// artifactKeys lists every key of the snapshot that belongs to the artifacts
// of the repository: records, their id index and blocks, and the usage record.
func artifactKeys(ctx context.Context, snap *kv.Snapshot, repo domain.RepoName) [][]byte {
	keys := [][]byte{artifactUseKey(repo)}
	for key, item := range snap.Prefix(ctx, []byte(artifactPrefix+repo.String()+"/")) {
		keys = append(keys, slices.Clone(key))
		if a, err := decodeArtifact(item); err == nil {
			keys = append(keys, artifactIDKey(a.ID))
			for k := range snap.Prefix(ctx, []byte(artifactBlocksPrefix(a.ID))) {
				keys = append(keys, slices.Clone(k))
			}
		}
	}
	return keys
}

// DeleteBlocks removes artifactblk/<id>/.
func (c *CIArtifacts) DeleteBlocks(ctx context.Context, id int64) error {
	var b kv.Batch
	for k := range c.db.Snapshot(ctx).Prefix(ctx, []byte(artifactBlocksPrefix(id))) {
		b.Delete(slices.Clone(k))
	}
	if b.Len() == 0 {
		return nil
	}
	if _, err := c.db.Apply(ctx, &b); err != nil {
		return fmt.Errorf("delete the blocks of artifact %d: %w", id, err)
	}
	return nil
}
