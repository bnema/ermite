// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"crypto/rand"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/clientlimit"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

func (r *issueRig) pending(repo, title string) domain.PendingIssue {
	r.t.Helper()
	id, err := domain.NewPendingIssueID()
	require.NoError(r.t, err)
	ti, err := domain.ParseIssueTitle(title)
	require.NoError(r.t, err)
	body, err := domain.ParseIssueBody("body of " + title)
	require.NoError(r.t, err)
	hash := make([]byte, domain.IPHashLen)
	_, err = rand.Read(hash)
	require.NoError(r.t, err)
	p := domain.PendingIssue{
		ID: id, Repo: r.name(repo), Title: ti, Body: body, Author: "visitor",
		CreatedAt: time.Now().UTC(), IPHash: hash,
	}
	require.NoError(r.t, r.forge.PendingIssues.Put(r.ctx, p))
	return p
}

func TestIssueModerationOverTheAPI(t *testing.T) {
	r := newIssueRig(t)
	ctx := r.ctx
	r.mode("pub", domain.IssuesOwner)
	owner := r.api(r.token(domain.AllScopes()))
	p1 := r.pending("pub", "first")
	p2 := r.pending("pub", "second")

	// Only owner rights see anything: everyone else gets NotFound.
	for name, c := range map[string]*clients{"anonymous": r.anon, "reader": r.reader, "writer": r.writer, "blind": r.blind, "branch": r.branch} {
		_, err := c.issue.ListPendingIssues(ctx, connect.NewRequest(&pb.ListPendingIssuesRequest{}))
		assert.Equal(t, "not_found", codeName(err), "%s list", name)
		_, err = c.issue.ApproveIssue(ctx, connect.NewRequest(&pb.ApproveIssueRequest{Repo: "pub", Id: p1.ID.String()}))
		assert.Equal(t, "not_found", codeName(err), "%s approve", name)
		_, err = c.issue.RejectIssue(ctx, connect.NewRequest(&pb.RejectIssueRequest{Repo: "pub", Id: p1.ID.String(), Ban: true}))
		assert.Equal(t, "not_found", codeName(err), "%s reject", name)
		// A malformed repository tells a non-owner nothing more.
		_, err = c.issue.ListPendingIssues(ctx, connect.NewRequest(&pb.ListPendingIssuesRequest{Repo: "Bad Repo!"}))
		assert.Equal(t, "not_found", codeName(err), "%s list, malformed repo", name)
		_, err = c.issue.ApproveIssue(ctx, connect.NewRequest(&pb.ApproveIssueRequest{Repo: "Bad Repo!", Id: p1.ID.String()}))
		assert.Equal(t, "not_found", codeName(err), "%s approve, malformed repo", name)
		_, err = c.issue.RejectIssue(ctx, connect.NewRequest(&pb.RejectIssueRequest{Repo: "Bad Repo!", Id: "nope"}))
		assert.Equal(t, "not_found", codeName(err), "%s reject, malformed repo", name)
	}
	still, err := r.forge.PendingIssues.Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, still, "nothing changed")

	for name, c := range map[string]*clients{"root": r.root, "owner token": owner} {
		res, err := c.issue.ListPendingIssues(ctx, connect.NewRequest(&pb.ListPendingIssuesRequest{}))
		require.NoError(t, err, name)
		require.Len(t, res.Msg.GetIssues(), 2, name)
		assert.Equal(t, p1.ID.String(), res.Msg.GetIssues()[0].GetId(), "oldest first")
		assert.Equal(t, "visitor", res.Msg.GetIssues()[0].GetAuthor())
	}
	res, err := owner.issue.ListPendingIssues(ctx, connect.NewRequest(&pb.ListPendingIssuesRequest{Repo: "priv"}))
	require.NoError(t, err)
	assert.Empty(t, res.Msg.GetIssues())

	// An owner does learn that a repository name is malformed.
	_, err = owner.issue.ApproveIssue(ctx, connect.NewRequest(&pb.ApproveIssueRequest{Repo: "Bad Repo!", Id: p1.ID.String()}))
	assert.Equal(t, "invalid_argument", codeName(err))

	// A malformed or unknown id is NotFound.
	_, err = owner.issue.ApproveIssue(ctx, connect.NewRequest(&pb.ApproveIssueRequest{Repo: "pub", Id: "nope"}))
	assert.Equal(t, "not_found", codeName(err))

	// Approval takes the next number shared with the merge requests and issues.
	made, err := owner.issue.CreateIssue(ctx, connect.NewRequest(&pb.CreateIssueRequest{Repo: "pub", Title: "by owner"}))
	require.NoError(t, err)
	assert.Equal(t, int64(1), made.Msg.GetIssue().GetNumber())
	ap, err := owner.issue.ApproveIssue(ctx, connect.NewRequest(&pb.ApproveIssueRequest{Repo: "pub", Id: p1.ID.String()}))
	require.NoError(t, err)
	assert.Equal(t, int64(2), ap.Msg.GetIssue().GetNumber())
	assert.Equal(t, pb.IssueState_ISSUE_STATE_OPEN, ap.Msg.GetIssue().GetState())
	assert.Equal(t, "first", ap.Msg.GetIssue().GetTitle())
	got, err := r.reader.issue.GetIssue(ctx, connect.NewRequest(&pb.GetIssueRequest{Repo: "pub", Number: 2}))
	require.NoError(t, err)
	assert.Equal(t, "visitor", got.Msg.GetIssue().GetAuthor())
	_, err = owner.issue.ApproveIssue(ctx, connect.NewRequest(&pb.ApproveIssueRequest{Repo: "pub", Id: p1.ID.String()}))
	assert.Equal(t, "not_found", codeName(err), "approved once")

	// Rejecting with a ban stores the ban and removes the record.
	_, err = owner.issue.RejectIssue(ctx, connect.NewRequest(&pb.RejectIssueRequest{Repo: "pub", Id: p2.ID.String(), Ban: true}))
	require.NoError(t, err)
	banned, err := r.forge.IssueBans.Banned(ctx, p2.IPHash, time.Now())
	require.NoError(t, err)
	assert.True(t, banned)
	banned, err = r.forge.IssueBans.Banned(ctx, p2.IPHash, time.Now().Add(domain.BanDuration+time.Hour))
	require.NoError(t, err)
	assert.False(t, banned, "the ban expires")
	_, err = r.forge.PendingIssues.Get(ctx, p2.Repo, p2.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)

	// Rejecting without a ban bans nobody.
	p3 := r.pending("pub", "third")
	_, err = owner.issue.RejectIssue(ctx, connect.NewRequest(&pb.RejectIssueRequest{Repo: "pub", Id: p3.ID.String()}))
	require.NoError(t, err)
	banned, err = r.forge.IssueBans.Banned(ctx, p3.IPHash, time.Now())
	require.NoError(t, err)
	assert.False(t, banned)

	// The inbox lists what awaits.
	r.pending("pub", "fourth")
	ov, err := r.forge.Inbox.Overview(ctx, domain.Root())
	require.NoError(t, err)
	assert.Equal(t, 1, ov.Counts.Pending)
	require.Len(t, ov.Pending, 1)
	assert.Nil(t, ov.Pending[0].IPHash)

	// The public mode is accepted.
	pub := domain.IssuesPublic
	_, err = r.forge.Repos.Update(ctx, domain.Root(), r.name("pub"), ports.UpdateRepoParams{Issues: &pub})
	assert.NoError(t, err)
}

// clientlimit may not import the domain: its copies of the limits must match.
func TestSubmissionLimiterUsesTheDomainLimits(t *testing.T) {
	assert.Equal(t, domain.SubmitPerHour, clientlimit.PerHour)
	assert.Equal(t, domain.SubmitPerDay, clientlimit.PerDay)
}
