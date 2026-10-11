// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestParseObjectID(t *testing.T) {
	t.Parallel()
	good := strings.Repeat("ab12", 10)
	id, err := domain.ParseObjectID(good)
	require.NoError(t, err)
	assert.Equal(t, good, id.String())
	assert.False(t, id.IsZero())
	assert.True(t, domain.ObjectID{}.IsZero())

	for _, bad := range []string{"", "abc", strings.ToUpper(good), good + "0", good[:39], strings.Repeat("g", 40), "main"} {
		_, err := domain.ParseObjectID(bad)
		assert.ErrorIs(t, err, domain.ErrInvalidObjectID, bad)
	}
}

func TestParseRevision(t *testing.T) {
	t.Parallel()
	valid := []string{
		"", "main", "feature/x", "refs/heads/feature/x", "v1.0.0", "HEAD",
		"abc1234", strings.Repeat("a", 40), "release-2026.10", "café",
	}
	for _, s := range valid {
		r, err := domain.ParseRevision(s)
		require.NoError(t, err, s)
		assert.Equal(t, s, r.String())
	}
	invalid := []string{
		"-x", "--upload-pack=x", "/x", "x/", "x.", "a..b", "a@{1}", "@", "a b", "a~1", "a^", "a:b", "a?", "a*", "a[",
		"a\\b", "a\nb", "a\x00b", "a//b", ".hidden", "a/.b", "x.lock", "a/x.lock/b", "\xff", strings.Repeat("a", 513),
	}
	for _, s := range invalid {
		_, err := domain.ParseRevision(s)
		assert.ErrorIs(t, err, domain.ErrInvalidRevision, "%q", s)
	}
}

func TestRevisionKinds(t *testing.T) {
	t.Parallel()
	head, _ := domain.ParseRevision("")
	assert.True(t, head.IsHead())
	full, _ := domain.ParseRevision(strings.Repeat("a", 40))
	assert.True(t, full.IsObjectID())
	assert.False(t, full.IsAbbrevID())
	short, _ := domain.ParseRevision("abcdef1")
	assert.True(t, short.IsAbbrevID())
	tooShort, _ := domain.ParseRevision("abcdef")
	assert.False(t, tooShort.IsAbbrevID())
	branch, _ := domain.ParseRevision("deadbeefx")
	assert.False(t, branch.IsAbbrevID())

	id, _ := domain.ParseObjectID(strings.Repeat("c", 40))
	assert.True(t, domain.RevisionOf(id).IsObjectID())
}

func TestParseTreePath(t *testing.T) {
	t.Parallel()
	root, err := domain.ParseTreePath("")
	require.NoError(t, err)
	assert.True(t, root.IsRoot())
	assert.Equal(t, "x", root.Join("x"))

	p, err := domain.ParseTreePath("a/b/")
	require.NoError(t, err)
	assert.Equal(t, "a/b", p.String())
	assert.Equal(t, "a/b/c", p.Join("c"))

	for _, bad := range []string{"/a", "a//b", "./a", "a/..", "..", ".", "a\x00b", "a\nb", "\xff", strings.Repeat("a", 4097), "//"} {
		_, err := domain.ParseTreePath(bad)
		assert.ErrorIs(t, err, domain.ErrInvalidPath, "%q", bad)
	}
}

func TestEntryKindOf(t *testing.T) {
	t.Parallel()
	tests := map[uint32]domain.EntryKind{
		0o100644: domain.EntryFile,
		0o100664: domain.EntryFile,
		0o100755: domain.EntryExecutable,
		0o040000: domain.EntryDirectory,
		0o120000: domain.EntrySymlink,
		0o160000: domain.EntrySubmodule,
		0o000000: "",
		0o060000: "",
	}
	for mode, want := range tests {
		assert.Equal(t, want, domain.EntryKindOf(mode), "%o", mode)
	}
}

func TestDefaultDiffLimits(t *testing.T) {
	t.Parallel()
	assert.Equal(t, domain.DiffLimits{MaxFiles: 300, MaxFileLines: 2000, MaxTotalLines: 20000, MaxTotalBytes: 8 << 20}, domain.DefaultDiffLimits())
}
