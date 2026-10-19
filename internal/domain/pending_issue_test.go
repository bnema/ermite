// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"strings"
	"testing"
	"time"

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

func TestSubmissionLimitsAreTheDocumentedOnes(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 3, domain.SubmitPerHour)
	assert.Equal(t, 10, domain.SubmitPerDay)
	assert.Equal(t, 200, domain.MaxPendingIssues)
	assert.Equal(t, 30*24*time.Hour, domain.BanDuration)
}

func TestApprovedIssue(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	now := at.Add(time.Hour)
	title, err := domain.ParseIssueTitle("t")
	require.NoError(t, err)
	repo, err := domain.ParseRepoName("proj")
	require.NoError(t, err)

	named := domain.ApprovedIssue(domain.PendingIssue{Repo: repo, Title: title, Author: "Alex", CreatedAt: at}, now)
	assert.Equal(t, domain.IssueStateOpen, named.State)
	assert.Equal(t, "Alex", named.Author)
	assert.True(t, named.External)
	assert.Equal(t, repo, named.Repo)
	assert.Equal(t, at, named.CreatedAt)
	assert.Equal(t, now, named.UpdatedAt)
	assert.Zero(t, named.Number, "the store numbers it")

	anon := domain.ApprovedIssue(domain.PendingIssue{Repo: repo, Title: title, CreatedAt: at}, now)
	assert.Equal(t, domain.AnonymousAuthor, anon.Author)
}
