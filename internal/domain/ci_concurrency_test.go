// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

// Groups compare like .NET's OrdinalIgnoreCase: upper-cased character by
// character, a non-ASCII character whose upper case is ASCII keeping itself.
func TestConcurrencyKeyFoldsLikeOrdinalIgnoreCase(t *testing.T) {
	t.Parallel()
	k := domain.ConcurrencyKey
	// The three sigmas are one letter.
	assert.Equal(t, k("Σ"), k("σ"))
	assert.Equal(t, k("σ"), k("ς"))
	assert.Equal(t, k("ΟΔΟΣ"), k("οδος"))
	// ASCII and ordinary letters.
	assert.Equal(t, k("Deploy"), k("DEPLOY"))
	assert.Equal(t, k("É"), k("é"))
	assert.NotEqual(t, k("a"), k("b"))
	// The Turkish i's: i == I, but ı (dotless) and İ (dotted capital) are not I.
	assert.Equal(t, k("i"), k("I"))
	assert.NotEqual(t, k("ı"), k("I"))
	assert.NotEqual(t, k("İ"), k("I"))
	assert.NotEqual(t, k("ı"), k("i"))
	assert.NotEqual(t, k("İ"), k("i"))
	assert.NotEqual(t, k("ı"), k("İ"))
	// ſ (long s) is not S.
	assert.NotEqual(t, k("ſ"), k("S"))
	assert.NotEqual(t, k("ſ"), k("s"))
	// One character stays one character.
	assert.Len(t, []rune(k("ß")), 1)
	assert.NotEqual(t, k("ß"), k("ss"))
}

func TestConcurrencyGroupLengthCountsUTF16Units(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 400, domain.ConcurrencyGroupLength(strings.Repeat("é", 400)), "a BMP character is one unit")
	assert.Equal(t, 2, domain.ConcurrencyGroupLength("😀"), "a supplementary character is two")
	assert.Equal(t, 400, domain.ConcurrencyGroupLength(strings.Repeat("😀", 200)))
}

// queue: max lines up to GitHub's 100 waiting entries in order and rejects one
// more; a waiting entry that leaves keeps the order of the others; the head
// takes the free running place.
func TestConcurrencyQueueMax(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 100, domain.CIMaxConcurrencyQueue)
	rec := domain.CIConcurrencyRecord{Running: &domain.CIConcurrencyEntry{RunID: 1, Attempt: 1}}
	for i := range domain.CIMaxConcurrencyQueue {
		replaced, rejected := rec.Enqueue(domain.CIConcurrencyEntry{RunID: int64(10 + i), Attempt: 1, Max: true}, domain.CIMaxConcurrencyQueue)
		require.False(t, rejected, "entry %d", i)
		require.Empty(t, replaced)
	}
	require.Len(t, rec.Pending, domain.CIMaxConcurrencyQueue)
	replaced, rejected := rec.Enqueue(domain.CIConcurrencyEntry{RunID: 999, Attempt: 1, Max: true}, domain.CIMaxConcurrencyQueue)
	assert.True(t, rejected, "the 101st waiting entry is rejected")
	assert.Empty(t, replaced)
	assert.Len(t, rec.Pending, domain.CIMaxConcurrencyQueue, "the line is left as it was")
	assert.False(t, rec.Names(domain.CIConcurrencyEntry{RunID: 999, Attempt: 1}))

	assert.True(t, rec.RemovePending(domain.CIConcurrencyEntry{RunID: 11, Attempt: 1}))
	assert.False(t, rec.RemovePending(domain.CIConcurrencyEntry{RunID: 11, Attempt: 1}))
	assert.EqualValues(t, []int64{10, 12, 13}, []int64{rec.Pending[0].RunID, rec.Pending[1].RunID, rec.Pending[2].RunID})

	assert.Nil(t, rec.Promote(), "the running place is taken")
	rec.Running = nil
	head := rec.Promote()
	require.NotNil(t, head)
	assert.EqualValues(t, 10, head.RunID, "first in, first out")
	assert.EqualValues(t, 12, rec.Pending[0].RunID)
	assert.Equal(t, 1, rec.PendingIndex(domain.CIConcurrencyEntry{RunID: 13, Attempt: 1}))
}

// A queue: single entry replaces the whole line, queue: max entries included,
// and is then the only waiting one; a max entry joins behind it.
func TestConcurrencyQueueSingleClearsTheLine(t *testing.T) {
	t.Parallel()
	rec := domain.CIConcurrencyRecord{Running: &domain.CIConcurrencyEntry{RunID: 1, Attempt: 1}}
	for i := range 3 {
		rec.Enqueue(domain.CIConcurrencyEntry{RunID: int64(10 + i), Attempt: 1, Max: true}, 3)
	}
	single := domain.CIConcurrencyEntry{RunID: 20, Attempt: 1}
	replaced, rejected := rec.Enqueue(single, 3)
	assert.False(t, rejected)
	require.Len(t, replaced, 3)
	assert.EqualValues(t, 10, replaced[0].RunID)
	assert.Equal(t, []domain.CIConcurrencyEntry{single}, rec.Pending)
	maxEntry := domain.CIConcurrencyEntry{RunID: 21, Attempt: 1, Max: true}
	replaced, _ = rec.Enqueue(maxEntry, 3)
	assert.Empty(t, replaced)
	assert.Equal(t, []domain.CIConcurrencyEntry{single, maxEntry}, rec.Pending)
	assert.True(t, domain.CIConcurrencyRejected.Owed())
	assert.True(t, domain.CIConcurrencyReplaced.Owed())
	assert.False(t, domain.CIConcurrencyPending.Owed())
	assert.Equal(t, "Canceling since the concurrency group 'g' already has 100 waiting requests", domain.ConcurrencyQueueFullMessage("g"))
}
