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

func TestParseArtifactName(t *testing.T) {
	for _, ok := range []string{"dist", "my artifact (linux) v1.2", "é", strings.Repeat("é", 256), "..", "a.b-c_d"} {
		assert.NoError(t, domain.ParseArtifactName(ok), "%q", ok)
	}
	for _, bad := range []string{"", strings.Repeat("a", 257), "a/b", `a\b`, "a:b", `a"b`, "a<b", "a>b", "a|b", "a*b", "a?b", "a\nb", "a\x00b", "\xff"} {
		assert.ErrorIs(t, domain.ParseArtifactName(bad), domain.ErrInvalid, "%q", bad)
	}
}

func TestParseArtifactMediaTypeAndDigest(t *testing.T) {
	mt, err := domain.ParseArtifactMediaType("")
	require.NoError(t, err)
	assert.Equal(t, domain.CIArtifactZip, mt)
	mt, err = domain.ParseArtifactMediaType("text/plain; charset=utf-8")
	require.NoError(t, err)
	assert.Equal(t, "text/plain; charset=utf-8", mt)
	for _, bad := range []string{strings.Repeat("a", 256), "text/plain\r\nX-Evil: 1", "tëxt/plain"} {
		_, err := domain.ParseArtifactMediaType(bad)
		assert.ErrorIs(t, err, domain.ErrInvalid, "%q", bad)
	}

	hex := strings.Repeat("ab", 32)
	d, err := domain.ParseArtifactDigest("SHA256:" + strings.ToUpper(hex))
	require.NoError(t, err)
	assert.Equal(t, "sha256:"+hex, d)
	d, err = domain.ParseArtifactDigest("")
	require.NoError(t, err)
	assert.Empty(t, d)
	for _, bad := range []string{hex, "sha256:" + hex[:62], "sha1:" + hex, "sha256:" + strings.Repeat("zz", 32)} {
		_, err := domain.ParseArtifactDigest(bad)
		assert.ErrorIs(t, err, domain.ErrInvalid, bad)
	}
}

func TestArtifactExpiry(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	assert.Equal(t, now.Add(90*day), domain.ArtifactExpiry(time.Time{}, now, 90*day), "unset: the retention")
	assert.Equal(t, now.Add(day), domain.ArtifactExpiry(now.Add(time.Hour), now, 90*day), "at least a day")
	assert.Equal(t, now.Add(day), domain.ArtifactExpiry(now.Add(-time.Hour), now, 90*day), "a time in the past")
	assert.Equal(t, now.Add(90*day), domain.ArtifactExpiry(now.Add(400*day), now, 90*day), "at most the retention")
	assert.Equal(t, now.Add(3*day), domain.ArtifactExpiry(now.Add(3*day), now, 90*day))
	assert.Equal(t, now.Add(day), domain.ArtifactExpiry(time.Time{}, now, time.Hour), "a retention below a day counts as a day")
}

func TestArtifactOwnership(t *testing.T) {
	tok := domain.HashTokenSecret("ermitejob_a")
	j := domain.CIJob{ID: "5-0-0", RunID: 5, RunAttempt: 2, TokenHash: tok}
	a := domain.CIArtifact{RunID: 5, Attempt: 2, JobID: "5-0-0", Assignment: tok, State: domain.CIArtifactPending}
	assert.True(t, a.PendingOf(j))
	other := j
	other.TokenHash = domain.HashTokenSecret("ermitejob_b")
	assert.False(t, a.PendingOf(other), "another assignment of the job")
	assert.True(t, a.UploadedBy(other))
	next := j
	next.RunAttempt = 3
	assert.False(t, a.UploadedBy(next), "another attempt")
	a.State = domain.CIArtifactFinal
	assert.False(t, a.PendingOf(j))
	a.ExpiresAt = time.Now().Add(time.Hour)
	assert.True(t, a.Visible(time.Now()))
	assert.False(t, a.Visible(a.ExpiresAt))
}
