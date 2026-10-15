// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestPullAttemptsRememberConflictsByTipsAndAreBounded(t *testing.T) {
	id := func(c string) domain.ObjectID {
		o, err := domain.ParseObjectID(strings.Repeat(c, 40))
		require.NoError(t, err)
		return o
	}
	key := func(i int) pullKey {
		r, err := domain.ParseRepoName("r" + strconv.Itoa(i))
		require.NoError(t, err)
		return pullKey{repo: r, number: 1}
	}
	a := newPullAttempts(2)
	a.conflicted(key(0), id("a"), id("b"))
	assert.True(t, a.tried(key(0), id("a"), id("b")), "the same tips were tried")
	assert.False(t, a.tried(key(0), id("c"), id("b")), "the target moved: try again")
	assert.False(t, a.tried(key(0), id("a"), id("c")), "the source moved: try again")
	assert.False(t, a.tried(key(1), id("a"), id("b")), "another merge request")

	a.conflicted(key(1), id("a"), id("b"))
	a.tried(key(0), id("a"), id("b")) // 0 is the most recently used; 1 the oldest
	a.conflicted(key(2), id("a"), id("b"))
	assert.False(t, a.tried(key(1), id("a"), id("b")), "the least recently used is forgotten")
	assert.True(t, a.tried(key(0), id("a"), id("b")))
	assert.True(t, a.tried(key(2), id("a"), id("b")))
	assert.Len(t, a.items, 2)
	assert.Len(t, a.order, 2)

	a.merged(key(0))
	assert.False(t, a.tried(key(0), id("a"), id("b")), "a merge that went through is forgotten")
	assert.Len(t, a.items, 1)
	assert.Len(t, a.order, 1)
}
