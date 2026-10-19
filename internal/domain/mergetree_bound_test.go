// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestMergeTreeResultBounded(t *testing.T) {
	var paths []string
	for i := 0; i < domain.MaxConflictPaths+2; i++ {
		paths = append(paths, strconv.Itoa(i))
	}
	got := domain.MergeTreeResult{ConflictPaths: paths}.Bounded()
	assert.Len(t, got.ConflictPaths, domain.MaxConflictPaths)
	assert.True(t, got.Truncated)

	got = domain.MergeTreeResult{ConflictPaths: paths[:domain.MaxConflictPaths]}.Bounded()
	assert.Len(t, got.ConflictPaths, domain.MaxConflictPaths)
	assert.False(t, got.Truncated)
}

func TestBoundBehind(t *testing.T) {
	n, capped := domain.BoundBehind(domain.MaxBehindCount)
	assert.Equal(t, domain.MaxBehindCount, n)
	assert.False(t, capped)
	n, capped = domain.BoundBehind(domain.BehindLimit)
	assert.Equal(t, domain.MaxBehindCount, n)
	assert.True(t, capped)
}

func TestMergeTreeResultBoundedCopiesThePaths(t *testing.T) {
	var paths []string
	for i := 0; i < domain.MaxConflictPaths+2; i++ {
		paths = append(paths, strconv.Itoa(i))
	}
	got := domain.MergeTreeResult{ConflictPaths: paths}.Bounded()
	require.Len(t, got.ConflictPaths, domain.MaxConflictPaths)
	assert.Equal(t, len(got.ConflictPaths), cap(got.ConflictPaths))
	assert.NotSame(t, &paths[0], &got.ConflictPaths[0], "a new array, not the git output's")
	assert.Equal(t, paths[:domain.MaxConflictPaths], got.ConflictPaths)

	got = domain.MergeTreeResult{ConflictPaths: paths[:3:10]}.Bounded()
	assert.Equal(t, 3, cap(got.ConflictPaths))
	assert.False(t, got.Truncated)
	assert.Nil(t, domain.MergeTreeResult{}.Bounded().ConflictPaths)
}
