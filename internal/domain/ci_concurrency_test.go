// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

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
