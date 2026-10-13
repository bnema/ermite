// SPDX-License-Identifier: AGPL-3.0-or-later

package cisummaries_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/cisummaries"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

var ref = ports.CILogRef{RunID: 7, JobID: "7-0-0", Attempt: 1}

func TestPutListReplaceAndRemove(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	s := cisummaries.New(root)

	got, more, err := s.List(ctx, ref, 20)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.False(t, more)

	require.NoError(t, s.Put(ctx, ref, 10, "# ten"))
	require.NoError(t, s.Put(ctx, ref, 2, "two"))
	// The layout is the documented one.
	b, err := os.ReadFile(filepath.Join(root, "7", "7-0-0", "1", "2.md"))
	require.NoError(t, err)
	assert.Equal(t, "two", string(b))

	got, _, err = s.List(ctx, ref, 20)
	require.NoError(t, err)
	assert.Equal(t, []domain.CIStepSummary{{Step: 2, Markdown: "two"}, {Step: 10, Markdown: "# ten"}}, got, "by step number, not by name")

	// Replace, and an empty markdown removes.
	require.NoError(t, s.Put(ctx, ref, 2, "again"))
	got, _, _ = s.List(ctx, ref, 20)
	assert.Equal(t, "again", got[0].Markdown)
	require.NoError(t, s.Put(ctx, ref, 2, ""))
	require.NoError(t, s.Put(ctx, ref, 3, ""), "removing what is not there is fine")
	got, _, _ = s.List(ctx, ref, 20)
	require.Len(t, got, 1)
	assert.EqualValues(t, 10, got[0].Step)

	// Another attempt and another job have their own.
	other := ports.CILogRef{RunID: 7, JobID: "7-0-0", Attempt: 2}
	got, _, _ = s.List(ctx, other, 20)
	assert.Empty(t, got)

	// A write leaves no temporary file behind.
	entries, err := os.ReadDir(filepath.Join(root, "7", "7-0-0", "1"))
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".tmp"), e.Name())
	}
}

func TestListStopsAtTheLimit(t *testing.T) {
	ctx := t.Context()
	s := cisummaries.New(t.TempDir())
	for i := int32(1); i <= 25; i++ {
		require.NoError(t, s.Put(ctx, ref, i, "s"))
	}
	got, more, err := s.List(ctx, ref, 20)
	require.NoError(t, err)
	assert.Len(t, got, 20)
	assert.True(t, more)
	assert.EqualValues(t, 20, got[19].Step, "the first 20 steps that have one")
	got, more, _ = s.List(ctx, ref, 25)
	assert.Len(t, got, 25)
	assert.False(t, more)
}

// A temporary file left by a crash, or a stray file, is not a summary.
func TestListIgnoresStrayFiles(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	s := cisummaries.New(root)
	require.NoError(t, s.Put(ctx, ref, 1, "one"))
	dir := filepath.Join(root, "7", "7-0-0", "1")
	for _, n := range []string{".tmp-123", "x.md", "01.md", "0.md", "-1.md", "notes.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte("stray"), 0o600))
	}
	got, _, err := s.List(ctx, ref, 20)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "one", got[0].Markdown)
}

func TestRemoveJobRemoveRunsAndRunIDs(t *testing.T) {
	ctx := t.Context()
	s := cisummaries.New(t.TempDir())
	ids, err := s.RunIDs(ctx)
	require.NoError(t, err)
	assert.Empty(t, ids, "no directory yet")

	second := ports.CILogRef{RunID: 7, JobID: "7-1-0", Attempt: 1}
	third := ports.CILogRef{RunID: 8, JobID: "8-0-0", Attempt: 1}
	for _, r := range []ports.CILogRef{ref, second, third} {
		require.NoError(t, s.Put(ctx, r, 1, "x"))
	}
	ids, _ = s.RunIDs(ctx)
	assert.ElementsMatch(t, []int64{7, 8}, ids)

	require.NoError(t, s.RemoveJob(ctx, ref))
	got, _, _ := s.List(ctx, ref, 20)
	assert.Empty(t, got)
	got, _, _ = s.List(ctx, second, 20)
	assert.Len(t, got, 1, "the other job of the run stays")

	require.NoError(t, s.RemoveRuns(ctx, []int64{7, 0, -3, 99}))
	got, _, _ = s.List(ctx, second, 20)
	assert.Empty(t, got)
	got, _, _ = s.List(ctx, third, 20)
	assert.Len(t, got, 1)
	ids, _ = s.RunIDs(ctx)
	assert.Equal(t, []int64{8}, ids)
}

// A reference that could leave the directory is refused, never joined.
func TestRefsCannotEscape(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	s := cisummaries.New(filepath.Join(root, "summaries"))
	for _, bad := range []ports.CILogRef{
		{RunID: 7, JobID: "../../x", Attempt: 1},
		{RunID: 7, JobID: "7-0-0/../..", Attempt: 1},
		{RunID: 0, JobID: "7-0-0", Attempt: 1},
		{RunID: 7, JobID: "7-0-0", Attempt: 0},
	} {
		assert.ErrorIs(t, s.Put(ctx, bad, 1, "x"), domain.ErrNotFound)
		got, _, err := s.List(ctx, bad, 20)
		require.NoError(t, err)
		assert.Empty(t, got)
	}
	assert.ErrorIs(t, s.Put(ctx, ref, 0, "x"), domain.ErrNotFound)
	assert.ErrorIs(t, s.Put(ctx, ref, domain.CIMaxSteps+1, "x"), domain.ErrNotFound)
	entries, _ := os.ReadDir(root)
	assert.Empty(t, entries, "nothing was written")
}

func TestASummaryOfTheMaximumSizeRoundTrips(t *testing.T) {
	ctx := t.Context()
	s := cisummaries.New(t.TempDir())
	big := strings.Repeat("x", domain.CIMaxSummaryBytes)
	require.NoError(t, s.Put(ctx, ref, 1, big))
	got, _, err := s.List(ctx, ref, 20)
	require.NoError(t, err)
	assert.Len(t, got[0].Markdown, domain.CIMaxSummaryBytes)
}
