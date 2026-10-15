// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestPullFilesCacheIsBoundedAndLeastRecentlyUsed(t *testing.T) {
	var c pullFilesCache
	key := func(i int) pullFilesKey {
		repo, err := domain.ParseRepoName("r" + strconv.Itoa(i))
		require.NoError(t, err)
		return pullFilesKey{repo: repo}
	}
	for i := range maxPullFileLists {
		c.put(key(i), []domain.FileDiff{{NewPath: strconv.Itoa(i)}})
	}
	_, ok := c.get(key(0)) // 0 is now the most recently used; 1 the oldest
	require.True(t, ok)
	c.put(key(maxPullFileLists), nil)

	_, oldest := c.get(key(1))
	assert.False(t, oldest, "the least recently used entry goes")
	files, kept := c.get(key(0))
	require.True(t, kept, "a used entry stays")
	assert.Equal(t, "0", files[0].NewPath)
	_, newest := c.get(key(maxPullFileLists))
	assert.True(t, newest)
	assert.Len(t, c.items, maxPullFileLists)
	assert.Len(t, c.order, maxPullFileLists)
}
