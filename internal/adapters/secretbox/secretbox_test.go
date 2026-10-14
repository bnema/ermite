// SPDX-License-Identifier: AGPL-3.0-or-later

package secretbox_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/secretbox"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

func keyOf(b byte) []byte { return bytes.Repeat([]byte{b}, secretbox.KeyLen) }

func box(t *testing.T, b byte) *secretbox.Box {
	t.Helper()
	x, err := secretbox.New(keyOf(b))
	require.NoError(t, err)
	return x
}

func TestSealOpenRoundTrip(t *testing.T) {
	t.Parallel()
	b := box(t, 1)
	repo, _ := domain.ParseRepoName("proj")
	aad := domain.SecretAAD(domain.RepoScope(repo), "TOKEN")
	for _, plain := range []string{"x", "multi\nline\r\nvalue", strings.Repeat("é", 20000), "\x00\xff"} {
		sealed := b.Seal([]byte(plain), aad)
		assert.NotContains(t, string(sealed), plain, "the value is not stored in clear")
		got, err := b.Open(sealed, aad)
		require.NoError(t, err)
		assert.Equal(t, plain, string(got))
	}
}

func TestEachValueHasItsOwnNonce(t *testing.T) {
	t.Parallel()
	b := box(t, 1)
	a := b.Seal([]byte("same"), []byte("aad"))
	c := b.Seal([]byte("same"), []byte("aad"))
	assert.NotEqual(t, a, c, "the same value sealed twice differs")
}

// A sealed value moved to another secret (another name or another scope) does
// not open: the additional data binds it.
func TestAdditionalDataBindsScopeAndName(t *testing.T) {
	t.Parallel()
	b := box(t, 1)
	repo, _ := domain.ParseRepoName("proj")
	other, _ := domain.ParseRepoName("other")
	sealed := b.Seal([]byte("v"), domain.SecretAAD(domain.RepoScope(repo), "TOKEN"))

	for name, aad := range map[string][]byte{
		"other name":         domain.SecretAAD(domain.RepoScope(repo), "OTHER"),
		"instance scope":     domain.SecretAAD(domain.InstanceScope(), "TOKEN"),
		"other repository":   domain.SecretAAD(domain.RepoScope(other), "TOKEN"),
		"an environment":     domain.SecretAAD(domain.EnvironmentScope(repo, "prod"), "TOKEN"),
		"no additional data": nil,
	} {
		_, err := b.Open(sealed, aad)
		assert.ErrorIs(t, err, ports.ErrSecretNotOpened, name)
	}
}

func TestAnotherKeyOrDamagedValueDoesNotOpen(t *testing.T) {
	t.Parallel()
	a, other := box(t, 1), box(t, 2)
	sealed := a.Seal([]byte("v"), []byte("aad"))
	_, err := other.Open(sealed, []byte("aad"))
	assert.ErrorIs(t, err, ports.ErrSecretNotOpened)

	for i := range sealed {
		damaged := bytes.Clone(sealed)
		damaged[i] ^= 1
		_, err := a.Open(damaged, []byte("aad"))
		assert.ErrorIs(t, err, ports.ErrSecretNotOpened, "byte %d", i)
	}
	for _, short := range [][]byte{nil, {1}, sealed[:10], sealed[:len(sealed)-1]} {
		_, err := a.Open(short, []byte("aad"))
		assert.ErrorIs(t, err, ports.ErrSecretNotOpened)
	}
}

func TestKeyCheckTellsKeysApartWithoutRevealingThem(t *testing.T) {
	t.Parallel()
	a, a2, other := box(t, 1), box(t, 1), box(t, 2)
	assert.Equal(t, a.KeyCheck(), a2.KeyCheck(), "stable for a key")
	assert.NotEqual(t, a.KeyCheck(), other.KeyCheck())
	assert.NotContains(t, string(a.KeyCheck()), string(keyOf(1)))
	a.KeyCheck()[0] ^= 1
	assert.Equal(t, a2.KeyCheck(), a.KeyCheck(), "the caller cannot change the stored check")
}

func TestNewRefusesAWrongKeyLengthWithoutEchoingIt(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 16, 31, 33, 64} {
		_, err := secretbox.New(bytes.Repeat([]byte("k"), n))
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "kkkk")
	}
}
