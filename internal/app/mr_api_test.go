// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
)

// mrRig is a harness with a repository holding main and a branch feat, and one
// caller per kind of principal.
type mrRig struct {
	*harness
	wt                         string
	head                       string
	anon, root, reader, writer *clients
	merger, noScope, agent     *clients
}

func newMRRig(t *testing.T, vis domain.Visibility) *mrRig {
	t.Helper()
	h := newHarness(t, harnessOpts{})
	r := &mrRig{harness: h}
	r.seed("proj", vis, "a.txt")
	r.wt = h.workTree("main")
	h.mustGit(r.wt, "fetch", "-q", h.urlAuth(adminToken, "/proj.git"), "main")
	h.mustGit(r.wt, "reset", "-q", "--hard", "FETCH_HEAD")
	h.mustGit(r.wt, "checkout", "-q", "-b", "feat")
	h.writeFile(r.wt, "a.txt", []byte("changed\n"))
	h.writeFile(r.wt, "b.txt", []byte("bee\n"))
	h.mustGit(r.wt, "push", "-q", h.urlAuth(adminToken, "/proj.git"), "feat")
	r.head = h.bareGit("proj", "rev-parse", "feat")

	r.anon, r.root = h.api(""), h.api(adminToken)
	r.reader = h.api(h.token(scopes(domain.ScopeRepoRead)))
	r.writer = h.api(h.token(scopes(domain.ScopeRepoRead, domain.ScopeMRWrite)))
	r.merger = h.api(h.token(scopes(domain.ScopeRepoRead, domain.ScopeMRWrite, domain.ScopeMRMerge)))
	// noScope holds neither repo:read nor an mr scope; agent is the agent preset.
	r.noScope = h.api(h.token(scopes(domain.ScopeRepoWrite)))
	r.agent = h.api(h.token(scopes(domain.ScopeRepoWrite, domain.ScopeMRWrite)))
	return r
}

func (r *mrRig) create(c *clients) (*pb.MergeRequest, error) {
	res, err := c.mr.CreateMergeRequest(r.ctx, connect.NewRequest(&pb.CreateMergeRequestRequest{
		Repo: "proj", Title: "Change a", Description: "why", SourceBranch: "feat",
	}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetMergeRequest(), nil
}

// AC2 for merge requests: who may read and who may write.
func TestMergeRequestAccessOverTheAPI(t *testing.T) {
	r := newMRRig(t, domain.VisibilityPrivate)
	ctx := r.ctx
	mr, err := r.create(r.writer)
	require.NoError(t, err)
	assert.EqualValues(t, 1, mr.GetNumber())
	assert.Equal(t, pb.MergeRequestState_MERGE_REQUEST_STATE_OPEN, mr.GetState())
	assert.Equal(t, "e2e", mr.GetAuthor(), "the label of the token")
	assert.Equal(t, "main", mr.GetTargetBranch())
	assert.Equal(t, r.head, mr.GetHeadId())

	reads := map[string]func(c *clients) error{
		"ListMergeRequests": func(c *clients) error {
			_, err := c.mr.ListMergeRequests(ctx, connect.NewRequest(&pb.ListMergeRequestsRequest{Repo: "proj"}))
			return err
		},
		"GetMergeRequest": func(c *clients) error {
			_, err := c.mr.GetMergeRequest(ctx, connect.NewRequest(&pb.GetMergeRequestRequest{Repo: "proj", Number: 1}))
			return err
		},
	}
	// A private repository: invisible without repo:read, never "forbidden".
	for name, call := range reads {
		assert.Equal(t, "not_found", codeName(call(r.anon)), name+" anonymous")
		assert.Equal(t, "not_found", codeName(call(r.noScope)), name+" without repo:read")
		assert.Equal(t, "ok", codeName(call(r.reader)), name+" reader")
		assert.Equal(t, "ok", codeName(call(r.root)), name+" root")
	}
	writes := map[string]func(c *clients) error{
		"CreateMergeRequest": func(c *clients) error {
			_, err := c.mr.CreateMergeRequest(ctx, connect.NewRequest(&pb.CreateMergeRequestRequest{Repo: "proj", Title: "t", SourceBranch: "nope"}))
			return err
		},
		"AddComment": func(c *clients) error {
			_, err := c.mr.AddComment(ctx, connect.NewRequest(&pb.AddCommentRequest{Repo: "proj", Number: 1, Text: "x"}))
			return err
		},
		"ResolveComment": func(c *clients) error {
			_, err := c.mr.ResolveComment(ctx, connect.NewRequest(&pb.ResolveCommentRequest{Repo: "proj", Number: 1, CommentId: "1", Resolved: true}))
			return err
		},
		"MarkRead": func(c *clients) error {
			_, err := c.mr.MarkRead(ctx, connect.NewRequest(&pb.MarkReadRequest{Repo: "proj", Number: 1}))
			return err
		},
		"MergeMergeRequest": func(c *clients) error {
			_, err := c.mr.MergeMergeRequest(ctx, connect.NewRequest(&pb.MergeMergeRequestRequest{Repo: "proj", Number: 1, HeadId: r.head}))
			return err
		},
		"CloseMergeRequest": func(c *clients) error {
			_, err := c.mr.CloseMergeRequest(ctx, connect.NewRequest(&pb.CloseMergeRequestRequest{Repo: "proj", Number: 99}))
			return err
		},
	}
	for name, call := range writes {
		assert.Equal(t, "not_found", codeName(call(r.anon)), name+" anonymous")
		assert.Equal(t, "not_found", codeName(call(r.noScope)), name+" without repo:read")
		assert.Equal(t, "permission_denied", codeName(call(r.reader)), name+" reader")
		if name == "MergeMergeRequest" {
			assert.Equal(t, "permission_denied", codeName(call(r.writer)), "mr:write does not merge")
		}
	}

	// GO-001: the agent preset has no repo:read, yet mr:write is enough to write
	// merge requests on the private repository; it still does not read them nor merge.
	assert.Equal(t, "invalid_argument", codeName(writes["CreateMergeRequest"](r.agent)), "create sees the repository: the branch is what is wrong")
	assert.Equal(t, "ok", codeName(writes["AddComment"](r.agent)), "AddComment agent")
	assert.Equal(t, "ok", codeName(writes["MarkRead"](r.agent)), "MarkRead agent")
	assert.Equal(t, "not_found", codeName(writes["MergeMergeRequest"](r.agent)), "mr:merge is not held: the repository stays hidden")
	for name, call := range reads {
		assert.Equal(t, "not_found", codeName(call(r.agent)), name+" agent: repo:write does not read")
	}

	// Public: anonymous reads, and writing asks for credentials.
	h2 := newMRRig(t, domain.VisibilityPublic)
	assert.Equal(t, "ok", codeName(reads["ListMergeRequests"](h2.anon)), "public list")
	_, err = h2.create(h2.anon)
	assert.Equal(t, "unauthenticated", codeName(err))
	_, err = h2.create(h2.reader)
	assert.Equal(t, "permission_denied", codeName(err))
	_, err = h2.create(h2.writer)
	require.NoError(t, err)
	_, err = h2.anon.mr.GetMergeRequest(h2.ctx, connect.NewRequest(&pb.GetMergeRequestRequest{Repo: "proj", Number: 1}))
	assert.NoError(t, err)

	// Bad names and numbers.
	_, err = r.root.mr.GetMergeRequest(ctx, connect.NewRequest(&pb.GetMergeRequestRequest{Repo: "Proj", Number: 1}))
	assert.Equal(t, "invalid_argument", codeName(err))
	_, err = r.root.mr.GetMergeRequest(ctx, connect.NewRequest(&pb.GetMergeRequestRequest{Repo: "proj", Number: 0}))
	assert.Equal(t, "not_found", codeName(err))
	_, err = r.root.mr.GetMergeRequest(ctx, connect.NewRequest(&pb.GetMergeRequestRequest{Repo: "ghost", Number: 1}))
	assert.Equal(t, "not_found", codeName(err))
}

// AC3: a request that breaks a rule is rejected whole, and the error carries a
// BadRequest detail naming the fields.
func TestMergeRequestRulesCarryABadRequestDetail(t *testing.T) {
	r := newMRRig(t, domain.VisibilityPublic)
	_, err := r.root.mr.CreateMergeRequest(r.ctx, connect.NewRequest(&pb.CreateMergeRequestRequest{
		Repo: "proj", Title: "two\nlines", Description: "bidi \u202e", SourceBranch: "feat", TargetBranch: "feat",
	}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
	var ce *connect.Error
	require.ErrorAs(t, err, &ce)
	require.Len(t, ce.Details(), 1)
	v, err := ce.Details()[0].Value()
	require.NoError(t, err)
	br, ok := v.(*errdetails.BadRequest)
	require.True(t, ok, "%T", v)
	fields := map[string]string{}
	for _, f := range br.GetFieldViolations() {
		fields[f.GetField()] = f.GetDescription()
	}
	assert.Contains(t, fields, "title")
	assert.Contains(t, fields, "description")
	assert.Contains(t, fields, "target_branch")

	// Nothing was created.
	res, err := r.root.mr.ListMergeRequests(r.ctx, connect.NewRequest(&pb.ListMergeRequestsRequest{Repo: "proj"}))
	require.NoError(t, err)
	assert.Empty(t, res.Msg.GetMergeRequests())

	// Other bad requests.
	_, err = r.root.mr.ListMergeRequests(r.ctx, connect.NewRequest(&pb.ListMergeRequestsRequest{Repo: "proj", State: 9}))
	assert.Equal(t, "invalid_argument", codeName(err))
	_, err = r.root.mr.ListMergeRequests(r.ctx, connect.NewRequest(&pb.ListMergeRequestsRequest{Repo: "proj", PageToken: "x"}))
	assert.Equal(t, "invalid_argument", codeName(err))
	_, err = r.root.mr.MergeMergeRequest(r.ctx, connect.NewRequest(&pb.MergeMergeRequestRequest{Repo: "proj", Number: 1, HeadId: "abc"}))
	assert.Equal(t, "invalid_argument", codeName(err), "a short id is not a commit id")
	_, err = r.root.mr.MergeMergeRequest(r.ctx, connect.NewRequest(&pb.MergeMergeRequestRequest{Repo: "proj", Number: 1}))
	assert.Equal(t, "invalid_argument", codeName(err), "head_id is required")
}

func TestMergeRequestLifecycleOverTheAPI(t *testing.T) {
	r := newMRRig(t, domain.VisibilityPublic)
	ctx := r.ctx
	mr, err := r.create(r.writer)
	require.NoError(t, err)

	_, err = r.create(r.writer)
	assert.Equal(t, "already_exists", codeName(err))

	// A comment on a line of the diff, a general one, and a wrong line.
	c1, err := r.writer.mr.AddComment(ctx, connect.NewRequest(&pb.AddCommentRequest{
		Repo: "proj", Number: 1, Path: "b.txt", Line: 1, Side: pb.DiffSide_DIFF_SIDE_NEW, Text: "nice",
	}))
	require.NoError(t, err)
	assert.Equal(t, "1", c1.Msg.GetComment().GetId())
	assert.Equal(t, r.head, c1.Msg.GetComment().GetCommitId())
	_, err = r.writer.mr.AddComment(ctx, connect.NewRequest(&pb.AddCommentRequest{Repo: "proj", Number: 1, Text: "general"}))
	require.NoError(t, err)
	_, err = r.writer.mr.AddComment(ctx, connect.NewRequest(&pb.AddCommentRequest{
		Repo: "proj", Number: 1, Path: "b.txt", Line: 7, Side: pb.DiffSide_DIFF_SIDE_NEW, Text: "no such line",
	}))
	assert.Equal(t, "invalid_argument", codeName(err))

	// Resolve, idempotent; an id nobody issued is not found.
	for range 2 {
		res, err := r.writer.mr.ResolveComment(ctx, connect.NewRequest(&pb.ResolveCommentRequest{Repo: "proj", Number: 1, CommentId: "1", Resolved: true}))
		require.NoError(t, err)
		assert.True(t, res.Msg.GetComment().GetResolved())
	}
	for _, id := range []string{"99", "0", "x", "01", ""} {
		_, err = r.writer.mr.ResolveComment(ctx, connect.NewRequest(&pb.ResolveCommentRequest{Repo: "proj", Number: 1, CommentId: id, Resolved: true}))
		assert.Equal(t, "not_found", codeName(err), "comment %q", id)
	}

	// Get, anonymously: the commits, the diff and the comments.
	got, err := r.anon.mr.GetMergeRequest(ctx, connect.NewRequest(&pb.GetMergeRequestRequest{Repo: "proj", Number: 1}))
	require.NoError(t, err)
	m := got.Msg
	assert.Equal(t, r.head, m.GetMergeRequest().GetHeadId())
	assert.Len(t, m.GetCommits(), 2)
	assert.False(t, m.GetCommitsTruncated())
	assert.NotEmpty(t, m.GetDiffBaseId())
	assert.EqualValues(t, 2, m.GetStats().GetFilesChanged())
	assert.Len(t, m.GetDiff().GetFiles(), 2)
	require.Len(t, m.GetComments(), 2)
	assert.Equal(t, "1", m.GetComments()[0].GetId())
	assert.True(t, m.GetComments()[0].GetResolved())
	assert.Empty(t, m.GetLastReadId())

	// MarkRead, then since_last_read after a push.
	_, err = r.writer.mr.MarkRead(ctx, connect.NewRequest(&pb.MarkReadRequest{Repo: "proj", Number: 1}))
	require.NoError(t, err)
	r.writeFile(r.wt, "c.txt", []byte("see\n"))
	r.mustGit(r.wt, "push", "-q", r.urlAuth(adminToken, "/proj.git"), "feat")
	got, err = r.reader.mr.GetMergeRequest(ctx, connect.NewRequest(&pb.GetMergeRequestRequest{Repo: "proj", Number: 1, SinceLastRead: true}))
	require.NoError(t, err)
	assert.Equal(t, r.head, got.Msg.GetLastReadId())
	assert.Equal(t, r.head, got.Msg.GetDiffBaseId())
	require.Len(t, got.Msg.GetDiff().GetFiles(), 1)
	assert.Equal(t, "c.txt", got.Msg.GetDiff().GetFiles()[0].GetNewPath())
	assert.False(t, got.Msg.GetLastReadLost())

	// The old head is refused: the branch moved.
	_, err = r.merger.mr.MergeMergeRequest(ctx, connect.NewRequest(&pb.MergeMergeRequestRequest{Repo: "proj", Number: 1, HeadId: r.head}))
	assert.Equal(t, "failed_precondition", codeName(err))
	newHead := r.bareGit("proj", "rev-parse", "feat")
	merged, err := r.merger.mr.MergeMergeRequest(ctx, connect.NewRequest(&pb.MergeMergeRequestRequest{
		Repo: "proj", Number: 1, Mode: pb.MergeMode_MERGE_MODE_FAST_FORWARD, HeadId: newHead,
	}))
	require.NoError(t, err)
	mm := merged.Msg.GetMergeRequest()
	assert.Equal(t, pb.MergeRequestState_MERGE_REQUEST_STATE_MERGED, mm.GetState())
	assert.Equal(t, newHead, mm.GetMergedCommitId())
	assert.Equal(t, pb.MergeMode_MERGE_MODE_FAST_FORWARD, mm.GetMergeMode())
	assert.Equal(t, newHead, r.bareGit("proj", "rev-parse", "main"))

	// Listing by state; repeating a merge or closing a merged request fails.
	list := func(st pb.MergeRequestState) int {
		res, err := r.anon.mr.ListMergeRequests(ctx, connect.NewRequest(&pb.ListMergeRequestsRequest{Repo: "proj", State: st}))
		require.NoError(t, err)
		return len(res.Msg.GetMergeRequests())
	}
	assert.Equal(t, 1, list(pb.MergeRequestState_MERGE_REQUEST_STATE_UNSPECIFIED))
	assert.Equal(t, 1, list(pb.MergeRequestState_MERGE_REQUEST_STATE_MERGED))
	assert.Equal(t, 0, list(pb.MergeRequestState_MERGE_REQUEST_STATE_OPEN))
	_, err = r.merger.mr.MergeMergeRequest(ctx, connect.NewRequest(&pb.MergeMergeRequestRequest{Repo: "proj", Number: 1, HeadId: newHead}))
	assert.Equal(t, "failed_precondition", codeName(err))
	_, err = r.writer.mr.CloseMergeRequest(ctx, connect.NewRequest(&pb.CloseMergeRequestRequest{Repo: "proj", Number: 1}))
	assert.Equal(t, "failed_precondition", codeName(err))
	_ = mr
}

func TestUpdateRepoSettingsOverTheAPI(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("proj", domain.VisibilityPublic)
	root := h.api(adminToken)
	ctx := h.ctx

	got, err := h.api("").repo.GetRepo(ctx, connect.NewRequest(&pb.GetRepoRequest{Repo: "proj"}))
	require.NoError(t, err)
	assert.Equal(t, []pb.MergeMode{pb.MergeMode_MERGE_MODE_FAST_FORWARD, pb.MergeMode_MERGE_MODE_SQUASH, pb.MergeMode_MERGE_MODE_MERGE_COMMIT}, got.Msg.GetRepo().GetSettings().GetAllowedMergeModes())
	assert.True(t, got.Msg.GetRepo().GetSettings().GetFailingChecksBlock())

	update := func(c *clients, paths []string, s *pb.RepoSettings) (*pb.Repo, error) {
		res, err := c.repo.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{
			Repo: "proj", Settings: s, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
		}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetRepo(), nil
	}
	repo, err := update(root, []string{"settings.allowed_merge_modes"}, &pb.RepoSettings{
		AllowedMergeModes: []pb.MergeMode{pb.MergeMode_MERGE_MODE_SQUASH}, FailingChecksBlock: false,
	})
	require.NoError(t, err)
	assert.Equal(t, []pb.MergeMode{pb.MergeMode_MERGE_MODE_SQUASH}, repo.GetSettings().GetAllowedMergeModes())
	assert.True(t, repo.GetSettings().GetFailingChecksBlock(), "not named in the mask: unchanged")

	repo, err = update(root, []string{"settings.failing_checks_block"}, &pb.RepoSettings{FailingChecksBlock: false})
	require.NoError(t, err)
	assert.False(t, repo.GetSettings().GetFailingChecksBlock())
	assert.Equal(t, []pb.MergeMode{pb.MergeMode_MERGE_MODE_SQUASH}, repo.GetSettings().GetAllowedMergeModes(), "not named in the mask: unchanged")

	repo, err = update(root, []string{"settings"}, &pb.RepoSettings{
		AllowedMergeModes: []pb.MergeMode{pb.MergeMode_MERGE_MODE_SQUASH, pb.MergeMode_MERGE_MODE_FAST_FORWARD, pb.MergeMode_MERGE_MODE_SQUASH}, FailingChecksBlock: true,
	})
	require.NoError(t, err)
	assert.Equal(t, []pb.MergeMode{pb.MergeMode_MERGE_MODE_FAST_FORWARD, pb.MergeMode_MERGE_MODE_SQUASH}, repo.GetSettings().GetAllowedMergeModes(), "canonical order, no duplicates")
	assert.True(t, repo.GetSettings().GetFailingChecksBlock())

	for name, s := range map[string]*pb.RepoSettings{
		"no modes":     {},
		"unspecified":  {AllowedMergeModes: []pb.MergeMode{pb.MergeMode_MERGE_MODE_UNSPECIFIED}},
		"unknown mode": {AllowedMergeModes: []pb.MergeMode{9}},
	} {
		_, err = update(root, []string{"settings.allowed_merge_modes"}, s)
		assert.Equal(t, "invalid_argument", codeName(err), name)
	}
	_, err = update(root, []string{"settings.nope"}, &pb.RepoSettings{})
	assert.Equal(t, "invalid_argument", codeName(err))

	// repo:admin is needed.
	_, err = update(h.api(h.token(scopes(domain.ScopeRepoRead, domain.ScopeMRMerge))), []string{"settings.failing_checks_block"}, &pb.RepoSettings{})
	assert.Equal(t, "permission_denied", codeName(err))
	got, err = root.repo.GetRepo(ctx, connect.NewRequest(&pb.GetRepoRequest{Repo: "proj"}))
	require.NoError(t, err)
	assert.True(t, got.Msg.GetRepo().GetSettings().GetFailingChecksBlock())
}

// AC4 over the API: a mode the settings do not allow is refused.
func TestMergeModeNotAllowedOverTheAPI(t *testing.T) {
	r := newMRRig(t, domain.VisibilityPublic)
	_, err := r.create(r.writer)
	require.NoError(t, err)
	_, err = r.root.repo.UpdateRepo(r.ctx, connect.NewRequest(&pb.UpdateRepoRequest{
		Repo: "proj", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"settings.allowed_merge_modes"}},
		Settings: &pb.RepoSettings{AllowedMergeModes: []pb.MergeMode{pb.MergeMode_MERGE_MODE_FAST_FORWARD}},
	}))
	require.NoError(t, err)
	_, err = r.merger.mr.MergeMergeRequest(r.ctx, connect.NewRequest(&pb.MergeMergeRequestRequest{
		Repo: "proj", Number: 1, Mode: pb.MergeMode_MERGE_MODE_SQUASH, HeadId: r.head,
	}))
	assert.Equal(t, "failed_precondition", codeName(err))
	assert.Equal(t, r.bareGit("proj", "rev-parse", "main"), r.bareGit("proj", "rev-parse", "feat~2"), "main did not move")
}

// delete_source_branch reaches the merge and the response says what happened;
// deleting the default branch is a failed precondition.
func TestMergeDeletesTheSourceOverTheAPI(t *testing.T) {
	r := newMRRig(t, domain.VisibilityPublic)
	_, err := r.create(r.writer)
	require.NoError(t, err)
	res, err := r.merger.mr.MergeMergeRequest(r.ctx, connect.NewRequest(&pb.MergeMergeRequestRequest{
		Repo: "proj", Number: 1, HeadId: r.head, DeleteSourceBranch: true,
	}))
	require.NoError(t, err)
	assert.True(t, res.Msg.GetSourceBranchDeleted(), "requested")
	r.forge.MergeRequests.Wait(r.ctx) // the deletion follows the response
	assert.Empty(t, r.bareGit("proj", "for-each-ref", "refs/heads/feat"))

	// The default branch as a source: refused before anything changes.
	r.bareGit("proj", "branch", "old", "main~1")
	main := r.bareGit("proj", "rev-parse", "main")
	_, err = r.root.mr.CreateMergeRequest(r.ctx, connect.NewRequest(&pb.CreateMergeRequestRequest{
		Repo: "proj", Title: "back", SourceBranch: "main", TargetBranch: "old",
	}))
	require.NoError(t, err)
	_, err = r.merger.mr.MergeMergeRequest(r.ctx, connect.NewRequest(&pb.MergeMergeRequestRequest{
		Repo: "proj", Number: 2, HeadId: main, DeleteSourceBranch: true,
	}))
	assert.Equal(t, "failed_precondition", codeName(err))
	assert.Equal(t, main, r.bareGit("proj", "rev-parse", "main"))
	assert.NotEqual(t, main, r.bareGit("proj", "rev-parse", "old"), "not merged")
}

// The signing key needs no authentication, whatever the credentials, and keeps
// its shape.
func TestGetSigningKey(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	var first *pb.GetSigningKeyResponse
	for _, who := range []string{"anonymous", "root", "bad token"} {
		c := map[string]*clients{
			"anonymous": h.api(""), "root": h.api(adminToken),
			"bad token": newDisposableClients(h.url("/-/api"), "ermite_"+strings.Repeat("x", 43)),
		}[who]
		res, err := c.server.GetSigningKey(h.ctx, connect.NewRequest(&pb.GetSigningKeyRequest{}))
		require.NoError(t, err, who)
		k := res.Msg
		assert.True(t, strings.HasPrefix(k.GetPublicKey(), "ssh-ed25519 "), who)
		assert.True(t, strings.HasPrefix(k.GetFingerprint(), "SHA256:"), who)
		assert.Equal(t, "ermite", k.GetCommitterName())
		assert.Equal(t, "ermite@example.test", k.GetCommitterEmail(), "the host of ERMITE_PUBLIC_URL")
		assert.Equal(t, k.GetCommitterEmail()+" "+k.GetPublicKey(), k.GetAllowedSigners())
		if first == nil {
			first = k
		}
		assert.Equal(t, first.GetPublicKey(), k.GetPublicKey(), "one key per instance")
	}
	// The private seed is nowhere in the logs.
	assert.NotContains(t, h.logs.String(), "seed")
	assert.Contains(t, h.logs.String(), first.GetFingerprint(), "the fingerprint is logged when the key is ready")
}

// AC5: the squash commit verifies with `git verify-commit`, against an
// allowed_signers file built from what the server says about its key.
func TestSquashCommitVerifiesWithGit(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("ssh-keygen not found: CI must have it (package openssh)")
		}
		t.Skip("ssh-keygen not found: install openssh to verify the signed commits")
	}
	r := newMRRig(t, domain.VisibilityPublic)
	_, err := r.create(r.writer)
	require.NoError(t, err)
	merged, err := r.merger.mr.MergeMergeRequest(r.ctx, connect.NewRequest(&pb.MergeMergeRequestRequest{
		Repo: "proj", Number: 1, Mode: pb.MergeMode_MERGE_MODE_SQUASH, HeadId: r.head,
	}))
	require.NoError(t, err)
	id := merged.Msg.GetMergeRequest().GetMergedCommitId()
	require.NotEqual(t, r.head, id)

	key, err := r.anon.server.GetSigningKey(r.ctx, connect.NewRequest(&pb.GetSigningKeyRequest{}))
	require.NoError(t, err)
	signers := filepath.Join(t.TempDir(), "allowed_signers")
	require.NoError(t, os.WriteFile(signers, []byte(key.Msg.GetAllowedSigners()+"\n"), 0o600))
	out, err := r.gitEnv(nil, r.repoDir("proj"), "-c", "gpg.format=ssh", "-c", "gpg.ssh.allowedSignersFile="+signers, "verify-commit", id)
	require.NoError(t, err, out)
	assert.Contains(t, out, "Good \"git\" signature for ermite@example.test")
	assert.Equal(t, "ermite <ermite@example.test>", r.bareGit("proj", "log", "-1", "--format=%cn <%ce>", id))
	assert.Equal(t, "Change a (!1)", r.bareGit("proj", "log", "-1", "--format=%s", id))
}

// A private repository stays invisible in the event stream: merge request
// events reach the callers that may read it, and nobody else.
func TestMergeRequestEventsAreFilteredByVisibility(t *testing.T) {
	r := newMRRig(t, domain.VisibilityPrivate)
	anon := r.watch(r.anon, "", "home")
	reader := r.watch(r.reader, "", "home")
	noScope := r.watch(r.noScope, "", "home")

	_, err := r.create(r.writer)
	require.NoError(t, err)
	_, err = r.writer.mr.AddComment(r.ctx, connect.NewRequest(&pb.AddCommentRequest{Repo: "proj", Number: 1, Text: "hello"}))
	require.NoError(t, err)

	opened := reader.next(t, watchWait)
	require.NotNil(t, opened.GetMergeRequestOpened(), "%v", opened)
	assert.EqualValues(t, 1, opened.GetMergeRequestOpened().GetMergeRequest().GetNumber())
	assert.Equal(t, "proj", opened.GetMergeRequestOpened().GetMergeRequest().GetRepo())
	added := reader.next(t, watchWait).GetCommentAdded()
	require.NotNil(t, added)
	assert.Equal(t, "proj", added.GetRepo())
	assert.EqualValues(t, 1, added.GetNumber())
	assert.Equal(t, "hello", added.GetComment().GetText())

	// Merging publishes merged, then pushed.
	_, err = r.merger.mr.MergeMergeRequest(r.ctx, connect.NewRequest(&pb.MergeMergeRequestRequest{Repo: "proj", Number: 1, HeadId: r.head}))
	require.NoError(t, err)
	assert.NotNil(t, reader.next(t, watchWait).GetMergeRequestMerged())
	pushed := reader.next(t, watchWait).GetPushed()
	require.NotNil(t, pushed)
	assert.Equal(t, "refs/heads/main", pushed.GetRefUpdates()[0].GetRef())
	assert.Equal(t, r.head, pushed.GetRefUpdates()[0].GetNewId())

	// A second request, closed.
	r.mustGit(r.wt, "checkout", "-q", "-b", "two")
	r.writeFile(r.wt, "two.txt", []byte("2\n"))
	r.mustGit(r.wt, "push", "-q", r.urlAuth(adminToken, "/proj.git"), "two")
	reader.next(t, watchWait) // the push of "two"
	_, err = r.writer.mr.CreateMergeRequest(r.ctx, connect.NewRequest(&pb.CreateMergeRequestRequest{Repo: "proj", Title: "two", SourceBranch: "two"}))
	require.NoError(t, err)
	assert.NotNil(t, reader.next(t, watchWait).GetMergeRequestOpened())
	_, err = r.writer.mr.CloseMergeRequest(r.ctx, connect.NewRequest(&pb.CloseMergeRequestRequest{Repo: "proj", Number: 2}))
	require.NoError(t, err)
	closed := reader.next(t, watchWait).GetMergeRequestClosed()
	require.NotNil(t, closed)
	assert.Equal(t, pb.MergeRequestState_MERGE_REQUEST_STATE_CLOSED, closed.GetMergeRequest().GetState())

	// The anonymous and unscoped watchers saw none of it: the sentinel, a
	// public repository, is the first thing they get.
	r.sentinel(t, anon, "s-anon")
	r.sentinel2(t, noScope, "s-none")
}

// sentinel2 checks the next event of a watcher is the creation of a public
// repository, which the sentinel call before it already made.
func (r *mrRig) sentinel2(t *testing.T, s *stream, name string) {
	t.Helper()
	ev := s.next(t, watchWait)
	assert.Equal(t, "s-anon", ev.GetRepoCreated().GetRepo().GetName(), "an event came before the sentinel %s: %v", name, ev)
}

// A topic of a private repository is refused to those who cannot read it, so
// merge request events cannot be followed that way either.
func TestPrivateRepositoryTopicIsNotFound(t *testing.T) {
	r := newMRRig(t, domain.VisibilityPrivate)
	assert.Equal(t, "not_found", codeName(watchErr(r.ctx, r.anon, "repo:proj")))
	assert.Equal(t, "not_found", codeName(watchErr(r.ctx, r.noScope, "repo:proj")))
}
