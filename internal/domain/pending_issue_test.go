// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestPendingIssueID(t *testing.T) {
	t.Parallel()
	id, err := domain.NewPendingIssueID()
	require.NoError(t, err)
	assert.Len(t, id.String(), domain.PendingIssueIDLen)
	got, err := domain.ParsePendingIssueID(id.String())
	require.NoError(t, err)
	assert.Equal(t, id, got)
	other, err := domain.NewPendingIssueID()
	require.NoError(t, err)
	assert.NotEqual(t, id, other)

	for _, bad := range []string{
		"", "short", strings.ToUpper(id.String()), id.String() + "a", id.String()[:25] + "1",
		strings.Repeat("a", 25) + "b", // spare bits set
		strings.Repeat("a", 24) + "=a", "../" + strings.Repeat("a", 23),
	} {
		_, err := domain.ParsePendingIssueID(bad)
		assert.ErrorIs(t, err, domain.ErrInvalid, "%q", bad)
	}
	_, err = domain.ParsePendingIssueID(strings.Repeat("a", 26))
	assert.NoError(t, err)
}

func TestParsePendingAuthor(t *testing.T) {
	t.Parallel()
	got, err := domain.ParsePendingAuthor("")
	require.NoError(t, err)
	assert.Empty(t, got)
	got, err = domain.ParsePendingAuthor("Al\u202ee\x01x")
	require.NoError(t, err)
	assert.Equal(t, "Alex", got)
	_, err = domain.ParsePendingAuthor(strings.Repeat("a", 41))
	assert.Error(t, err)
	_, err = domain.ParsePendingAuthor("\xff")
	assert.Error(t, err)
	got, err = domain.ParsePendingAuthor(strings.Repeat("é", 40))
	require.NoError(t, err)
	assert.Len(t, []rune(got), 40)
}

func TestIssueSubmittedKind(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "issue.submitted", domain.IssueSubmitted{}.EventKind())
}
