// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The key is generated once and survives a restart; the epoch counts up and
// survives it too.
func TestSessionsPersist(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openStore(t, dir)

	key, err := s.Sessions().Key(ctx)
	require.NoError(t, err)
	assert.Len(t, key, 32)
	again, err := s.Sessions().Key(ctx)
	require.NoError(t, err)
	assert.Equal(t, key, again)

	e, err := s.Sessions().Epoch(ctx)
	require.NoError(t, err)
	assert.Zero(t, e)
	for want := uint64(1); want <= 3; want++ {
		got, err := s.Sessions().BumpEpoch(ctx)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	require.NoError(t, s.Close(ctx))

	s = openStore(t, dir)
	reopened, err := s.Sessions().Key(ctx)
	require.NoError(t, err)
	assert.Equal(t, key, reopened, "a restart keeps the key")
	e, err = s.Sessions().Epoch(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(3), e)
}

// Concurrent bumps never give the same epoch twice, and concurrent first
// reads agree on one key.
func TestSessionsConcurrent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	var wg sync.WaitGroup
	keys := make([][]byte, 8)
	epochs := make(chan uint64, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k, err := s.Sessions().Key(ctx)
			assert.NoError(t, err)
			keys[i] = k
			if e, err := s.Sessions().BumpEpoch(ctx); err == nil {
				epochs <- e
			}
		}()
	}
	wg.Wait()
	close(epochs)
	for _, k := range keys[1:] {
		assert.Equal(t, keys[0], k)
	}
	seen := map[uint64]bool{}
	for e := range epochs {
		assert.False(t, seen[e], "epoch %d twice", e)
		seen[e] = true
	}
}
