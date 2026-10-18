// SPDX-License-Identifier: AGPL-3.0-or-later

package readertest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// CommitObjectsMaker builds a CommitObjectReader serving the repositories
// below data/repos, and returns it with the data directory.
type CommitObjectsMaker func(t *testing.T) (ports.CommitObjectReader, string)

// CommitObjects runs the CommitObjectReader contract against the readers mk
// builds: the raw bytes of unsigned and SSH-signed commits equal `git cat-file
// commit`; missing ids, trees and blobs are left out; a missing repository is
// domain.ErrNotFound; too many ids are domain.ErrInvalid.
func CommitObjects(t *testing.T, mk CommitObjectsMaker) {
	ctx := context.Background()
	r, data := mk(t)
	f := NewFixture(t, data, "signed")

	f.Write("a.txt", "one\n")
	unsigned := f.Commit("unsigned")
	signed := signedCommit(t, f, "b.txt", "signed with ssh")
	f.Write("c.txt", "three\n")
	again := f.Commit("unsigned again")
	tree := f.Git("rev-parse", signed+"^{tree}")
	blob := f.Git("rev-parse", signed+":b.txt")
	missing := ObjectID(t, strings.Repeat("1", 40))

	ids := []domain.ObjectID{ObjectID(t, unsigned), ObjectID(t, signed), ObjectID(t, again)}
	got, err := r.ReadCommitObjects(ctx, f.Name, ids)
	require.NoError(t, err)
	require.Len(t, got, len(ids))
	for _, id := range ids {
		assert.Equal(t, f.Git("cat-file", "commit", id.String()), strings.TrimSpace(string(got[id])), "commit %s", id)
		assert.True(t, bytes.Equal(got[id], rawCommit(t, f, id.String())), "raw bytes of %s", id)
	}
	assert.Contains(t, string(got[ids[1]]), "gpgsig -----BEGIN SSH SIGNATURE-----")
	assert.NotContains(t, string(got[ids[0]]), "gpgsig")

	// Missing ids, a tree and a blob are left out; duplicates are fine.
	mixed := []domain.ObjectID{ids[1], missing, ObjectID(t, tree), ObjectID(t, blob), ids[1], ids[0]}
	got, err = r.ReadCommitObjects(ctx, f.Name, mixed)
	require.NoError(t, err)
	assert.Len(t, got, 2)
	assert.Contains(t, got, ids[0])
	assert.Contains(t, got, ids[1])

	got, err = r.ReadCommitObjects(ctx, f.Name, nil)
	require.NoError(t, err)
	assert.Empty(t, got)

	// A repository reads the same after its objects were packed.
	f.Git("gc", "--quiet")
	got, err = r.ReadCommitObjects(ctx, f.Name, ids)
	require.NoError(t, err)
	assert.Equal(t, rawCommit(t, f, signed), got[ids[1]], "packed")

	t.Run("budget", func(t *testing.T) { commitObjectsBudget(t, r, f) })

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = r.ReadCommitObjects(cancelled, f.Name, ids)
	assert.ErrorIs(t, err, context.Canceled)

	_, err = r.ReadCommitObjects(ctx, RepoName(t, "nope"), ids)
	assert.ErrorIs(t, err, domain.ErrNotFound)

	many := make([]domain.ObjectID, ports.MaxCommitLimit+1)
	for i := range many {
		many[i] = missing
	}
	_, err = r.ReadCommitObjects(ctx, f.Name, many)
	assert.ErrorIs(t, err, domain.ErrInvalid)
	_, err = r.ReadCommitObjects(ctx, f.Name, many[:ports.MaxCommitLimit])
	require.NoError(t, err)
}

// commitObjectsBudget checks the aggregate budget: ten commits of about
// 900 KiB each (under the per-object limit) do not fit in
// domain.MaxCommitObjectsBytes, so the read stops before the one that would
// not fit, and the others are left out.
func commitObjectsBudget(t *testing.T, r ports.CommitObjectReader, f *Fixture) {
	const n, msgSize = 10, 900 << 10
	tree := f.Git("rev-parse", "HEAD^{tree}")
	big := make([]domain.ObjectID, n)
	for i := range big {
		msg := fmt.Sprintf("big %d\n", i) + strings.Repeat("x", msgSize-8) + "\n"
		raw := fmt.Sprintf("tree %s\nauthor A <a@x.org> 1 +0000\ncommitter A <a@x.org> 1 +0000\n\n%s", tree, msg)
		big[i] = ObjectID(t, f.WriteObject("commit", raw))
	}
	got, err := r.ReadCommitObjects(context.Background(), f.Name, big)
	require.NoError(t, err)
	var total int
	for id, raw := range got {
		assert.Contains(t, big, id)
		total += len(raw)
	}
	assert.LessOrEqual(t, total, domain.MaxCommitObjectsBytes)
	assert.Len(t, got, domain.MaxCommitObjectsBytes/(msgSize+200), "as many as fit in the budget")
	assert.Less(t, len(got), n)
	for _, id := range big[:len(got)] {
		assert.Contains(t, got, id, "the budget is spent in the order of the ids")
	}
}

// ObjectID parses a full object id.
func ObjectID(t testing.TB, s string) domain.ObjectID {
	t.Helper()
	id, err := domain.ParseObjectID(s)
	require.NoError(t, err)
	return id
}

// rawCommit returns `git cat-file commit <id>` byte for byte (Fixture.Git
// trims its output).
func rawCommit(t testing.TB, f *Fixture, id string) []byte {
	t.Helper()
	cmd := exec.Command("git", "--git-dir="+f.Bare, "cat-file", "commit", id)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.Work, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	out, err := cmd.Output()
	require.NoError(t, err)
	return out
}

// signedCommit writes a file and commits it with an SSH signature (a key made
// for the test, `git commit -S` with gpg.format=ssh). Without ssh-keygen it
// skips locally and fails when CI is set.
func signedCommit(t testing.TB, f *Fixture, file, msg string) string {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("ssh-keygen not found")
		}
		t.Skip("ssh-keygen not found")
	}
	key := filepath.Join(t.TempDir(), "id_ed25519")
	out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "test", "-f", key).CombinedOutput()
	require.NoError(t, err, string(out))
	f.Write(file, msg+"\n")
	f.Git("add", "-A")
	f.Git("-c", "gpg.format=ssh", "-c", "user.signingkey="+key, "commit", "--quiet", "-S", "-m", msg)
	return f.Git("rev-parse", "HEAD")
}
