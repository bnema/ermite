// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/config"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The suite runs with the reader of ERMITE_GIT_READER (ermite-git by default).
// This test says which one that is, so that a run cannot claim a reader it did
// not exercise, and checks the read-after-push guarantee through the real
// HTTP push path and the browse use case.
func TestGitReaderOfTheForgeAndReadAfterPush(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	want := os.Getenv(config.EnvGitReader)
	if want == "" {
		want = config.GitReaderErmiteGit
	}
	assert.Equal(t, want, h.forge.cfg.GitReader)
	assert.Equal(t, want == config.GitReaderErmiteGit, h.forge.reader != nil, "the ermite-git adapter exists only for ermite-git")

	_, err := h.forge.Repos.Create(h.ctx, domain.Root(), ports.CreateRepoParams{
		Name: h.name("fresh"), Visibility: domain.VisibilityPublic,
	})
	require.NoError(t, err)
	wt := h.t.TempDir()
	h.mustGit(wt, "init", "-q", "--initial-branch=main")
	remote := h.urlAuth(adminToken, "/fresh.git")
	var last string
	for _, content := range []string{"one\n", "two\n", "three\n"} {
		h.writeFile(wt, "f.txt", []byte(content)) // commits
		h.mustGit(wt, "push", "-q", remote, "main")
		last = strings.TrimSpace(h.mustGit(wt, "rev-parse", "HEAD"))

		// The very next read, no wait: the browse sees the push.
		res, err := h.forge.Browse.ResolveRevision(h.ctx, domain.Root(), h.name("fresh"), domain.Revision{})
		require.NoError(t, err)
		assert.Equal(t, last, res.CommitID.String())
		tree, err := h.forge.Browse.GetTree(h.ctx, domain.Root(), h.name("fresh"), domain.Revision{}, domain.TreePath{}, true)
		require.NoError(t, err)
		require.Len(t, tree.Entries, 1)
		assert.Equal(t, last, tree.Entries[0].LastCommit.ID.String())
	}

	// Deleting the repository drops what the reader holds; a repository of the
	// same name is a new one.
	require.NoError(t, h.forge.Repos.Delete(h.ctx, domain.Root(), h.name("fresh")))
	_, err = h.forge.Browse.ResolveRevision(h.ctx, domain.Root(), h.name("fresh"), domain.Revision{})
	require.Error(t, err)
}
