// SPDX-License-Identifier: AGPL-3.0-or-later

package ciartifacts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/ciartifacts"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

var ref = ports.CIArtifactRef{RunID: 7, ID: 42}

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestBlocksAssembleInTheOrderOfTheList(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	s := ciartifacts.New(root)

	// Uploaded out of order; the list decides.
	for file, data := range map[string]string{"02.aa": "world", "01.bb": "hello ", "03.cc": "!"} {
		n, err := s.PutBlock(ctx, ref, file, strings.NewReader(data))
		require.NoError(t, err)
		assert.EqualValues(t, len(data), n)
	}
	size, digest, err := s.Assemble(ctx, ref, []string{"01.bb", "02.aa", "03.cc", "03.cc"})
	require.NoError(t, err)
	assert.EqualValues(t, len("hello world!!"), size)
	assert.Equal(t, digestOf("hello world!!"), digest, "a repeated id is repeated content")

	// The documented layout, named by ids only.
	b, err := os.ReadFile(filepath.Join(root, "00000000000000000007", "00000000000000000042"))
	require.NoError(t, err)
	assert.Equal(t, "hello world!!", string(b))
	for _, e := range entries(t, filepath.Join(root, "00000000000000000007")) {
		assert.False(t, strings.HasPrefix(e, ".tmp-"), "no temporary file left: %s", e)
	}

	// Offset reads.
	r, err := s.Open(ctx, ref, 6)
	require.NoError(t, err)
	rest, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	assert.Equal(t, "world!!", string(rest))

	// A new assembly replaces the content whole.
	size, digest, err = s.Assemble(ctx, ref, nil)
	require.NoError(t, err)
	assert.Zero(t, size, "an empty list is an empty content")
	assert.Equal(t, digestOf(""), digest)

	ids, err := s.ArtifactIDs(ctx, 7)
	require.NoError(t, err)
	assert.Equal(t, []int64{42}, ids)
	runs, err := s.RunIDs(ctx)
	require.NoError(t, err)
	assert.Equal(t, []int64{7}, runs)

	require.NoError(t, s.RemoveBlocks(ctx, ref))
	assert.Equal(t, []string{"00000000000000000042"}, entries(t, filepath.Join(root, "00000000000000000007")))
	require.NoError(t, s.Remove(ctx, ref))
	assert.Empty(t, entries(t, filepath.Join(root, "00000000000000000007")))
	_, err = s.Open(ctx, ref, 0)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	require.NoError(t, s.RemoveRuns(ctx, []int64{7}))
	assert.Empty(t, entries(t, root))
}

func TestAMissingBlockIsNotFoundAndLeavesNoContent(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	s := ciartifacts.New(root)
	_, err := s.PutBlock(ctx, ref, "01.aa", strings.NewReader("x"))
	require.NoError(t, err)
	_, _, err = s.Assemble(ctx, ref, []string{"01.aa", "02.aa"})
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = s.Open(ctx, ref, 0)
	assert.ErrorIs(t, err, domain.ErrNotFound, "no content appears")
	for _, e := range entries(t, filepath.Join(root, "00000000000000000007")) {
		assert.False(t, strings.HasPrefix(e, ".tmp-"), e)
	}
}

type failingReader struct{ n int }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n == 0 {
		return 0, errors.New("stream broken")
	}
	f.n--
	p[0] = 'x'
	return 1, nil
}

func TestABrokenSourceLeavesNoBlock(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	s := ciartifacts.New(root)
	_, err := s.PutBlock(ctx, ref, "01.aa", &failingReader{n: 3})
	require.EqualError(t, err, "stream broken", "the source's error as it is")
	assert.Empty(t, entries(t, filepath.Join(root, "00000000000000000007", "00000000000000000042.blocks")))

	// A file name is never reused: the second put of a name fails.
	_, err = s.PutBlock(ctx, ref, "01.aa", strings.NewReader("a"))
	require.NoError(t, err)
	_, err = s.PutBlock(ctx, ref, "01.aa", strings.NewReader("b"))
	assert.Error(t, err)
}

func TestNamesOutsideTheRulesNeverReachTheFileSystem(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	s := ciartifacts.New(filepath.Join(root, "store"))
	for _, name := range []string{"", "../x", "a/b", ".hidden", "..", "ZZ", "01.aa/../../x", strings.Repeat("a", 201)} {
		_, err := s.PutBlock(ctx, ref, name, strings.NewReader("x"))
		assert.ErrorIs(t, err, domain.ErrInvalid, "%q", name)
		_, _, err = s.Assemble(ctx, ref, []string{name})
		assert.Error(t, err, "%q", name)
	}
	for _, bad := range []ports.CIArtifactRef{{RunID: 0, ID: 1}, {RunID: 1, ID: -1}} {
		_, err := s.PutBlock(ctx, bad, "01.aa", strings.NewReader("x"))
		assert.ErrorIs(t, err, domain.ErrInvalid)
		_, err = s.Open(ctx, bad, 0)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	}
	for _, e := range entries(t, root) {
		assert.Equal(t, "store", e, "nothing written beside the store")
	}
}

func TestOrphanListingSeesEveryKindOfFile(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	s := ciartifacts.New(root)
	run := filepath.Join(root, "00000000000000000009")
	require.NoError(t, os.MkdirAll(filepath.Join(run, "00000000000000000003.blocks"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(run, "00000000000000000001"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(run, ".tmp-00000000000000000002-123"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(run, "junk"), nil, 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "not-a-run"), 0o700))

	ids, err := s.ArtifactIDs(ctx, 9)
	require.NoError(t, err)
	assert.Equal(t, []int64{1, 2, 3}, ids)
	runs, err := s.RunIDs(ctx)
	require.NoError(t, err)
	assert.Equal(t, []int64{9}, runs)

	// Remove takes the assembly in progress too.
	require.NoError(t, s.Remove(ctx, ports.CIArtifactRef{RunID: 9, ID: 2}))
	ids, _ = s.ArtifactIDs(ctx, 9)
	assert.Equal(t, []int64{1, 3}, ids)
}
