// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestNewRefUpdate(t *testing.T) {
	t.Parallel()
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)

	u, err := domain.NewRefUpdate(domain.ZeroOID, a, "refs/heads/main")
	require.NoError(t, err)
	assert.True(t, u.IsCreate())
	assert.False(t, u.IsDelete())

	u, err = domain.NewRefUpdate(a, domain.ZeroOID, "refs/tags/v1")
	require.NoError(t, err)
	assert.True(t, u.IsDelete())

	u, err = domain.NewRefUpdate(a, b, "refs/heads/a b")
	require.NoError(t, err)
	assert.False(t, u.IsCreate() || u.IsDelete())
	assert.Equal(t, domain.RefUpdate{Old: a, New: b, Ref: "refs/heads/a b"}, u)

	for name, args := range map[string][3]string{
		"short old":   {"abc", b, "refs/heads/x"},
		"upper case":  {strings.ToUpper(a), b, "refs/heads/x"},
		"non hex":     {strings.Repeat("g", 40), b, "refs/heads/x"},
		"long new":    {a, b + "0", "refs/heads/x"},
		"empty ref":   {a, b, ""},
		"huge ref":    {a, b, strings.Repeat("r", 5000)},
		"empty oid":   {"", b, "refs/heads/x"},
		"sha256 oid":  {strings.Repeat("a", 64), b, "refs/heads/x"},
		"newline oid": {a + "\n", b, "refs/heads/x"},
	} {
		_, err := domain.NewRefUpdate(args[0], args[1], args[2])
		assert.Error(t, err, name)
	}
}

func TestEventKinds(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "repo.created", domain.RepoCreated{}.EventKind())
	assert.Equal(t, "repo.pushed", domain.PushEvent{}.EventKind())
	assert.Equal(t, "gap", domain.EventGap{}.EventKind())
}
