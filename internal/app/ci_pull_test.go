// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// CI-3 D5/D6, AC5: the pull_request event of merge requests, the refs
// refs/pull/<n>/{head,merge} and the REST files endpoint paths-filter reads.

const pullWorkflow = `name: pr
on:
  pull_request:
%s
jobs:
  j:
    runs-on: ubuntu-latest
%s
    steps:
      - run: echo hi
`

func pullWF(on, jobExtra string) map[string]string {
	return map[string]string{".github/workflows/pr.yml": fmt.Sprintf(pullWorkflow, on, jobExtra)}
}

// openMR opens a merge request from source into main as client.
func (c *ciHarness) openMR(cl *clients, source string) int64 {
	c.t.Helper()
	res, err := cl.mr.CreateMergeRequest(c.ctx, connect.NewRequest(&pb.CreateMergeRequestRequest{
		Repo: c.repo, Title: "Title of " + source, Description: "About " + source, SourceBranch: source, TargetBranch: "main",
	}))
	require.NoError(c.t, err)
	return res.Msg.GetMergeRequest().GetNumber()
}

func (c *ciHarness) ghContext(j *pb.Job) map[string]any {
	c.t.Helper()
	var ctxs map[string]map[string]any
	require.NoError(c.t, json.Unmarshal([]byte(j.GetContexts()), &ctxs))
	return ctxs["github"]
}

func (c *ciHarness) ref(name string) string {
	c.t.Helper()
	out, err := c.git(c.repoDirOf(), "rev-parse", "--verify", "-q", name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (c *ciHarness) repoDirOf() string { return c.repoDir("proj") }

// finish runs the job of the next run to success.
func (c *ciHarness) finish(r *fakeRunner) *pb.Job {
	c.t.Helper()
	j := r.await()
	r.runJob(j, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
	return j
}

func TestPullRequestOpenedSynchronizeRuns(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", pullWF("", ""), "")
	c.push("main", map[string]string{"a.txt": "a\n"}, "")
	main := strings.TrimSpace(c.mustGit(c.wt, "rev-parse", "HEAD"))
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	feat1 := c.push("feat", map[string]string{"b.txt": "b\n"}, "")

	n := c.openMR(c.root, "feat")
	job := c.finish(runner)

	// The refs: head is the branch, merge a test merge commit of main and feat.
	assert.Equal(t, feat1, c.ref(domain.PullHeadRef(n)))
	merge := c.ref(domain.PullMergeRef(n))
	require.NotEmpty(t, merge)
	assert.Equal(t, main+" "+feat1, strings.TrimSpace(c.git2("log", "-1", "--format=%P", merge)), "parents: base then head")
	assert.Equal(t, "Merge "+feat1+" into "+main, strings.TrimSpace(c.git2("log", "-1", "--format=%s", merge)))
	assert.Contains(t, c.git2("ls-tree", "--name-only", merge), "a.txt", "the tree has main's file")
	assert.Contains(t, c.git2("ls-tree", "--name-only", merge), "b.txt", "and feat's")

	runs := c.runs()
	require.Len(t, runs, 1)
	assert.Equal(t, "pull_request", runs[0].GetEvent())
	assert.Equal(t, "refs/pull/1/merge", runs[0].GetRef())
	assert.Equal(t, merge, runs[0].GetSha())
	assert.True(t, runs[0].GetTrusted())
	assert.Equal(t, "Title of feat", runs[0].GetTitle())

	// Checks attach to the head, not to the merge commit.
	require.Len(t, c.checks(feat1), 1)
	assert.Equal(t, "j", c.checks(feat1)[0].GetName())
	assert.Empty(t, c.checks(merge))

	gh := c.ghContext(job)
	assert.Equal(t, "pull_request", gh["event_name"])
	assert.Equal(t, "refs/pull/1/merge", gh["ref"])
	assert.Equal(t, "1/merge", gh["ref_name"])
	assert.Equal(t, merge, gh["sha"])
	assert.Equal(t, "feat", gh["head_ref"])
	assert.Equal(t, "main", gh["base_ref"])
	assert.Equal(t, merge, job.GetSha())
	ev := gh["event"].(map[string]any)
	assert.Equal(t, "opened", ev["action"])
	assert.EqualValues(t, 1, ev["number"])
	pr := ev["pull_request"].(map[string]any)
	assert.EqualValues(t, 1, pr["number"])
	assert.Equal(t, "open", pr["state"])
	assert.Equal(t, "Title of feat", pr["title"])
	assert.Equal(t, "About feat", pr["body"])
	assert.Equal(t, false, pr["merged"])
	assert.Equal(t, merge, pr["merge_commit_sha"])
	assert.Equal(t, publicOrigin+"/owner/proj/merge-requests/1", pr["html_url"])
	head, base := pr["head"].(map[string]any), pr["base"].(map[string]any)
	assert.Equal(t, "feat", head["ref"])
	assert.Equal(t, feat1, head["sha"])
	assert.Equal(t, "main", base["ref"])
	assert.Equal(t, main, base["sha"])
	assert.Equal(t, "owner/proj", head["repo"].(map[string]any)["full_name"])
	assert.Equal(t, "owner/proj", ev["repository"].(map[string]any)["full_name"])
	assert.NotEmpty(t, ev["sender"].(map[string]any)["login"])

	// A push to the source branch is a synchronize run, with the new merge.
	feat2 := c.push("feat", map[string]string{"c.txt": "c\n"}, "")
	job = c.finish(runner)
	c.waitFor("two runs", func() bool { return len(c.runs()) == 2 })
	assert.Equal(t, feat2, c.ref(domain.PullHeadRef(n)))
	merge2 := c.ref(domain.PullMergeRef(n))
	assert.NotEqual(t, merge, merge2)
	gh = c.ghContext(job)
	assert.Equal(t, merge2, gh["sha"])
	ev = gh["event"].(map[string]any)
	assert.Equal(t, "synchronize", ev["action"])
	assert.Equal(t, feat1, ev["before"])
	assert.Equal(t, feat2, ev["after"])
	assert.Len(t, c.checks(feat2), 1)
}

// git2 runs git in the bare repository.
func (c *ciHarness) git2(args ...string) string { return c.bareGit("proj", args...) }

func TestPullRequestConflictingMergeRunsNothing(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	files := pullWF("", "")
	files["a.txt"] = "base\n"
	c.push("main", files, "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "conflict")
	c.push("conflict", map[string]string{"a.txt": "feature\n"}, "")
	c.mustGit(c.wt, "checkout", "-q", "main")
	c.push("main", map[string]string{"a.txt": "main\n"}, "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "clean", "main")
	c.push("clean", map[string]string{"z.txt": "z\n"}, "")

	n1 := c.openMR(c.root, "conflict")
	n2 := c.openMR(c.root, "clean")
	job := c.finish(runner) // the one run is the clean request's
	assert.EqualValues(t, 2, c.ghContext(job)["event"].(map[string]any)["number"])
	assert.NotEmpty(t, c.ref(domain.PullHeadRef(n1)), "the head ref is kept")
	assert.Empty(t, c.ref(domain.PullMergeRef(n1)), "no merge ref when the merge conflicts")
	assert.NotEmpty(t, c.ref(domain.PullMergeRef(n2)))
	require.Len(t, c.runs(), 1)

	// A push that does not fix the conflict makes no run either.
	c.mustGit(c.wt, "checkout", "-q", "conflict")
	c.push("conflict", map[string]string{"other.txt": "o\n"}, "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "later", "main")
	c.push("later", map[string]string{"l.txt": "l\n"}, "")
	c.openMR(c.root, "later")
	job = c.finish(runner)
	assert.EqualValues(t, 3, c.ghContext(job)["event"].(map[string]any)["number"])
	assert.Len(t, c.runs(), 2)
}

func TestPullRequestTypesAndClosed(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	// closed only: opened and synchronize start nothing.
	c.push("main", pullWF("    types: [closed]", ""), "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "f1")
	c.push("f1", map[string]string{"1.txt": "1\n"}, "")
	n1 := c.openMR(c.root, "f1")
	f1 := c.push("f1", map[string]string{"1b.txt": "1\n"}, "")
	c.mustGit(c.wt, "checkout", "-q", "main")
	c.mustGit(c.wt, "checkout", "-q", "-b", "f2")
	f2 := c.push("f2", map[string]string{"2.txt": "2\n"}, "")
	n2 := c.openMR(c.root, "f2")
	// The refs follow the MR creation and the push asynchronously: wait for
	// both of each request, at the latest source sha.
	c.waitFor("the refs of both requests", func() bool {
		mergesHead := func(n int64, head string) bool {
			m := c.ref(domain.PullMergeRef(n))
			parents, err := c.git(c.repoDirOf(), "log", "-1", "--format=%P", m) // no require: runs in the poller
			return m != "" && err == nil && strings.HasSuffix(strings.TrimSpace(parents), " "+head)
		}
		return c.ref(domain.PullHeadRef(n1)) == f1 && mergesHead(n1, f1) &&
			c.ref(domain.PullHeadRef(n2)) == f2 && mergesHead(n2, f2)
	})
	require.Empty(t, c.runs(), "no run for opened and synchronize")
	head1 := c.ref(domain.PullHeadRef(n1))
	merge1 := c.ref(domain.PullMergeRef(n1))
	require.Equal(t, f1, head1)

	// Closed without merging: merged false, the last computed merge commit.
	_, err := c.root.mr.CloseMergeRequest(c.ctx, connect.NewRequest(&pb.CloseMergeRequestRequest{Repo: "proj", Number: n1}))
	require.NoError(t, err)
	job := c.finish(runner)
	gh := c.ghContext(job)
	ev := gh["event"].(map[string]any)
	assert.Equal(t, "closed", ev["action"])
	pr := ev["pull_request"].(map[string]any)
	assert.Equal(t, false, pr["merged"])
	assert.Equal(t, "closed", pr["state"])
	assert.Equal(t, merge1, gh["sha"], "the last computed merge commit")
	assert.Equal(t, merge1, pr["merge_commit_sha"])
	assert.Equal(t, "refs/pull/1/merge", gh["ref"])

	// Merged: merged true, merge_commit_sha is the commit the merge made; the
	// source branch is deleted and the refs are not computed again.
	head2 := c.ref(domain.PullHeadRef(n2))
	merge2 := c.ref(domain.PullMergeRef(n2))
	_, err = c.root.mr.MergeMergeRequest(c.ctx, connect.NewRequest(&pb.MergeMergeRequestRequest{
		Repo: "proj", Number: n2, HeadId: head2, DeleteSourceBranch: true,
	}))
	require.NoError(t, err)
	job = c.finish(runner)
	gh = c.ghContext(job)
	ev = gh["event"].(map[string]any)
	pr = ev["pull_request"].(map[string]any)
	assert.Equal(t, "closed", ev["action"])
	assert.Equal(t, true, pr["merged"])
	assert.Equal(t, head2, pr["merge_commit_sha"], "fast-forward: the merged commit is the head")
	assert.Equal(t, merge2, gh["sha"], "github.sha stays the last test merge")
	assert.Equal(t, head2, pr["head"].(map[string]any)["sha"])
	assert.Equal(t, head2, c.ref(domain.PullHeadRef(n2)))
	assert.Equal(t, merge2, c.ref(domain.PullMergeRef(n2)))
	assert.Len(t, c.checks(head2), 1)
}

func TestPullRequestPathsAndBranchesFilters(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", pullWF("    branches: [main]\n    paths: ['docs/**']", ""), "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "f1")
	c.push("f1", map[string]string{"src/x.go": "x\n"}, "")
	n := c.openMR(c.root, "f1")
	// Another request whose changes match: the only run is its.
	c.mustGit(c.wt, "checkout", "-q", "-b", "f2", "main")
	c.push("f2", map[string]string{"docs/readme.md": "d\n"}, "")
	n2 := c.openMR(c.root, "f2")
	job := c.finish(runner)
	assert.EqualValues(t, n2, c.ghContext(job)["event"].(map[string]any)["number"])
	require.Len(t, c.runs(), 1)

	// A later push that adds a docs file to the first request: the files are
	// those between the merge base and the head, so it runs now.
	c.mustGit(c.wt, "checkout", "-q", "f1")
	c.push("f1", map[string]string{"docs/new.md": "n\n"}, "")
	job = c.finish(runner)
	assert.EqualValues(t, n, c.ghContext(job)["event"].(map[string]any)["number"])
	assert.Equal(t, "synchronize", c.ghContext(job)["event"].(map[string]any)["action"])
}

func TestPullRequestTrustFollowsTheActor(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", pullWF("", ""), "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "agent/x")
	c.push("agent/x", map[string]string{"x.txt": "x\n"}, "")
	secret, _ := c.branchToken(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite, domain.ScopeMRWrite), []string{"agent/"}, "proj")

	// Opened by a branch-limited token: untrusted, no secrets, read-only token.
	c.openMR(c.api(secret), "agent/x")
	job := c.finish(runner)
	assert.False(t, job.GetTrusted())
	runs := c.runs()
	require.Len(t, runs, 1)
	assert.False(t, runs[0].GetTrusted())
	assert.Equal(t, "claude", runs[0].GetActor())

	// A synchronize pushed by the owner (root token) is trusted.
	c.push("agent/x", map[string]string{"y.txt": "y\n"}, "")
	job = c.finish(runner)
	assert.True(t, job.GetTrusted())
	assert.Equal(t, "root", c.runs()[0].GetActor())

	// A synchronize pushed by the limited token is not.
	c.writeFile(c.wt, "z.txt", []byte("z\n"))
	c.mustGit(c.wt, "push", "-q", c.urlAuth(secret, "/proj.git"), "agent/x")
	job = c.finish(runner)
	assert.False(t, job.GetTrusted())
}

func TestPullRefsAreServedHiddenAndNotPushable(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", pullWF("", "    permissions:\n      contents: write"), "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	feat := c.push("feat", map[string]string{"f.txt": "f\n"}, "")
	n := c.openMR(c.root, "feat")
	job := runner.await()
	defer runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
	merge := c.ref(domain.PullMergeRef(n))

	// Fetchable, as checkout does.
	dir := c.t.TempDir()
	c.mustGit(dir, "init", "-q")
	c.mustGit(dir, "fetch", "-q", c.urlAuth(adminToken, "/proj.git"), "refs/pull/1/merge:refs/remotes/pull/1/merge", "refs/pull/1/head:refs/remotes/pull/1/head")
	assert.Equal(t, merge, strings.TrimSpace(c.mustGit(dir, "rev-parse", "refs/remotes/pull/1/merge")))
	assert.Equal(t, feat, strings.TrimSpace(c.mustGit(dir, "rev-parse", "refs/remotes/pull/1/head")))

	// Hidden from the branch and tag lists.
	res, err := c.root.git.ListRefs(c.ctx, connect.NewRequest(&pb.ListRefsRequest{Repo: "proj"}))
	require.NoError(t, err)
	for _, r := range res.Msg.GetRefs() {
		assert.NotContains(t, r.GetName(), "refs/pull/")
	}

	// Refused on push: the owner, a stored token with repo:write, and the job
	// token with contents: write (the workflow guard does not reach it first).
	other, _ := c.branchToken(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite), nil, "proj")
	for _, tc := range []struct{ name, url string }{
		{"owner", c.urlAuth(adminToken, "/proj.git")},
		{"token", c.urlAuth(other, "/proj.git")},
	} {
		for _, ref := range []string{"refs/pull/1/head", "refs/pull/9/head", "refs/pull/1/merge", "refs/pull/x"} {
			out, err := c.git(c.wt, "push", tc.url, "HEAD:"+ref)
			require.Error(t, err, "%s pushes %s: %s", tc.name, ref, out)
		}
		out, err := c.git(c.wt, "push", "--delete", tc.url, "refs/pull/1/merge")
		require.Error(t, err, out)
	}
	out, err := c.tokenPush(c.wt, job.GetGithubToken(), "HEAD:refs/pull/1/head")
	require.Error(t, err, out)
	assert.Equal(t, merge, c.ref(domain.PullMergeRef(n)))
	assert.Equal(t, feat, c.ref(domain.PullHeadRef(n)))
	assert.Empty(t, c.ref("refs/pull/9/head"))
}

func (c *ciHarness) apiGet(token, path string) (int, http.Header, string) {
	c.t.Helper()
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, c.url("/-/github/api"+path), nil)
	require.NoError(c.t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(c.t, err)
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	require.NoError(c.t, err)
	return res.StatusCode, res.Header, string(b)
}

func TestPullFilesEndpoint(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", pullWF("", "    permissions:\n      contents: read\n      pull-requests: read"), "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	files := map[string]string{}
	for i := range 5 {
		files[fmt.Sprintf("d/f%d.txt", i)] = fmt.Sprintf("line %d\n", i)
	}
	c.writeFile(c.wt, "d/gone.txt", []byte("x\n"))
	for name, content := range files {
		c.writeFile(c.wt, name, []byte(content))
	}
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), "feat")
	c.openMR(c.root, "feat")
	job := runner.await() // the job token lives while the job runs
	defer runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")

	type entry struct {
		Filename, Status, Sha, Patch  string
		Additions, Deletions, Changes int
	}
	list := func(token, q string) (int, http.Header, []entry) {
		code, hdr, body := c.apiGet(token, "/repos/owner/proj/pulls/1/files"+q)
		var es []entry
		if code == http.StatusOK {
			require.NoError(t, json.Unmarshal([]byte(body), &es), body)
		}
		return code, hdr, es
	}

	code, hdr, es := list(adminToken, "?per_page=4&page=1")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, es, 4)
	assert.Equal(t, "d/f0.txt", es[0].Filename)
	assert.Equal(t, "added", es[0].Status)
	assert.Equal(t, 1, es[0].Additions)
	assert.Equal(t, 1, es[0].Changes)
	assert.Equal(t, "@@ -0,0 +1 @@\n+line 0", es[0].Patch)
	assert.Len(t, es[0].Sha, 40)
	link := hdr.Get("Link")
	assert.Contains(t, link, `page=2>; rel="next"`)
	assert.Contains(t, link, `page=2>; rel="last"`)
	assert.NotContains(t, link, `rel="prev"`)

	code, hdr, es = list(adminToken, "?per_page=4&page=2")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, es, 2, "6 files in all")
	assert.NotContains(t, hdr.Get("Link"), `rel="next"`)
	assert.Contains(t, hdr.Get("Link"), `rel="prev"`)
	assert.Contains(t, hdr.Get("Link"), `rel="first"`)

	code, _, es = list(adminToken, "?page=9")
	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, es)
	for _, q := range []string{"?page=9223372036854775807", "?per_page=100&page=9223372036854775807", "?page=2305843009213693953&per_page=4"} {
		code, _, es = list(adminToken, q)
		require.Equal(t, http.StatusOK, code, q)
		assert.Empty(t, es, q)
	}
	code, hdr, es = list(adminToken, "")
	require.Equal(t, http.StatusOK, code)
	assert.Len(t, es, 6, "30 per page by default")
	assert.Empty(t, hdr.Get("Link"))
	code, _, es = list(adminToken, "?per_page=1000")
	require.Equal(t, http.StatusOK, code)
	assert.Len(t, es, 6, "100 at most")

	// The job token with pull-requests: read; the answer is 404 for a request
	// that does not exist and for anonymous callers on a private repository.
	code, _, es = list(job.GetGithubToken(), "")
	require.Equal(t, http.StatusOK, code)
	assert.Len(t, es, 6)
	code, _, _ = c.apiGet(job.GetGithubToken(), "/repos/owner/proj/pulls/7/files")
	assert.Equal(t, http.StatusNotFound, code)
	code, _, _ = c.apiGet("", "/repos/owner/proj/pulls/1/files")
	assert.Equal(t, http.StatusNotFound, code)
	code, _, _ = c.apiGet(adminToken, "/repos/owner/proj/pulls/1")
	assert.Equal(t, http.StatusNotFound, code, "GET /pulls/{n} is not served: paths-filter does not call it")
}

// A job token with pull-requests: read and contents: none reads the merge
// requests of its own private repository, and only of that one.
func TestPullFilesWithPullRequestsPermissionOnly(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", pullWF("", "    permissions:\n      contents: none\n      pull-requests: read"), "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	c.push("feat", map[string]string{"f.txt": "f\n"}, "")
	c.openMR(c.root, "feat")
	job := runner.await()
	defer runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
	code, _, body := c.apiGet(job.GetGithubToken(), "/repos/owner/proj/pulls/1/files")
	assert.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, body, "f.txt")
	// Not the repository itself (contents: none) and not another repository.
	code, _, _ = c.apiGet(job.GetGithubToken(), "/repos/owner/proj")
	assert.Equal(t, http.StatusNotFound, code)
	_, err := c.forge.Repos.Create(c.ctx, domain.Root(), ports.CreateRepoParams{Name: c.name("other"), Visibility: domain.VisibilityPrivate})
	require.NoError(t, err)
	code, _, _ = c.apiGet(job.GetGithubToken(), "/repos/owner/other/pulls/1/files")
	assert.Equal(t, http.StatusNotFound, code)
}

// The merge requests of a public repository are read by anyone, except by the
// holder of an archive token, which reads one archive and nothing else.
func TestPullFilesOfAPublicRepositoryAreNotReadByAnArchiveToken(t *testing.T) {
	c := newCIHarness(t, nil)
	c.push("main", pullWF("", ""), "")
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	c.push("feat", map[string]string{"f.txt": "f\n"}, "")
	c.openMR(c.root, "feat")
	public := domain.VisibilityPublic
	_, err := c.forge.Repos.Update(c.ctx, domain.Root(), c.name("proj"), ports.UpdateRepoParams{Visibility: &public})
	require.NoError(t, err)

	code, _, body := c.apiGet("", "/repos/owner/proj/pulls/1/files")
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, body, "f.txt", "anyone reads a public repository")

	archive := domain.RepoTokenPrincipal(domain.CIRepoToken{ID: "a", Repo: c.name("proj"), Archive: true, Sha: strings.Repeat("a", 40)})
	require.True(t, archive.ViaJobToken(), "the archive token passes ViaJobToken")
	_, _, err = c.forge.CI.ListPullFiles(c.ctx, archive, "owner", "proj", 1, 1, 30)
	assert.ErrorIs(t, err, domain.ErrNotFound, "but it reads no merge request")
}

func TestPullFilesNeedsPullRequestsPermission(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", pullWF("", ""), "") // the default permissions: contents read only
	c.mustGit(c.wt, "checkout", "-q", "-b", "feat")
	c.push("feat", map[string]string{"f.txt": "f\n"}, "")
	c.openMR(c.root, "feat")
	job := runner.await()
	defer runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "ok")
	code, _, body := c.apiGet(job.GetGithubToken(), "/repos/owner/proj/pulls/1/files")
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Contains(t, body, "Resource not accessible by integration")
}
