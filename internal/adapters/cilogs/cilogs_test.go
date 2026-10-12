// SPDX-License-Identifier: AGPL-3.0-or-later

package cilogs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/cilogs"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

var (
	ref0 = ports.CILogRef{RunID: 7, JobID: "7-0-0", Attempt: 1}
	t0   = time.Date(2026, 10, 12, 10, 0, 0, 123456789, time.UTC)
)

func lines(texts ...string) []domain.CILogLine {
	out := make([]domain.CILogLine, len(texts))
	for i, s := range texts {
		out[i] = domain.CILogLine{Time: t0.Add(time.Duration(i) * time.Millisecond), Text: s}
	}
	return out
}

const big = 1 << 30

func TestAppendAndReadKeepTextTimeAndContinuation(t *testing.T) {
	ctx := context.Background()
	s := cilogs.New(t.TempDir())
	in := lines("##[group]Run echo hi", "hi", "##[endgroup]")
	in = append(in, domain.CILogLine{Time: t0, Text: "second half", Continued: true})
	next, added, err := s.Append(ctx, ref0, 0, in, big)
	require.NoError(t, err)
	assert.Equal(t, int64(4), next)
	assert.Equal(t, 4, added)

	got, err := s.Read(ctx, ref0, 0, 100, 1<<20)
	require.NoError(t, err)
	require.Len(t, got, 4)
	assert.Equal(t, "##[group]Run echo hi", got[0].Text)
	assert.True(t, got[0].Time.Equal(t0), "nanosecond precision is kept: %v", got[0].Time)
	assert.True(t, got[3].Continued)
	assert.False(t, got[2].Continued)

	tail, err := s.Read(ctx, ref0, 2, 100, 1<<20)
	require.NoError(t, err)
	assert.Equal(t, "##[endgroup]", tail[0].Text)
}

func TestAppendIsIdempotentByOffset(t *testing.T) {
	ctx := context.Background()
	s := cilogs.New(t.TempDir())
	_, _, err := s.Append(ctx, ref0, 0, lines("a", "b", "c"), big)
	require.NoError(t, err)

	// A retry of the same request changes nothing.
	next, added, err := s.Append(ctx, ref0, 0, lines("a", "b", "c"), big)
	require.NoError(t, err)
	assert.Equal(t, int64(3), next)
	assert.Zero(t, added)

	// A request that overlaps appends only its new tail.
	next, added, err = s.Append(ctx, ref0, 1, lines("b", "c", "d"), big)
	require.NoError(t, err)
	assert.Equal(t, int64(4), next)
	assert.Equal(t, 1, added)

	// A retry whose lines differ is silently dropped, as the contract says.
	next, added, err = s.Append(ctx, ref0, 0, lines("X", "Y"), big)
	require.NoError(t, err)
	assert.Equal(t, int64(4), next)
	assert.Zero(t, added)
	got, _ := s.Read(ctx, ref0, 0, 10, 1<<20)
	assert.Equal(t, "a", got[0].Text)
}

func TestAppendAboveTheCountIsAGap(t *testing.T) {
	ctx := context.Background()
	s := cilogs.New(t.TempDir())
	_, _, err := s.Append(ctx, ref0, 0, lines("a"), big)
	require.NoError(t, err)
	_, _, err = s.Append(ctx, ref0, 5, lines("z"), big)
	require.ErrorIs(t, err, domain.ErrOutOfRange)
	var gap *ports.ErrCILogGap
	require.ErrorAs(t, err, &gap)
	assert.Equal(t, int64(1), gap.Count)
}

func TestTheLimitRefusesTheWholeRequestUnlessForced(t *testing.T) {
	ctx := context.Background()
	s := cilogs.New(t.TempDir())
	_, _, err := s.Append(ctx, ref0, 0, lines("aaaa"), 200)
	require.NoError(t, err)
	_, _, err = s.Append(ctx, ref0, 1, lines(strings.Repeat("b", 300)), 200)
	require.ErrorIs(t, err, ports.ErrCILogLimit)
	n, _ := s.Count(ctx, ref0)
	assert.Equal(t, int64(1), n, "nothing of the refused request was appended")

	next, err := s.AppendForced(ctx, ref0, lines("limit reached"))
	require.NoError(t, err)
	assert.Equal(t, int64(2), next)
}

func TestReadBoundsLinesAndBytesAndReportsMissingLogs(t *testing.T) {
	ctx := context.Background()
	s := cilogs.New(t.TempDir())
	_, err := s.Read(ctx, ref0, 0, 10, 100)
	require.ErrorIs(t, err, domain.ErrNotFound)

	_, _, err = s.Append(ctx, ref0, 0, lines("12345", "12345", "12345", "12345"), big)
	require.NoError(t, err)
	got, err := s.Read(ctx, ref0, 0, 3, 1<<20)
	require.NoError(t, err)
	assert.Len(t, got, 3)
	got, err = s.Read(ctx, ref0, 0, 100, 12)
	require.NoError(t, err)
	assert.Len(t, got, 2, "the text of a batch is bounded")
	got, err = s.Read(ctx, ref0, 4, 10, 100)
	require.NoError(t, err)
	assert.Empty(t, got, "reading at the end is empty, not an error")
	_, err = s.Read(ctx, ref0, 5, 10, 100)
	assert.ErrorIs(t, err, domain.ErrOutOfRange)
}

func TestReadFindsLinesDeepInALargeLog(t *testing.T) {
	ctx := context.Background()
	s := cilogs.New(t.TempDir())
	var all []domain.CILogLine
	for i := range 3000 {
		all = append(all, domain.CILogLine{Time: t0, Text: "line " + itoa(i)})
	}
	for off := 0; off < len(all); off += 1000 {
		_, _, err := s.Append(ctx, ref0, int64(off), all[off:off+1000], big)
		require.NoError(t, err)
	}
	got, err := s.Read(ctx, ref0, 2500, 2, 1<<20)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "line 2500", got[0].Text)
	assert.Equal(t, "line 2501", got[1].Text)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestCountSurvivesARestartAndAChangedChannelFiresOnAppend(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := cilogs.New(dir)
	ch := s.Changed(ref0)
	_, _, err := s.Append(ctx, ref0, 0, lines("a", "b"), big)
	require.NoError(t, err)
	select {
	case <-ch:
	default:
		t.Fatal("Changed did not fire on append")
	}

	s2 := cilogs.New(dir)
	n, err := s2.Count(ctx, ref0)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
	_, _, err = s2.Append(ctx, ref0, 2, lines("c"), big)
	require.NoError(t, err)
	got, _ := s2.Read(ctx, ref0, 0, 10, 1<<20)
	assert.Len(t, got, 3)
}

func TestATornLastLineIsCutOnLoad(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := cilogs.New(dir)
	_, _, err := s.Append(ctx, ref0, 0, lines("a", "b"), big)
	require.NoError(t, err)
	path := filepath.Join(dir, "7", "7-0-0", "1.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString("- 2026-10-12T10:00:00.1234")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	s2 := cilogs.New(dir)
	n, err := s2.Count(ctx, ref0)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
	_, _, err = s2.Append(ctx, ref0, 2, lines("c"), big)
	require.NoError(t, err)
	got, err := s2.Read(ctx, ref0, 0, 10, 1<<20)
	require.NoError(t, err)
	assert.Len(t, got, 3)
}

func TestRemoveRunsAndResetAndRunIDs(t *testing.T) {
	ctx := context.Background()
	s := cilogs.New(t.TempDir())
	other := ports.CILogRef{RunID: 9, JobID: "9-0-0", Attempt: 1}
	_, _, _ = s.Append(ctx, ref0, 0, lines("a"), big)
	_, _, _ = s.Append(ctx, other, 0, lines("b"), big)
	ids, err := s.RunIDs(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []int64{7, 9}, ids)

	require.NoError(t, s.Reset(ctx, ref0))
	n, _ := s.Count(ctx, ref0)
	assert.Zero(t, n)

	require.NoError(t, s.RemoveRuns(ctx, []int64{9}))
	_, err = s.Read(ctx, other, 0, 10, 100)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestARefThatCouldEscapeTheDirectoryIsRefused(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := cilogs.New(dir)
	_, _, err := s.Append(ctx, ports.CILogRef{RunID: 1, JobID: "../../etc", Attempt: 1}, 0, lines("x"), big)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, _, err = s.Append(ctx, ports.CILogRef{RunID: 1, JobID: "1-0-0", Attempt: 0}, 0, lines("x"), big)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}
