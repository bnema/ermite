// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli_test

import (
	"strings"
	"testing"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/readertest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Markdown callers must distinguish a matching commit prefix from a revision
// that merely peels to a commit (tag object IDs and hex-named refs).
func TestResolveRevisionHexNonCommitReferences(t *testing.T) {
	t.Parallel()
	g, data := newGit(t)
	f := readertest.NewFixture(t, data, "proj")
	f.Write("file", "content")
	commit := f.Commit("initial")
	f.Git("tag", "-a", "release", "-m", "release", commit)
	tag := f.Git("rev-parse", "refs/tags/release")
	f.Git("branch", "deadbeef", commit)
	for _, input := range []string{tag, "deadbeef", commit[:7]} {
		rev, err := domain.ParseRevision(input)
		require.NoError(t, err)
		resolved, err := g.ResolveRevision(t.Context(), f.Name, rev)
		require.NoError(t, err)
		assert.Equal(t, commit, resolved.CommitID.String())
		accepted := resolved.Ref == "" && strings.HasPrefix(resolved.CommitID.String(), input)
		assert.Equal(t, input == commit[:7], accepted, input)
	}
}
