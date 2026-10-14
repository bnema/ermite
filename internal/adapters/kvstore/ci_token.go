// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	kv "bnema.dev/ermite-kv"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Key layout of the job repository tokens and of the action resolutions,
// written by CI.Apply under the lock of their run:
//
//	ghtoken/<id>                                        CIRepoToken
//	ghtokenhash/<hash>                                  token id
//	ghtokenjob/<run:020d>/<attempt:010d>/<job>/<id>     the id: the tokens of a job
//	ghtokenrepo/<repo>/<id>                             the job key: the tokens that read a repository
//	actres/<run:020d>/<attempt:010d>/<sha256(key)>      CIActionResolution
//
// Stored tokens use token/ and tokenhash/: the two families never meet, so a
// job token is never listed, revoked or managed as a stored token.
const (
	repoTokenPrefix     = "ghtoken/"
	repoTokenHashPrefix = "ghtokenhash/"
	repoTokenJobPrefix  = "ghtokenjob/"
	repoTokenRepoPrefix = "ghtokenrepo/"
	resolutionPrefix    = "actres/"
)

var _ ports.CIRepoTokenStore = (*CI)(nil)

func repoTokenKey(id string) []byte { return []byte(repoTokenPrefix + id) }
func repoTokenHashKey(h domain.TokenHash) []byte {
	return []byte(repoTokenHashPrefix + h.String())
}
func repoTokenJobPrefixOf(runID int64, attempt int32, jobID string) string {
	return fmt.Sprintf("%s%020d/%010d/%s/", repoTokenJobPrefix, runID, attempt, jobID)
}
func repoTokenJobKey(t domain.CIRepoToken) []byte {
	return []byte(repoTokenJobPrefixOf(t.RunID, t.RunAttempt, t.JobID) + t.ID)
}
func repoTokenRepoPrefixOf(repo domain.RepoName) string {
	return repoTokenRepoPrefix + repo.String() + "/"
}
func repoTokenRepoKey(repo domain.RepoName, id string) []byte {
	return []byte(repoTokenRepoPrefixOf(repo) + id)
}
func resolutionKey(runID int64, attempt int32, key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return []byte(fmt.Sprintf("%s%020d/%010d/%s", resolutionPrefix, runID, attempt, hex.EncodeToString(sum[:])))
}

func encodeRepoToken(t domain.CIRepoToken) []byte {
	return mustMarshal(&storagepb.CIRepoToken{
		Id: t.ID, Hash: t.Hash[:], Repo: t.Repo.String(), RunId: t.RunID, RunAttempt: t.RunAttempt, JobId: t.JobID,
		Archive: t.Archive, ExpiresAt: encodeTime(t.ExpiresAt), CreatedAt: encodeTime(t.CreatedAt), Sha: t.Sha, Permissions: t.Permissions,
	})
}

func decodeRepoToken(val []byte) (domain.CIRepoToken, error) {
	var pb storagepb.CIRepoToken
	if err := proto.Unmarshal(val, &pb); err != nil {
		return domain.CIRepoToken{}, fmt.Errorf("decode repo token: %w", err)
	}
	repo, err := domain.ParseRepoName(pb.GetRepo())
	if err != nil {
		return domain.CIRepoToken{}, fmt.Errorf("decode repo token: %w", err)
	}
	t := domain.CIRepoToken{
		ID: pb.GetId(), Repo: repo, RunID: pb.GetRunId(), RunAttempt: pb.GetRunAttempt(), JobID: pb.GetJobId(),
		Archive: pb.GetArchive(), ExpiresAt: decodeTime(pb.GetExpiresAt()), CreatedAt: decodeTime(pb.GetCreatedAt()),
		Sha: pb.GetSha(), Permissions: pb.GetPermissions(),
	}
	copy(t.Hash[:], pb.GetHash())
	return t, nil
}

// stageRepoTokens stages the creation of the tokens, the deletion of the
// tokens of the jobs to revoke, and the resolutions to keep.
func (c *CI) stageRepoTokens(ctx context.Context, b *kv.Batch, w ports.CIWrite, guarded map[domain.RepoName]bool) {
	snap := c.db.Snapshot(ctx)
	for _, ref := range w.RevokeJobs {
		for key, item := range snap.Prefix(ctx, []byte(repoTokenJobPrefixOf(ref.RunID, ref.RunAttempt, ref.JobID))) {
			b.Delete(append([]byte(nil), key...))
			id := string(item.Value)
			if id == "" {
				id = string(key[strings.LastIndexByte(string(key), '/')+1:])
			}
			b.Delete(repoTokenKey(id))
			if it, ok := snap.Get(ctx, repoTokenKey(id)); ok {
				if t, err := decodeRepoToken(it.Value); err == nil {
					b.Delete(repoTokenHashKey(t.Hash))
					b.Delete(repoTokenRepoKey(t.Repo, id))
				}
			}
		}
	}
	for _, t := range w.RepoTokens {
		b.RequireAbsent(repoTokenKey(t.ID))
		b.RequireAbsent(repoTokenHashKey(t.Hash))
		b.Put(repoTokenKey(t.ID), encodeRepoToken(t))
		b.Put(repoTokenHashKey(t.Hash), []byte(t.ID))
		b.Put(repoTokenJobKey(t), []byte(t.ID))
		b.Put(repoTokenRepoKey(t.Repo, t.ID), []byte(t.ID))
		guarded[t.Repo] = true
	}
	for _, r := range w.Resolutions {
		b.Put(resolutionKey(r.RunID, r.RunAttempt, r.Key), mustMarshal(&storagepb.CIActionResolution{
			Key: r.Key, NameWithOwner: r.NameWithOwner, ResolvedSha: r.ResolvedSHA, Source: int32(r.Source), //nolint:gosec // 1 or 2
		}))
	}
}

// FindRepoTokenByHash resolves the hash index, then reads the token.
func (c *CI) FindRepoTokenByHash(ctx context.Context, hash domain.TokenHash) (domain.CIRepoToken, error) {
	snap := c.db.Snapshot(ctx)
	idx, ok := snap.Get(ctx, repoTokenHashKey(hash))
	if !ok {
		return domain.CIRepoToken{}, domain.ErrNotFound
	}
	it, ok := snap.Get(ctx, repoTokenKey(string(idx.Value)))
	if !ok {
		return domain.CIRepoToken{}, domain.ErrNotFound
	}
	t, err := decodeRepoToken(it.Value)
	if err != nil {
		return domain.CIRepoToken{}, err
	}
	if t.Hash != hash {
		return domain.CIRepoToken{}, domain.ErrNotFound
	}
	return t, nil
}

// GetRepoToken reads ghtoken/<id>.
func (c *CI) GetRepoToken(ctx context.Context, id string) (domain.CIRepoToken, error) {
	if !validTokenID(id) {
		return domain.CIRepoToken{}, domain.ErrNotFound
	}
	it, ok := c.db.Get(ctx, repoTokenKey(id))
	if !ok {
		return domain.CIRepoToken{}, domain.ErrNotFound
	}
	return decodeRepoToken(it.Value)
}

// GetActionResolution reads the resolution kept for the key.
func (c *CI) GetActionResolution(ctx context.Context, runID int64, attempt int32, key string) (domain.CIActionResolution, error) {
	it, ok := c.db.Get(ctx, resolutionKey(runID, attempt, key))
	if !ok {
		return domain.CIActionResolution{}, domain.ErrNotFound
	}
	var pb storagepb.CIActionResolution
	if err := proto.Unmarshal(it.Value, &pb); err != nil {
		return domain.CIActionResolution{}, fmt.Errorf("decode action resolution: %w", err)
	}
	return domain.CIActionResolution{
		RunID: runID, RunAttempt: attempt, Key: pb.GetKey(), NameWithOwner: pb.GetNameWithOwner(),
		ResolvedSHA: pb.GetResolvedSha(), Source: domain.CIActionSource(pb.GetSource()),
	}, nil
}

// tokenKeys lists the keys of the token with that id: the record, its hash
// index, its entry in the list of its job and in the list of its repository.
func tokenKeys(ctx context.Context, snap *kv.Snapshot, id string) [][]byte {
	keys := [][]byte{repoTokenKey(id)}
	if it, ok := snap.Get(ctx, repoTokenKey(id)); ok {
		if t, err := decodeRepoToken(it.Value); err == nil {
			keys = append(keys, repoTokenHashKey(t.Hash), repoTokenJobKey(t), repoTokenRepoKey(t.Repo, id))
		}
	}
	return keys
}

// repoTokenKeys lists the keys of the repository tokens and action
// resolutions of a run, for its deletion.
func repoTokenKeys(ctx context.Context, snap *kv.Snapshot, runID int64) [][]byte {
	var keys [][]byte
	for key, item := range snap.Prefix(ctx, []byte(fmt.Sprintf("%s%020d/", repoTokenJobPrefix, runID))) {
		keys = append(keys, append([]byte(nil), key...))
		keys = append(keys, tokenKeys(ctx, snap, string(item.Value))...)
	}
	for key := range snap.Prefix(ctx, []byte(fmt.Sprintf("%s%020d/", resolutionPrefix, runID))) {
		keys = append(keys, append([]byte(nil), key...))
	}
	return keys
}

// repoReaderKeys lists the keys of every repository token that reads the
// repository, whichever job holds it: the archive tokens given to the jobs of
// other repositories go with the repository they read.
func repoReaderKeys(ctx context.Context, snap *kv.Snapshot, repo domain.RepoName) [][]byte {
	var keys [][]byte
	for key, item := range snap.Prefix(ctx, []byte(repoTokenRepoPrefixOf(repo))) {
		keys = append(keys, append([]byte(nil), key...))
		keys = append(keys, tokenKeys(ctx, snap, string(item.Value))...)
	}
	return keys
}

// CountRepoTokens counts the job repository tokens that read the repository
// (every job's, archive tokens included). It serves tests and diagnostics.
func (c *CI) CountRepoTokens(ctx context.Context, repo domain.RepoName) (int, error) {
	n := 0
	for range c.db.Snapshot(ctx).Prefix(ctx, []byte(repoTokenRepoPrefixOf(repo))) {
		n++
	}
	return n, nil
}
