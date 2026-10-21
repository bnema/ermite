// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// issueRig is a harness with the repositories pub and priv, and one caller per
// kind of principal.
type issueRig struct {
	*harness
	anon, root, reader, writer, blind, branch *clients
}

func newIssueRig(t *testing.T) *issueRig {
	t.Helper()
	h := newHarness(t, harnessOpts{})
	r := &issueRig{harness: h}
	h.seed("pub", domain.VisibilityPublic)
	h.seed("priv", domain.VisibilityPrivate)
	r.anon, r.root = h.api(""), h.api(adminToken)
	// reader has no issue:write; writer reads and writes; blind holds only
	// issue:write; branch is writer limited to branch prefixes.
	r.reader = h.api(h.token(scopes(domain.ScopeRepoRead)))
	r.writer = h.api(h.token(scopes(domain.ScopeRepoRead, domain.ScopeIssueWrite)))
	r.blind = h.api(h.token(scopes(domain.ScopeIssueWrite)))
	secret, _ := h.branchToken(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite, domain.ScopeIssueWrite), []string{"agent/"})
	r.branch = h.api(secret)
	return r
}

func (r *issueRig) mode(repo string, mode domain.IssuesMode) {
	r.t.Helper()
	_, err := r.forge.Repos.Update(r.ctx, domain.Root(), r.name(repo), ports.UpdateRepoParams{Issues: &mode})
	require.NoError(r.t, err)
}

func TestIssueAccessMatrixOverTheAPI(t *testing.T) {
	r := newIssueRig(t)
	ctx := r.ctx
	type who struct {
		name string
		c    *clients
	}
	all := []who{{"anonymous", r.anon}, {"reader", r.reader}, {"writer", r.writer}, {"blind", r.blind}, {"branch-limited", r.branch}, {"root", r.root}}

	calls := func(repo string) (reads, writes map[string]func(c *clients) error) {
		reads = map[string]func(c *clients) error{
			"ListIssues": func(c *clients) error {
				_, err := c.issue.ListIssues(ctx, connect.NewRequest(&pb.ListIssuesRequest{Repo: repo}))
				return err
			},
			"GetIssue": func(c *clients) error {
				_, err := c.issue.GetIssue(ctx, connect.NewRequest(&pb.GetIssueRequest{Repo: repo, Number: 1}))
				return err
			},
		}
		writes = map[string]func(c *clients) error{
			"CreateIssue": func(c *clients) error {
				_, err := c.issue.CreateIssue(ctx, connect.NewRequest(&pb.CreateIssueRequest{Repo: repo, Title: "t", Body: "b"}))
				return err
			},
			"AddIssueComment": func(c *clients) error {
				_, err := c.issue.AddIssueComment(ctx, connect.NewRequest(&pb.AddIssueCommentRequest{Repo: repo, Number: 1, Text: "x"}))
				return err
			},
			"CloseIssue": func(c *clients) error {
				_, err := c.issue.CloseIssue(ctx, connect.NewRequest(&pb.CloseIssueRequest{Repo: repo, Number: 1}))
				return err
			},
			"ReopenIssue": func(c *clients) error {
				_, err := c.issue.ReopenIssue(ctx, connect.NewRequest(&pb.ReopenIssueRequest{Repo: repo, Number: 1}))
				return err
			},
		}
		return reads, writes
	}
	// setState puts issue 1 in the state a call needs to succeed.
	setState := func(repo, call string) {
		name := r.name(repo)
		if call == "ReopenIssue" {
			_, _ = r.forge.Issues.Close(ctx, domain.Root(), name, 1)
		} else {
			_, _ = r.forge.Issues.Reopen(ctx, domain.Root(), name, 1)
		}
	}

	// Mode owner: the first issue of each repository is #1.
	for _, repo := range []string{"pub", "priv"} {
		r.mode(repo, domain.IssuesOwner)
		_, err := r.forge.Issues.Create(ctx, domain.Root(), r.name(repo), ports.CreateIssueParams{Title: "first"})
		require.NoError(t, err)
	}

	readWant := map[string]map[string]string{
		"priv": {"anonymous": "not_found", "reader": "ok", "writer": "ok", "blind": "not_found", "branch-limited": "ok", "root": "ok"},
		"pub":  {"anonymous": "ok", "reader": "ok", "writer": "ok", "blind": "ok", "branch-limited": "ok", "root": "ok"},
	}
	writeWant := map[string]map[string]string{
		// A token with issue:write alone sees the repository for the action,
		// as mr:write does; a reader without the scope is refused.
		"priv": {"anonymous": "not_found", "reader": "permission_denied", "writer": "ok", "blind": "ok", "branch-limited": "ok", "root": "ok"},
		"pub":  {"anonymous": "unauthenticated", "reader": "permission_denied", "writer": "ok", "blind": "ok", "branch-limited": "ok", "root": "ok"},
	}
	for _, repo := range []string{"priv", "pub"} {
		reads, writes := calls(repo)
		for _, w := range all {
			for name, call := range reads {
				assert.Equal(t, readWant[repo][w.name], codeName(call(w.c)), "%s %s %s", repo, w.name, name)
			}
			for name, call := range writes {
				setState(repo, name)
				assert.Equal(t, writeWant[repo][w.name], codeName(call(w.c)), "%s %s %s", repo, w.name, name)
			}
		}
	}

	// Issues off, an unknown repository: not found for everyone, root included.
	r.mode("priv", domain.IssuesOff)
	r.mode("pub", domain.IssuesOff)
	for _, repo := range []string{"priv", "pub", "nope"} {
		reads, writes := calls(repo)
		for _, w := range all {
			for name, call := range reads {
				assert.Equal(t, "not_found", codeName(call(w.c)), "%s %s %s", repo, w.name, name)
			}
			for name, call := range writes {
				assert.Equal(t, "not_found", codeName(call(w.c)), "%s %s %s", repo, w.name, name)
			}
		}
	}
}

func TestIssueLifecycleAndSharedNumbersOverTheAPI(t *testing.T) {
	r := newMRRig(t, domain.VisibilityPublic)
	ctx := r.ctx
	_, err := r.forge.Repos.Update(ctx, domain.Root(), r.name("proj"), ports.UpdateRepoParams{Issues: new(domain.IssuesOwner)})
	require.NoError(t, err)

	mr, err := r.create(r.root)
	require.NoError(t, err)
	assert.EqualValues(t, 1, mr.GetNumber())

	created, err := r.root.issue.CreateIssue(ctx, connect.NewRequest(&pb.CreateIssueRequest{Repo: "proj", Title: "Crash", Body: "it crashes"}))
	require.NoError(t, err)
	issue := created.Msg.GetIssue()
	assert.EqualValues(t, 2, issue.GetNumber(), "numbers are shared with merge requests")
	assert.Equal(t, pb.IssueState_ISSUE_STATE_OPEN, issue.GetState())
	assert.Equal(t, "root", issue.GetAuthor())

	// An issue is not a merge request, and the other way round.
	_, err = r.root.mr.GetMergeRequest(ctx, connect.NewRequest(&pb.GetMergeRequestRequest{Repo: "proj", Number: 2}))
	assert.Equal(t, "not_found", codeName(err))
	_, err = r.root.issue.GetIssue(ctx, connect.NewRequest(&pb.GetIssueRequest{Repo: "proj", Number: 1}))
	assert.Equal(t, "not_found", codeName(err))

	_, err = r.root.issue.AddIssueComment(ctx, connect.NewRequest(&pb.AddIssueCommentRequest{Repo: "proj", Number: 2, Text: "seen"}))
	require.NoError(t, err)
	_, err = r.root.issue.AddIssueComment(ctx, connect.NewRequest(&pb.AddIssueCommentRequest{Repo: "proj", Number: 2, Text: " "}))
	assert.Equal(t, "invalid_argument", codeName(err), "a blank comment")
	_, err = r.root.issue.CreateIssue(ctx, connect.NewRequest(&pb.CreateIssueRequest{Repo: "proj", Title: ""}))
	assert.Equal(t, "invalid_argument", codeName(err), "an empty title")

	_, err = r.root.issue.ReopenIssue(ctx, connect.NewRequest(&pb.ReopenIssueRequest{Repo: "proj", Number: 2}))
	assert.Equal(t, "failed_precondition", codeName(err), "reopening an open issue")
	closed, err := r.root.issue.CloseIssue(ctx, connect.NewRequest(&pb.CloseIssueRequest{Repo: "proj", Number: 2}))
	require.NoError(t, err)
	assert.Equal(t, pb.IssueState_ISSUE_STATE_CLOSED, closed.Msg.GetIssue().GetState())
	_, err = r.root.issue.CloseIssue(ctx, connect.NewRequest(&pb.CloseIssueRequest{Repo: "proj", Number: 2}))
	assert.Equal(t, "failed_precondition", codeName(err), "closing a closed issue")

	got, err := r.anon.issue.GetIssue(ctx, connect.NewRequest(&pb.GetIssueRequest{Repo: "proj", Number: 2}))
	require.NoError(t, err)
	assert.Equal(t, pb.IssueState_ISSUE_STATE_CLOSED, got.Msg.GetIssue().GetState())
	assert.EqualValues(t, 1, got.Msg.GetIssue().GetCommentCount())
	require.Len(t, got.Msg.GetComments(), 1)
	assert.Equal(t, "seen", got.Msg.GetComments()[0].GetText())

	reopened, err := r.root.issue.ReopenIssue(ctx, connect.NewRequest(&pb.ReopenIssueRequest{Repo: "proj", Number: 2}))
	require.NoError(t, err)
	assert.Equal(t, pb.IssueState_ISSUE_STATE_OPEN, reopened.Msg.GetIssue().GetState())

	list, err := r.anon.issue.ListIssues(ctx, connect.NewRequest(&pb.ListIssuesRequest{Repo: "proj", State: pb.IssueState_ISSUE_STATE_OPEN}))
	require.NoError(t, err)
	require.Len(t, list.Msg.GetIssues(), 1)
	list, err = r.anon.issue.ListIssues(ctx, connect.NewRequest(&pb.ListIssuesRequest{Repo: "proj", State: pb.IssueState_ISSUE_STATE_CLOSED}))
	require.NoError(t, err)
	assert.Empty(t, list.Msg.GetIssues())
}

func TestIssuesFeatureAndSettingOverTheAPI(t *testing.T) {
	r := newIssueRig(t)
	ctx := r.ctx
	ver, err := r.anon.server.GetVersion(ctx, connect.NewRequest(&pb.GetVersionRequest{}))
	require.NoError(t, err)
	assert.Contains(t, ver.Msg.GetFeatures(), "issues")

	update := func(c *clients, mode pb.IssuesMode) (*pb.Repo, error) {
		res, err := c.repo.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{
			Repo: "priv", UpdateMask: &fieldmaskpb.FieldMask{}, Issues: &mode,
		}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetRepo(), nil
	}
	repo, err := r.root.repo.GetRepo(ctx, connect.NewRequest(&pb.GetRepoRequest{Repo: "priv"}))
	require.NoError(t, err)
	assert.Equal(t, pb.IssuesMode_ISSUES_MODE_OFF, repo.Msg.GetRepo().GetSettings().GetIssues())

	updated, err := update(r.root, pb.IssuesMode_ISSUES_MODE_OWNER)
	require.NoError(t, err)
	assert.Equal(t, pb.IssuesMode_ISSUES_MODE_OWNER, updated.GetSettings().GetIssues())
	_, err = r.root.issue.CreateIssue(ctx, connect.NewRequest(&pb.CreateIssueRequest{Repo: "priv", Title: "t"}))
	require.NoError(t, err)

	updated, err = update(r.root, pb.IssuesMode_ISSUES_MODE_PUBLIC)
	require.NoError(t, err, "public issues are accepted")
	assert.Equal(t, pb.IssuesMode_ISSUES_MODE_PUBLIC, updated.GetSettings().GetIssues())
	_, err = update(r.root, pb.IssuesMode_ISSUES_MODE_OWNER)
	require.NoError(t, err)
	_, err = update(r.root, pb.IssuesMode_ISSUES_MODE_UNSPECIFIED)
	assert.Equal(t, "invalid_argument", codeName(err))
	repo, err = r.root.repo.GetRepo(ctx, connect.NewRequest(&pb.GetRepoRequest{Repo: "priv"}))
	require.NoError(t, err)
	assert.Equal(t, pb.IssuesMode_ISSUES_MODE_OWNER, repo.Msg.GetRepo().GetSettings().GetIssues())

	_, err = update(r.writer, pb.IssuesMode_ISSUES_MODE_OFF)
	assert.Equal(t, "permission_denied", codeName(err), "repo:admin is needed")
	_, err = update(r.anon, pb.IssuesMode_ISSUES_MODE_OFF)
	assert.Equal(t, "not_found", codeName(err), "anonymous cannot see the private repository")
	_, err = update(r.root, pb.IssuesMode_ISSUES_MODE_OFF)
	require.NoError(t, err)
}

func TestIssueReferencesAndClosedByOverTheAPI(t *testing.T) {
	r := newMRRig(t, domain.VisibilityPublic)
	ctx := r.ctx
	_, err := r.forge.Repos.Update(ctx, domain.Root(), r.name("proj"), ports.UpdateRepoParams{Issues: new(domain.IssuesOwner)})
	require.NoError(t, err)
	for _, title := range []string{"Crash", "Slow"} {
		_, err = r.root.issue.CreateIssue(ctx, connect.NewRequest(&pb.CreateIssueRequest{Repo: "proj", Title: title}))
		require.NoError(t, err)
	}
	// The merge request mentions #1 (closing) and #2 (plain).
	created, err := r.merger.mr.CreateMergeRequest(ctx, connect.NewRequest(&pb.CreateMergeRequestRequest{
		Repo: "proj", Title: "Change a", Description: "Fixes #1, see also #2", SourceBranch: "feat",
	}))
	require.NoError(t, err)
	number := created.Msg.GetMergeRequest().GetNumber()

	get := func(n int64) *pb.GetIssueResponse {
		res, err := r.anon.issue.GetIssue(ctx, connect.NewRequest(&pb.GetIssueRequest{Repo: "proj", Number: n}))
		require.NoError(t, err)
		return res.Msg
	}
	one := get(1)
	require.Len(t, one.GetReferences(), 1)
	ref := one.GetReferences()[0]
	assert.Equal(t, number, ref.GetNumber())
	assert.Equal(t, "Change a", ref.GetTitle())
	assert.Equal(t, pb.MergeRequestState_MERGE_REQUEST_STATE_OPEN, ref.GetState())
	assert.True(t, ref.GetCloses())
	two := get(2)
	require.Len(t, two.GetReferences(), 1)
	assert.False(t, two.GetReferences()[0].GetCloses())
	assert.Zero(t, one.GetIssue().GetClosedByMergeRequest())

	_, err = r.merger.mr.MergeMergeRequest(ctx, connect.NewRequest(&pb.MergeMergeRequestRequest{Repo: "proj", Number: number, HeadId: r.head}))
	require.NoError(t, err)
	r.forge.MergeRequests.Wait(ctx)

	one = get(1)
	assert.Equal(t, pb.IssueState_ISSUE_STATE_CLOSED, one.GetIssue().GetState())
	assert.Equal(t, number, one.GetIssue().GetClosedByMergeRequest())
	require.Len(t, one.GetReferences(), 1)
	assert.Equal(t, pb.MergeRequestState_MERGE_REQUEST_STATE_MERGED, one.GetReferences()[0].GetState())
	two = get(2)
	assert.Equal(t, pb.IssueState_ISSUE_STATE_OPEN, two.GetIssue().GetState(), "a plain mention does not close")
	assert.Zero(t, two.GetIssue().GetClosedByMergeRequest())

	// Reopening by hand clears "closed by".
	reopened, err := r.root.issue.ReopenIssue(ctx, connect.NewRequest(&pb.ReopenIssueRequest{Repo: "proj", Number: 1}))
	require.NoError(t, err)
	assert.Zero(t, reopened.Msg.GetIssue().GetClosedByMergeRequest())
}
