// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-actions/workflow"
	"bnema.dev/ermite/internal/domain"
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
	_, ok = usecase.FilePatch(domain.FileDiff{Truncated: true})
	assert.False(t, ok, "truncated")
	_, ok = usecase.FilePatch(domain.FileDiff{Status: domain.FileRenamed})
	assert.False(t, ok, "a pure rename has no hunk")
}
