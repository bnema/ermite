// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// branchMerger holds mr:merge but may only write the branches under
// agent/claude/.
var branchMerger = domain.TokenPrincipal(domain.Token{
	ID: "bm", Label: "claude", BranchPrefixes: []string{"agent/claude/"},
	Scopes: []domain.Scope{domain.ScopeRepoRead, domain.ScopeMRWrite, domain.ScopeMRMerge},
})

// AC5: a merge writes the target, and the source when it is deleted; a token
// limited to branch prefixes may do neither outside them.
func TestMergeWithBranchLimitedToken(t *testing.T) {
	t.Parallel()

	t.Run("into main is forbidden and nothing moves", func(t *testing.T) {
		t.Parallel()
		r := newMRRig(t)
		main := r.seed()
		head := r.feature("agent/claude/feat")
		mr := r.open("agent/claude/feat") // targets main
		r.drain()

		for _, del := range []bool{false, true} {
			_, err := r.mrs.Merge(r.ctx, branchMerger, r.repo, mr.Number, ports.MergeParams{HeadID: head, DeleteSource: del})
			require.ErrorIs(t, err, domain.ErrForbidden)
			assert.Contains(t, err.Error(), "this token may not write main")
		}
		assert.Equal(t, main, r.branchHead("main"), "main did not move")
		assert.Equal(t, head, r.branchHead("agent/claude/feat"), "the source stays")
		d, err := r.mrs.Get(r.ctx, reader, r.repo, mr.Number, false)
		require.NoError(t, err)
		assert.Equal(t, domain.MRStateOpen, d.MergeRequest.State)
		assert.Empty(t, r.drain(), "no event")

		// The same merge by an unlimited token works.
		_, err = r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head})
		require.NoError(t, err)
		assert.Equal(t, head, r.branchHead("main"))
	})

	t.Run("into a branch under the prefixes is allowed, deleting the source too", func(t *testing.T) {
		t.Parallel()
		r := newMRRig(t)
		r.seed()
		r.sh("checkout", "-q", "-B", "agent/claude/base", "main")
		r.push("agent/claude/base")
		r.sh("checkout", "-q", "-B", "agent/claude/feat", "agent/claude/base")
		head := r.commit(author, "f.txt", "f\n", "add f")
		r.push("agent/claude/feat")
		mr, err := r.mrs.Create(r.ctx, branchMerger, r.repo, ports.CreateMergeRequestParams{
			Title: "feat", Source: "agent/claude/feat", Target: "agent/claude/base",
		})
		require.NoError(t, err)

		res, err := r.mrs.Merge(r.ctx, branchMerger, r.repo, mr.Number, ports.MergeParams{HeadID: head, DeleteSource: true})
		require.NoError(t, err)
		assert.True(t, res.SourceDeleted)
		assert.Equal(t, head, r.branchHead("agent/claude/base"))
		_, err = r.git.BranchHead(r.ctx, r.repo, "agent/claude/feat")
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("deleting a source outside the prefixes is forbidden, merging without deleting is not", func(t *testing.T) {
		t.Parallel()
		r := newMRRig(t)
		r.seed()
		r.sh("checkout", "-q", "-B", "agent/claude/base", "main")
		r.push("agent/claude/base")
		base := r.branchHead("agent/claude/base")
		r.sh("checkout", "-q", "-B", "other", "agent/claude/base")
		head := r.commit(author, "o.txt", "o\n", "add o")
		r.push("other")
		mr, err := r.mrs.Create(r.ctx, branchMerger, r.repo, ports.CreateMergeRequestParams{
			Title: "other", Source: "other", Target: "agent/claude/base",
		})
		require.NoError(t, err)

		_, err = r.mrs.Merge(r.ctx, branchMerger, r.repo, mr.Number, ports.MergeParams{HeadID: head, DeleteSource: true})
		require.ErrorIs(t, err, domain.ErrForbidden)
		assert.Contains(t, err.Error(), "this token may not write other")
		assert.Equal(t, base, r.branchHead("agent/claude/base"), "not merged")
		assert.Equal(t, head, r.branchHead("other"), "the source stays")

		res, err := r.mrs.Merge(r.ctx, branchMerger, r.repo, mr.Number, ports.MergeParams{HeadID: head})
		require.NoError(t, err)
		assert.False(t, res.SourceDeleted)
		assert.Equal(t, head, r.branchHead("agent/claude/base"))
		assert.Equal(t, head, r.branchHead("other"), "the source stays")
	})

	t.Run("a squash into main is forbidden too", func(t *testing.T) {
		t.Parallel()
		r := newMRRig(t)
		main := r.seed()
		head := r.feature("agent/claude/feat")
		mr := r.open("agent/claude/feat")
		_, err := r.mrs.Merge(r.ctx, branchMerger, r.repo, mr.Number, ports.MergeParams{HeadID: head, Mode: domain.MergeModeSquash})
		require.ErrorIs(t, err, domain.ErrForbidden)
		assert.Equal(t, main, r.branchHead("main"))
	})
}
