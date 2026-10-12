// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zerowrap"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/config"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// harness is a forge wired like `ermite serve`, behind an httptest server,
// and a git client isolated from the machine's configuration.
type harness struct {
	t         *testing.T
	forge     *forge
	srv       *httptest.Server
	data      string
	logs      *syncBuffer
	home      string // HOME of the git client
	ctx       context.Context
	cancel    context.CancelFunc
	hooksDone chan error
}

type harnessOpts struct {
	trustedProxies string
	maintenance    time.Duration
	// hookExe is the executable the hook scripts exec; empty builds ermite.
	hookExe string
	// maxBody overrides the limit of a push body when positive.
	maxBody int64
	// eventBuffer overrides the size of the event ring when positive.
	eventBuffer int
	// revalidateEvery overrides how often a watcher's token is re-checked.
	revalidateEvery time.Duration
}

// newHarness starts a forge. The data directory is short: the hook socket
// lives in it and Unix socket paths are limited to about 100 bytes.
func newHarness(t *testing.T, o harnessOpts) *harness {
	t.Helper()
	gitBin := requireGit(t)
	data, err := os.MkdirTemp("", "ermite")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(data) })

	env := map[string]string{
		config.EnvDataDir:    data,
		config.EnvPublicURL:  "http://example.test",
		config.EnvAdminToken: adminToken,
		config.EnvGit:        gitBin,
		config.EnvAddr:       "127.0.0.1:1",
	}
	if o.trustedProxies != "" {
		env[config.EnvTrustedProxies] = o.trustedProxies
	}
	// The suite runs with either reader: ERMITE_GIT_READER=cli go test ./cmd/ermite
	if r := os.Getenv(config.EnvGitReader); r != "" {
		env[config.EnvGitReader] = r
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	require.NoError(t, err)

	logs := &syncBuffer{}
	log := zerowrap.New(zerowrap.Config{Level: "trace", Format: "json", Output: logs})
	ctx, cancel := context.WithCancel(zerowrap.WithCtx(context.Background(), log))

	require.NoError(t, prepareDataDir(data))
	hookExe := o.hookExe
	if hookExe == "" {
		hookExe = binary(t)
	}
	f, err := newForge(ctx, cfg, options{hookExe: hookExe, maintenanceDelay: o.maintenance, maxBodyBytes: o.maxBody, eventBuffer: o.eventBuffer, revalidateEvery: o.revalidateEvery})
	require.NoError(t, err)

	h := &harness{t: t, forge: f, data: data, logs: logs, ctx: ctx, cancel: cancel, hooksDone: make(chan error, 1)}
	go func() { h.hooksDone <- f.hooks.Serve(ctx); close(h.hooksDone) }()
	h.srv = httptest.NewUnstartedServer(f.server.Handler())
	h.srv.Config.BaseContext = func(net.Listener) context.Context { return ctx }
	h.srv.Start()

	h.home = t.TempDir()
	t.Cleanup(func() {
		// Event streams never end by themselves: the real server closes them
		// when its shutdown begins, the test server needs the same push.
		f.EventLog.Close(context.WithoutCancel(ctx))
		h.srv.Close()
		cancel()
		select {
		case <-h.hooksDone:
		case <-time.After(10 * time.Second):
			t.Error("hook server did not stop")
		}
		f.Close(context.WithoutCancel(ctx))
	})
	return h
}

func (h *harness) url(path string) string { return h.srv.URL + path }

// urlAuth returns a URL with the token as the password.
func (h *harness) urlAuth(secret, path string) string {
	return strings.Replace(h.srv.URL, "http://", "http://user:"+secret+"@", 1) + path
}

func (h *harness) name(s string) domain.RepoName {
	h.t.Helper()
	n, err := domain.ParseRepoName(s)
	require.NoError(h.t, err)
	return n
}

// clientEnv is the complete environment of the git client in tests: nothing
// of the machine's configuration, a fixed identity and locale.
func clientEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.test",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.test",
		"LC_ALL=C",
	}
}

// git runs the git client in dir with an isolated configuration.
func (h *harness) git(dir string, args ...string) (string, error) {
	h.t.Helper()
	return h.gitEnv(nil, dir, args...)
}

func (h *harness) mustGit(dir string, args ...string) string {
	h.t.Helper()
	out, err := h.git(dir, args...)
	require.NoError(h.t, err, "git %s\n%s", strings.Join(args, " "), out)
	return out
}

// workTree makes a work tree with one commit per file on branch.
func (h *harness) workTree(branch string, files ...string) string {
	h.t.Helper()
	dir := h.t.TempDir()
	h.mustGit(dir, "init", "-q", "--initial-branch="+branch)
	for _, f := range files {
		require.NoError(h.t, os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755))
		require.NoError(h.t, os.WriteFile(filepath.Join(dir, f), []byte("content of "+f+"\n"), 0o644))
		h.mustGit(dir, "add", f)
		h.mustGit(dir, "commit", "-q", "-m", "add "+f)
	}
	return dir
}

// status does a plain HTTP request and returns the status code.
func (h *harness) status(method, path string, hdr http.Header) int {
	h.t.Helper()
	req, err := http.NewRequest(method, h.url(path), nil)
	require.NoError(h.t, err)
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// token creates a stored token through the use case, as root.
func (h *harness) token(scopes []domain.Scope, repos ...string) string {
	h.t.Helper()
	p := ports.CreateTokenParams{Label: "e2e", Scopes: scopes}
	for _, r := range repos {
		p.Repos = append(p.Repos, h.name(r))
	}
	got, err := h.forge.Tokens.Create(h.ctx, domain.Root(), p)
	require.NoError(h.t, err)
	return got.Secret
}

// gitEnv runs git like git() with extra environment variables.
func (h *harness) gitEnv(extra []string, dir string, args ...string) (string, error) {
	h.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(clientEnv(h.home), extra...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// repoDir is the bare repository on disk.
func (h *harness) repoDir(name string) string {
	return filepath.Join(h.data, "repos", name+".git")
}

// bareGit runs git against the bare repository on disk.
func (h *harness) bareGit(name string, args ...string) string {
	h.t.Helper()
	return strings.TrimSpace(h.mustGit(h.repoDir(name), args...))
}
