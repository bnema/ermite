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
}

// ParseTreePath and IsAddressablePath share one rule. The only differences
// are the root ("" is a valid path to parse, and not addressable) and the
// trailing slash (ParseTreePath drops one, IsAddressablePath wants the path
// as it is).
func TestTreePathRule(t *testing.T) {
	long := strings.Repeat("a", 4096)
	for _, tt := range []struct {
		path string
		ok   bool // parses, and is addressable
	}{
		{"a", true}, {"a/b.txt", true}, {"dir/ünï.go", true}, {"a b/c", true}, {".github/ci.yml", true}, {long, true},
		{"/a", false}, {"a//b", false}, {"./a", false}, {"a/..", false}, {"..", false}, {".", false},
		{"a/../b", false}, {"tab\tname", false}, {"a\nb", false}, {"a\x00b", false}, {"a\x7fb", false},
		{"\xff", false}, {"a/\xfe/b", false}, {long + "a", false}, {"//", false},
	} {
		_, err := domain.ParseTreePath(tt.path)
		assert.Equal(t, tt.ok, err == nil, "ParseTreePath(%q)", tt.path)
		if !tt.ok {
			assert.ErrorIs(t, err, domain.ErrInvalidPath, "%q", tt.path)
		}
		assert.Equal(t, tt.ok, domain.IsAddressablePath(tt.path), "IsAddressablePath(%q)", tt.path)
		assert.Equal(t, tt.ok, domain.IsAddressablePathBytes([]byte(tt.path)), "IsAddressablePathBytes(%q)", tt.path)
	}

	// The exceptions.
	_, err := domain.ParseTreePath("")
	assert.NoError(t, err, "the root parses")
	assert.False(t, domain.IsAddressablePath(""), "the root is not addressable")
	assert.False(t, domain.IsAddressablePathBytes(nil))
	_, err = domain.ParseTreePath("a/")
	assert.NoError(t, err, "one trailing slash is dropped")
	assert.False(t, domain.IsAddressablePath("a/"), "an addressable path has none")
	assert.False(t, domain.IsAddressablePathBytes([]byte("a/")))
	_, err = domain.ParseTreePath("/")
	assert.NoError(t, err, `"/" is the root with its trailing slash dropped`)
	assert.False(t, domain.IsAddressablePath("/"))
}

func TestPathListAdd(t *testing.T) {
	for _, tt := range []struct {
		name               string
		adds               []string
		maxPaths, maxBytes int
		want               []string
		wantStopAt         int // index of the Add that returned false, -1 for none
		truncated          bool
	}{
		{"all fit", []string{"a", "b/c"}, 10, 100, []string{"a", "b/c"}, -1, false},
		{"not addressable are skipped", []string{"a", "", "b//c", "d/", "e\nf", "g"}, 10, 100, []string{"a", "g"}, -1, false},
		{"skipped paths do not count", []string{"", "..", "a"}, 1, 1, []string{"a"}, -1, false},
		{"exactly maxPaths", []string{"a", "b"}, 2, 100, []string{"a", "b"}, -1, false},
		{"over maxPaths", []string{"a", "b", "c"}, 2, 100, []string{"a", "b"}, 2, true},
		{"exactly maxBytes", []string{"ab", "cd"}, 10, 4, []string{"ab", "cd"}, -1, false},
		{"over maxBytes", []string{"ab", "cde", "f"}, 10, 4, []string{"ab"}, 1, true},
		{"nothing fits", []string{"abc"}, 10, 2, nil, 0, true},
		{"no paths allowed", []string{"a"}, 0, 100, nil, 0, true},
		{"unaddressable past the limit is no truncation", []string{"a", "b/"}, 1, 100, []string{"a"}, -1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var l domain.PathList
			stop := -1
			for i, p := range tt.adds {
				if !l.Add(p, tt.maxPaths, tt.maxBytes) {
					stop = i
					break
				}
			}
			assert.Equal(t, tt.want, l.Paths)
			assert.Equal(t, tt.wantStopAt, stop)
			assert.Equal(t, tt.truncated, l.Truncated)
		})
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
