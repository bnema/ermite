// SPDX-License-Identifier: AGPL-3.0-or-later
package domain_test

import (
	"bnema.dev/ermite/internal/domain"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

func TestReleaseParsers(t *testing.T) {
	for _, tc := range []struct {
		parse     func(string) (string, error)
		good, bad []string
	}{
		{domain.ParseReleaseTag, []string{"v1.0", "nightly/x"}, []string{"", "refs/tags/v1", "..", "a..b", "a b"}},
		{domain.ParseReleaseTitle, []string{"", strings.Repeat("é", 256)}, []string{strings.Repeat("x", 257), "\xff"}},
		{domain.ParseReleaseNotes, []string{"", "a\n\t\rb", strings.Repeat("x", 125000)}, []string{"\xff", strings.Repeat("x", 125001)}},
		{domain.ParseReleaseAssetName, []string{"a b", "é", strings.Repeat("x", 255)}, []string{"", ".", "..", "a/b", "a\\b", "a\x00", "\xff", strings.Repeat("x", 256)}},
		{domain.ParseReleaseAssetLabel, []string{"", strings.Repeat("x", 255)}, []string{"\xff", strings.Repeat("x", 256)}},
		{domain.ParseReleaseContentType, []string{"", "text/plain; charset=utf-8"}, []string{"bad/type/type", "text/plain\n", strings.Repeat("x", 256), "é"}},
	} {
		for _, s := range tc.good {
			_, err := tc.parse(s)
			require.NoError(t, err, s)
		}
		for _, s := range tc.bad {
			_, err := tc.parse(s)
			require.ErrorIs(t, err, domain.ErrInvalid, s)
		}
	}
	title, err := domain.ParseReleaseTitle("e\u0301\x00\u202e")
	require.NoError(t, err)
	require.Equal(t, "é", title)
	notes, err := domain.ParseReleaseNotes("a\x00\n\t\r")
	require.NoError(t, err)
	require.Equal(t, "a\n\t\r", notes)
	require.Equal(t, "release/00000000000000000001", (domain.ReleaseAsset{ID: 1}).BlobKey())
	scope, err := domain.ParseScope("release:write")
	require.NoError(t, err)
	require.Equal(t, domain.ScopeReleaseWrite, scope)
}
func TestLatestRelease(t *testing.T) {
	now := time.Now()
	rs := []domain.Release{{ID: 1, CreatedAt: now, MakeLatest: true}, {ID: 2, CreatedAt: now.Add(time.Hour)}, {ID: 3, Draft: true, MakeLatest: true}, {ID: 4, Prerelease: true, MakeLatest: true}}
	r, ok := domain.LatestRelease(rs)
	require.True(t, ok)
	require.EqualValues(t, 1, r.ID)
	rs[0].MakeLatest = false
	r, ok = domain.LatestRelease(rs)
	require.True(t, ok)
	require.EqualValues(t, 2, r.ID)
	rs[0].CreatedAt = rs[1].CreatedAt
	r, _ = domain.LatestRelease(rs)
	require.EqualValues(t, 2, r.ID)
	_, ok = domain.LatestRelease(rs[2:])
	require.False(t, ok)
}
