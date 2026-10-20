// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-actions/workflow"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

func parsePR(t *testing.T, on string) *workflow.Workflow {
	t.Helper()
	wf, errs := workflow.Parse("pr.yml", []byte("on:\n"+on+"\njobs:\n  j:\n    runs-on: x\n    steps:\n      - run: 'true'\n"))
	require.Empty(t, errs)
	return wf
}

func TestMatchPullRequestTypes(t *testing.T) {
	def := parsePR(t, "  pull_request:\n")
	for action, want := range map[string]bool{"opened": true, "synchronize": true, "reopened": true, "closed": false} {
		assert.Equal(t, want, usecase.MatchPullRequest(def, action, "main", nil, false), "default types, "+action)
	}
	closed := parsePR(t, "  pull_request:\n    types: [closed]\n")
	assert.True(t, usecase.MatchPullRequest(closed, "closed", "main", nil, false))
	assert.False(t, usecase.MatchPullRequest(closed, "opened", "main", nil, false))
	both := parsePR(t, "  pull_request:\n    types: [opened, closed]\n")
	assert.True(t, usecase.MatchPullRequest(both, "opened", "main", nil, false))
	assert.True(t, usecase.MatchPullRequest(both, "closed", "main", nil, false))
	assert.False(t, usecase.MatchPullRequest(both, "synchronize", "main", nil, false))
	assert.False(t, usecase.MatchPullRequest(parsePR(t, "  push:\n"), "opened", "main", nil, false), "no pull_request event")
	assert.True(t, usecase.MatchPullRequest(parsePR(t, "  [push, pull_request]\n"), "opened", "main", nil, false))
}

// CI-4 D4: pull_request_target has the types and the filters of pull_request,
// under its own name; types ermite never emits parse and never match.
func TestMatchPullRequestTarget(t *testing.T) {
	def := parsePR(t, "  pull_request_target:\n")
	for action, want := range map[string]bool{"opened": true, "synchronize": true, "reopened": true, "closed": false} {
		assert.Equal(t, want, usecase.MatchPullRequestTarget(def, action, "main", nil, false), "default types, "+action)
	}
	assert.False(t, usecase.MatchPullRequest(def, "opened", "main", nil, false), "not a pull_request workflow")
	assert.False(t, usecase.MatchPullRequestTarget(parsePR(t, "  pull_request:\n"), "opened", "main", nil, false))
	labeled := parsePR(t, "  pull_request_target:\n    types: [labeled, closed]\n")
	assert.False(t, usecase.MatchPullRequestTarget(labeled, "opened", "main", nil, false))
	assert.True(t, usecase.MatchPullRequestTarget(labeled, "closed", "main", nil, false))

	wf := parsePR(t, "  pull_request_target:\n    branches: [main]\n    paths: ['src/**']\n")
	assert.True(t, usecase.MatchPullRequestTarget(wf, "opened", "main", []string{"src/a.go"}, false))
	assert.False(t, usecase.MatchPullRequestTarget(wf, "opened", "dev", []string{"src/a.go"}, false), "the base branch is filtered")
	assert.False(t, usecase.MatchPullRequestTarget(wf, "opened", "main", []string{"README.md"}, false), "the changed files are filtered")
	ignore := parsePR(t, "  pull_request_target:\n    branches-ignore: [wip]\n")
	assert.False(t, usecase.MatchPullRequestTarget(ignore, "opened", "wip", nil, false))
}

func TestMatchPullRequestFilters(t *testing.T) {
	wf := parsePR(t, "  pull_request:\n    branches: [main, 'release/**']\n    paths: ['src/**', '!src/gen/**']\n")
	files := func(f ...string) []string { return f }
	assert.True(t, usecase.MatchPullRequest(wf, "opened", "main", files("src/a.go"), false))
	assert.True(t, usecase.MatchPullRequest(wf, "opened", "release/1.2", files("README.md", "src/b.go"), false))
	assert.False(t, usecase.MatchPullRequest(wf, "opened", "dev", files("src/a.go"), false), "the base branch is filtered")
	assert.False(t, usecase.MatchPullRequest(wf, "opened", "main", files("README.md"), false), "the changed files are filtered")
	assert.False(t, usecase.MatchPullRequest(wf, "opened", "main", files("src/gen/x.go"), false))
	assert.True(t, usecase.MatchPullRequest(wf, "opened", "main", nil, true), "an unavailable list passes the paths filter")

	ignore := parsePR(t, "  pull_request:\n    branches-ignore: [wip]\n    paths-ignore: ['docs/**']\n")
	assert.True(t, usecase.MatchPullRequest(ignore, "synchronize", "main", files("docs/a.md", "x.go"), false))
	assert.False(t, usecase.MatchPullRequest(ignore, "synchronize", "main", files("docs/a.md"), false))
	assert.False(t, usecase.MatchPullRequest(ignore, "synchronize", "wip", files("x.go"), false))
}

func TestPullFilesLink(t *testing.T) {
	const base = "http://x/pulls/1/files"
	assert.Empty(t, usecase.PullFilesLink(base, 1, 30, 0))
	assert.Empty(t, usecase.PullFilesLink(base, 1, 30, 30))
	assert.Equal(t, `<http://x/pulls/1/files?per_page=30&page=2>; rel="next", <http://x/pulls/1/files?per_page=30&page=3>; rel="last"`, usecase.PullFilesLink(base, 1, 30, 61))
	assert.Equal(t,
		`<http://x/pulls/1/files?per_page=30&page=3>; rel="next", <http://x/pulls/1/files?per_page=30&page=3>; rel="last", <http://x/pulls/1/files?per_page=30&page=1>; rel="first", <http://x/pulls/1/files?per_page=30&page=1>; rel="prev"`,
		usecase.PullFilesLink(base, 2, 30, 61))
	last := usecase.PullFilesLink(base, 3, 30, 61)
	assert.NotContains(t, last, "next")
	assert.Contains(t, last, `page=2>; rel="prev"`)
	beyond := usecase.PullFilesLink(base, 9, 30, 61)
	assert.NotContains(t, beyond, "next")
	assert.Contains(t, beyond, `page=3>; rel="prev"`)
}

func TestFilePatch(t *testing.T) {
	f := domain.FileDiff{Hunks: []domain.Hunk{
		{OldStart: 1, OldLines: 2, NewStart: 1, NewLines: 2, Header: "func x()", Lines: []domain.Line{
			{Kind: domain.LineContext, Text: "a"}, {Kind: domain.LineDeletion, Text: "b"}, {Kind: domain.LineAddition, Text: "c", NoNewlineAtEOF: true},
		}},
		{OldStart: 9, OldLines: 1, NewStart: 9, NewLines: 1, Lines: []domain.Line{{Kind: domain.LineAddition, Text: "z"}}},
	}}
	got, ok := usecase.FilePatch(f)
	require.True(t, ok)
	assert.Equal(t, "@@ -1,2 +1,2 @@ func x()\n a\n-b\n+c\n\\ No newline at end of file\n@@ -9 +9 @@\n+z", got)
	_, ok = usecase.FilePatch(domain.FileDiff{Binary: true, Hunks: f.Hunks})
	assert.False(t, ok, "binary")
	_, ok = usecase.FilePatch(domain.FileDiff{Omitted: domain.OmitLarge})
	assert.False(t, ok, "truncated")
	_, ok = usecase.FilePatch(domain.FileDiff{Status: domain.FileRenamed})
	assert.False(t, ok, "a pure rename has no hunk")
}

// CI-4 D1: every path of the trust of an activity that is not a push ends
// untrusted unless the provenance of the code it runs is positively known.
func TestPullTipsTrustedFailsClosed(t *testing.T) {
	oid := func(c byte) domain.ObjectID {
		id, err := domain.ParseObjectID(strings.Repeat(string(c), 40))
		require.NoError(t, err)
		return id
	}
	head, base, merge, merged, other := oid('1'), oid('2'), oid('3'), oid('4'), oid('9')
	boom := errors.New("boom")
	repo := name(t, "proj")
	open := domain.MergeRequest{Repo: repo, Number: 1, Source: "feat", Target: "main", State: domain.MRStateOpen}
	closed := open
	closed.State = domain.MRStateClosed
	mergedMR := open
	mergedMR.State, mergedMR.HeadID, mergedMR.MergedCommitID = domain.MRStateMerged, head, merged
	tip := func(ref string, sha domain.ObjectID, trusted bool) domain.CIRefTip {
		return domain.CIRefTip{Repo: repo, Ref: ref, Sha: sha.String(), Trusted: trusted}
	}
	goodTips := []domain.CIRefTip{tip("refs/heads/feat", head, true), tip("refs/heads/main", base, true)}
	mergeCommit := []domain.Commit{{ID: merge, Parents: []domain.ObjectID{base, head}}}

	type world struct {
		mr       domain.MergeRequest
		mrErr    error
		tips     []domain.CIRefTip
		tipErr   error
		mergeRef domain.ObjectID // zero: none
		refErr   error
		commits  []domain.Commit
		listErr  error
		mb       domain.ObjectID
		mbErr    error
	}
	opened := ports.PullRequestEvent{Number: 1, Action: domain.PullOpened, Head: head, Base: base}
	closing := ports.PullRequestEvent{Number: 1, Action: domain.PullClosed, Head: head}
	merging := ports.PullRequestEvent{Number: 1, Action: domain.PullClosed, Head: head, Merged: true}
	mergedTips := []domain.CIRefTip{tip("refs/heads/main", merged, true)}

	cases := []struct {
		name string
		ev   ports.PullRequestEvent
		w    world
		want bool
	}{
		{"opened: head and base pushed by the owner", opened, world{mr: open, tips: goodTips}, true},
		{"opened: the request cannot be read", opened, world{mrErr: boom}, false},
		{"opened: the request is gone", opened, world{mrErr: domain.ErrNotFound}, false},
		{"opened: a record cannot be read", opened, world{mr: open, tipErr: boom}, false},
		{"opened: unknown head", ports.PullRequestEvent{Number: 1, Action: domain.PullOpened, Base: base}, world{mr: open, tips: goodTips}, false},
		{"opened: unknown base", ports.PullRequestEvent{Number: 1, Action: domain.PullOpened, Head: head}, world{mr: open, tips: goodTips}, false},
		{"opened: head without a record", opened, world{mr: open, tips: goodTips[1:]}, false},
		{"opened: base without a record", opened, world{mr: open, tips: goodTips[:1]}, false},
		{"opened: base pushed by an agent", opened, world{mr: open, tips: []domain.CIRefTip{goodTips[0], tip("refs/heads/main", base, false)}}, false},
		{"opened: base record for another commit", opened, world{mr: open, tips: []domain.CIRefTip{goodTips[0], tip("refs/heads/main", other, true)}}, false},
		{"an unknown action is checked like an opening", ports.PullRequestEvent{Number: 1, Action: "edited", Head: head, Base: base}, world{mr: open, tips: goodTips[:1]}, false},
		{"closed: no merge commit, owner's head", closing, world{mr: closed, tips: goodTips[:1]}, true},
		{"closed: merge commit of owner's tips", closing, world{mr: closed, tips: goodTips, mergeRef: merge, commits: mergeCommit}, true},
		{"closed: the merge ref cannot be read", closing, world{mr: closed, tips: goodTips, refErr: boom}, false},
		{"closed: the merge commit cannot be read", closing, world{mr: closed, tips: goodTips, mergeRef: merge, listErr: boom}, false},
		{"closed: the merge ref is not a merge", closing, world{mr: closed, tips: goodTips, mergeRef: merge, commits: []domain.Commit{{ID: merge, Parents: []domain.ObjectID{base}}}}, false},
		{"closed: the merge commit is of another head", closing, world{mr: closed, tips: goodTips, mergeRef: merge, commits: []domain.Commit{{ID: merge, Parents: []domain.ObjectID{base, other}}}}, false},
		{"closed: merge commit on an agent's base", closing, world{mr: closed, tips: []domain.CIRefTip{goodTips[0], tip("refs/heads/main", base, false)}, mergeRef: merge, commits: mergeCommit}, false},
		{"closed, not by a merge, of a merged request: checked like a closing", closing, world{mr: mergedMR, tips: mergedTips}, false},
		{"merged: the merger's", merging, world{mr: mergedMR, tips: mergedTips, mergeRef: merge, commits: mergeCommit, mb: base}, true},
		{"merged without a merge commit", merging, world{mr: mergedMR, tips: mergedTips}, true},
		{"merged flag on a request that is not merged", merging, world{mr: closed, tips: append(goodTips, mergedTips...)}, false},
		{"merged flag on an opening", ports.PullRequestEvent{Number: 1, Action: domain.PullOpened, Head: head, Merged: true}, world{mr: mergedMR, tips: mergedTips}, false},
		{"merged: another head than the merged one", ports.PullRequestEvent{Number: 1, Action: domain.PullClosed, Head: other, Merged: true}, world{mr: mergedMR, tips: mergedTips}, false},
		{"merged: unknown head", ports.PullRequestEvent{Number: 1, Action: domain.PullClosed, Merged: true}, world{mr: mergedMR, tips: mergedTips}, false},
		{"merged: the target moved since", merging, world{mr: mergedMR, tips: []domain.CIRefTip{tip("refs/heads/main", other, true)}}, false},
		{"merged: the target set by an agent", merging, world{mr: mergedMR, tips: []domain.CIRefTip{tip("refs/heads/main", merged, false)}}, false},
		{"merged: the base is not in the merge result", merging, world{mr: mergedMR, tips: mergedTips, mergeRef: merge, commits: mergeCommit, mb: other}, false},
		{"merged: the merge base cannot be read", merging, world{mr: mergedMR, tips: mergedTips, mergeRef: merge, commits: mergeCommit, mbErr: boom}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.w
			mrs := mocks.NewMergeRequestStore(t)
			mrs.EXPECT().Get(mock.Anything, repo, int64(1)).Return(w.mr, w.mrErr).Maybe()
			tips := mocks.NewCIRefTipStore(t)
			tips.EXPECT().GetRefTip(mock.Anything, repo, mock.Anything).RunAndReturn(func(_ context.Context, _ domain.RepoName, ref string) (domain.CIRefTip, error) {
				if w.tipErr != nil {
					return domain.CIRefTip{}, w.tipErr
				}
				for _, rt := range w.tips {
					if rt.Ref == ref {
						return rt, nil
					}
				}
				return domain.CIRefTip{}, domain.ErrNotFound
			}).Maybe()
			pull := mocks.NewPullGit(t)
			refErr := w.refErr
			if refErr == nil && w.mergeRef.IsZero() {
				refErr = domain.ErrNotFound
			}
			pull.EXPECT().PullRef(mock.Anything, repo, domain.PullMergeRef(1)).Return(w.mergeRef, refErr).Maybe()
			git := mocks.NewMergeGit(t)
			git.EXPECT().ListRange(mock.Anything, repo, domain.ObjectID{}, merge, 1).Return(w.commits, false, w.listErr).Maybe()
			git.EXPECT().MergeBase(mock.Anything, repo, mock.Anything, merged).Return(w.mb, w.mbErr).Maybe()

			cfg := usecase.CIConfig{MRs: mrs, RefTips: tips, Pull: pull, Git: git}
			got, _ := usecase.PullTipsTrusted(context.Background(), cfg, repo, tc.ev)
			assert.Equal(t, tc.want, got)
		})
	}
}
