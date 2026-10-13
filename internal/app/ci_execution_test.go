// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

// CI-2 execution (phase E1): the job repository token, the git URLs of the
// owner, the REST subset and the resolution of actions.

// fakeRemote stands in for github.com: no test reaches the network. It is the
// generated mock of ports.GitRemote answering from a table the test edits.
type fakeRemote struct {
	m     *mocks.GitRemote
	mu    sync.Mutex
	refs  map[string][]ports.RemoteRef
	err   map[string]error
	calls []string
}

func newFakeRemote(t *testing.T) *fakeRemote {
	t.Helper()
	f := &fakeRemote{m: mocks.NewGitRemote(t), refs: map[string][]ports.RemoteRef{}, err: map[string]error{}}
	f.m.EXPECT().ListRefs(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, owner, repo string) ([]ports.RemoteRef, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			key := strings.ToLower(owner + "/" + repo)
			f.calls = append(f.calls, key)
			if err := f.err[key]; err != nil {
				return nil, err
			}
			refs, ok := f.refs[key]
			if !ok {
				return nil, domain.ErrNotFound
			}
			return refs, nil
		}).Maybe()
	return f
}

func (f *fakeRemote) set(repo string, refs ...ports.RemoteRef) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs[repo] = refs
}

func (f *fakeRemote) asked(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.calls, key)
}

func (f *fakeRemote) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// newExecHarness is newCIHarness with a fake github.com.
func newExecHarness(t *testing.T, remote *fakeRemote, tweak func(*usecase.CIConfig)) *ciHarness {
	t.Helper()
	h := newHarnessWith(t, harnessOpts{}, func(o *options) { o.ci, o.remoteLister = tweak, remote.m })
	c := &ciHarness{harness: h, root: h.api(adminToken), repo: "proj"}
	_, err := h.forge.Repos.Create(h.ctx, domain.Root(), ports.CreateRepoParams{Name: h.name("proj"), Visibility: domain.VisibilityPrivate})
	require.NoError(t, err)
	c.wt = h.workTree("main")
	return c
}

// startJob pushes a workflow and returns the job given to a runner.
func (c *ciHarness) startJob(runner *fakeRunner) *pb.Job {
	c.t.Helper()
	c.push("main", map[string]string{".github/workflows/ci.yml": strings.Split(ciWorkflow, "  test:")[0], "README": "hello\n"}, "")
	return runner.await()
}

func (c *ciHarness) heartbeat(runner *fakeRunner, job *pb.Job) {
	c.t.Helper()
	_, err := runner.jobAPI(job).runner.Heartbeat(c.ctx, connect.NewRequest(&pb.HeartbeatRequest{JobId: job.GetJobId()}))
	require.NoError(c.t, err)
}

// checkoutHeader is what actions/checkout sets in http.extraheader.
func checkoutHeader(token string) string {
	return "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

// clone clones url as actions/checkout does: the token in http.extraheader.
func (c *ciHarness) clone(token, url string, args ...string) (string, error) {
	dest := filepath.Join(c.t.TempDir(), "w")
	a := []string{}
	if token != "" {
		a = append(a, "-c", "http.extraheader="+checkoutHeader(token))
	}
	a = append(a, "clone", "-q")
	a = append(a, args...)
	a = append(a, url, dest)
	return c.git(c.t.TempDir(), a...)
}

func (c *ciHarness) get(path, authorization string) (int, string, http.Header) {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.url(path), nil)
	require.NoError(c.t, err)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(c.t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(c.t, err)
	return resp.StatusCode, string(b), resp.Header
}

func basic(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

// AC3: the job gets a repository token at its assignment, in the Job and not in
// the contexts; it clones its repository, public or private, over the URL
// github.repository gives, with and without .git, the way checkout does.
func TestJobTokenClonesTheRepositoryOfTheRun(t *testing.T) {
	c := newExecHarness(t, newFakeRemote(t), nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	job := c.startJob(runner)
	tok := job.GetGithubToken()
	require.True(t, domain.IsRepoToken(tok), "ermitegh_ and 43 base64url characters")
	assert.NotEqual(t, job.GetJobToken(), tok)
	assert.NotContains(t, job.GetContexts(), tok, "the token does not sit in the contexts the runner stores")
	assert.NotContains(t, job.GetWorkflowYaml(), tok)
	c.heartbeat(runner, job)

	// Private: checkout's clone, shallow, without .git.
	out, err := c.clone(tok, c.url("/owner/proj"), "--depth", "1")
	require.NoError(t, err, out)
	out, err = c.clone(tok, c.url("/owner/proj.git"))
	require.NoError(t, err, out)
	out, err = c.clone(tok, c.url("/proj.git"))
	require.NoError(t, err, "the plain URL works too: "+out)
	// Without a token a private repository asks for credentials.
	_, err = c.clone("", c.url("/owner/proj"))
	require.Error(t, err)
	// Another owner has no repositories.
	out, err = c.clone(tok, c.url("/someone/proj"))
	require.Error(t, err, out)

	// The same token and the other schemes, on the wire.
	for _, h := range []string{basic(tok), "Bearer " + tok, "token " + tok} {
		status, _, _ := c.get("/owner/proj/info/refs?service=git-upload-pack", h)
		assert.Equal(t, http.StatusOK, status, h[:12])
	}

	// Public: anyone clones, the token too.
	vis := domain.VisibilityPublic
	_, err = c.forge.Repos.Update(c.ctx, domain.Root(), c.h("proj"), ports.UpdateRepoParams{Visibility: &vis})
	require.NoError(t, err)
	out, err = c.clone("", c.url("/owner/proj"))
	require.NoError(t, err, out)
	out, err = c.clone(tok, c.url("/owner/proj"))
	require.NoError(t, err, out)
}

func (c *ciHarness) h(name string) domain.RepoName { return c.name(name) }

// AC3: refused on another private repository, for push, and for the REST
// subset of another repository; never a stored token for the API.
func TestJobTokenReadsItsRepositoryOnly(t *testing.T) {
	c := newExecHarness(t, newFakeRemote(t), nil)
	c.seed("secret", domain.VisibilityPrivate, "a.txt")
	runner := c.registerRunner("alpha", "ubuntu-latest")
	job := c.startJob(runner)
	tok := job.GetGithubToken()
	c.heartbeat(runner, job)

	out, err := c.clone(tok, c.url("/owner/secret"))
	require.Error(t, err)
	assert.Contains(t, out, "not found", "the other private repository does not show itself")
	status, _, _ := c.get("/owner/secret/info/refs?service=git-upload-pack", basic(tok))
	assert.Equal(t, http.StatusNotFound, status)
	status, _, _ = c.get("/-/github/api/repos/owner/secret", basic(tok))
	assert.Equal(t, http.StatusNotFound, status)

	// Push: the receive-pack advertisement and the push itself are 403.
	status, _, _ = c.get("/owner/proj/info/refs?service=git-receive-pack", basic(tok))
	assert.Equal(t, http.StatusForbidden, status)
	c.mustGit(c.wt, "checkout", "-q", "-b", "x")
	out, err = c.git(c.wt, "-c", "http.extraheader="+checkoutHeader(tok), "push", c.url("/owner/proj"), "x:refs/heads/x")
	require.Error(t, err)
	assert.Contains(t, out, "403")
	assert.Empty(t, c.bareGit("proj", "branch", "--list", "x"), "nothing was pushed")

	// The API takes it as the principal it is (repo:read on one repository): it
	// lists that repository and no other, and writes nothing.
	list, err := c.api(tok).repo.ListRepos(c.ctx, connect.NewRequest(&pb.ListReposRequest{}))
	require.NoError(t, err)
	require.Len(t, list.Msg.GetRepos(), 1)
	assert.Equal(t, "proj", list.Msg.GetRepos()[0].GetName())
	_, err = c.api(tok).repo.DeleteRepo(c.ctx, connect.NewRequest(&pb.DeleteRepoRequest{Repo: "proj"}))
	assert.Equal(t, connect.CodePermissionDenied, ciErrCode(err))
	_, err = c.api(tok).token.CreateToken(c.ctx, connect.NewRequest(&pb.CreateTokenRequest{Label: "x", Scopes: []pb.Scope{pb.Scope_SCOPE_REPO_READ}}))
	assert.Equal(t, connect.CodePermissionDenied, ciErrCode(err))
	_, err = c.api(tok).token.ListTokens(c.ctx, connect.NewRequest(&pb.ListTokensRequest{}))
	assert.Equal(t, connect.CodePermissionDenied, ciErrCode(err))
	_, err = c.api(tok).run.CancelRun(c.ctx, connect.NewRequest(&pb.CancelRunRequest{Repo: "proj", RunId: c.runs()[0].GetId()}))
	assert.Equal(t, connect.CodePermissionDenied, ciErrCode(err), "no ci:write")

	// A made-up token of the right shape is refused, and so are wrong ones.
	for _, bad := range []string{"ermitegh_" + strings.Repeat("A", 43), "ermitegh_short", tok + "x"} {
		status, _, _ := c.get("/owner/proj/info/refs?service=git-upload-pack", basic(bad))
		assert.Equal(t, http.StatusUnauthorized, status, bad)
	}
}

// The token is not a stored token: ListTokens does not show it, token:admin
// does not manage it, and the principal it gives is read-only on one repository.
func TestJobTokenIsNotAStoredToken(t *testing.T) {
	c := newExecHarness(t, newFakeRemote(t), nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	job := c.startJob(runner)
	tok := job.GetGithubToken()

	before, err := c.root.token.ListTokens(c.ctx, connect.NewRequest(&pb.ListTokensRequest{}))
	require.NoError(t, err)
	assert.Empty(t, before.Msg.GetTokens(), "no stored token exists")

	p, err := c.forge.Auth.Authenticate(c.ctx, tok)
	require.NoError(t, err)
	assert.Equal(t, []domain.Scope{domain.ScopeRepoRead}, p.Scopes())
	assert.Equal(t, []domain.RepoName{c.h("proj")}, p.Repos())
	assert.False(t, p.ExpiresAt().IsZero())
	assert.False(t, p.HasScope(domain.ScopeTokenAdmin))
	assert.False(t, p.IsRoot())

	// Revoking by its id (the "job:" id the principal shows) finds nothing.
	_, err = c.root.token.RevokeToken(c.ctx, connect.NewRequest(&pb.RevokeTokenRequest{Id: strings.TrimPrefix(p.TokenID(), "job:")}))
	assert.Equal(t, connect.CodeNotFound, ciErrCode(err))
	_, err = c.root.token.RevokeToken(c.ctx, connect.NewRequest(&pb.RevokeTokenRequest{Id: p.TokenID()}))
	assert.Equal(t, connect.CodeNotFound, ciErrCode(err))
	_, err = c.forge.Auth.Authenticate(c.ctx, tok)
	require.NoError(t, err, "still valid")
	require.NoError(t, c.forge.Auth.Revalidate(c.ctx, p))
}

// AC3: refused after the job ends, however it ends.
func TestJobTokenEndsWithTheJob(t *testing.T) {
	cloneStatus := func(c *ciHarness, tok string) int {
		status, _, _ := c.get("/owner/proj/info/refs?service=git-upload-pack", basic(tok))
		return status
	}

	t.Run("CompleteJob", func(t *testing.T) {
		c := newExecHarness(t, newFakeRemote(t), nil)
		runner := c.registerRunner("alpha", "ubuntu-latest")
		job := c.startJob(runner)
		tok := job.GetGithubToken()
		p, err := c.forge.Auth.Authenticate(c.ctx, tok)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, cloneStatus(c, tok))
		runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "x")
		assert.Equal(t, http.StatusUnauthorized, cloneStatus(c, tok))
		assert.ErrorIs(t, c.forge.Auth.Revalidate(c.ctx, p), domain.ErrUnauthenticated, "a long operation stops")
		out, err := c.clone(tok, c.url("/owner/proj"))
		require.Error(t, err, out)
	})

	t.Run("cancel", func(t *testing.T) {
		c := newExecHarness(t, newFakeRemote(t), func(cfg *usecase.CIConfig) {
			cfg.CancelGrace, cfg.Tick, cfg.PollTimeout = 300*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond
		})
		runner := c.registerRunner("alpha", "ubuntu-latest")
		job := c.startJob(runner)
		c.heartbeat(runner, job)
		tok := job.GetGithubToken()
		assert.Equal(t, http.StatusOK, cloneStatus(c, tok))
		_, err := c.root.run.CancelRun(c.ctx, connect.NewRequest(&pb.CancelRunRequest{Repo: "proj", RunId: c.runs()[0].GetId()}))
		require.NoError(t, err)
		// The runner is told to stop, and the job concludes when it answers or
		// when the grace ends: either way the token goes with it.
		c.waitFor("the token is revoked", func() bool { return cloneStatus(c, tok) == http.StatusUnauthorized })
		assert.Equal(t, pb.CiStatus_CI_STATUS_COMPLETED, c.runs()[0].GetStatus())
	})

	t.Run("lost runner", func(t *testing.T) {
		c := newExecHarness(t, newFakeRemote(t), func(cfg *usecase.CIConfig) {
			cfg.HeartbeatDeadline, cfg.Tick, cfg.PollTimeout = 300*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond
		})
		runner := c.registerRunner("alpha", "ubuntu-latest")
		job := c.startJob(runner)
		c.heartbeat(runner, job)
		tok := job.GetGithubToken()
		assert.Equal(t, http.StatusOK, cloneStatus(c, tok))
		c.waitFor("the token is revoked", func() bool { return cloneStatus(c, tok) == http.StatusUnauthorized })
		assert.Contains(t, c.getRun(c.runs()[0].GetId()).GetJobs()[0].GetError(), "lost communication")
	})

	t.Run("assignment not acknowledged", func(t *testing.T) {
		c := newExecHarness(t, newFakeRemote(t), func(cfg *usecase.CIConfig) {
			cfg.AckTimeout, cfg.Tick, cfg.PollTimeout = 200*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond
		})
		runner := c.registerRunner("alpha", "ubuntu-latest")
		job := c.startJob(runner)
		tok := job.GetGithubToken()
		c.waitFor("the assignment is taken back", func() bool { return cloneStatus(c, tok) == http.StatusUnauthorized })
		again := runner.await()
		assert.NotEqual(t, tok, again.GetGithubToken(), "the next assignment has its own token")
		assert.Equal(t, http.StatusOK, cloneStatus(c, again.GetGithubToken()))
		assert.Equal(t, http.StatusUnauthorized, cloneStatus(c, tok), "the old one stays revoked")
	})

	t.Run("repository deleted", func(t *testing.T) {
		c := newExecHarness(t, newFakeRemote(t), nil)
		runner := c.registerRunner("alpha", "ubuntu-latest")
		job := c.startJob(runner)
		tok := job.GetGithubToken()
		_, err := c.forge.Auth.Authenticate(c.ctx, tok)
		require.NoError(t, err)
		require.NoError(t, c.forge.Repos.Delete(c.ctx, domain.Root(), c.h("proj")))
		_, err = c.forge.Auth.Authenticate(c.ctx, tok)
		assert.ErrorIs(t, err, domain.ErrUnauthenticated, "the repository, its runs and their tokens are gone")
		assert.Equal(t, http.StatusUnauthorized, cloneStatus(c, tok))
	})

	t.Run("expiry", func(t *testing.T) {
		// The deadline of the job plus five minutes: a job of one minute.
		c := newExecHarness(t, newFakeRemote(t), nil)
		runner := c.registerRunner("alpha", "ubuntu-latest")
		c.push("main", map[string]string{".github/workflows/t.yml": "on: push\njobs:\n  j:\n    runs-on: ubuntu-latest\n    timeout-minutes: 1\n    steps:\n      - run: echo\n"}, "")
		job := runner.await()
		p, err := c.forge.Auth.Authenticate(c.ctx, job.GetGithubToken())
		require.NoError(t, err)
		want := time.Now().Add(time.Minute + domain.CITimeoutSlack + domain.RepoTokenGrace)
		assert.WithinDuration(t, want, p.ExpiresAt(), 5*time.Second)
	})
}

// The retry of a poll gets the token of the first call, and a job token of the
// protocol never doubles as the repository token.
func TestJobTokenSurvivesAPollRetry(t *testing.T) {
	c := newExecHarness(t, newFakeRemote(t), nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/ci.yml": strings.Split(ciWorkflow, "  test:")[0]}, "")
	var first *pb.Job
	c.waitFor("a job", func() bool {
		j, err := runner.fetch("poll-1")
		require.NoError(t, err)
		first = j
		return j != nil
	})
	again, err := runner.fetch("poll-1")
	require.NoError(t, err)
	require.NotNil(t, again)
	assert.Equal(t, first.GetJobToken(), again.GetJobToken())
	assert.Equal(t, first.GetGithubToken(), again.GetGithubToken())
	status, _, _ := c.get("/owner/proj/info/refs?service=git-upload-pack", basic(first.GetJobToken()))
	assert.Equal(t, http.StatusUnauthorized, status, "the runner's job token reads nothing")
}

// The REST subset: GitHub's shapes for what checkout and github-script call,
// GitHub's 404 for everything else.
func TestGitHubRESTSubset(t *testing.T) {
	c := newExecHarness(t, newFakeRemote(t), nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	job := c.startJob(runner)
	tok := job.GetGithubToken()
	sha := strings.TrimSpace(c.mustGit(c.wt, "rev-parse", "HEAD"))
	notFound := `{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`

	// Private: the token reads it, anonymous sees nothing.
	status, body, hdr := c.get("/-/github/api/repos/owner/proj", basic(tok))
	require.Equal(t, http.StatusOK, status, body)
	assert.Contains(t, hdr.Get("Content-Type"), "application/json")
	var repo map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &repo))
	assert.Equal(t, "proj", repo["name"])
	assert.Equal(t, "owner/proj", repo["full_name"])
	assert.Equal(t, true, repo["private"])
	assert.Equal(t, "private", repo["visibility"])
	assert.Equal(t, "main", repo["default_branch"])
	assert.Equal(t, "http://example.test/owner/proj", repo["html_url"])
	assert.Equal(t, "http://example.test/owner/proj.git", repo["clone_url"])
	assert.Equal(t, "http://example.test/-/github/api/repos/owner/proj", repo["url"])
	assert.NotZero(t, repo["id"])
	assert.NotEmpty(t, repo["node_id"])
	owner, _ := repo["owner"].(map[string]any)
	assert.Equal(t, "owner", owner["login"])
	assert.Equal(t, "User", owner["type"])
	assert.Equal(t, false, repo["fork"])
	assert.Equal(t, false, repo["archived"])
	assert.Equal(t, false, repo["has_issues"] == nil, "present")
	assert.Regexp(t, `^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$`, repo["created_at"], "REST dates are ISO 8601")
	assert.Regexp(t, `^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$`, repo["pushed_at"])
	perms, _ := repo["permissions"].(map[string]any)
	assert.Equal(t, true, perms["pull"])
	assert.Equal(t, false, perms["push"], "the job token cannot write")
	assert.Equal(t, false, perms["admin"])
	assert.NotContains(t, repo, "master_branch", "the push event has it, the REST object does not")

	status, body, _ = c.get("/-/github/api/repos/owner/proj", "")
	assert.Equal(t, http.StatusNotFound, status, "a private repository does not show itself")
	assert.JSONEq(t, notFound, body)
	status, _, _ = c.get("/-/github/api/repos/Owner/PROJ", basic(tok))
	assert.Equal(t, http.StatusOK, status, "names are case-insensitive as on GitHub")
	status, _, _ = c.get("/-/github/api/repos/other/proj", basic(tok))
	assert.Equal(t, http.StatusNotFound, status, "another owner")

	status, body, _ = c.get("/-/github/api/repos/owner/proj/hash-algorithm", basic(tok))
	assert.Equal(t, http.StatusOK, status)
	assert.JSONEq(t, `{"hash_algorithm":"sha1"}`, body)
	status, _, _ = c.get("/-/github/api/repos/owner/proj/hash-algorithm", "")
	assert.Equal(t, http.StatusNotFound, status)

	// The tarball: a gzip tar with one top-level directory, the contents of the
	// commit, for a branch, a tag and a full id alike.
	c.mustGit(c.wt, "tag", "v1")
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/proj.git"), "v1")
	for _, ref := range []string{"main", "v1", sha, ""} {
		status, body, hdr := c.get("/-/github/api/repos/owner/proj/tarball/"+ref, basic(tok))
		require.Equal(t, http.StatusOK, status, ref)
		assert.Contains(t, hdr.Get("Content-Disposition"), "owner-proj-"+sha[:7]+".tar.gz")
		files := untar(t, body)
		assert.Equal(t, "hello\n", files["owner-proj-"+sha[:7]+"/README"], ref)
	}
	status, body, _ = c.get("/-/github/api/repos/owner/proj/tarball/nope", basic(tok))
	assert.Equal(t, http.StatusNotFound, status)
	assert.JSONEq(t, notFound, body)
	status, _, _ = c.get("/-/github/api/repos/owner/proj/tarball/main", "")
	assert.Equal(t, http.StatusNotFound, status, "private and anonymous")
	status, _, _ = c.get("/-/github/api/repos/owner/proj/tarball/main", "Bearer "+adminToken)
	assert.Equal(t, http.StatusOK, status, "any principal that reads the repository")

	// A public repository answers anonymous callers.
	vis := domain.VisibilityPublic
	_, err := c.forge.Repos.Update(c.ctx, domain.Root(), c.h("proj"), ports.UpdateRepoParams{Visibility: &vis})
	require.NoError(t, err)
	status, body, _ = c.get("/-/github/api/repos/owner/proj", "")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `"visibility":"public"`)
	assert.NotContains(t, body, `"permissions"`, "an anonymous caller has none")
	status, _, _ = c.get("/-/github/api/repos/owner/proj/tarball/main", "")
	assert.Equal(t, http.StatusOK, status)

	// Everything else is GitHub's 404, whatever the method.
	for _, p := range []string{
		"/-/github/api", "/-/github/api/", "/-/github/api/user", "/-/github/api/graphql",
		"/-/github/api/repos/owner/proj/issues", "/-/github/api/repos/owner/proj/git/refs/heads/main",
		"/-/github/api/repos/owner/proj/zipball/main", "/-/github/api/repos/owner/proj/actions/runs",
		"/-/github/api/repos/owner", "/-/github/api/rate_limit", "/-/github/api/repos/owner/proj/contents/README",
	} {
		status, body, hdr := c.get(p, basic(tok))
		assert.Equal(t, http.StatusNotFound, status, p)
		assert.JSONEq(t, notFound, body, p)
		assert.Contains(t, hdr.Get("Content-Type"), "application/json", p)
	}
	req, err := http.NewRequest(http.MethodPost, c.url("/-/github/api/repos/owner/proj"), strings.NewReader("{}"))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)

	// A bad credential is refused as GitHub does.
	status, body, _ = c.get("/-/github/api/repos/owner/proj", basic("ermitegh_"+strings.Repeat("A", 43)))
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Contains(t, body, "Bad credentials")
}

func untar(t *testing.T, gz string) map[string]string {
	t.Helper()
	zr, err := gzip.NewReader(strings.NewReader(gz))
	require.NoError(t, err)
	tr := tar.NewReader(zr)
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if h.Typeflag != tar.TypeReg {
			continue
		}
		var b bytes.Buffer
		_, err = io.Copy(&b, tr)
		require.NoError(t, err)
		files[h.Name] = b.String()
	}
	return files
}

func (c *ciHarness) resolve(runner *fakeRunner, job *pb.Job, uses ...string) []*pb.ResolvedAction {
	c.t.Helper()
	res, err := c.resolveErr(runner, job, uses...)
	require.NoError(c.t, err)
	return res
}

func (c *ciHarness) resolveErr(runner *fakeRunner, job *pb.Job, uses ...string) ([]*pb.ResolvedAction, error) {
	refs := make([]*pb.ActionRef, 0, len(uses))
	for _, u := range uses {
		refs = append(refs, &pb.ActionRef{Uses: u})
	}
	res, err := runner.jobAPI(job).runner.ResolveActions(c.ctx, connect.NewRequest(&pb.ResolveActionsRequest{JobId: job.GetJobId(), Refs: refs}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetActions(), nil
}

const (
	shaMain   = "1111111111111111111111111111111111111111"
	shaTag    = "2222222222222222222222222222222222222222"
	shaTagObj = "3333333333333333333333333333333333333333"
	shaPeeled = "4444444444444444444444444444444444444444"
	shaOther  = "5555555555555555555555555555555555555555"
)

// AC2, AC4: github.com actions are resolved with ls-remote (tag before branch,
// the peeled commit of an annotated tag, a full id as it is), ermite
// repositories win, and nothing reaches the network but through the fake.
func TestResolveActionsFromGitHub(t *testing.T) {
	remote := newFakeRemote(t)
	remote.set("actions/checkout",
		ports.RemoteRef{Name: "refs/heads/main", ID: shaMain},
		ports.RemoteRef{Name: "refs/tags/v4", ID: shaTagObj, Peeled: shaPeeled},
		ports.RemoteRef{Name: "refs/tags/v3", ID: shaTag},
		ports.RemoteRef{Name: "refs/heads/v3", ID: shaOther}, // a tag and a branch of the same name: the tag
	)
	c := newExecHarness(t, remote, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	job := c.startJob(runner)
	c.heartbeat(runner, job)

	got := c.resolve(runner, job,
		"actions/checkout@v4", "actions/checkout@main", "actions/checkout@v3", "Actions/Checkout/sub/dir@v4",
		"actions/checkout@"+strings.ToUpper(shaOther), "nobody/unknown-action@"+shaOther, "actions/checkout@v4",
	)
	require.Len(t, got, 7)
	assert.Equal(t, "actions/checkout@v4", got[0].GetRef())
	assert.Equal(t, "actions/checkout", got[0].GetNameWithOwner())
	assert.Equal(t, shaPeeled, got[0].GetResolvedSha(), "an annotated tag gives its commit")
	assert.Equal(t, "https://codeload.github.com/actions/checkout/tar.gz/"+shaPeeled, got[0].GetTarballUrl())
	assert.Equal(t, pb.ActionSource_ACTION_SOURCE_GITHUB, got[0].GetSource())
	assert.Empty(t, got[0].GetToken(), "no token goes to github.com")
	assert.Empty(t, got[0].GetError())
	assert.Equal(t, shaMain, got[1].GetResolvedSha(), "a branch")
	assert.Equal(t, shaTag, got[2].GetResolvedSha(), "a tag wins over a branch of the same name")
	assert.Equal(t, "Actions/Checkout/sub/dir@v4", got[3].GetRef(), "the reference is returned as written")
	assert.Equal(t, "Actions/Checkout", got[3].GetNameWithOwner())
	assert.Equal(t, "sub/dir", got[3].GetPath())
	assert.Equal(t, shaPeeled, got[3].GetResolvedSha(), "owner and repository are case-insensitive")
	assert.Equal(t, strings.ToLower(shaOther), got[4].GetResolvedSha(), "a full commit id is not checked")
	assert.Equal(t, strings.ToLower(shaOther), got[5].GetResolvedSha(), "not even for a repository ls-remote does not know")
	assert.Equal(t, got[0].GetResolvedSha(), got[6].GetResolvedSha(), "duplicates are answered one by one")
	assert.Equal(t, 1, remote.count(), "one ls-remote per repository and call")

	// GitHub's words for what cannot be resolved; the call itself succeeds.
	bad := c.resolve(runner, job, "actions/checkout@nope", "nobody/missing@v1", "noslash@v1", "a/b", "a/b@", "a/b@c@d")
	assert.Equal(t, "Unable to resolve action `actions/checkout@nope`, unable to find version `nope`", bad[0].GetError())
	assert.Equal(t, "Unable to resolve action `nobody/missing@v1`, repository not found", bad[1].GetError())
	assert.Equal(t, "Expected format {org}/{repo}[/path]@ref. Actual 'noslash@v1'", bad[2].GetError())
	assert.Equal(t, "Expected format {org}/{repo}[/path]@ref. Actual 'a/b'", bad[3].GetError())
	assert.NotEmpty(t, bad[4].GetError())
	assert.NotEmpty(t, bad[5].GetError())
	for _, b := range bad {
		assert.Empty(t, b.GetResolvedSha(), b.GetRef())
		assert.Empty(t, b.GetTarballUrl(), b.GetRef())
		assert.Equal(t, pb.ActionSource_ACTION_SOURCE_UNSPECIFIED, b.GetSource(), b.GetRef())
	}

	// github.com unreachable is an error of the call, not of one action.
	remote.err["flaky/action"] = domain.ErrUnavailable
	_, err := c.resolveErr(runner, job, "flaky/action@v1")
	assert.Equal(t, connect.CodeUnavailable, ciErrCode(err))
}

func TestResolveActionsRejectsWhatTheRunnerHandles(t *testing.T) {
	c := newExecHarness(t, newFakeRemote(t), nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	job := c.startJob(runner)
	c.heartbeat(runner, job)
	for _, u := range []string{"./local", "./.github/actions/x", "docker://alpine:3", "actions/checkout@v4"} {
		_, err := c.resolveErr(runner, job, "actions/checkout@"+shaMain, u)
		if u == "actions/checkout@v4" {
			assert.NoError(t, err)
			continue
		}
		assert.Equal(t, connect.CodeInvalidArgument, ciErrCode(err), u)
	}
	many := make([]string, 101)
	for i := range many {
		many[i] = "a/b@" + shaMain
	}
	_, err := c.resolveErr(runner, job, many...)
	assert.Equal(t, connect.CodeInvalidArgument, ciErrCode(err), "at most 100")
	many = many[:100]
	got, err := c.resolveErr(runner, job, many...)
	require.NoError(t, err)
	assert.Len(t, got, 100)
	got, err = c.resolveErr(runner, job)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// AC2: another job's token, a repository token, an ended job and a bad token
// are refused.
func TestResolveActionsAuthentication(t *testing.T) {
	c := newExecHarness(t, newFakeRemote(t), nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	job := c.startJob(runner)
	c.heartbeat(runner, job)
	call := func(secret, id string) error {
		_, err := c.api(secret).runner.ResolveActions(c.ctx, connect.NewRequest(&pb.ResolveActionsRequest{JobId: id, Refs: []*pb.ActionRef{{Uses: "a/b@" + shaMain}}}))
		return err
	}
	assert.NoError(t, call(job.GetJobToken(), job.GetJobId()))
	assert.Equal(t, connect.CodeUnauthenticated, ciErrCode(call(job.GetGithubToken(), job.GetJobId())), "the repository token is not a job token")
	assert.Equal(t, connect.CodeUnauthenticated, ciErrCode(call(runner.secret, job.GetJobId())), "nor the runner token")
	assert.Equal(t, connect.CodeUnauthenticated, ciErrCode(call("ermitejob_"+strings.Repeat("A", 43), job.GetJobId())))
	assert.Equal(t, connect.CodeUnauthenticated, ciErrCode(call("", job.GetJobId())))
	assert.Equal(t, connect.CodePermissionDenied, ciErrCode(call(job.GetJobToken(), "7-0-0")), "a token of another job")

	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "x")
	err := call(job.GetJobToken(), job.GetJobId())
	assert.Equal(t, connect.CodeFailedPrecondition, ciErrCode(err), "an ended job")
	var ce *connect.Error
	require.ErrorAs(t, err, &ce)
	assert.NotEmpty(t, ce.Details(), "with a JobEnded detail")
}

// AC4: an action of a repository of ermite wins over github.com, is downloaded
// with a per-job read token for that repository only, and the same ref gives the
// same commit to every job of the attempt (and a new one to a re-run).
func TestResolveActionsFromErmite(t *testing.T) {
	remote := newFakeRemote(t)
	remote.set("owner/myaction", ptrRef("refs/heads/main", shaMain)) // shadowed by ermite
	c := newExecHarness(t, remote, nil)
	c.seed("myaction", domain.VisibilityPrivate, "action.yml", "dist/index.js")
	tag := c.bareGit("myaction", "rev-parse", "HEAD")
	c.mustGit(c.repoDir("myaction"), "tag", "v1")
	c.mustGit(c.repoDir("myaction"), "tag", "-a", "-m", "annotated", "v2")
	c.mustGit(c.repoDir("myaction"), "branch", "dev")
	c.seed("hidden", domain.VisibilityPrivate, "secret.txt")

	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/m.yml": "on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n  b:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n"}, "")
	a, b := runner.await(), runner.await()
	c.heartbeat(runner, a)
	c.heartbeat(runner, b)

	got := c.resolve(runner, a, "owner/myaction@v1", "owner/MyAction/sub@v2", "owner/myaction@dev", "owner/myaction@"+tag, "owner/myaction@gone", "owner/nothere@v1")
	require.Len(t, got, 6)
	for i := range 4 {
		assert.Empty(t, got[i].GetError(), got[i].GetRef())
		assert.Equal(t, tag, got[i].GetResolvedSha(), got[i].GetRef())
		assert.Equal(t, pb.ActionSource_ACTION_SOURCE_ERMITE, got[i].GetSource(), got[i].GetRef())
		assert.True(t, domain.IsRepoToken(got[i].GetToken()), got[i].GetRef())
		assert.Equal(t, "http://example.test/-/github/api/repos/owner/myaction/tarball/"+tag, got[i].GetTarballUrl())
	}
	assert.Equal(t, "owner/myaction", got[0].GetNameWithOwner())
	assert.Equal(t, "sub", got[1].GetPath())
	assert.Equal(t, got[0].GetToken(), got[2].GetToken(), "one token per repository and job")
	assert.Contains(t, got[4].GetError(), "unable to find version `gone`")
	assert.False(t, remote.asked("owner/myaction"), "ermite is asked first, github.com not at all")
	assert.Contains(t, got[5].GetError(), "Unable to resolve action `owner/nothere@v1`")
	assert.True(t, remote.asked("owner/nothere"), "an owner repository that ermite lacks falls through to github.com")

	// The archive token downloads that repository, whatever the private
	// repository of the run's token reaches, and nothing else.
	arch := got[0].GetToken()
	status, body, _ := c.get("/-/github/api/repos/owner/myaction/tarball/"+tag, basic(arch))
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "content of dist/index.js\n", untar(t, body)["owner-myaction-"+tag[:7]+"/dist/index.js"])
	status, _, _ = c.get("/owner/myaction/info/refs?service=git-upload-pack", basic(arch))
	assert.Equal(t, http.StatusOK, status, "and clones it")
	status, _, _ = c.get("/owner/myaction/info/refs?service=git-receive-pack", basic(arch))
	assert.Equal(t, http.StatusForbidden, status, "never writes")
	status, _, _ = c.get("/owner/hidden/info/refs?service=git-upload-pack", basic(arch))
	assert.Equal(t, http.StatusNotFound, status, "another private repository")
	status, _, _ = c.get("/owner/proj/info/refs?service=git-upload-pack", basic(arch))
	assert.Equal(t, http.StatusNotFound, status, "not even the repository of the run")
	status, _, _ = c.get("/-/github/api/repos/owner/hidden", basic(arch))
	assert.Equal(t, http.StatusNotFound, status)
	// The job's own token does not read the action repository: this is why the
	// archive has a token of its own.
	status, _, _ = c.get("/-/github/api/repos/owner/myaction/tarball/"+tag, basic(a.GetGithubToken()))
	assert.Equal(t, http.StatusNotFound, status)

	// Per job: the same commit, another token.
	other := c.resolve(runner, b, "owner/myaction@v1")
	assert.Equal(t, tag, other[0].GetResolvedSha())
	assert.NotEqual(t, arch, other[0].GetToken(), "tokens are not shared between jobs")

	// Stable for the attempt: the branch moves, the answer does not.
	c.mustGit(c.wt, "checkout", "-q", "-b", "scratch")
	c.writeFile(c.wt, "later.txt", []byte("later"))
	c.mustGit(c.wt, "push", "-q", c.urlAuth(adminToken, "/myaction.git"), "scratch:refs/heads/dev", "-f")
	moved := c.bareGit("myaction", "rev-parse", "dev")
	require.NotEqual(t, tag, moved)
	again := c.resolve(runner, b, "owner/myaction@dev")
	assert.Equal(t, tag, again[0].GetResolvedSha(), "the first resolution of the attempt stands")

	// The token goes with its job.
	runner.runJob(a, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "x")
	status, _, _ = c.get("/-/github/api/repos/owner/myaction/tarball/"+tag, basic(arch))
	assert.Equal(t, http.StatusUnauthorized, status, "revoked with the job")
	status, _, _ = c.get("/-/github/api/repos/owner/myaction/tarball/"+tag, basic(other[0].GetToken()))
	assert.Equal(t, http.StatusOK, status, "the other job's token lives on")
}

func ptrRef(name, id string) ports.RemoteRef { return ports.RemoteRef{Name: name, ID: id} }

// A re-run resolves again, and github.com is asked once for a repository in
// each attempt.
func TestResolveActionsAreCachedPerAttempt(t *testing.T) {
	remote := newFakeRemote(t)
	remote.set("actions/setup-go", ptrRef("refs/tags/v5", shaMain))
	c := newExecHarness(t, remote, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	job := c.startJob(runner)
	c.heartbeat(runner, job)

	assert.Equal(t, shaMain, c.resolve(runner, job, "actions/setup-go@v5")[0].GetResolvedSha())
	remote.set("actions/setup-go", ptrRef("refs/tags/v5", shaOther))
	assert.Equal(t, shaMain, c.resolve(runner, job, "actions/setup-go@v5")[0].GetResolvedSha(), "stable within the attempt")
	assert.Equal(t, 1, remote.count(), "the second call is answered from the attempt's resolution")
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "x")

	_, err := c.root.run.RerunRun(c.ctx, connect.NewRequest(&pb.RerunRunRequest{Repo: "proj", RunId: c.runs()[0].GetId()}))
	require.NoError(t, err)
	job2 := runner.await()
	assert.Equal(t, int32(2), job2.GetAttempt())
	c.heartbeat(runner, job2)
	assert.Equal(t, shaOther, c.resolve(runner, job2, "actions/setup-go@v5")[0].GetResolvedSha(), "a re-run resolves again")
	assert.NotEqual(t, job.GetGithubToken(), job2.GetGithubToken())
}

// AC2: no secret reaches the logs of the server: the repository token, the
// archive tokens and the job token, whatever happened with them.
func TestRepositoryTokensAreNeverLogged(t *testing.T) {
	remote := newFakeRemote(t)
	c := newExecHarness(t, remote, nil)
	c.seed("myaction", domain.VisibilityPrivate, "action.yml")
	runner := c.registerRunner("alpha", "ubuntu-latest")
	job := c.startJob(runner)
	c.heartbeat(runner, job)
	tok := job.GetGithubToken()

	arch := c.resolve(runner, job, "owner/myaction@main")[0]
	require.NotEmpty(t, arch.GetToken())
	_, err := c.clone(tok, c.url("/owner/proj"))
	require.NoError(t, err)
	c.get("/-/github/api/repos/owner/proj", basic(tok))
	c.get("/-/github/api/repos/owner/proj/tarball/main", "Bearer "+tok)
	c.get("/owner/proj/info/refs?service=git-receive-pack", basic(tok))
	c.get("/owner/secret/info/refs?service=git-upload-pack", basic("ermitegh_"+strings.Repeat("Z", 43))) // a wrong one
	runner.runJob(job, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil, "x")
	c.get("/owner/proj/info/refs?service=git-upload-pack", basic(tok))

	logs := c.logs.String()
	require.NotEmpty(t, logs)
	for _, secret := range []string{tok, arch.GetToken(), job.GetJobToken(), base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))} {
		assert.NotContains(t, logs, secret)
	}
	assert.NotContains(t, logs, strings.Repeat("Z", 43))
}
