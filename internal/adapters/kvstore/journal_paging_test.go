// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

// Pages chained by the ID of their last entry cover every accepted entry
// once, newest first, entries of the same time included; a malformed cursor
// is an invalid page token.
func TestJournalListSinceBefore(t *testing.T) {
	ctx := context.Background()
	j := openStore(t, t.TempDir()).Journal()
	a, b := name(t, "alpha"), name(t, "beta")
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var want []string
	for i, title := range []string{"a0", "b1", "a2", "a3", "b4", "a5", "a6"} {
		repo := a
		if title[0] == 'b' {
			repo = b
		}
		at := base.Add(time.Duration(i/2) * time.Minute) // pairs share a time
		require.NoError(t, j.Append(ctx, domain.JournalEntry{At: at, Repo: repo, Kind: domain.JournalIssueOpened, Title: title}))
		if repo == a {
			want = append([]string{title}, want...)
		}
	}

	var got []string
	before := ""
	for range 10 {
		page, err := j.ListSince(ctx, base, before, a, nil, 2)
		require.NoError(t, err)
		for _, e := range page.Entries {
			require.True(t, domain.ValidJournalID(e.ID), e.ID)
			got = append(got, e.Title)
		}
		if !page.Truncated {
			break
		}
		before = page.Entries[len(page.Entries)-1].ID
	}
	assert.Equal(t, want, got)

	all, err := j.ListSince(ctx, base, "", domain.RepoName{}, nil, 10)
	require.NoError(t, err)
	require.Len(t, all.Entries, 7)
	for i := 1; i < len(all.Entries); i++ {
		assert.Greater(t, all.Entries[i-1].ID, all.Entries[i].ID, "IDs sort like the entries")
	}
	older, err := j.ListSince(ctx, base, all.Entries[0].ID, domain.RepoName{}, nil, 10)
	require.NoError(t, err)
	assert.Equal(t, all.Entries[1:], older.Entries, "strictly older than the cursor")

	for _, bad := range []string{"x", "0123456789abcdef0123456789ABCDEF", "0123456789abcdef0123456789abcde/"} {
		_, err := j.ListSince(ctx, base, bad, domain.RepoName{}, nil, 10)
		assert.ErrorIs(t, err, domain.ErrInvalidPageToken, bad)
	}
}
