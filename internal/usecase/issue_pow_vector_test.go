// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"encoding/base64"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The proof-of-work vector shared with the browser solver: the same nonce,
// difficulty and smallest solution are asserted in
// internal/adapters/web/jstest/issue-form.test.mjs. If one changes, change both.
const (
	powVectorNonce      = "AAECAwQFBgcICQoLDA0ODw" // bytes 0..15, base64url without padding
	powVectorDifficulty = 12
	powVectorSolution   = "2491"
)

func TestPowVector(t *testing.T) {
	nonce, err := base64.RawURLEncoding.DecodeString(powVectorNonce)
	require.NoError(t, err)
	require.Len(t, nonce, nonceLen)

	first := ""
	for n := 0; n < 1<<22; n++ {
		s := strconv.Itoa(n)
		if leadingZeroBits(powHash(nonce, s)) >= powVectorDifficulty {
			first = s
			break
		}
	}
	require.NotEmpty(t, first)
	assert.Equal(t, powVectorSolution, first, "the smallest solution of the shared vector")
	assert.True(t, validSolution(first))
}
