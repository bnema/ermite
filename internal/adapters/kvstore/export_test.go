// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"

	kv "bnema.dev/ermite-kv"

	"bnema.dev/ermite/internal/domain"
)

// SetAfterSettingsSnapshot installs the hook that runs between the snapshot of
// DeleteEnvironment and its batch, and returns the function that removes it.
func SetAfterSettingsSnapshot(f func()) (restore func()) {
	afterSettingsSnapshot = f
	return func() { afterSettingsSnapshot = nil }
}

// DecodeConcurrencyRecord, EncodeConcurrencyRecord and EncodeConcurrencyMark
// expose the stored layout of a concurrency group to the tests.
var (
	DecodeConcurrencyRecord = decodeConcurrencyRecord
	EncodeConcurrencyRecord = encodeConcurrencyRecord
	EncodeConcurrencyMark   = encodeConcurrencyMark
)

// RawVersionForTest is the kv version of a runner record: it moves on every
// write.
func (c *CI) RawVersionForTest(ctx context.Context, id string) (uint64, bool) {
	item, ok := c.db.Get(ctx, runnerKey(id))
	return item.Version, ok
}

// CountKeysForTest counts the keys of the database under prefix.
func (s *Store) CountKeysForTest(ctx context.Context, prefix string) int {
	n := 0
	for range s.db.Snapshot(ctx).Prefix(ctx, []byte(prefix)) {
		n++
	}
	return n
}

// DropIssueRecordForTest deletes the issue record and leaves its open index
// entry: the state of an index that is stale.
func (i *Issues) DropIssueRecordForTest(ctx context.Context, repo domain.RepoName, n int64) error {
	var b kv.Batch
	b.Delete(issueKey(repo, n))
	_, err := i.db.Apply(ctx, &b)
	return err
}

// DropMRRecordForTest is DropIssueRecordForTest for a merge request.
func (m *MergeRequests) DropMRRecordForTest(ctx context.Context, repo domain.RepoName, n int64) error {
	var b kv.Batch
	b.Delete(mrKey(repo, n))
	_, err := m.db.Apply(ctx, &b)
	return err
}

// PutRawIssueForTest stores value as the issue record, bypassing the codec.
func (i *Issues) PutRawIssueForTest(ctx context.Context, repo domain.RepoName, n int64, value []byte) error {
	var b kv.Batch
	b.Put(issueKey(repo, n), value)
	_, err := i.db.Apply(ctx, &b)
	return err
}

// RawMirrorKey is the stored protobuf of the mirror key, for encryption-at-rest tests.
func (s *Store) RawMirrorKey(ctx context.Context) ([]byte, bool) {
	item, ok := s.db.Get(ctx, []byte(domain.MirrorKeyPath))
	return item.Value, ok
}
