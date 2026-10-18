// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

// ADR 0030: the commits of a merge request carry their signature state when a
// verifier is set, in one call; without one they stay unchecked.
func TestGetMergeRequestVerifiesItsCommits(t *testing.T) {
	t.Parallel()
	r := newMRRig(t)
	r.seed()
	head := r.feature("feat")
	mr := r.open("feat")

	d, err := r.mrs.Get(r.ctx, reader, r.repo, mr.Number, ports.GetMergeRequestOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, d.Commits)
	assert.Equal(t, domain.StatusUnchecked, d.Commits[0].Verification.Status, "no verifier: unchecked")

	v := mocks.NewCommitVerifier(t)
	want := domain.CommitVerification{Format: domain.FormatSSH, Status: domain.StatusVerified, KeyTitle: "laptop"}
	v.EXPECT().VerifyCommits(mock.Anything, r.repo, mock.MatchedBy(func(cs []domain.Commit) bool {
		return len(cs) == len(d.Commits) && cs[0].ID == head
	})).Return(map[domain.ObjectID]domain.CommitVerification{head: want}).Once()
	r.mrs.WithVerifier(v)

	d, err = r.mrs.Get(r.ctx, reader, r.repo, mr.Number, ports.GetMergeRequestOptions{})
	require.NoError(t, err)
	assert.Equal(t, want, d.Commits[0].Verification)

	// A diff-only read lists no commits and checks nothing.
	d, err = r.mrs.Get(r.ctx, reader, r.repo, mr.Number, ports.GetMergeRequestOptions{DiffOnly: true})
	require.NoError(t, err)
	assert.Empty(t, d.Commits)
}
