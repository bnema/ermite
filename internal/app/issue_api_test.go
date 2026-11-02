// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"testing"

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
				_, err := c.issue.ListIssues(ctx, &pb.ListIssuesRequest{Repo: repo})
				return err
			},
			"GetIssue": func(c *clients) error {
				_, err := c.issue.GetIssue(ctx, &pb.GetIssueRequest{Repo: repo, Number: 1})
				return err
			},
		}
		writes = map[string]func(c *clients) error{
			"CreateIssue": func(c *clients) error {
				_, err := c.issue.CreateIssue(ctx, &pb.CreateIssueRequest{Repo: repo, Title: "t", Body: "b"})
				return err
			},
			"AddIssueComment": func(c *clients) error {
				_, err := c.issue.AddIssueComment(ctx, &pb.AddIssueCommentRequest{Repo: repo, Number: 1, Text: "x"})
				return err
			},
			"ResolveIssue": func(c *clients) error {
				_, err := c.issue.ResolveIssue(ctx, &pb.ResolveIssueRequest{Repo: repo, Number: 1})
				return err
			},
			"CloseIssue": func(c *clients) error {
				_, err := c.issue.CloseIssue(ctx, &pb.CloseIssueRequest{Repo: repo, Number: 1})
				return err
			},
			"ReopenIssue": func(c *clients) error {
				_, err := c.issue.ReopenIssue(ctx, &pb.ReopenIssueRequest{Repo: repo, Number: 1})
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

	created, err := r.root.issue.CreateIssue(ctx, &pb.CreateIssueRequest{Repo: "proj", Title: "Crash", Body: "it crashes"})
	require.NoError(t, err)
	issue := created.GetIssue()
	assert.EqualValues(t, 2, issue.GetNumber(), "numbers are shared with merge requests")
	assert.Equal(t, pb.IssueState_ISSUE_STATE_OPEN, issue.GetState())
	assert.Equal(t, "root", issue.GetAuthor())

	// An issue is not a merge request, and the other way round.
	_, err = r.root.mr.GetMergeRequest(ctx, &pb.GetMergeRequestRequest{Repo: "proj", Number: 2})
	assert.Equal(t, "not_found", codeName(err))
	_, err = r.root.issue.GetIssue(ctx, &pb.GetIssueRequest{Repo: "proj", Number: 1})
	assert.Equal(t, "not_found", codeName(err))

	_, err = r.root.issue.AddIssueComment(ctx, &pb.AddIssueCommentRequest{Repo: "proj", Number: 2, Text: "seen"})
	require.NoError(t, err)
	_, err = r.root.issue.AddIssueComment(ctx, &pb.AddIssueCommentRequest{Repo: "proj", Number: 2, Text: " "})
	assert.Equal(t, "invalid_argument", codeName(err), "a blank comment")
	_, err = r.root.issue.CreateIssue(ctx, &pb.CreateIssueRequest{Repo: "proj", Title: ""})
	assert.Equal(t, "invalid_argument", codeName(err), "an empty title")

	_, err = r.root.issue.ReopenIssue(ctx, &pb.ReopenIssueRequest{Repo: "proj", Number: 2})
	assert.Equal(t, "failed_precondition", codeName(err), "reopening an open issue")
	closed, err := r.root.issue.CloseIssue(ctx, &pb.CloseIssueRequest{Repo: "proj", Number: 2})
	require.NoError(t, err)
	assert.Equal(t, pb.IssueState_ISSUE_STATE_CLOSED, closed.GetIssue().GetState())
	_, err = r.root.issue.CloseIssue(ctx, &pb.CloseIssueRequest{Repo: "proj", Number: 2})
	assert.Equal(t, "failed_precondition", codeName(err), "closing a closed issue")

	got, err := r.anon.issue.GetIssue(ctx, &pb.GetIssueRequest{Repo: "proj", Number: 2})
	require.NoError(t, err)
	assert.Equal(t, pb.IssueState_ISSUE_STATE_CLOSED, got.GetIssue().GetState())
	assert.EqualValues(t, 1, got.GetIssue().GetCommentCount())
	require.Len(t, got.GetComments(), 1)
	assert.Equal(t, "seen", got.GetComments()[0].GetText())

	reopened, err := r.root.issue.ReopenIssue(ctx, &pb.ReopenIssueRequest{Repo: "proj", Number: 2})
	require.NoError(t, err)
	assert.Equal(t, pb.IssueState_ISSUE_STATE_OPEN, reopened.GetIssue().GetState())

	list, err := r.anon.issue.ListIssues(ctx, &pb.ListIssuesRequest{Repo: "proj", State: pb.IssueState_ISSUE_STATE_OPEN})
	require.NoError(t, err)
	require.Len(t, list.GetIssues(), 1)
	list, err = r.anon.issue.ListIssues(ctx, &pb.ListIssuesRequest{Repo: "proj", State: pb.IssueState_ISSUE_STATE_CLOSED})
	require.NoError(t, err)
	assert.Empty(t, list.GetIssues())
}

func TestIssuesFeatureAndSettingOverTheAPI(t *testing.T) {
	r := newIssueRig(t)
	ctx := r.ctx
	ver, err := r.anon.server.GetVersion(ctx, &pb.GetVersionRequest{})
	require.NoError(t, err)
	assert.Contains(t, ver.GetFeatures(), "issues")
	assert.Contains(t, ver.GetFeatures(), "issues.resolve")

	update := func(c *clients, mode pb.IssuesMode) (*pb.Repo, error) {
		res, err := c.repo.UpdateRepo(ctx, &pb.UpdateRepoRequest{
			Repo: "priv", UpdateMask: &fieldmaskpb.FieldMask{}, Issues: &mode,
		})
		if err != nil {
			return nil, err
		}
		return res.GetRepo(), nil
	}
	repo, err := r.root.repo.GetRepo(ctx, &pb.GetRepoRequest{Repo: "priv"})
	require.NoError(t, err)
	assert.Equal(t, pb.IssuesMode_ISSUES_MODE_OFF, repo.GetRepo().GetSettings().GetIssues())

	updated, err := update(r.root, pb.IssuesMode_ISSUES_MODE_OWNER)
	require.NoError(t, err)
	assert.Equal(t, pb.IssuesMode_ISSUES_MODE_OWNER, updated.GetSettings().GetIssues())
	_, err = r.root.issue.CreateIssue(ctx, &pb.CreateIssueRequest{Repo: "priv", Title: "t"})
	require.NoError(t, err)

	updated, err = update(r.root, pb.IssuesMode_ISSUES_MODE_PUBLIC)
	require.NoError(t, err, "public issues are accepted")
	assert.Equal(t, pb.IssuesMode_ISSUES_MODE_PUBLIC, updated.GetSettings().GetIssues())
	_, err = update(r.root, pb.IssuesMode_ISSUES_MODE_OWNER)
	require.NoError(t, err)
	_, err = update(r.root, pb.IssuesMode_ISSUES_MODE_UNSPECIFIED)
	assert.Equal(t, "invalid_argument", codeName(err))
	repo, err = r.root.repo.GetRepo(ctx, &pb.GetRepoRequest{Repo: "priv"})
	require.NoError(t, err)
	assert.Equal(t, pb.IssuesMode_ISSUES_MODE_OWNER, repo.GetRepo().GetSettings().GetIssues())

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
		_, err = r.root.issue.CreateIssue(ctx, &pb.CreateIssueRequest{Repo: "proj", Title: title})
		require.NoError(t, err)
	}
	// The merge request mentions #1 (closing) and #2 (plain).
	created, err := r.merger.mr.CreateMergeRequest(ctx, &pb.CreateMergeRequestRequest{
		Repo: "proj", Title: "Change a", Description: "Fixes #1, see also #2", SourceBranch: "feat",
	})
	require.NoError(t, err)
	number := created.GetMergeRequest().GetNumber()

	get := func(n int64) *pb.GetIssueResponse {
		res, err := r.anon.issue.GetIssue(ctx, &pb.GetIssueRequest{Repo: "proj", Number: n})
		require.NoError(t, err)
		return res
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

	watch := r.watch(r.anon, "", "repo:proj")
	_, err = r.merger.mr.MergeMergeRequest(ctx, &pb.MergeMergeRequestRequest{Repo: "proj", Number: number, HeadId: r.head})
	require.NoError(t, err)
	r.forge.MergeRequests.Wait(ctx)
	var streamed *pb.IssueResolved
	for range 8 {
		event := watch.next(t, watchWait)
		if streamed = event.GetIssueResolved(); streamed != nil {
			break
		}
	}
	require.NotNil(t, streamed, "merge publishes issue_resolved")
	assert.EqualValues(t, 1, streamed.GetIssue().GetNumber())
	assert.Equal(t, pb.IssueState_ISSUE_STATE_RESOLVED, streamed.GetIssue().GetState())

	one = get(1)
	assert.Equal(t, pb.IssueState_ISSUE_STATE_RESOLVED, one.GetIssue().GetState())
	assert.Equal(t, number, one.GetIssue().GetClosedByMergeRequest())
	require.Len(t, one.GetEvents(), 2)
	referenced, resolved := one.GetEvents()[0], one.GetEvents()[1]
	assert.Equal(t, pb.IssueEventKind_ISSUE_EVENT_KIND_REFERENCED, referenced.GetKind())
	assert.True(t, referenced.GetCloses())
	assert.Equal(t, "Change a", referenced.GetMergeRequestTitle())
	assert.Equal(t, pb.IssueEventKind_ISSUE_EVENT_KIND_RESOLVED, resolved.GetKind())
	assert.Equal(t, number, resolved.GetMergeRequest())
	assert.NotEmpty(t, resolved.GetActor())
	assert.Equal(t, referenced.GetActor(), resolved.GetActor(), "same token opened and merged")
	detail, err := r.anon.mr.GetMergeRequest(ctx, &pb.GetMergeRequestRequest{Repo: "proj", Number: number})
	require.NoError(t, err)
	require.Len(t, detail.GetIssues(), 2)
	assert.EqualValues(t, 1, detail.GetIssues()[0].GetNumber())
	assert.Equal(t, pb.MergeRequestIssueState_MERGE_REQUEST_ISSUE_STATE_RESOLVED, detail.GetIssues()[0].GetState())
	assert.True(t, detail.GetIssues()[0].GetCloses())
	require.Len(t, detail.GetIssueEvents(), 3)

	require.Len(t, one.GetReferences(), 1)
	assert.Equal(t, pb.MergeRequestState_MERGE_REQUEST_STATE_MERGED, one.GetReferences()[0].GetState())
	two = get(2)
	assert.Equal(t, pb.IssueState_ISSUE_STATE_OPEN, two.GetIssue().GetState(), "a plain mention does not close")
	assert.Zero(t, two.GetIssue().GetClosedByMergeRequest())

	// Reopening by hand clears "closed by".
	reopened, err := r.root.issue.ReopenIssue(ctx, &pb.ReopenIssueRequest{Repo: "proj", Number: 1})
	require.NoError(t, err)
	assert.Zero(t, reopened.GetIssue().GetClosedByMergeRequest())
}

func TestIssueResolveRoundTripOverTheAPI(t *testing.T) {
	r := newIssueRig(t)
	r.mode("pub", domain.IssuesOwner)
	_, err := r.root.issue.CreateIssue(r.ctx, &pb.CreateIssueRequest{Repo: "pub", Title: "Fix"})
	require.NoError(t, err)
	resolved, err := r.root.issue.ResolveIssue(r.ctx, &pb.ResolveIssueRequest{Repo: "pub", Number: 1})
	require.NoError(t, err)
	assert.Equal(t, pb.IssueState_ISSUE_STATE_RESOLVED, resolved.GetIssue().GetState())
	assert.Zero(t, resolved.GetIssue().GetClosedByMergeRequest())
	_, err = r.root.issue.ResolveIssue(r.ctx, &pb.ResolveIssueRequest{Repo: "pub", Number: 1})
	assert.Equal(t, "failed_precondition", codeName(err))
	list, err := r.anon.issue.ListIssues(r.ctx, &pb.ListIssuesRequest{Repo: "pub", State: pb.IssueState_ISSUE_STATE_RESOLVED})
	require.NoError(t, err)
	require.Len(t, list.GetIssues(), 1)
	reopened, err := r.root.issue.ReopenIssue(r.ctx, &pb.ReopenIssueRequest{Repo: "pub", Number: 1})
	require.NoError(t, err)
	assert.Equal(t, pb.IssueState_ISSUE_STATE_OPEN, reopened.GetIssue().GetState())
	d, err := r.anon.issue.GetIssue(r.ctx, &pb.GetIssueRequest{Repo: "pub", Number: 1})
	require.NoError(t, err)
	require.Len(t, d.GetEvents(), 2)
	assert.Equal(t, pb.IssueEventKind_ISSUE_EVENT_KIND_RESOLVED, d.GetEvents()[0].GetKind())
	assert.Equal(t, "root", d.GetEvents()[0].GetActor())
	assert.Equal(t, pb.IssueEventKind_ISSUE_EVENT_KIND_REOPENED, d.GetEvents()[1].GetKind())
}
