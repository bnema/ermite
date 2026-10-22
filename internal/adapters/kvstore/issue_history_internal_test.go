// SPDX-License-Identifier: AGPL-3.0-or-later
package kvstore

import (
	"context"
	"testing"

	kv "bnema.dev/ermite-kv"
	"bnema.dev/ermite/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestPreparedEventSequenceConflict(t *testing.T) {
	ctx := context.Background()
	db, err := kv.Open(ctx, t.TempDir(), kv.Options{})
	require.NoError(t, err)
	defer db.Close(ctx)
	repo, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	events := []domain.IssueEvent{{Kind: domain.IssueEventClosed, MergeRequest: 9}}
	var a, b kv.Batch
	require.NoError(t, prepareIssueEvents(ctx, db, &a, repo, 1, events))
	require.NoError(t, prepareIssueEvents(ctx, db, &b, repo, 1, events))
	b.Put([]byte("unrelated"), []byte{1})
	_, err = db.Apply(ctx, &a)
	require.NoError(t, err)
	_, err = db.Apply(ctx, &b)
	require.ErrorIs(t, err, kv.ErrConflict)
	_, exists := db.Get(ctx, []byte("unrelated"))
	require.False(t, exists)
	var retry kv.Batch
	require.NoError(t, prepareIssueEvents(ctx, db, &retry, repo, 1, events))
	_, err = db.Apply(ctx, &retry)
	require.NoError(t, err)
	history, err := (&Issues{db: db}).ListEvents(ctx, repo, 1)
	require.NoError(t, err)
	require.Len(t, history, 2)
	require.Equal(t, int64(2), history[1].ID)
}
