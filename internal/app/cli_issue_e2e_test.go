// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-api/gen/ermite"
)

type issueOut struct {
	Repo         string  `json:"repo"`
	Number       int64   `json:"number"`
	Title        string  `json:"title"`
	Body         string  `json:"body"`
	State        string  `json:"state"`
	Author       string  `json:"author"`
	CreatedAt    *string `json:"created_at"`
	UpdatedAt    *string `json:"updated_at"`
	CommentCount int     `json:"comment_count"`
}

var issueKeys = []string{"author", "body", "closed_by_merge_request", "comment_count", "created_at", "number", "repo", "state", "title", "updated_at"}

type issueCommentOut struct {
	ID     string `json:"id"`
	Author string `json:"author"`
	Text   string `json:"text"`
}

// The issue commands against a real forge: issues share the numbering of the
// merge requests.
func TestIssuesCLIEndToEnd(t *testing.T) {
	c := newCLIHarness(t)
	agent := strings.TrimSpace(func() string {
		out, errOut, code := c.bin(t, adminToken, "token", "create", "agent-7", "--preset", "agent", "--scope", "issue:write", "--repo", "proj")
		require.Equal(t, 0, code, errOut)
		return out
	}())
	c.cli(t, adminToken, "repo", "create", "proj").ok(t)

	// Issues are off by default.
	res := c.cli(t, adminToken, "repo", "show", "proj", "--json").ok(t)
	assert.Equal(t, "off", decode[struct {
		Issues string `json:"issues"`
	}](t, res.stdout).Issues)
	res = c.cli(t, adminToken, "issue", "create", "proj", "--title", "too early")
	require.Error(t, res.err)

	// repo edit --issues.
	res = c.cli(t, adminToken, "repo", "edit", "proj", "--issues", "owner", "--json").ok(t)
	assert.Equal(t, "owner", decode[struct {
		Issues string `json:"issues"`
	}](t, res.stdout).Issues)
	res = c.cli(t, adminToken, "repo", "show", "proj").ok(t)
	assert.Regexp(t, `issues\s+owner`, res.stdout)
	require.Error(t, c.cli(t, adminToken, "repo", "edit", "proj", "--issues", "everyone").err)
	res = c.cli(t, adminToken, "repo", "edit", "proj", "--issues", "public", "--json").ok(t)
	assert.Equal(t, "public", decode[struct {
		Issues string `json:"issues"`
	}](t, res.stdout).Issues)
	c.cli(t, adminToken, "repo", "edit", "proj", "--issues", "owner").ok(t)

	// A merge request first: the issue takes the next number.
	wt := c.workTree("main", "README.md")
	url := c.urlAuth(adminToken, "/proj.git")
	c.mustGit(wt, "push", "-q", url, "main")
	c.mustGit(wt, "checkout", "-q", "-b", "feat/x")
	c.writeFile(wt, "x.txt", []byte("x\n"))
	c.mustGit(wt, "push", "-q", url, "feat/x")
	c.cli(t, adminToken, "mr", "create", "proj", "--source", "feat/x", "--title", "First").ok(t)

	res = c.cli(t, agent, "issue", "create", "proj", "--title", "Crash on start", "--body", "It crashes.\nSecond line.", "--json").ok(t)
	assert.Equal(t, issueKeys, keys(t, res.stdout))
	issue := decode[issueOut](t, res.stdout)
	assert.EqualValues(t, 2, issue.Number, "shared numbering with merge requests")
	assert.Equal(t, "open", issue.State)
	assert.Equal(t, "agent-7", issue.Author)
	assert.Equal(t, "It crashes.\nSecond line.", issue.Body)

	res = runCLI(t, c.vars(agent), "from stdin\n", "issue", "create", "proj", "--title", "Second", "--body-file", "-").ok(t)
	assert.Equal(t, "opened proj#3 (Second)\n", res.stdout)

	// list.
	res = c.cli(t, adminToken, "issue", "list", "proj", "--json").ok(t)
	list := decode[[]issueOut](t, res.stdout)
	require.Len(t, list, 2)
	assert.EqualValues(t, 3, list[0].Number, "highest number first")
	res = c.cli(t, adminToken, "issue", "list", "proj").ok(t)
	lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
	require.Len(t, lines, 3)
	assert.Regexp(t, `^NUMBER\s+STATE\s+AUTHOR\s+COMMENTS\s+UPDATED\s+TITLE$`, lines[0])
	assert.Regexp(t, `^#3\s+open\s+agent-7\s+0\s+\d{4}-\d{2}-\d{2} \d{2}:\d{2}\s+Second$`, lines[1])

	// comment, then show with it.
	res = c.cli(t, agent, "issue", "comment", "proj", "2", "-m", "Reproduced.", "--json").ok(t)
	cm := decode[issueCommentOut](t, res.stdout)
	assert.Equal(t, "agent-7", cm.Author)
	assert.Equal(t, "Reproduced.", cm.Text)
	res = c.cli(t, adminToken, "issue", "comment", "proj", "#2", "-m", "Thanks.").ok(t)
	assert.Regexp(t, `^comment \S+ added to proj#2\n$`, res.stdout)
	res = c.cli(t, adminToken, "issue", "show", "proj", "2", "--json").ok(t)
	type showOut struct {
		issueOut
		Comments   []issueCommentOut `json:"comments"`
		References []json.RawMessage `json:"references"`
	}
	show := decode[showOut](t, res.stdout)
	assert.Equal(t, 2, show.CommentCount)
	require.Len(t, show.Comments, 2)
	assert.Equal(t, "Reproduced.", show.Comments[0].Text, "oldest first")
	res = c.cli(t, adminToken, "issue", "show", "proj", "2").ok(t)
	assert.Contains(t, res.stdout, "Crash on start")
	assert.Contains(t, res.stdout, "comments (2)")
	assert.NotContains(t, res.stdout, "merge requests", "no merge request names the issue")
	assert.NotNil(t, show.References, "references is always present")
	assert.Empty(t, show.References)

	// close and reopen.
	res = c.cli(t, agent, "issue", "close", "proj", "2", "--json").ok(t)
	assert.Equal(t, "closed", decode[issueOut](t, res.stdout).State)
	res = c.cli(t, adminToken, "issue", "list", "proj", "--json").ok(t)
	assert.Len(t, decode[[]issueOut](t, res.stdout), 1)
	res = c.cli(t, adminToken, "issue", "list", "proj", "--state", "closed", "--json").ok(t)
	assert.Len(t, decode[[]issueOut](t, res.stdout), 1)
	res = c.cli(t, adminToken, "issue", "list", "proj", "--state", "all", "--json").ok(t)
	assert.Len(t, decode[[]issueOut](t, res.stdout), 2)
	res = c.cli(t, agent, "issue", "reopen", "proj", "2").ok(t)
	assert.Equal(t, "reopened proj#2 (Crash on start)\n", res.stdout)

	// An issue is not a merge request and the other way round.
	_, stderr, code := c.bin(t, adminToken, "issue", "show", "proj", "1")
	requireOneLineError(t, stderr, code, "not found")
	_, stderr, code = c.bin(t, adminToken, "issue", "show", "proj", "9")
	requireOneLineError(t, stderr, code, "not found")
}

// A server without the issues feature is refused before anything is created.
func TestIssueWriteTokenRefusedWithoutTheFeature(t *testing.T) {
	f := &fakeAPI{
		getVersion: func(*ermite.GetVersionRequest) (*ermite.GetVersionResponse, error) {
			return &ermite.GetVersionResponse{Version: "v0.4.0", Features: []string{"merge_requests"}}, nil
		},
		createToken: func(*ermite.CreateTokenRequest) (*ermite.CreateTokenResponse, error) {
			return &ermite.CreateTokenResponse{Token: &ermite.Token{Id: "t"}, Secret: "ermite_S"}, nil
		},
	}
	res := runCLI(t, envFor(f.serve(t)), "", "token", "create", "bot", "--preset", "agent", "--scope", "issue:write")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "does not support issues")
	assert.Empty(t, res.stdout)
	calls, _ := f.snapshot()
	assert.NotContains(t, calls, "CreateToken")
}
