// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/domain"
)

func scopes(s ...domain.Scope) []domain.Scope { return s }

// publicRepo creates a public repository with one commit pushed by root.
func (h *harness) publicRepo(name string, files ...string) {
	h.t.Helper()
	_, err := h.forge.Repos.Create(h.ctx, domain.Root(), app.CreateParams{Name: h.name(name), Visibility: domain.VisibilityPublic})
	require.NoError(h.t, err)
	wt := h.workTree("main", files...)
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/"+name+".git"), "main")
}

// AC1: anonymous clone of a public repository works; a private one gives
// 401 and then 404 with a valid unrelated token, like a missing repository.
func TestAC1AnonymousCloneAndPrivateRepo(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.publicRepo("pub", "README.md", "docs/a.txt")

	// A private repository, pushed by root.
	wt := h.workTree("main", "secret.txt")
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/priv.git"), "main")

	dir := t.TempDir()
	out, err := h.git(dir, "clone", "-q", h.url("/pub.git"), "pub")
	require.NoError(t, err, out)
	assert.FileExists(t, filepath.Join(dir, "pub", "docs", "a.txt"))
	// The .git suffix is optional.
	out, err = h.git(dir, "clone", "-q", h.url("/pub"), "pub2")
	require.NoError(t, err, out)

	// Private and missing look the same to an anonymous client: 401 + challenge.
	for _, name := range []string{"priv", "nothing-here"} {
		req, _ := http.NewRequest(http.MethodGet, h.url("/"+name+".git/info/refs?service=git-upload-pack"), nil)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, 401, resp.StatusCode, name)
		assert.Equal(t, `Basic realm="ermite"`, resp.Header.Get("WWW-Authenticate"), name)
	}
	out, err = h.git(dir, "clone", "-q", h.url("/priv.git"), "nope")
	require.Error(t, err)
	assert.NotContains(t, out, "secret.txt")
	assert.NoDirExists(t, filepath.Join(dir, "nope"))

	// A valid token without access gets "not found", the same as a missing repository.
	other := h.token(scopes(domain.ScopeRepoRead), "pub")
	out, err = h.git(dir, "clone", "-q", h.urlAuth(other, "/priv.git"), "nope")
	require.Error(t, err)
	assert.Contains(t, out, "not found")
	out2, err := h.git(dir, "clone", "-q", h.urlAuth(other, "/nothing-here.git"), "nope2")
	require.Error(t, err)
	assert.Contains(t, out2, "not found")

	// A reader of the repository, and root, can clone it.
	reader := h.token(scopes(domain.ScopeRepoRead), "priv")
	for _, secret := range []string{reader, adminToken} {
		d := t.TempDir()
		out, err = h.git(d, "clone", "-q", h.urlAuth(secret, "/priv.git"), "c")
		require.NoError(t, err, out)
		assert.FileExists(t, filepath.Join(d, "c", "secret.txt"))
	}
	// A token with repo:read for every repository, passed as a Bearer header.
	all := h.token(scopes(domain.ScopeRepoRead))
	d := t.TempDir()
	out, err = h.git(d, "-c", "http.extraHeader=Authorization: Bearer "+all, "clone", "-q", h.url("/priv.git"), "c")
	require.NoError(t, err, out)
}

// Only the smart HTTP routes exist; the rest of the repository tree is 404.
func TestRoutes(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.publicRepo("pub", "a")

	for _, path := range []string{
		"/pub.git/HEAD",
		"/pub.git/objects/info/packs",
		"/pub.git/info/refs", // dumb protocol: no service
		"/pub.git/info/refs?service=git-bogus",
		"/pub.git/config",
		"/pub/tree/main",
		"/",
		"/-/nothing",
		"/Bad.git/info/refs?service=git-upload-pack",
		"/..%2f..%2fetc/info/refs?service=git-upload-pack",
	} {
		assert.Equal(t, 404, h.status("GET", path, nil), path)
	}
	assert.Equal(t, 405, h.status("POST", "/pub.git/info/refs?service=git-upload-pack", nil))
	assert.Equal(t, 405, h.status("GET", "/pub.git/git-upload-pack", nil))
	assert.Equal(t, 200, h.status("GET", "/-/health", nil), "internal routes are intact")
	assert.Equal(t, 200, h.status("GET", "/pub.git/info/refs?service=git-upload-pack", nil))
	// Anonymous receive-pack is a challenge even for a public repository.
	assert.Equal(t, 401, h.status("GET", "/pub.git/info/refs?service=git-receive-pack", nil))
	assert.Equal(t, 401, h.status("POST", "/pub.git/git-receive-pack", nil))
}

// AC2: who may push.
func TestAC2PushAuthorization(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	wt := h.workTree("main", "a.txt")

	// Root creates the repository.
	out, err := h.git(wt, "push", "-q", h.urlAuth(adminToken, "/proj.git"), "main")
	require.NoError(t, err, out)

	writer := h.token(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite), "proj")
	writerAll := h.token(scopes(domain.ScopeRepoWrite))
	reader := h.token(scopes(domain.ScopeRepoRead))
	elsewhere := h.token(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite), "other")

	next := func(file string) {
		require.NoError(t, os.WriteFile(filepath.Join(wt, file), []byte(file), 0o644))
		h.mustGit(wt, "add", file)
		h.mustGit(wt, "commit", "-q", "-m", file)
	}

	for _, secret := range []string{writer, writerAll, adminToken} {
		next("w-" + secret[len(secret)-6:])
		out, err = h.git(wt, "push", "-q", h.urlAuth(secret, "/proj.git"), "main")
		require.NoError(t, err, out)
	}

	// A reader is known but may not write: 403.
	next("by-reader")
	out, err = h.git(wt, "push", "-q", h.urlAuth(reader, "/proj.git"), "main")
	require.Error(t, err)
	assert.Contains(t, out, "403")

	// A token limited to another repository cannot even see this private one.
	out, err = h.git(wt, "push", "-q", h.urlAuth(elsewhere, "/proj.git"), "main")
	require.Error(t, err)
	assert.Contains(t, out, "not found")

	// ... and it may not create a repository outside its list.
	_, err = h.git(wt, "push", "-q", h.urlAuth(elsewhere, "/brand-new.git"), "main")
	require.Error(t, err)
	assert.NoDirExists(t, h.repoDir("brand-new"))

	// Nothing from the refused pushes arrived.
	assert.NotContains(t, h.bareGit("proj", "log", "--format=%s", "main"), "by-reader")

	// Garbage credentials: 401.
	out, err = h.git(wt, "push", "-q", h.urlAuth("ermite_not-a-token", "/proj.git"), "main")
	require.Error(t, err)
	assert.Contains(t, out, "Authentication failed")

	// Expired token (written to the store directly: the use case refuses a past expiry).
	expired := "ermite_expired-secret-0123456789"
	require.NoError(t, h.forge.store.Tokens().Create(h.ctx, domain.Token{
		ID: "expired1", Label: "old", Hash: domain.HashTokenSecret(expired),
		Scopes: scopes(domain.ScopeRepoWrite), ExpiresAt: time.Now().Add(-time.Minute), CreatedAt: time.Now().Add(-time.Hour),
	}))
	out, err = h.git(wt, "push", "-q", h.urlAuth(expired, "/proj.git"), "main")
	require.Error(t, err)
	assert.Contains(t, out, "Authentication failed")

	// Revoked token.
	created, err := h.forge.Tokens.Create(h.ctx, domain.Root(), app.CreateTokenParams{Label: "short", Scopes: scopes(domain.ScopeRepoWrite)})
	require.NoError(t, err)
	next("before-revoke")
	out, err = h.git(wt, "push", "-q", h.urlAuth(created.Secret, "/proj.git"), "main")
	require.NoError(t, err, out)
	require.NoError(t, h.forge.Tokens.Revoke(h.ctx, domain.Root(), created.Token.ID))
	next("after-revoke")
	out, err = h.git(wt, "push", "-q", h.urlAuth(created.Secret, "/proj.git"), "main")
	require.Error(t, err)
	assert.Contains(t, out, "Authentication failed")
	assert.NotContains(t, h.bareGit("proj", "log", "--format=%s", "main"), "after-revoke")

	// Anonymous pushes are a plain 401.
	_, err = h.git(wt, "push", "-q", h.url("/proj.git"), "main")
	require.Error(t, err)

	// The token store index stays consistent with revocation (AC8).
	_, err = h.forge.store.Tokens().FindByHash(h.ctx, domain.HashTokenSecret(created.Secret))
	assert.ErrorIs(t, err, domain.ErrNotFound)
	list, err := h.forge.Tokens.List(h.ctx, domain.Root())
	require.NoError(t, err)
	for _, tok := range list {
		_, err := h.forge.store.Tokens().FindByHash(h.ctx, tok.Hash)
		assert.NoError(t, err, tok.ID)
	}
}

// AC3: first push creates a private repository with HEAD on the pushed branch.
func TestAC3CreateOnPush(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	events := h.forge.Bus.Subscribe(h.ctx)

	// Only "dev" is pushed: HEAD (initially main) moves to it.
	wt := h.workTree("dev", "a")
	out, err := h.git(wt, "push", "-q", h.urlAuth(adminToken, "/only-dev.git"), "dev")
	require.NoError(t, err, out)
	assert.Equal(t, "refs/heads/dev", h.bareGit("only-dev", "symbolic-ref", "HEAD"))

	repo, err := h.forge.Repos.Get(h.ctx, domain.Root(), h.name("only-dev"))
	require.NoError(t, err)
	assert.Equal(t, domain.VisibilityPrivate, repo.Visibility)
	assert.False(t, repo.PushedAt.IsZero(), "pushed_at is set by post-receive")
	_, err = h.forge.Repos.Get(h.ctx, domain.Anonymous(), h.name("only-dev"))
	assert.ErrorIs(t, err, domain.ErrNotFound, "private by default")

	var created domain.RepoCreated
	select {
	case ev := <-events:
		created = ev.(domain.RepoCreated)
	case <-time.After(5 * time.Second):
		t.Fatal("no RepoCreated event")
	}
	assert.Equal(t, h.name("only-dev"), created.Repo)
	assert.Equal(t, domain.VisibilityPrivate, created.Visibility)

	// Both main and dev are pushed: main is preferred.
	wt2 := h.workTree("dev", "a")
	h.mustGit(wt2, "branch", "main")
	out, err = h.git(wt2, "push", "-q", h.urlAuth(adminToken, "/both.git"), "dev", "main")
	require.NoError(t, err, out)
	assert.Equal(t, "refs/heads/main", h.bareGit("both", "symbolic-ref", "HEAD"))

	// An existing valid HEAD is left alone by later pushes.
	h.mustGit(wt2, "checkout", "-q", "-b", "feature")
	out, err = h.git(wt2, "push", "-q", h.urlAuth(adminToken, "/both.git"), "feature")
	require.NoError(t, err, out)
	assert.Equal(t, "refs/heads/main", h.bareGit("both", "symbolic-ref", "HEAD"))

	// An invalid name: 404 and nothing created, by a token that could create.
	before, err := h.forge.Repos.List(h.ctx, domain.Root())
	require.NoError(t, err)
	for _, bad := range []string{"/Upper.git", "/has%20space.git", "/-.git", "/robots.txt", "/a..b.git"} {
		assert.Equal(t, 404, h.statusAuth("GET", bad+"/info/refs?service=git-receive-pack", adminToken), bad)
	}
	after, err := h.forge.Repos.List(h.ctx, domain.Root())
	require.NoError(t, err)
	assert.Equal(t, before, after)
	entries, err := os.ReadDir(filepath.Join(h.data, "repos"))
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, strings.ToLower(e.Name()), "upper")
	}

	// A fetch of an unknown repository never creates it, even for root.
	assert.Equal(t, 404, h.statusAuth("GET", "/ghost.git/info/refs?service=git-upload-pack", adminToken))
	assert.NoDirExists(t, h.repoDir("ghost"))
	// Neither does the second request of a push.
	assert.Equal(t, 404, h.statusAuth("POST", "/ghost.git/git-receive-pack", adminToken))
	assert.NoDirExists(t, h.repoDir("ghost"))
}

// statusAuth is status with the token as Basic credentials.
func (h *harness) statusAuth(method, path, secret string) int {
	h.t.Helper()
	return h.status(method, path, http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte("x:"+secret))}})
}

// AC4: hooks fail closed, and post-receive records exactly what was pushed.
func TestAC4HooksFailClosed(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	wt := h.workTree("main", "a")
	out, err := h.git(wt, "push", "-q", h.urlAuth(adminToken, "/proj.git"), "main")
	require.NoError(t, err, out)
	before := h.bareGit("proj", "rev-parse", "main")

	// The hook socket goes away: pre-receive cannot ask ermite and rejects.
	h.forge.hooks.Close(h.ctx)
	select {
	case <-h.hooksDone:
	case <-time.After(5 * time.Second):
		t.Fatal("hook server did not stop")
	}
	require.NoError(t, os.WriteFile(filepath.Join(wt, "b"), []byte("b"), 0o644))
	h.mustGit(wt, "add", "b")
	h.mustGit(wt, "commit", "-q", "-m", "b")
	out, err = h.git(wt, "push", h.urlAuth(adminToken, "/proj.git"), "main")
	require.Error(t, err)
	assert.Contains(t, out, "unreachable")
	assert.Contains(t, out, "rejected")
	assert.Equal(t, before, h.bareGit("proj", "rev-parse", "main"), "the ref did not move")
}

func TestAC4HooksUnknownTokenAndMissingBinary(t *testing.T) {
	// The hook scripts point at a binary that does not exist: git cannot run
	// the hook, so the push fails (it never succeeds unchecked).
	h := newHarness(t, harnessOpts{hookExe: "/nonexistent/ermite"})
	wt := h.workTree("main", "a")
	out, err := h.git(wt, "push", h.urlAuth(adminToken, "/proj.git"), "main")
	require.Error(t, err, out)
	_, gerr := os.Stat(h.repoDir("proj"))
	require.NoError(t, gerr, "the repository was created by the first request")
	assert.Empty(t, h.bareGit("proj", "for-each-ref"), "nothing was accepted")
}

func TestAC4PostReceiveAttribution(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	events := h.forge.Bus.Subscribe(h.ctx)

	// Two repositories, pushed at the same time with different refs.
	const n = 4
	type job struct {
		repo, branch string
		wt           string
	}
	var jobs []job
	for i := range n {
		repo := "repo" + string(rune('a'+i))
		branch := "branch-" + string(rune('a'+i))
		jobs = append(jobs, job{repo: repo, branch: branch, wt: h.workTree(branch, "f-"+repo)})
	}
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := h.git(j.wt, "push", "-q", h.urlAuth(adminToken, "/"+j.repo+".git"), j.branch)
			assert.NoError(t, err, out)
		}()
	}
	wg.Wait()

	got := map[domain.RepoName]domain.PushEvent{}
	deadline := time.After(10 * time.Second)
	for len(got) < n {
		select {
		case ev := <-events:
			if pe, ok := ev.(domain.PushEvent); ok {
				require.NotContains(t, got, pe.Repo, "one event per push")
				got[pe.Repo] = pe
			}
		case <-deadline:
			t.Fatalf("got %d push events, want %d", len(got), n)
		}
	}
	for _, j := range jobs {
		pe := got[h.name(j.repo)]
		require.Len(t, pe.Updates, 1, j.repo)
		assert.Equal(t, "refs/heads/"+j.branch, pe.Updates[0].Ref, j.repo)
		assert.Equal(t, domain.ZeroOID, pe.Updates[0].Old)
		assert.Equal(t, h.bareGit(j.repo, "rev-parse", j.branch), pe.Updates[0].New, "the exact new object id")
		repo, err := h.forge.Repos.Get(h.ctx, domain.Root(), h.name(j.repo))
		require.NoError(t, err)
		assert.False(t, repo.PushedAt.IsZero())
	}

	// A second push reports the old id, a deletion reports the zero id.
	wt := jobs[0].wt
	old := h.bareGit(jobs[0].repo, "rev-parse", jobs[0].branch)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "again"), []byte("x"), 0o644))
	h.mustGit(wt, "add", "again")
	h.mustGit(wt, "commit", "-q", "-m", "again")
	h.mustGit(wt, "tag", "v1")
	out, err := h.git(wt, "push", "-q", h.urlAuth(adminToken, "/"+jobs[0].repo+".git"), jobs[0].branch, "v1")
	require.NoError(t, err, out)
	pe := waitPush(t, events)
	require.Len(t, pe.Updates, 2)
	byRef := map[string]domain.RefUpdate{}
	for _, u := range pe.Updates {
		byRef[u.Ref] = u
	}
	assert.Equal(t, old, byRef["refs/heads/"+jobs[0].branch].Old)
	assert.True(t, byRef["refs/tags/v1"].IsCreate())

	out, err = h.git(wt, "push", "-q", h.urlAuth(adminToken, "/"+jobs[0].repo+".git"), ":refs/tags/v1")
	require.NoError(t, err, out)
	pe = waitPush(t, events)
	require.Len(t, pe.Updates, 1)
	assert.True(t, pe.Updates[0].IsDelete())
}

func waitPush(t *testing.T, events <-chan domain.Event) domain.PushEvent {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-events:
			if pe, ok := ev.(domain.PushEvent); ok {
				return pe
			}
		case <-deadline:
			t.Fatal("no PushEvent")
		}
	}
}

// AC5: the failed-authentication limiter, with and without trusted proxies.
func TestAC5Limiter(t *testing.T) {
	t.Run("direct clients", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		bad := func(hdr http.Header) int {
			req, _ := http.NewRequest(http.MethodGet, h.url("/x.git/info/refs?service=git-upload-pack"), nil)
			req.SetBasicAuth("u", "ermite_wrong")
			for k, v := range hdr {
				req.Header[k] = v
			}
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			_ = resp.Body.Close()
			return resp.StatusCode
		}
		for i := range 10 {
			require.Equal(t, 401, bad(nil), "failure %d", i+1)
		}
		assert.Equal(t, 429, bad(nil), "the 11th request is blocked")
		// Even good credentials are blocked while the address is.
		assert.Equal(t, 429, h.statusAuth("GET", "/x.git/info/refs?service=git-upload-pack", adminToken))
		assert.Equal(t, 429, h.status("GET", "/x.git/info/refs?service=git-upload-pack", nil), "anonymous too")
		// X-Forwarded-For from an untrusted peer is ignored.
		assert.Equal(t, 429, bad(http.Header{"X-Forwarded-For": {"198.51.100.9"}}))
		// Non-Git routes are not limited.
		assert.Equal(t, 200, h.status("GET", "/-/health", nil))

		resp, err := http.Get(h.url("/x.git/info/refs?service=git-upload-pack"))
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.NotEmpty(t, resp.Header.Get("Retry-After"))
	})

	t.Run("trusted proxy", func(t *testing.T) {
		h := newHarness(t, harnessOpts{trustedProxies: "127.0.0.0/8"})
		bad := func(xff string) int {
			req, _ := http.NewRequest(http.MethodGet, h.url("/x.git/info/refs?service=git-upload-pack"), nil)
			req.SetBasicAuth("u", "ermite_wrong")
			req.Header.Set("X-Forwarded-For", xff)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			_ = resp.Body.Close()
			return resp.StatusCode
		}
		// A spoofed left entry does not help: the right-most untrusted one counts.
		for i := range 10 {
			require.Equal(t, 401, bad("1.1.1."+string(rune('0'+i))+", 198.51.100.7"), "failure %d", i+1)
		}
		assert.Equal(t, 429, bad("2.2.2.2, 198.51.100.7"), "client 198.51.100.7 is blocked")
		assert.Equal(t, 401, bad("198.51.100.8"), "another client behind the proxy is not")
		req, _ := http.NewRequest(http.MethodGet, h.url("/x.git/info/refs?service=git-upload-pack"), nil)
		req.Header.Set("X-Forwarded-For", "198.51.100.7")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, 429, resp.StatusCode)
	})

	t.Run("successes do not count", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		for range 25 {
			assert.Equal(t, 404, h.statusAuth("GET", "/x.git/info/refs?service=git-upload-pack", adminToken))
		}
		// Anonymous requests for a private/missing repo are challenges, not failures.
		for range 25 {
			assert.Equal(t, 401, h.status("GET", "/x.git/info/refs?service=git-upload-pack", nil))
		}
	})
}

// AC6: protocol v2 and partial clone.
func TestAC6ProtocolV2AndPartialClone(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.publicRepo("pub", "README.md", "src/main.go", "src/util.go")

	dir := t.TempDir()
	out, err := h.gitEnv([]string{"GIT_TRACE_PACKET=1"}, dir, "-c", "protocol.version=2", "clone", "-q", h.url("/pub.git"), "v2")
	require.NoError(t, err, out)
	assert.Contains(t, out, "version 2", "the server speaks protocol v2")
	assert.Contains(t, out, "ls-refs")

	// Protocol v0 still works for old clients.
	out, err = h.git(dir, "-c", "protocol.version=0", "clone", "-q", h.url("/pub.git"), "v0")
	require.NoError(t, err, out)

	// A later fetch uses v2 as well.
	wt := h.workTree("main", "x")
	h.mustGit(wt, "push", "-q", "-f", h.urlAuth(adminToken, "/pub.git"), "main")
	out, err = h.gitEnv([]string{"GIT_TRACE_PACKET=1"}, filepath.Join(dir, "v2"), "-c", "protocol.version=2", "fetch", "-q")
	require.NoError(t, err, out)
	assert.Contains(t, out, "fetch=")

	// Partial clone: no blobs come down until they are needed.
	out, err = h.git(dir, "-c", "protocol.version=2", "clone", "-q", "--no-checkout", "--filter=blob:none", h.url("/pub.git"), "partial")
	require.NoError(t, err, out)
	partial := filepath.Join(dir, "partial")
	assert.Equal(t, "blob:none", strings.TrimSpace(h.mustGit(partial, "config", "remote.origin.partialclonefilter")))
	missing := h.mustGit(partial, "rev-list", "--objects", "--missing=print", "--all")
	assert.Contains(t, missing, "?", "blobs are missing after the clone")
	// ... and are fetched on demand (the promisor remote is the same URL).
	out, err = h.git(partial, "checkout", "-q", "main")
	require.NoError(t, err, out)
	assert.FileExists(t, filepath.Join(partial, "x"))
}

// AC7: tokens never appear in logs, nor in the environment of the hooks.
func TestAC7NoSecretsInLogsOrHookEnvironment(t *testing.T) {
	envDump := filepath.Join(t.TempDir(), "hookenv.txt")
	bin := binary(t)
	// A wrapper that records the environment of the hook, then runs the real hook.
	wrapper := filepath.Join(t.TempDir(), "ermite-wrapper")
	require.NoError(t, os.WriteFile(wrapper, []byte("#!/bin/sh\nenv >> '"+envDump+"'\nexec '"+bin+"' \"$@\"\n"), 0o755))
	h := newHarness(t, harnessOpts{hookExe: wrapper})

	stored := h.token(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite))
	wt := h.workTree("main", "a")
	for _, secret := range []string{stored, adminToken} {
		out, err := h.git(wt, "push", "-q", "-f", h.urlAuth(secret, "/proj.git"), "main")
		require.NoError(t, err, out)
	}
	// Bad credentials, a Bearer header, and a clone as well.
	_, _ = h.git(wt, "push", "-q", h.urlAuth("ermite_secret-that-is-wrong", "/proj.git"), "main")
	out, err := h.git(wt, "-c", "http.extraHeader=Authorization: Bearer "+stored, "push", "-q", "-f", h.url("/proj.git"), "main")
	require.NoError(t, err, out)
	out, err = h.git(t.TempDir(), "clone", "-q", h.urlAuth(stored, "/proj.git"), "c")
	require.NoError(t, err, out)

	logs := h.logs.String()
	require.Contains(t, logs, "push received", "the logs of the pushes were captured")
	secrets := []string{stored, adminToken, "ermite_secret-that-is-wrong"}
	for _, s := range secrets {
		basic := base64.StdEncoding.EncodeToString([]byte("user:" + s))
		assert.NotContains(t, logs, s)
		assert.NotContains(t, logs, basic)
		assert.NotContains(t, logs, base64.StdEncoding.EncodeToString([]byte(s)))
	}
	assert.NotContains(t, strings.ToLower(logs), "authorization")
	assert.NotContains(t, logs, "Basic ")
	assert.NotContains(t, logs, "Bearer ")

	dumped, err := os.ReadFile(envDump)
	require.NoError(t, err)
	require.Contains(t, string(dumped), "ERMITE_HOOK_TOKEN=", "the hooks ran with their token")
	assert.Contains(t, string(dumped), "ERMITE_HOOK_SOCKET=")
	for _, s := range secrets {
		assert.NotContains(t, string(dumped), s, "the hook environment")
	}
	assert.NotContains(t, strings.ToUpper(string(dumped)), "HTTP_AUTHORIZATION")
	assert.NotContains(t, strings.ToUpper(string(dumped)), "HTTP_COOKIE")
}

// AC8, through the wired use cases: no escalation and a consistent index.
func TestAC8TokenCreationThroughTheForge(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	ctx := context.Background()

	admin, err := h.forge.Tokens.Create(ctx, domain.Root(), app.CreateTokenParams{
		Label: "limited admin", Scopes: scopes(domain.ScopeTokenAdmin, domain.ScopeRepoWrite), Repos: []domain.RepoName{h.name("a")},
	})
	require.NoError(t, err)
	p, err := h.forge.Auth.Authenticate(ctx, admin.Secret)
	require.NoError(t, err)

	_, err = h.forge.Tokens.Create(ctx, p, app.CreateTokenParams{Label: "x", Scopes: scopes(domain.ScopeRepoAdmin), Repos: []domain.RepoName{h.name("a")}})
	assert.ErrorIs(t, err, domain.ErrForbidden, "scope escalation")
	_, err = h.forge.Tokens.Create(ctx, p, app.CreateTokenParams{Label: "x", Scopes: scopes(domain.ScopeRepoWrite)})
	assert.ErrorIs(t, err, domain.ErrForbidden, "repository list escalation")
	_, err = h.forge.Tokens.Create(ctx, p, app.CreateTokenParams{Label: "x", Scopes: scopes(domain.ScopeRepoWrite), Repos: []domain.RepoName{h.name("b")}})
	assert.ErrorIs(t, err, domain.ErrForbidden, "another repository")
	child, err := h.forge.Tokens.Create(ctx, p, app.CreateTokenParams{Label: "child", Scopes: scopes(domain.ScopeRepoWrite), Repos: []domain.RepoName{h.name("a")}})
	require.NoError(t, err)

	// The child works on its repository, and revoking the index entry kills it at once.
	wt := h.workTree("main", "f")
	out, err := h.git(wt, "push", "-q", h.urlAuth(child.Secret, "/a.git"), "main")
	require.NoError(t, err, out)
	require.NoError(t, h.forge.Tokens.Revoke(ctx, p, child.Token.ID))
	out, err = h.git(wt, "push", "-q", h.urlAuth(child.Secret, "/a.git"), "main")
	require.Error(t, err, out)

	list, err := h.forge.Tokens.List(ctx, domain.Root())
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, admin.Token.ID, list[0].ID)
	_, err = h.forge.store.Tokens().FindByHash(ctx, child.Token.Hash)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

// Large pushes arrive chunked (git's http.postBuffer is 1 MiB): the handler
// streams them to Git. Hooks, a path with quotes in the hook executable, and the
// debounced maintenance are exercised on the way.
func TestLargePushHookPathQuotingAndMaintenance(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "it's a \"dir\" with spaces")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	exe := filepath.Join(dir, "ermite")
	raw, err := os.ReadFile(binary(t))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(exe, raw, 0o755))

	h := newHarness(t, harnessOpts{hookExe: exe, maintenance: 50 * time.Millisecond})
	wt := h.workTree("main", "a")
	big := make([]byte, 3<<20)
	_, err = rand.Read(big)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "big.bin"), big, 0o644))
	h.mustGit(wt, "add", "big.bin")
	h.mustGit(wt, "commit", "-q", "-m", "big")

	out, err := h.git(wt, "push", "-q", h.urlAuth(adminToken, "/big.git"), "main")
	require.NoError(t, err, out)
	assert.Equal(t, h.mustGit(wt, "rev-parse", "main"), h.mustGit(h.repoDir("big"), "rev-parse", "main"))

	// Maintenance wrote a commit-graph after the debounce delay.
	chain := filepath.Join(h.repoDir("big"), "objects", "info", "commit-graphs", "commit-graph-chain")
	require.Eventually(t, func() bool { _, err := os.Stat(chain); return err == nil }, 10*time.Second, 20*time.Millisecond)

	// The clone gets the big blob back intact.
	d := t.TempDir()
	out, err = h.git(d, "clone", "-q", h.urlAuth(adminToken, "/big.git"), "c")
	require.NoError(t, err, out)
	got, err := os.ReadFile(filepath.Join(d, "c", "big.bin"))
	require.NoError(t, err)
	assert.Equal(t, big, got)

	// Hook scripts: 0755, exec the binary quoted.
	for _, hook := range []string{"pre-receive", "post-receive"} {
		fi, err := os.Stat(filepath.Join(h.data, "run", "hooks", hook))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), fi.Mode().Perm())
	}
	// Nothing was written into the repository itself.
	entries, err := os.ReadDir(filepath.Join(h.repoDir("big"), "hooks"))
	require.NoError(t, err)
	for _, e := range entries {
		assert.True(t, strings.HasSuffix(e.Name(), ".sample"), "hooks dir is untouched: %s", e.Name())
	}
	conf, err := os.ReadFile(filepath.Join(h.repoDir("big"), "config"))
	require.NoError(t, err)
	assert.NotContains(t, string(conf), "hooksPath")
	assert.NotContains(t, string(conf), "receivepack")
}

// A chunked push has no declared length, so the limit cuts the body off while
// it streams: the push fails and no ref moves. A small push, which git sends
// with a Content-Length, is refused up front.
func TestPushOverTheBodyLimitFailsAndMovesNoRef(t *testing.T) {
	const limit = 256 << 10
	h := newHarness(t, harnessOpts{maxBody: limit})
	small := h.workTree("main", "a")
	out, err := h.git(small, "push", "-q", h.urlAuth(adminToken, "/proj.git"), "main")
	require.NoError(t, err, "a push under the limit works\n"+out)
	before := h.bareGit("proj", "rev-parse", "main")

	big := make([]byte, 3<<20) // over postBuffer: git sends it chunked
	_, err = rand.Read(big)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(small, "big.bin"), big, 0o644))
	h.mustGit(small, "add", "big.bin")
	h.mustGit(small, "commit", "-q", "-m", "big")

	out, err = h.git(small, "push", h.urlAuth(adminToken, "/proj.git"), "main")
	require.Error(t, err, "a chunked push over the limit fails\n"+out)
	assert.Equal(t, before, h.bareGit("proj", "rev-parse", "main"), "no ref moved")

	// A new branch of the same objects fails the same way and is not created.
	h.mustGit(small, "branch", "other")
	out, err = h.git(small, "push", h.urlAuth(adminToken, "/proj.git"), "other")
	require.Error(t, err, out)
	assert.Empty(t, h.bareGit("proj", "for-each-ref", "refs/heads/other"))

	// With a declared length (postBuffer raised above the pack) it is a 413.
	out, err = h.git(small, "-c", "http.postBuffer=104857600", "push", h.urlAuth(adminToken, "/proj.git"), "main")
	require.Error(t, err, out)
	assert.Contains(t, out, "413")
	assert.Equal(t, before, h.bareGit("proj", "rev-parse", "main"))
}

// receive.fsckObjects is on: a push with a malformed object is refused, and
// nothing is stored in the repository's own configuration to get there.
func TestFsckRejectsMalformedObjects(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	wt := h.workTree("main", "a")
	out, err := h.git(wt, "push", "-q", h.urlAuth(adminToken, "/proj.git"), "main")
	require.NoError(t, err, out)
	before := h.bareGit("proj", "rev-parse", "main")

	// A tree whose entries are not sorted: "b" before "a".
	blob := strings.TrimSpace(h.mustGit(wt, "rev-parse", "HEAD:a"))
	raw := make([]byte, 0, 64)
	for _, name := range []string{"b", "a"} {
		raw = append(raw, []byte("100644 "+name+"\x00")...)
		oid, err := hex.DecodeString(blob)
		require.NoError(t, err)
		raw = append(raw, oid...)
	}
	treeFile := filepath.Join(t.TempDir(), "tree")
	require.NoError(t, os.WriteFile(treeFile, raw, 0o644))
	tree := strings.TrimSpace(h.mustGit(wt, "hash-object", "-w", "-t", "tree", "--literally", treeFile))
	commit := strings.TrimSpace(h.mustGit(wt, "commit-tree", "-p", "HEAD", "-m", "malformed", tree))
	h.mustGit(wt, "update-ref", "refs/heads/bad", commit)

	out, err = h.git(wt, "push", h.urlAuth(adminToken, "/proj.git"), "bad")
	require.Error(t, err, out)
	assert.Equal(t, before, h.bareGit("proj", "rev-parse", "main"))
	assert.Empty(t, h.bareGit("proj", "for-each-ref", "refs/heads/bad"), "the ref was not created")
	conf, err := os.ReadFile(filepath.Join(h.repoDir("proj"), "config"))
	require.NoError(t, err)
	assert.NotContains(t, string(conf), "fsck")
}

// The built binary end to end: `ermite serve` as a process, its hook scripts
// exec'ing the same binary, and a real git client pushing and cloning.
func TestBuiltBinaryPushAndClone(t *testing.T) {
	gitBin := requireGit(t)
	data, err := os.MkdirTemp("", "ermite")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(data) })
	addr := freeAddr(t)

	cmd := exec.Command(binary(t), "serve")
	cmd.Env = append(baseEnv(),
		"ERMITE_ADDR="+addr,
		"ERMITE_DATA_DIR="+data,
		"ERMITE_PUBLIC_URL=http://"+addr,
		"ERMITE_ADMIN_TOKEN="+adminToken,
		"ERMITE_GIT="+gitBin,
		"ERMITE_LOG_LEVEL=debug",
	)
	var logs syncBuffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	require.NoError(t, cmd.Start())
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + addr + "/-/health")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == 200
	}, 15*time.Second, 50*time.Millisecond, logs.String())

	home := t.TempDir()
	run := func(dir string, args ...string) string {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = clientEnv(home)
		out, err := c.CombinedOutput()
		require.NoError(t, err, "git %v\n%s\nserver logs:\n%s", args, out, logs.String())
		return string(out)
	}
	wt := t.TempDir()
	run(wt, "init", "-q", "--initial-branch=main")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "hello.txt"), []byte("hello\n"), 0o644))
	run(wt, "add", ".")
	run(wt, "commit", "-q", "-m", "hello")
	url := "http://u:" + adminToken + "@" + addr + "/bin-test.git"
	run(wt, "push", "-q", url, "main")
	cl := t.TempDir()
	run(cl, "clone", "-q", url, "c")
	assert.FileExists(t, filepath.Join(cl, "c", "hello.txt"))

	// The hooks ran: post-receive recorded the push.
	require.Eventually(t, func() bool { return strings.Contains(logs.String(), `"message":"push received"`) }, 5*time.Second, 20*time.Millisecond, logs.String())
	assert.NotContains(t, logs.String(), adminToken)

	// Hook scripts exec this very binary.
	script, err := os.ReadFile(filepath.Join(data, "run", "hooks", "pre-receive"))
	require.NoError(t, err)
	assert.Contains(t, string(script), binary(t))

	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-exited:
		require.NoError(t, err, logs.String())
	case <-time.After(15 * time.Second):
		t.Fatalf("no clean stop\n%s", logs.String())
	}
	assert.NoFileExists(t, filepath.Join(data, "run", "hook.sock"), "the socket is removed on stop")
}

// `ermite hook` is hidden, fails closed for pre-receive and never fails the
// push for post-receive.
func TestHookCommand(t *testing.T) {
	bin := binary(t)
	input := domainZero + " " + strings.Repeat("a", 40) + " refs/heads/main\n"

	run := func(hook string, env ...string) (string, error) {
		c := exec.Command(bin, "hook", hook)
		c.Env = append(baseEnv(), env...)
		c.Stdin = strings.NewReader(input)
		out, err := c.CombinedOutput()
		return string(out), err
	}
	out, err := run("pre-receive")
	require.Error(t, err)
	assert.Contains(t, out, "ERMITE_HOOK_SOCKET")
	out, err = run("pre-receive", "ERMITE_HOOK_SOCKET=/nonexistent.sock", "ERMITE_HOOK_TOKEN=x")
	require.Error(t, err)
	assert.Contains(t, out, "unreachable")
	out, err = run("post-receive", "ERMITE_HOOK_SOCKET=/nonexistent.sock", "ERMITE_HOOK_TOKEN=x")
	require.NoError(t, err)
	assert.Contains(t, out, "post-receive")

	help, err := exec.Command(bin, "--help").CombinedOutput()
	require.NoError(t, err)
	assert.NotContains(t, string(help), "hook", "the command is hidden")
}

const domainZero = "0000000000000000000000000000000000000000"
