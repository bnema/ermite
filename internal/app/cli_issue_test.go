// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-api/gen/ermite"
)

var issueOne = &ermite.Issue{
	Repo: "alpha", Number: 2, Title: "Crash on start", Body: "It crashes.", State: ermite.IssueState_ISSUE_STATE_OPEN,
	Author: "owner", CreatedAt: ts("2026-10-01T10:00:00Z"), UpdatedAt: ts("2026-10-02T11:30:00Z"), CommentCount: 1,
}

func withIssues(*ermite.GetVersionRequest) (*ermite.GetVersionResponse, error) {
	return &ermite.GetVersionResponse{Version: "v0.5.0", Features: []string{"issues"}}, nil
}

func TestIssueCreateMapsFlags(t *testing.T) {
	f := &fakeAPI{getVersion: withIssues, createIssue: func(*ermite.CreateIssueRequest) (*ermite.CreateIssueResponse, error) {
		return &ermite.CreateIssueResponse{Issue: issueOne}, nil
	}}
	url := f.serve(t)
	res := runCLI(t, envFor(url), "", "issue", "create", "alpha", "--title", "Crash on start", "--body", "It crashes.").ok(t)
	assert.Equal(t, "opened alpha#2 (Crash on start)\n"+url+"/alpha/issues/2\n", res.stdout)
	_, reqs := f.snapshot()
	got := reqs[1].(*ermite.CreateIssueRequest)
	assert.Equal(t, "alpha", got.GetRepo())
	assert.Equal(t, "Crash on start", got.GetTitle())
	assert.Equal(t, "It crashes.", got.GetBody())

	runCLI(t, envFor(url), "from\nstdin\n", "issue", "create", "alpha", "--title", "t", "--body", "-").ok(t)
	runCLI(t, envFor(url), "piped\n", "issue", "create", "alpha", "--title", "t", "--body-file", "-").ok(t)
	file := filepath.Join(t.TempDir(), "body.txt")
	require.NoError(t, os.WriteFile(file, []byte("from file\n"), 0o600))
	runCLI(t, envFor(url), "", "issue", "create", "alpha", "--title", "t", "--body-file", file).ok(t)
	_, reqs = f.snapshot()
	var bodies []string
	for _, r := range reqs {
		if c, ok := r.(*ermite.CreateIssueRequest); ok {
			bodies = append(bodies, c.GetBody())
		}
	}
	assert.Equal(t, []string{"It crashes.", "from\nstdin", "piped", "from file"}, bodies)

	res = runCLI(t, envFor(url), "", "issue", "create", "alpha", "--title", "t", "--json").ok(t)
	assert.JSONEq(t, `{"url":"`+url+`/alpha/issues/2","repo":"alpha","number":2,"title":"Crash on start","body":"It crashes.","state":"open","author":"owner","created_at":"2026-10-01T10:00:00Z","updated_at":"2026-10-02T11:30:00Z","comment_count":1,"closed_by_merge_request":0}`, res.stdout)

	for _, args := range [][]string{
		{"issue", "create", "alpha"},
		{"issue", "create", "alpha", "--title", "t", "--body", "a", "--body-file", file},
	} {
		require.Error(t, runCLI(t, envFor(url), "", args...).err, "%v", args)
	}
}

func TestIssueListTableJSONAndPages(t *testing.T) {
	closed := &ermite.Issue{Repo: "alpha", Number: 1, Title: "Old", State: ermite.IssueState_ISSUE_STATE_CLOSED, Author: "bot", UpdatedAt: ts("2026-09-01T08:00:00Z")}
	f := &fakeAPI{getVersion: withIssues, listIssues: func(r *ermite.ListIssuesRequest) (*ermite.ListIssuesResponse, error) {
		if r.GetPageToken() == "" {
			return &ermite.ListIssuesResponse{Issues: []*ermite.Issue{issueOne}, NextPageToken: "p2"}, nil
		}
		return &ermite.ListIssuesResponse{Issues: []*ermite.Issue{closed}}, nil
	}}
	url := f.serve(t)
	res := runCLI(t, envFor(url), "", "issue", "list", "alpha", "--state", "all").ok(t)
	assert.Regexp(t, `NUMBER\s+STATE\s+AUTHOR\s+COMMENTS\s+UPDATED\s+TITLE\n#2\s+open\s+owner\s+1\s+2026-10-02 11:30\s+Crash on start\n#1\s+closed\s+bot\s+0\s+2026-09-01 08:00\s+Old\n`, res.stdout)
	_, reqs := f.snapshot()
	require.Len(t, reqs, 3) // GetVersion, two pages
	assert.Equal(t, ermite.IssueState_ISSUE_STATE_UNSPECIFIED, reqs[1].(*ermite.ListIssuesRequest).GetState())
	assert.Equal(t, "p2", reqs[2].(*ermite.ListIssuesRequest).GetPageToken())

	runCLI(t, envFor(url), "", "issue", "list", "alpha").ok(t)
	_, reqs = f.snapshot()
	assert.Equal(t, ermite.IssueState_ISSUE_STATE_OPEN, reqs[len(reqs)-2].(*ermite.ListIssuesRequest).GetState())

	res = runCLI(t, envFor(url), "", "issue", "list", "alpha", "--json").ok(t)
	assert.Contains(t, res.stdout, `"state": "closed"`)

	require.Error(t, runCLI(t, envFor(url), "", "issue", "list", "alpha", "--state", "merged").err)

	empty := &fakeAPI{getVersion: withIssues, listIssues: func(*ermite.ListIssuesRequest) (*ermite.ListIssuesResponse, error) {
		return &ermite.ListIssuesResponse{}, nil
	}}
	res = runCLI(t, envFor(empty.serve(t)), "", "issue", "list", "alpha", "--json").ok(t)
	assert.Equal(t, "[]\n", res.stdout)
	res = runCLI(t, envFor(empty.serve(t)), "", "issue", "list", "alpha").ok(t)
	assert.Equal(t, "ermite: no issues\n", res.stderr)
}

func TestIssueShowCommentCloseReopen(t *testing.T) {
	cmt := &ermite.IssueComment{Id: "1", Author: "agent-7", Text: "On it.", CreatedAt: ts("2026-10-02T11:30:00Z")}
	reopened := &ermite.Issue{Repo: "alpha", Number: 2, Title: "Crash on start", State: ermite.IssueState_ISSUE_STATE_OPEN}
	f := &fakeAPI{
		getVersion: withIssues,
		getIssue: func(*ermite.GetIssueRequest) (*ermite.GetIssueResponse, error) {
			return &ermite.GetIssueResponse{Issue: issueOne, Comments: []*ermite.IssueComment{cmt}}, nil
		},
		addIssueCmt: func(*ermite.AddIssueCommentRequest) (*ermite.AddIssueCommentResponse, error) {
			return &ermite.AddIssueCommentResponse{Comment: cmt}, nil
		},
		closeIssue: func(*ermite.CloseIssueRequest) (*ermite.CloseIssueResponse, error) {
			c := &ermite.Issue{Repo: "alpha", Number: 2, Title: "Crash on start", State: ermite.IssueState_ISSUE_STATE_CLOSED}
			return &ermite.CloseIssueResponse{Issue: c}, nil
		},
		reopenIssue: func(*ermite.ReopenIssueRequest) (*ermite.ReopenIssueResponse, error) {
			return &ermite.ReopenIssueResponse{Issue: reopened}, nil
		},
	}
	url := f.serve(t)

	res := runCLI(t, envFor(url), "", "issue", "show", "alpha", "#2").ok(t)
	assert.Contains(t, res.stdout, "issue    alpha#2")
	assert.Contains(t, res.stdout, "It crashes.")
	assert.Contains(t, res.stdout, "comments (1)")
	assert.Contains(t, res.stdout, "#1 agent-7")
	res = runCLI(t, envFor(url), "", "issue", "show", "alpha", "2", "--json").ok(t)
	assert.Contains(t, res.stdout, `"comments"`)
	assert.Contains(t, res.stdout, `"text": "On it."`)

	res = runCLI(t, envFor(url), "", "issue", "comment", "alpha", "2", "-m", "On it.").ok(t)
	assert.Equal(t, "comment 1 added to alpha#2\n", res.stdout)
	runCLI(t, envFor(url), "multi\nline\n", "issue", "comment", "alpha", "2", "--text", "-").ok(t)
	res = runCLI(t, envFor(url), "", "issue", "comment", "alpha", "2", "-m", "x", "--json").ok(t)
	assert.JSONEq(t, `{"id":"1","author":"agent-7","text":"On it.","created_at":"2026-10-02T11:30:00Z"}`, res.stdout)
	_, reqs := f.snapshot()
	var texts []string
	for _, r := range reqs {
		if c, ok := r.(*ermite.AddIssueCommentRequest); ok {
			assert.EqualValues(t, 2, c.GetNumber())
			texts = append(texts, c.GetText())
		}
	}
	assert.Equal(t, []string{"On it.", "multi\nline", "x"}, texts)
	require.Error(t, runCLI(t, envFor(url), "", "issue", "comment", "alpha", "2").err)

	res = runCLI(t, envFor(url), "", "issue", "close", "alpha", "2").ok(t)
	assert.Equal(t, "closed alpha#2 (Crash on start)\n"+url+"/alpha/issues/2\n", res.stdout)
	res = runCLI(t, envFor(url), "", "issue", "reopen", "alpha", "2", "--json").ok(t)
	assert.Contains(t, res.stdout, `"state": "open"`)

	for _, n := range []string{"0", "x", "-1"} {
		require.Error(t, runCLI(t, envFor(url), "", "issue", "show", "alpha", n).err, n)
	}
}

func TestIssueNeedsTheFeature(t *testing.T) {
	f := &fakeAPI{getVersion: func(*ermite.GetVersionRequest) (*ermite.GetVersionResponse, error) {
		return &ermite.GetVersionResponse{Version: "v0.4.0", Features: []string{"merge_requests"}}, nil
	}}
	url := f.serve(t)
	for _, args := range [][]string{
		{"issue", "list", "alpha"}, {"issue", "show", "alpha", "1"}, {"issue", "create", "alpha", "--title", "t"},
		{"issue", "comment", "alpha", "1", "-m", "x"}, {"issue", "close", "alpha", "1"}, {"issue", "reopen", "alpha", "1"},
		{"repo", "edit", "alpha", "--issues", "owner"},
	} {
		res := runCLI(t, envFor(url), "", args...)
		require.Error(t, res.err, "%v", args)
		assert.Contains(t, res.err.Error(), "does not support issues", "%v", args)
	}
	calls, _ := f.snapshot()
	for _, c := range calls {
		assert.Equal(t, "GetVersion", c)
	}
}

func TestRepoEditIssues(t *testing.T) {
	updated := &ermite.Repo{Name: "alpha", Settings: &ermite.RepoSettings{Issues: ermite.IssuesMode_ISSUES_MODE_OWNER}}
	f := &fakeAPI{getVersion: withIssues, updateRepo: func(*ermite.UpdateRepoRequest) (*ermite.UpdateRepoResponse, error) {
		return &ermite.UpdateRepoResponse{Repo: updated}, nil
	}}
	url := f.serve(t)
	for value, want := range map[string]ermite.IssuesMode{
		"off": ermite.IssuesMode_ISSUES_MODE_OFF, "owner": ermite.IssuesMode_ISSUES_MODE_OWNER, "PUBLIC": ermite.IssuesMode_ISSUES_MODE_PUBLIC,
	} {
		res := runCLI(t, envFor(url), "", "repo", "edit", "alpha", "--issues", value, "--json").ok(t)
		assert.Contains(t, res.stdout, `"issues": "owner"`)
		_, reqs := f.snapshot()
		got := reqs[len(reqs)-1].(*ermite.UpdateRepoRequest)
		require.NotNil(t, got.Issues, value)
		assert.Equal(t, want, got.GetIssues())
	}
	res := runCLI(t, envFor(url), "", "repo", "edit", "alpha", "--issues", "all")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "off, owner or public")
	res = runCLI(t, envFor(url), "", "repo", "show", "alpha")
	require.Error(t, res.err) // getRepo not set: unimplemented
}

func TestRepoShowIssues(t *testing.T) {
	r := &ermite.Repo{Name: "alpha", Settings: &ermite.RepoSettings{Issues: ermite.IssuesMode_ISSUES_MODE_OWNER}}
	f := &fakeAPI{getRepo: func(*ermite.GetRepoRequest) (*ermite.GetRepoResponse, error) {
		return &ermite.GetRepoResponse{Repo: r}, nil
	}}
	url := f.serve(t)
	res := runCLI(t, envFor(url), "", "repo", "show", "alpha").ok(t)
	assert.Contains(t, res.stdout, "issues            owner\n")
	res = runCLI(t, envFor(url), "", "repo", "show", "alpha", "--json").ok(t)
	assert.Contains(t, res.stdout, `"issues": "owner"`)
}

func TestTokenCreateIssueWriteNeedsTheFeature(t *testing.T) {
	echo := func(r *ermite.CreateTokenRequest) (*ermite.CreateTokenResponse, error) {
		return &ermite.CreateTokenResponse{Token: &ermite.Token{Id: "t", Label: r.GetLabel(), Scopes: r.GetScopes()}, Secret: "ermite_S"}, nil
	}
	f := &fakeAPI{createToken: echo, getVersion: func(*ermite.GetVersionRequest) (*ermite.GetVersionResponse, error) {
		return &ermite.GetVersionResponse{Version: "v0.4.0"}, nil
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "token", "create", "x", "--scope", "issue:write")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "does not support issues")
	calls, _ := f.snapshot()
	assert.NotContains(t, calls, "CreateToken")

	f = &fakeAPI{createToken: echo, getVersion: withIssues}
	runCLI(t, envFor(f.serve(t)), "", "token", "create", "x", "--preset", "agent", "--scope", "issue:write").ok(t)
	assert.Contains(t, lastCreateToken(t, f).GetScopes(), ermite.Scope_SCOPE_ISSUE_WRITE)
}

func TestTokenCreatePresetCLIOnAnOldServerDropsIssueWrite(t *testing.T) {
	f := &fakeAPI{getVersion: func(*ermite.GetVersionRequest) (*ermite.GetVersionResponse, error) {
		return &ermite.GetVersionResponse{Version: "v0.4.0"}, nil
	}, createToken: func(r *ermite.CreateTokenRequest) (*ermite.CreateTokenResponse, error) {
		return &ermite.CreateTokenResponse{Token: &ermite.Token{Id: "t", Label: r.GetLabel(), Scopes: r.GetScopes()}, Secret: "ermite_S"}, nil
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "token", "create", "x", "--preset", "cli").ok(t)
	got := lastCreateToken(t, f).GetScopes()
	assert.NotContains(t, got, ermite.Scope_SCOPE_ISSUE_WRITE)
	assert.NotEmpty(t, got)
	assert.Contains(t, res.stderr, "no issue:write")

	res = runCLI(t, envFor(f.serve(t)), "", "token", "create", "x", "--preset", "cli", "--scope", "issue:write")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "does not support issues")
}

type failingWriter struct{ after int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.after <= 0 {
		return 0, errors.New("write failed")
	}
	w.after--
	return len(p), nil
}

func TestPrintIssueDetailReturnsWriteErrors(t *testing.T) {
	d := &ermite.GetIssueResponse{Issue: &ermite.Issue{Repo: "alpha", Number: 1, Title: "t"}, Comments: []*ermite.IssueComment{{Id: "1", Text: "c"}}}
	for after := range 60 {
		err := printIssueDetail(&failingWriter{after: after}, d)
		if err == nil {
			return // all writes succeeded: every earlier failure was reported
		}
		assert.ErrorContains(t, err, "write failed", after)
	}
	t.Fatal("never completed")
}

func TestIssueShowMergeRequestLinks(t *testing.T) {
	closed := &ermite.Issue{
		Repo: "alpha", Number: 2, Title: "Crash on start", State: ermite.IssueState_ISSUE_STATE_CLOSED,
		ClosedByMergeRequest: 5,
	}
	f := &fakeAPI{
		getVersion: withIssues,
		getIssue: func(*ermite.GetIssueRequest) (*ermite.GetIssueResponse, error) {
			return &ermite.GetIssueResponse{Issue: closed, References: []*ermite.IssueReference{
				{Number: 5, Title: "Fix the parser", State: ermite.MergeRequestState_MERGE_REQUEST_STATE_OPEN, Closes: true},
				{Number: 7, Title: "Follow-up", State: ermite.MergeRequestState_MERGE_REQUEST_STATE_MERGED},
			}}, nil
		},
	}
	url := f.serve(t)
	res := runCLI(t, envFor(url), "", "issue", "show", "alpha", "2").ok(t)
	assert.Regexp(t, `state\s+closed\n.*closed by\s+!5\n`, res.stdout)
	assert.Contains(t, res.stdout, "\nmerge requests (2)\n  !5 open   Fix the parser · closes\n  !7 merged Follow-up\n")
	assert.Less(t, strings.Index(res.stdout, "merge requests (2)"), strings.Index(res.stdout, "comments (0)"))

	res = runCLI(t, envFor(url), "", "issue", "show", "alpha", "2", "--json").ok(t)
	assert.JSONEq(t, `{"url":"`+url+`/alpha/issues/2","repo":"alpha","number":2,"title":"Crash on start","body":"","state":"closed","author":"","created_at":null,"updated_at":null,"comment_count":0,"closed_by_merge_request":5,"comments":[],"references":[{"number":5,"title":"Fix the parser","state":"open","closes":true},{"number":7,"title":"Follow-up","state":"merged","closes":false}]}`, res.stdout)

	// No references: no block in text, an empty array in JSON.
	f.getIssue = func(*ermite.GetIssueRequest) (*ermite.GetIssueResponse, error) {
		return &ermite.GetIssueResponse{Issue: issueOne}, nil
	}
	res = runCLI(t, envFor(url), "", "issue", "show", "alpha", "2").ok(t)
	assert.NotContains(t, res.stdout, "merge requests")
	assert.NotContains(t, res.stdout, "closed by")
	res = runCLI(t, envFor(url), "", "issue", "show", "alpha", "2", "--json").ok(t)
	assert.Contains(t, res.stdout, `"references": []`)
}

func TestIssueWebURL(t *testing.T) {
	assert.Equal(t, "https://git.example.org/my-repo/issues/42", issueWebURL("https://git.example.org/", "my-repo", 42))
	assert.Equal(t, "https://git.example.org/team%2Frepo%20%23%3F%25/issues/42", issueWebURL("https://git.example.org", "team/repo #?%", 42))
}
