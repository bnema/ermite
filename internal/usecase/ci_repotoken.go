// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"crypto/rand"
	"encoding/hex"
	"slices"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Job repository tokens (ADR 0025, CI-2). A job gets one at its assignment:
// "ermitegh_" and 32 random bytes, stored hashed, read-only on the repository
// of its run. Auth.Authenticate resolves it to a principal shaped like a
// stored token, so git over HTTP, the API and the REST subset need no rule of
// their own. It is written in the batch of the assignment, deleted in the
// batch that ends the job (CompleteJob, a cancellation that concludes, a lost
// runner, a timeout, the queue and log limits) or that takes the assignment
// back, and with the run when the run is deleted. Its expiry is the deadline of
// the job plus a few minutes, whatever happens.

func newRepoTokenID() string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// repoTokenExpiry is when a token of the job expires at the latest: the
// deadline of the job and the grace.
func (c *CI) repoTokenExpiry(j domain.CIJob) (expires domain.CIRepoToken) {
	expires.ExpiresAt = c.deadline(j).Add(domain.RepoTokenGrace)
	return expires
}

// issueRepoToken adds a repository token for j to the next flush and returns
// its secret. j must be assigned (its AssignedAt is set).
func (rs *runState) issueRepoToken(j *domain.CIJob, archive bool) string {
	return rs.issueRepoTokenFor(j, rs.run.Repo, archive)
}

// issueRepoTokenFor is issueRepoToken for a token that reads repo.
func (rs *runState) issueRepoTokenFor(j *domain.CIJob, repo domain.RepoName, archive bool) string {
	c := rs.c
	now := c.now()
	at := *j
	if at.AssignedAt.IsZero() {
		at.AssignedAt = now
	}
	secret := newToken(domain.RepoTokenPrefix)
	t := c.repoTokenExpiry(at)
	t.ID, t.Hash, t.Repo = newRepoTokenID(), domain.HashTokenSecret(secret), repo
	t.RunID, t.RunAttempt, t.JobID, t.Archive, t.CreatedAt = j.RunID, j.RunAttempt, j.ID, archive, now
	rs.tokens = append(rs.tokens, t)
	return secret
}

// revokeTokens deletes, with the next flush, every repository token of j, and
// forgets the archive tokens kept in memory.
func (rs *runState) revokeTokens(j *domain.CIJob) {
	ref := ports.CIJobRef{RunID: j.RunID, RunAttempt: j.RunAttempt, JobID: j.ID}
	if !slices.Contains(rs.revoke, ref) {
		rs.revoke = append(rs.revoke, ref)
	}
	rs.c.mu.Lock()
	delete(rs.c.archiveTokens, jobRef(*j))
	rs.c.mu.Unlock()
}
