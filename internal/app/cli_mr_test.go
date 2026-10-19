// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"bnema.dev/ermite-api/gen/ermite"
)

var (
	headA = strings.Repeat("a", 40)
	mrOne = &ermite.MergeRequest{
		Repo: "alpha", Number: 1, Title: "Add search", Description: "Finds things.", SourceBranch: "feat/x", TargetBranch: "main",
		State: ermite.MergeRequestState_MERGE_REQUEST_STATE_OPEN, Author: "agent-7",
		CreatedAt: ts("2026-10-01T10:00:00Z"), UpdatedAt: ts("2026-10-02T11:30:00Z"), HeadId: headA,
	}
)

func TestMRCreateMapsFlags(t *testing.T) {
	f := &fakeAPI{createMR: func(*ermite.CreateMergeRequestRequest) (*ermite.CreateMergeRequestResponse, error) {
		return &ermite.CreateMergeRequestResponse{MergeRequest: mrOne}, nil
	}}
	url := f.serve(t)
	res := runCLI(t, envFor(url), "", "mr", "create", "alpha", "--title", "Add search", "--description", "Finds things.",
		"--source", "refs/heads/feat/x", "--target", " main ").ok(t)
	assert.Equal(t, "opened alpha!1 (feat/x into main)\n", res.stdout)
	_, reqs := f.snapshot()
	got := reqs[0].(*ermite.CreateMergeRequestRequest)
	assert.Equal(t, "alpha", got.GetRepo())
	assert.Equal(t, "Add search", got.GetTitle())
	assert.Equal(t, "Finds things.", got.GetDescription())
	assert.Equal(t, "feat/x", got.GetSourceBranch(), "a full ref name is shortened")
	assert.Equal(t, "main", got.GetTargetBranch())

	// The target defaults to nothing, which is the default branch for the server;
	// the description comes from stdin with -.
	res = runCLI(t, envFor(url), "line one\nline two\n", "mr", "create", "alpha", "--title", "t", "--source", "feat/x", "--description", "-").ok(t)
	_, reqs = f.snapshot()
	got = reqs[1].(*ermite.CreateMergeRequestRequest)
	assert.Empty(t, got.GetTargetBranch())
	assert.Equal(t, "line one\nline two", got.GetDescription())
	_ = res

	res = runCLI(t, envFor(url), "", "mr", "create", "alpha", "--title", "t", "--source", "x", "--json").ok(t)
	assert.JSONEq(t, `{"repo":"alpha","number":1,"title":"Add search","description":"Finds things.","source_branch":"feat/x","target_branch":"main","state":"open","author":"agent-7","created_at":"2026-10-01T10:00:00Z","updated_at":"2026-10-02T11:30:00Z","head_id":"`+headA+`","merged_commit_id":"","merge_mode":""}`, res.stdout)

	for _, args := range [][]string{{"mr", "create", "alpha", "--source", "x"}, {"mr", "create", "alpha", "--title", "t"}, {"mr", "create", "--title", "t", "--source", "x"}} {
		require.Error(t, runCLI(t, envFor(url), "", args...).err, "%v", args)
	}
}

func TestMRListTableJSONAndStates(t *testing.T) {
	merged := &ermite.MergeRequest{
		Repo: "alpha", Number: 2, Title: "Fix\tthe \x1b[31mbug", SourceBranch: "fix", TargetBranch: "main",
		State: ermite.MergeRequestState_MERGE_REQUEST_STATE_MERGED, Author: "root", UpdatedAt: ts("2026-10-03T00:00:00Z"),
		MergedCommitId: headA, MergeMode: ermite.MergeMode_MERGE_MODE_SQUASH,
	}
	f := &fakeAPI{listMR: func(r *ermite.ListMergeRequestsRequest) (*ermite.ListMergeRequestsResponse, error) {
		return &ermite.ListMergeRequestsResponse{MergeRequests: []*ermite.MergeRequest{merged, mrOne}}, nil
	}}
	url := f.serve(t)
	res := runCLI(t, envFor(url), "", "mr", "list", "alpha", "--state", "all").ok(t)
	assert.Equal(t, ""+
		"NUMBER  STATE   BRANCHES        AUTHOR   UPDATED           TITLE\n"+
		"!2      merged  fix -> main     root     2026-10-03 00:00  Fix the ?[31mbug\n"+
		"!1      open    feat/x -> main  agent-7  2026-10-02 11:30  Add search\n", res.stdout)
	assert.NotContains(t, res.stdout, "\x1b", "server text is made safe for a terminal")

	_, reqs := f.snapshot()
	assert.Equal(t, ermite.MergeRequestState_MERGE_REQUEST_STATE_UNSPECIFIED, reqs[0].(*ermite.ListMergeRequestsRequest).GetState(), "all")
	runCLI(t, envFor(url), "", "mr", "list", "alpha").ok(t)
	_, reqs = f.snapshot()
	assert.Equal(t, ermite.MergeRequestState_MERGE_REQUEST_STATE_OPEN, reqs[1].(*ermite.ListMergeRequestsRequest).GetState(), "open by default")
	runCLI(t, envFor(url), "", "mr", "list", "alpha", "--state", "closed").ok(t)
	_, reqs = f.snapshot()
	assert.Equal(t, ermite.MergeRequestState_MERGE_REQUEST_STATE_CLOSED, reqs[2].(*ermite.ListMergeRequestsRequest).GetState())

	res = runCLI(t, envFor(url), "", "mr", "list", "alpha", "--json").ok(t)
	assert.Equal(t, []string{"alpha", "alpha"}, []string{decode[[]mrOut](t, res.stdout)[0].Repo, decode[[]mrOut](t, res.stdout)[1].Repo})
	assert.Equal(t, "squash", decode[[]mrOut](t, res.stdout)[0].MergeMode)
}

func TestMRListFollowsPages(t *testing.T) {
	f := &fakeAPI{listMR: func(r *ermite.ListMergeRequestsRequest) (*ermite.ListMergeRequestsResponse, error) {
		if r.GetPageToken() == "" {
			return &ermite.ListMergeRequestsResponse{MergeRequests: []*ermite.MergeRequest{mrOne}, NextPageToken: "p2"}, nil
		}
		m := proto.Clone(mrOne).(*ermite.MergeRequest)
		m.Number = 7
		return &ermite.ListMergeRequestsResponse{MergeRequests: []*ermite.MergeRequest{m}}, nil
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "mr", "list", "alpha", "--json").ok(t)
	got := decode[[]mrOut](t, res.stdout)
	require.Len(t, got, 2)
	assert.EqualValues(t, 7, got[1].Number)
}

func TestMRShow(t *testing.T) {
	other := &ermite.Signature{Name: "Bob", Email: "bob@example.org"}
	detail := &ermite.GetMergeRequestResponse{
		MergeRequest: mrOne,
		Commits:      []*ermite.Commit{{Id: headA, Author: other, Summary: "add search"}},
		DiffBaseId:   strings.Repeat("b", 40),
		Stats:        &ermite.DiffStats{FilesChanged: 1, Additions: 2, Deletions: 1},
		Diff: &ermite.Diff{Files: []*ermite.FileDiff{{
			OldPath: "a.txt", NewPath: "a.txt", Status: ermite.FileStatus_FILE_STATUS_MODIFIED, Additions: 2, Deletions: 1,
			Hunks: []*ermite.Hunk{{OldStart: 4, OldLines: 2, NewStart: 4, NewLines: 3, Header: "func f()", Lines: []*ermite.Line{
				{Kind: ermite.LineKind_LINE_KIND_CONTEXT, OldNumber: 4, NewNumber: 4, Text: "keep"},
				{Kind: ermite.LineKind_LINE_KIND_DELETION, OldNumber: 5, Text: "old"},
				{Kind: ermite.LineKind_LINE_KIND_ADDITION, NewNumber: 5, Text: "new \x1b[0m"},
				{Kind: ermite.LineKind_LINE_KIND_ADDITION, NewNumber: 6, Text: "more"},
			}}},
		}}},
		Comments: []*ermite.Comment{
			{Id: "1", Author: "agent-7", Text: "general\nnote", CreatedAt: ts("2026-10-02T09:00:00Z")},
			{Id: "2", Author: "root", Path: "a.txt", Line: 5, Side: ermite.DiffSide_DIFF_SIDE_NEW, Text: "why?", Resolved: true, CreatedAt: ts("2026-10-02T10:00:00Z")},
		},
		LastReadId: strings.Repeat("c", 40),
	}
	f := &fakeAPI{getMR: func(*ermite.GetMergeRequestRequest) (*ermite.GetMergeRequestResponse, error) { return detail, nil }}
	url := f.serve(t)

	res := runCLI(t, envFor(url), "", "mr", "show", "alpha", "!1", "--diff").ok(t)
	out := res.stdout
	assert.Contains(t, out, "merge request  alpha!1")
	assert.Contains(t, out, "branches       feat/x -> main")
	assert.Contains(t, out, "commits (1)")
	assert.Contains(t, out, "changes: 1 file(s), +2 -1, from bbbbbbbbbb")
	assert.Contains(t, out, "modified +2 -1  a.txt")
	assert.Contains(t, out, "  #1 agent-7")
	assert.Contains(t, out, "\n    note\n", "comment lines are indented")
	assert.Contains(t, out, "a.txt:5 (new) [resolved]")
	assert.Contains(t, out, "\n          5 +new ?[0m\n", "an addition has a new number only")
	assert.Contains(t, out, "\n    5       -old\n", "a deletion has an old number only")
	assert.Contains(t, out, "\n    4     4  keep\n", "a context line has both")
	assert.NotContains(t, out, "\x1b")
	assert.Empty(t, res.stderr)
	_, reqs := f.snapshot()
	assert.EqualValues(t, 1, reqs[0].(*ermite.GetMergeRequestRequest).GetNumber())
	assert.False(t, reqs[0].(*ermite.GetMergeRequestRequest).GetSinceLastRead())

	// Without --diff the lines are left out of the human and the JSON output.
	res = runCLI(t, envFor(url), "", "mr", "show", "alpha", "1").ok(t)
	assert.NotContains(t, res.stdout, "+new")
	js := runCLI(t, envFor(url), "", "mr", "show", "alpha", "1", "--json").ok(t)
	assert.NotContains(t, js.stdout, `"hunks"`)
	js = runCLI(t, envFor(url), "", "mr", "show", "alpha", "1", "--json", "--diff").ok(t)
	show := decode[showOut](t, js.stdout)
	require.Len(t, show.Files, 1)
	require.Len(t, show.Files[0].Hunks, 1)
	assert.Equal(t, "addition", show.Files[0].Hunks[0].Lines[2].Kind)
	assert.Equal(t, 5, show.Files[0].Hunks[0].Lines[2].NewNumber)
	require.Len(t, show.Comments, 2)
	assert.Equal(t, "new", show.Comments[1].Side)
	assert.Empty(t, show.Comments[0].Side)
	assert.Equal(t, strings.Repeat("c", 40), show.LastReadID)

	// --since-last-read asks for it, and says when the head is lost.
	detail.LastReadLost = true
	res = runCLI(t, envFor(url), "", "mr", "show", "alpha", "1", "--since-last-read").ok(t)
	_, reqs = f.snapshot()
	assert.True(t, reqs[len(reqs)-1].(*ermite.GetMergeRequestRequest).GetSinceLastRead())
	assert.Contains(t, res.stderr, "head you last read is gone")
}

func TestMRShowMarkRead(t *testing.T) {
	f := &fakeAPI{
		getMR: func(*ermite.GetMergeRequestRequest) (*ermite.GetMergeRequestResponse, error) {
			return &ermite.GetMergeRequestResponse{MergeRequest: mrOne}, nil
		},
		markRead: func(*ermite.MarkReadRequest) (*ermite.MarkReadResponse, error) {
			return &ermite.MarkReadResponse{}, nil
		},
	}
	url := f.serve(t)
	runCLI(t, envFor(url), "", "mr", "show", "alpha", "1").ok(t)
	calls, _ := f.snapshot()
	assert.Equal(t, []string{"GetMergeRequest"}, calls, "reading does not mark read")
	runCLI(t, envFor(url), "", "mr", "show", "alpha", "1", "--mark-read").ok(t)
	calls, reqs := f.snapshot()
	assert.Equal(t, []string{"GetMergeRequest", "GetMergeRequest", "MarkRead"}, calls)
	assert.Equal(t, headA, reqs[2].(*ermite.MarkReadRequest).GetHeadId(), "marks the head that was shown")
}

func TestMRCommentMapsFlags(t *testing.T) {
	f := &fakeAPI{addComment: func(r *ermite.AddCommentRequest) (*ermite.AddCommentResponse, error) {
		return &ermite.AddCommentResponse{Comment: &ermite.Comment{Id: "3", Path: r.GetPath(), Line: r.GetLine(), Side: r.GetSide(), Text: r.GetText()}}, nil
	}}
	url := f.serve(t)

	res := runCLI(t, envFor(url), "", "mr", "comment", "alpha", "1", "-m", "hello").ok(t)
	assert.Equal(t, "comment 3 added to alpha!1\n", res.stdout)
	runCLI(t, envFor(url), "", "mr", "comment", "alpha", "1", "-m", "on a line", "--path", "a.txt", "--line", "5").ok(t)
	runCLI(t, envFor(url), "", "mr", "comment", "alpha", "1", "-m", "old side", "--path", "a.txt", "--line", "5", "--side", "OLD").ok(t)
	runCLI(t, envFor(url), "text\n", "mr", "comment", "alpha", "!1", "-m", "-").ok(t)
	_, reqs := f.snapshot()
	general := reqs[0].(*ermite.AddCommentRequest)
	assert.Empty(t, general.GetPath())
	assert.Zero(t, general.GetLine())
	assert.Equal(t, ermite.DiffSide_DIFF_SIDE_UNSPECIFIED, general.GetSide(), "a general comment has no side, though --side defaults to new")
	onNew := reqs[1].(*ermite.AddCommentRequest)
	assert.Equal(t, "a.txt", onNew.GetPath())
	assert.EqualValues(t, 5, onNew.GetLine())
	assert.Equal(t, ermite.DiffSide_DIFF_SIDE_NEW, onNew.GetSide(), "new is the default side")
	assert.Equal(t, ermite.DiffSide_DIFF_SIDE_OLD, reqs[2].(*ermite.AddCommentRequest).GetSide())
	assert.Equal(t, "text", reqs[3].(*ermite.AddCommentRequest).GetText())
	assert.EqualValues(t, 1, reqs[3].(*ermite.AddCommentRequest).GetNumber())

	before, _ := f.snapshot()
	for _, args := range [][]string{
		{"mr", "comment", "alpha", "1"},                                                            // no -m
		{"mr", "comment", "alpha", "x", "-m", "t"},                                                 // bad number
		{"mr", "comment", "alpha", "0", "-m", "t"},                                                 // bad number
		{"mr", "comment", "alpha", "1", "-m", "t", "--path", "a"},                                  // no line
		{"mr", "comment", "alpha", "1", "-m", "t", "--line", "3"},                                  // no path
		{"mr", "comment", "alpha", "1", "-m", "t", "--side", "old"},                                // no path
		{"mr", "comment", "alpha", "1", "-m", "t", "--path", "a", "--line", "1", "--side", "left"}, // bad side
	} {
		require.Error(t, runCLI(t, envFor(url), "", args...).err, "%v", args)
	}
	after, _ := f.snapshot()
	assert.Equal(t, before, after, "nothing reaches the server")
}

func TestMRResolveMergeAndClose(t *testing.T) {
	merged := proto.Clone(mrOne).(*ermite.MergeRequest)
	merged.State = ermite.MergeRequestState_MERGE_REQUEST_STATE_MERGED
	merged.MergedCommitId, merged.MergeMode = strings.Repeat("d", 40), ermite.MergeMode_MERGE_MODE_SQUASH
	closed := proto.Clone(mrOne).(*ermite.MergeRequest)
	closed.State = ermite.MergeRequestState_MERGE_REQUEST_STATE_CLOSED
	f := &fakeAPI{
		resolveCmt: func(r *ermite.ResolveCommentRequest) (*ermite.ResolveCommentResponse, error) {
			return &ermite.ResolveCommentResponse{Comment: &ermite.Comment{Id: r.GetCommentId(), Resolved: r.GetResolved()}}, nil
		},
		getMR: func(*ermite.GetMergeRequestRequest) (*ermite.GetMergeRequestResponse, error) {
			return &ermite.GetMergeRequestResponse{MergeRequest: mrOne}, nil
		},
		mergeMR: func(*ermite.MergeMergeRequestRequest) (*ermite.MergeMergeRequestResponse, error) {
			return &ermite.MergeMergeRequestResponse{MergeRequest: merged}, nil
		},
		closeMR: func(*ermite.CloseMergeRequestRequest) (*ermite.CloseMergeRequestResponse, error) {
			return &ermite.CloseMergeRequestResponse{MergeRequest: closed}, nil
		},
	}
	url := f.serve(t)

	res := runCLI(t, envFor(url), "", "mr", "resolve", "alpha", "1", "#4").ok(t)
	assert.Equal(t, "comment 4 of alpha!1 resolved\n", res.stdout)
	res = runCLI(t, envFor(url), "", "mr", "resolve", "alpha", "1", "4", "--undo").ok(t)
	assert.Equal(t, "comment 4 of alpha!1 reopened\n", res.stdout)
	_, reqs := f.snapshot()
	assert.Equal(t, "4", reqs[0].(*ermite.ResolveCommentRequest).GetCommentId(), "a leading # is dropped")
	assert.True(t, reqs[0].(*ermite.ResolveCommentRequest).GetResolved())
	assert.False(t, reqs[1].(*ermite.ResolveCommentRequest).GetResolved())

	// Merge with an explicit head and mode.
	res = runCLI(t, envFor(url), "", "mr", "merge", "alpha", "1", "--squash", "--head", headA).ok(t)
	assert.Equal(t, "merged alpha!1 into main (squash) at dddddddddd\n", res.stdout)
	assert.Empty(t, res.stderr)
	calls, reqs := f.snapshot()
	assert.Equal(t, []string{"ResolveComment", "ResolveComment", "MergeMergeRequest"}, calls, "an explicit head needs no read first")
	m := reqs[2].(*ermite.MergeMergeRequestRequest)
	assert.Equal(t, ermite.MergeMode_MERGE_MODE_SQUASH, m.GetMode())
	assert.Equal(t, headA, m.GetHeadId())
	assert.EqualValues(t, 1, m.GetNumber())

	// The default mode is a fast-forward.
	res = runCLI(t, envFor(url), "", "mr", "merge", "alpha", "1", "--head", headA, "--json").ok(t)
	assert.Equal(t, "merged", decode[mrOut](t, res.stdout).State)
	calls, reqs = f.snapshot()
	assert.Equal(t, "MergeMergeRequest", calls[len(calls)-1])
	m = reqs[len(reqs)-1].(*ermite.MergeMergeRequestRequest)
	assert.Equal(t, ermite.MergeMode_MERGE_MODE_FAST_FORWARD, m.GetMode(), "fast-forward by default")
	assert.Equal(t, headA, m.GetHeadId())

	res = runCLI(t, envFor(url), "", "mr", "close", "alpha", "1").ok(t)
	assert.Equal(t, "closed alpha!1 (feat/x into main)\n", res.stdout)
	res = runCLI(t, envFor(url), "", "mr", "close", "alpha", "1", "--json").ok(t)
	assert.Equal(t, "closed", decode[mrOut](t, res.stdout).State)
}

// ADR 0017: the merge is of the commit that was reviewed, so --head is
// required, and the CLI does not fill it in from the live head.
func TestMRMergeNeedsTheHead(t *testing.T) {
	f := &fakeAPI{}
	url := f.serve(t)
	for _, args := range [][]string{
		{"mr", "merge", "alpha", "1"},
		{"mr", "merge", "alpha", "1", "--squash"},
		{"mr", "merge", "alpha", "1", "--head", "  "},
	} {
		res := runCLI(t, envFor(url), "", args...)
		require.Error(t, res.err, args)
		assert.Contains(t, res.err.Error(), "--head is required")
		assert.Contains(t, res.err.Error(), "ermite mr show alpha 1", "tells where to get the head")
	}
	calls, _ := f.snapshot()
	assert.Empty(t, calls, "nothing reaches the server")

	res := runCLI(t, envFor(url), "", "mr", "merge", "--help").ok(t)
	assert.Contains(t, res.stdout, "(required)")
	assert.NotContains(t, res.stdout, "default: the current head")
}

func TestMRMergeDeleteBranch(t *testing.T) {
	merged := proto.Clone(mrOne).(*ermite.MergeRequest)
	merged.State = ermite.MergeRequestState_MERGE_REQUEST_STATE_MERGED
	merged.MergedCommitId, merged.MergeMode = headA, ermite.MergeMode_MERGE_MODE_FAST_FORWARD
	deleted := true
	f := &fakeAPI{mergeMR: func(*ermite.MergeMergeRequestRequest) (*ermite.MergeMergeRequestResponse, error) {
		return &ermite.MergeMergeRequestResponse{MergeRequest: merged, SourceBranchDeleted: deleted}, nil
	}}
	url := f.serve(t)

	res := runCLI(t, envFor(url), "", "mr", "merge", "alpha", "1", "--head", headA, "--delete-branch").ok(t)
	assert.Equal(t, "merged alpha!1 into main (fast-forward) at "+headA[:10]+"\ndeleted branch feat/x\n", res.stdout)
	assert.Empty(t, res.stderr)
	_, reqs := f.snapshot()
	assert.True(t, reqs[0].(*ermite.MergeMergeRequestRequest).GetDeleteSourceBranch())

	res = runCLI(t, envFor(url), "", "mr", "merge", "alpha", "1", "--head", headA, "--delete-branch", "--json").ok(t)
	assert.Contains(t, res.stdout, `"source_branch_deleted": true`)

	// The merge stands when the branch is kept: a warning, no failure.
	deleted = false
	res = runCLI(t, envFor(url), "", "mr", "merge", "alpha", "1", "--head", headA, "--delete-branch").ok(t)
	assert.Equal(t, "merged alpha!1 into main (fast-forward) at "+headA[:10]+"\n", res.stdout)
	assert.Contains(t, res.stderr, "ermite: warning: the branch feat/x was kept")

	// Without the flag nothing is asked and nothing is said.
	res = runCLI(t, envFor(url), "", "mr", "merge", "alpha", "1", "--head", headA).ok(t)
	assert.Empty(t, res.stderr)
	_, reqs = f.snapshot()
	assert.False(t, reqs[len(reqs)-1].(*ermite.MergeMergeRequestRequest).GetDeleteSourceBranch())
}

func TestMRErrorsAreFriendly(t *testing.T) {
	f := &fakeAPI{
		listMR: func(*ermite.ListMergeRequestsRequest) (*ermite.ListMergeRequestsResponse, error) {
			return nil, connect.NewError(connect.CodeUnimplemented, nil)
		},
		mergeMR: func(*ermite.MergeMergeRequestRequest) (*ermite.MergeMergeRequestResponse, error) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, assert.AnError)
		},
	}
	url := f.serve(t)
	res := runCLI(t, envFor(url), "", "mr", "list", "alpha")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "does not implement")
	res = runCLI(t, envFor(url), "", "mr", "merge", "alpha", "1", "--head", headA)
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "precondition failed")
}

func TestSigningKeyShowOutput(t *testing.T) {
	key := &ermite.GetSigningKeyResponse{
		PublicKey: "ssh-ed25519 AAAAC3Nz", Fingerprint: "SHA256:abc", CommitterName: "ermite", CommitterEmail: "ermite@git.example.org",
		AllowedSigners: "ermite@git.example.org ssh-ed25519 AAAAC3Nz",
	}
	f := &fakeAPI{getSigningKey: func(*ermite.GetSigningKeyRequest) (*ermite.GetSigningKeyResponse, error) { return key, nil }}
	url := f.serve(t)

	res := runCLI(t, map[string]string{"ERMITE_URL": url}, "", "signing-key", "show").ok(t)
	assert.Equal(t, ""+
		"fingerprint      SHA256:abc\n"+
		"committer        ermite <ermite@git.example.org>\n"+
		"public key       ssh-ed25519 AAAAC3Nz\n"+
		"allowed signers  ermite@git.example.org ssh-ed25519 AAAAC3Nz\n", res.stdout)
	assert.Empty(t, f.header[0].Values("Authorization"), "no token is needed")
	res = runCLI(t, envFor(url), "", "signing-key", "show", "--public-key").ok(t)
	assert.Equal(t, "ssh-ed25519 AAAAC3Nz\n", res.stdout)
	res = runCLI(t, envFor(url), "", "signing-key", "show", "--allowed-signers").ok(t)
	assert.Equal(t, "ermite@git.example.org ssh-ed25519 AAAAC3Nz\n", res.stdout)
	res = runCLI(t, envFor(url), "", "signing-key", "show", "--json").ok(t)
	assert.JSONEq(t, `{"public_key":"ssh-ed25519 AAAAC3Nz","fingerprint":"SHA256:abc","committer_name":"ermite","committer_email":"ermite@git.example.org","allowed_signers":"ermite@git.example.org ssh-ed25519 AAAAC3Nz"}`, res.stdout)
}

func TestNoteRangesAndIDs(t *testing.T) {
	for _, tc := range []struct {
		input      string
		start, end int32
	}{{"1", 0, 1}, {"10-24", 10, 24}, {"2147483647", 0, 2147483647}} {
		start, end, err := parseLineRange(tc.input)
		require.NoError(t, err)
		assert.Equal(t, tc.start, start)
		assert.Equal(t, tc.end, end)
	}
	for _, input := range []string{"", "0", "-1", "1-1", "24-10", "0-2", "1-2-3", "2147483648", "a", "1-"} {
		_, _, err := parseLineRange(input)
		require.Error(t, err, input)
	}
	for _, input := range []string{"12", "^12", "#12"} {
		got, err := parseNoteID(input)
		require.NoError(t, err)
		assert.Equal(t, "12", got)
	}
}

func TestNoteFlags(t *testing.T) {
	f := &fakeAPI{addComment: func(r *ermite.AddCommentRequest) (*ermite.AddCommentResponse, error) {
		return &ermite.AddCommentResponse{Comment: &ermite.Comment{Id: "1", ReplyTo: r.GetReplyTo(), StartLine: r.GetStartLine(), Line: r.GetLine(), Verdict: r.GetVerdict()}}, nil
	}}
	url := f.serve(t)
	for _, tc := range []struct {
		flags   []string
		reply   string
		start   int32
		verdict string
	}{
		{[]string{"--reply", "^12", "-m", "reply"}, "12", 0, ""},
		{[]string{"--path", "x", "--line", "10-24", "-m", "range"}, "", 10, ""},
		{[]string{"--approve"}, "", 0, "approve"},
		{[]string{"--request-changes"}, "", 0, "changes_requested"},
	} {
		res := runCLI(t, envFor(url), "", append([]string{"mr", "comment", "alpha", "1", "--json"}, tc.flags...)...).ok(t)
		got := decode[jsonComment](t, res.stdout)
		assert.Equal(t, tc.reply, got.ReplyTo)
		assert.Equal(t, tc.start, got.StartLine)
		assert.Equal(t, tc.verdict, got.Verdict)
	}
	for _, flags := range [][]string{{"--approve", "--request-changes"}, {"--reply", "1", "--path", "x"}, {"--reply", "1", "--line", "2"}, {"--reply", "1", "--side", "new"}, {"--reply", "1", "--approve"}, {"--reply", "1", "--request-changes"}} {
		require.Error(t, runCLI(t, envFor(url), "", append([]string{"mr", "comment", "alpha", "1", "-m", "x"}, flags...)...).err)
	}
}

func TestNoteDetailOutput(t *testing.T) {
	d := &ermite.GetMergeRequestResponse{MergeRequest: mrOne, Comments: []*ermite.Comment{
		{Id: "1", Path: "x", StartLine: 10, Line: 24, Text: "root"},
		{Id: "3", Text: "other", Deleted: true},
		{Id: "2", ReplyTo: "1", Text: "reply", EditedAt: ts("2026-10-10T12:00:00Z")},
	}, Verdicts: []*ermite.AuthorVerdict{
		{Author: "owner", Verdict: ermite.Verdict_VERDICT_APPROVE, CommentId: "1", Stale: true},
		{Author: "bot", Verdict: ermite.Verdict_VERDICT_CHANGES_REQUESTED, CommentId: "3"},
	}}
	var out strings.Builder
	require.NoError(t, printMRDetail(&out, d, false, false))
	text := out.String()
	assert.Contains(t, text, "owner approved (stale), bot changes requested")
	assert.Contains(t, text, "lines 10-24")
	assert.Contains(t, text, "(edited)")
	assert.Contains(t, text, "[deleted]")
	assert.Less(t, strings.Index(text, "      #2"), strings.Index(text, "  #3"))
	got := toJSONDetail(d, false)
	require.Len(t, got.Verdicts, 2)
	assert.True(t, got.Verdicts[0].Stale)
	assert.Equal(t, "changes_requested", got.Verdicts[1].Verdict)
	assert.Equal(t, "1", got.Comments[2].ReplyTo)
	assert.NotNil(t, got.Comments[2].EditedAt)
}

func TestUnknownVerdictDetailOutput(t *testing.T) {
	var out strings.Builder
	require.NoError(t, printMRDetail(&out, &ermite.GetMergeRequestResponse{MergeRequest: mrOne, Verdicts: []*ermite.AuthorVerdict{{Author: "future", Verdict: ermite.Verdict(99)}}}, false, false))
	assert.Contains(t, out.String(), "future verdict 99")
	assert.NotContains(t, out.String(), "changes requested")
	assert.NotContains(t, out.String(), "approved")
}

func TestMRReviewScopeCompletion(t *testing.T) {
	cmd := newTokenCreateCmd(&cliEnv{})
	complete, ok := cmd.GetFlagCompletionFunc("scope")
	require.True(t, ok)
	values, _ := complete(cmd, nil, "")
	assert.Contains(t, values, "mr:review")
}
