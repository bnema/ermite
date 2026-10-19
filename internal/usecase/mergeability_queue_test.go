// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestMergeQueueNextSkipsAClaimedKey(t *testing.T) {
	repo, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	k := mergeKey{repo, 1}
	q := newMergeQueue(8)

	q.add(k)                     // pending, in order
	require.True(t, q.tryRun(k)) // a rescan claims it: no longer pending, still in order
	key, dirty := q.next()
	assert.Nil(t, key, "a claimed key is not run again by a worker")
	assert.Nil(t, dirty)
	assert.True(t, q.running[k], "the rescan's marker is intact")

	// Queued again while it runs: one more pass, still no concurrent run.
	q.add(k)
	key, _ = q.next()
	assert.Nil(t, key)
	assert.True(t, q.finish(k), "another pass is due")
	assert.False(t, q.finish(k))
	assert.False(t, q.running[k])
}

func TestMergeQueueNextMarksAgainForARunningKey(t *testing.T) {
	repo, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	k := mergeKey{repo, 1}
	q := newMergeQueue(8)

	// A stale order entry for a key that is pending again and running.
	q.pending[k], q.order, q.running[k] = true, []mergeKey{k}, true
	key, _ := q.next()
	assert.Nil(t, key)
	assert.True(t, q.again[k])
	assert.False(t, q.pending[k])
}
