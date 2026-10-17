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

func oid(t *testing.T, c byte) domain.ObjectID {
	t.Helper()
	id, err := domain.ParseObjectID(strings.Repeat(string(c), domain.ObjectIDLen))
	require.NoError(t, err)
	return id
}

func TestDefaultRepoSettings(t *testing.T) {
	t.Parallel()
	s := domain.DefaultRepoSettings()
	assert.Equal(t, []domain.MergeMode{domain.MergeModeFastForward, domain.MergeModeSquash}, s.AllowedMergeModes)
	assert.True(t, s.FailingChecksBlock)
	assert.True(t, s.Allows(domain.MergeModeSquash))
	assert.False(t, domain.RepoSettings{}.Allows(domain.MergeModeSquash))
	assert.True(t, domain.RepoSettings{}.IsZero())
	assert.False(t, s.IsZero())

	// The default list is a copy: callers cannot alter the next default.
	s.AllowedMergeModes[0] = domain.MergeModeSquash
	assert.Equal(t, domain.MergeModeFastForward, domain.DefaultRepoSettings().AllowedMergeModes[0])
}

func TestNormalizeMergeModes(t *testing.T) {
	t.Parallel()
	got, err := domain.NormalizeMergeModes([]domain.MergeMode{domain.MergeModeSquash, domain.MergeModeFastForward, domain.MergeModeSquash})
	require.NoError(t, err)
	assert.Equal(t, domain.MergeModes(), got)

	got, err = domain.NormalizeMergeModes([]domain.MergeMode{domain.MergeModeSquash})
	require.NoError(t, err)
	assert.Equal(t, []domain.MergeMode{domain.MergeModeSquash}, got)

	_, err = domain.NormalizeMergeModes(nil)
	assert.ErrorIs(t, err, domain.ErrInvalid)
	_, err = domain.NormalizeMergeModes([]domain.MergeMode{"rebase"})
	assert.ErrorIs(t, err, domain.ErrInvalid)
}

func TestParseMergeMode(t *testing.T) {
	t.Parallel()
	m, err := domain.ParseMergeMode("fast-forward")
	require.NoError(t, err)
	assert.Equal(t, domain.MergeModeFastForward, m)
	_, err = domain.ParseMergeMode("merge")
	assert.ErrorIs(t, err, domain.ErrInvalid)
}

func hunkDiff() domain.Diff {
	return domain.Diff{Files: []domain.FileDiff{
		{OldPath: "a.txt", NewPath: "a.txt", Status: domain.FileModified, Hunks: []domain.Hunk{{
			Lines: []domain.Line{
				{Kind: domain.LineContext, OldNumber: 4, NewNumber: 4},
				{Kind: domain.LineDeletion, OldNumber: 5},
				{Kind: domain.LineAddition, NewNumber: 5},
				{Kind: domain.LineAddition, NewNumber: 6},
			},
		}}},
		{OldPath: "gone.txt", Status: domain.FileDeleted, Hunks: []domain.Hunk{{
			Lines: []domain.Line{{Kind: domain.LineDeletion, OldNumber: 1}},
		}}},
		{NewPath: "big.txt", Status: domain.FileAdded, Truncated: true},
		{OldPath: "old.txt", NewPath: "new.txt", Status: domain.FileRenamed, Hunks: []domain.Hunk{{
			Lines: []domain.Line{{Kind: domain.LineAddition, NewNumber: 2}},
		}}},
	}}
}

func TestDiffHasLine(t *testing.T) {
	t.Parallel()
	d := hunkDiff()
	tests := []struct {
		name string
		path string
		side domain.DiffSide
		line int
		want bool
	}{
		{"context on the new side", "a.txt", domain.SideNew, 4, true},
		{"context on the old side", "a.txt", domain.SideOld, 4, true},
		{"deletion on the old side", "a.txt", domain.SideOld, 5, true},
		{"addition on the new side", "a.txt", domain.SideNew, 6, true},
		{"addition is not on the old side", "a.txt", domain.SideOld, 6, false},
		{"new line 5 is the addition, not the deletion", "a.txt", domain.SideNew, 5, true},
		{"line outside the hunk", "a.txt", domain.SideNew, 99, false},
		{"line zero", "a.txt", domain.SideNew, 0, false},
		{"negative line", "a.txt", domain.SideNew, -1, false},
		{"no side", "a.txt", domain.SideNone, 4, false},
		{"unknown path", "nope.txt", domain.SideNew, 4, false},
		{"empty path", "", domain.SideNew, 4, false},
		{"deleted file by its old path", "gone.txt", domain.SideOld, 1, true},
		{"deleted file has no new side", "gone.txt", domain.SideNew, 1, false},
		{"truncated file has no lines", "big.txt", domain.SideNew, 1, false},
		{"renamed file by its new path", "new.txt", domain.SideNew, 2, true},
		{"renamed file by its old path", "old.txt", domain.SideNew, 2, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, d.HasLine(tc.path, tc.side, tc.line))
		})
	}
}

func sig(name, email string, sec int64) domain.Signature {
	return domain.Signature{Name: name, Email: email, Time: time.Unix(sec, 0).UTC()}
}

func TestNewSquashCommit(t *testing.T) {
	t.Parallel()
	title, err := domain.ParseMRTitle("Add search")
	require.NoError(t, err)
	mr := domain.MergeRequest{Number: 7, Title: title}
	// Newest first, as Git lists them.
	commits := []domain.Commit{
		{Summary: "fix typo", Author: sig("Bob", "bob@example.org", 300)},
		{Summary: "add tests", Author: sig("Ada", "ADA@example.org", 200)},
		{Summary: "add search", Author: sig("Ada", "ada@example.org", 100)},
		{Summary: "wip", Author: sig("Cy", "cy@example.org", 150)},
		{Summary: "polish", Author: sig("Bob", "bob@example.org", 160)},
	}
	committer := domain.Identity{Name: "ermite", Email: "ermite@git.example.org"}
	at := time.Unix(1_800_000_000, 0)

	c := domain.NewSquashCommit(mr, oid(t, 'a'), oid(t, 'b'), commits, committer, at)

	// The author is the author of the oldest commit (the last one listed).
	assert.Equal(t, sig("Bob", "bob@example.org", 160), c.Author)
	assert.Equal(t, domain.Signature{Name: "ermite", Email: "ermite@git.example.org", Time: at.UTC()}, c.Committer)
	assert.Equal(t, "Add search (!7)\n"+
		"\n"+
		"- polish\n- wip\n- add search\n- add tests\n- fix typo\n"+
		"\n"+
		"Co-authored-by: Cy <cy@example.org>\n"+
		"Co-authored-by: Ada <ada@example.org>\n", c.Message)

	want := "tree " + strings.Repeat("a", 40) + "\n" +
		"parent " + strings.Repeat("b", 40) + "\n" +
		"author Bob <bob@example.org> 160 +0000\n" +
		"committer ermite <ermite@git.example.org> 1800000000 +0000\n" +
		"\n" + c.Message
	assert.Equal(t, want, string(c.Payload()))
}

func TestSquashCommitSigned(t *testing.T) {
	t.Parallel()
	title, err := domain.ParseMRTitle("T")
	require.NoError(t, err)
	c := domain.NewSquashCommit(domain.MergeRequest{Number: 1, Title: title}, oid(t, 'a'), oid(t, 'b'),
		[]domain.Commit{{Summary: "s", Author: sig("A", "a@x.org", 1)}}, domain.Identity{Name: "e", Email: "e@x.org"}, time.Unix(2, 0))

	got := string(c.Signed("-----BEGIN SSH SIGNATURE-----\nAAAA\nBBBB\n-----END SSH SIGNATURE-----\n"))
	assert.Equal(t, "tree "+strings.Repeat("a", 40)+"\n"+
		"parent "+strings.Repeat("b", 40)+"\n"+
		"author A <a@x.org> 1 +0000\n"+
		"committer e <e@x.org> 2 +0000\n"+
		"gpgsig -----BEGIN SSH SIGNATURE-----\n"+
		" AAAA\n"+
		" BBBB\n"+
		" -----END SSH SIGNATURE-----\n"+
		"\n"+c.Message, got)
}

func TestSquashCommitCleansIdentitiesAndSummaries(t *testing.T) {
	t.Parallel()
	title, err := domain.ParseMRTitle("T")
	require.NoError(t, err)
	commits := []domain.Commit{
		{Summary: "second", Author: sig("Eve <evil@x.org>\nCo-authored-by: Z", "eve@x.org", 2)},
		{Summary: "first\x1b[0m", Author: sig("Ada", "ada@x.org", 1)},
	}
	c := domain.NewSquashCommit(domain.MergeRequest{Number: 2, Title: title}, oid(t, 'a'), oid(t, 'b'), commits,
		domain.Identity{Name: "e", Email: "e@x.org"}, time.Unix(3, 0))
	assert.Contains(t, c.Message, "- first [0m\n")
	assert.Contains(t, c.Message, "\nCo-authored-by: Eve evil@x.orgCo-authored-by: Z <eve@x.org>\n")
	assert.NotContains(t, c.Message, "\nCo-authored-by: Z")
}

func TestSigningKeyInfoAllowedSigners(t *testing.T) {
	t.Parallel()
	i := domain.SigningKeyInfo{
		Key:       domain.SigningPublicKey{AuthorizedKey: "ssh-ed25519 AAAA"},
		Committer: domain.Identity{Name: "ermite", Email: "ermite@git.example.org"},
	}
	assert.Equal(t, "ermite@git.example.org ssh-ed25519 AAAA", i.AllowedSigners())
}

func TestParseBranchName(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"main", "feat/x", "v1.2", "a-b_c"} {
		got, err := domain.ParseBranchName(ok)
		require.NoError(t, err, ok)
		assert.Equal(t, ok, got)
	}
	for _, bad := range []string{"", "refs/heads/main", "-x", "a..b", "a b", "x/", "a:b", "x.lock", "a\x00b"} {
		_, err := domain.ParseBranchName(bad)
		assert.ErrorIs(t, err, domain.ErrInvalidRevision, "%q", bad)
	}
}

// A title that starts with "WIP:" or "[WIP]", in any case, is a draft; ready
// drops the prefix and the spaces after it.
func TestMergeRequestDraft(t *testing.T) {
	for _, tc := range []struct {
		title, ready string
		draft        bool
	}{
		{"WIP: feat: drag and drop", "feat: drag and drop", true},
		{"wip:fix", "fix", true},
		{"  [WIP]  tidy", "tidy", true},
		{"WIP: [wip] both", "both", true}, // every prefix goes
		{"feat: wip: later", "", false},
		{"WIPE the cache", "", false},
		{"WIP:", "", true}, // a draft, but nothing would be left
	} {
		title, err := domain.ParseMRTitle(tc.title)
		require.NoError(t, err)
		assert.Equal(t, tc.draft, domain.MergeRequest{Title: title}.IsDraft(), tc.title)
		ready, ok := domain.ReadyTitle(title)
		assert.Equal(t, tc.ready != "", ok, tc.title)
		assert.Equal(t, tc.ready, ready.String(), tc.title)
	}
}
