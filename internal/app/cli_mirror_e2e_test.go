// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"bnema.dev/ermite/internal/adapters/gitcli"
	"bnema.dev/ermite/internal/adapters/gitpush"
	"bnema.dev/ermite/internal/config"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/usecase"
)

// Pushes mark mirrors dirty; a due cron tick performs the asynchronous sync.
// Advancing the injected clock exercises that contract without waiting five minutes.
func TestMirrorCLIEndToEnd(t *testing.T) {
	clk := &testClock{}
	clk.set("2025-12-31 23:59:00")
	transport := t.TempDir()
	var dataDir string
	h := newHarnessWith(t, harnessOpts{env: map[string]string{config.EnvSecretsKey: secretsKeyA}}, func(o *options) {
		o.mirrors = func(c *usecase.MirrorsConfig) {
			c.AllowAnyURL = true
			c.Clock = clk
			c.ScheduleTick = time.Hour
			c.Pusher = gitpush.New(gitpush.Config{
				Bin: requireGit(t), Path: os.Getenv("PATH"), Home: transport,
				KnownHosts: filepath.Join(transport, "known_hosts"), TempDir: filepath.Join(transport, "keys"),
				AllowedProtocols: "file", RepoDir: func(repo domain.RepoName) string { return gitcli.RepoDir(dataDir, repo) },
			})
		}
	})
	// The transport needs the harness data directory, which is known after newForge.
	// Replace the test-only closure's captured directory before any sync is queued.
	dataDir = h.data
	c := &cliHarness{harness: h, url: h.srv.URL}
	run := func(args ...string) result { return c.cli(t, adminToken, append([]string{"mirror"}, args...)...) }
	h.createRepo("proj", domain.VisibilityPrivate)
	remote := filepath.Join(t.TempDir(), "remote.git")
	h.mustGit(transport, "init", "--bare", remote)
	run("add", "proj", "file://"+remote, "--cron", "*/5 * * * *").ok(t)
	key := run("key", "show").ok(t)
	assert.Empty(t, key.stderr)
	pub, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(key.stdout))
	require.NoError(t, err)
	assert.Empty(t, rest)
	assert.Equal(t, "ssh-ed25519", pub.Type())
	assert.Equal(t, 1, strings.Count(key.stdout, "\n"))
	assert.Contains(t, c.cli(t, adminToken, "version").ok(t).stdout, "mirror")
	events := h.forge.Bus.Subscribe(h.ctx)
	wt := h.workTree("main", "README.md")
	h.mustGit(wt, "tag", "v1")
	h.mustGit(wt, "push", h.urlAuth(adminToken, "/proj.git"), "main", "--tags")
	clk.set("2026-01-01 00:00:00")
	h.forge.Mirrors.ScheduleNow(h.ctx)
	require.Eventually(t, func() bool {
		s, err := h.forge.Mirrors.GetStatus(h.ctx, domain.Root(), h.name("proj"))
		return err == nil && !s.Status.LastSuccessAt.IsZero()
	}, 10*time.Second, 10*time.Millisecond)
	main := strings.TrimSpace(h.mustGit(wt, "rev-parse", "main"))
	assert.Equal(t, main, strings.TrimSpace(h.mustGit(transport, "--git-dir="+remote, "rev-parse", "refs/heads/main")))
	assert.Equal(t, main, strings.TrimSpace(h.mustGit(transport, "--git-dir="+remote, "rev-parse", "refs/tags/v1")))
	clk.set("2026-01-01 00:02:00")
	assert.Contains(t, run("sync", "proj").ok(t).stdout, "true", "unchanged refs are a no-op")

	// Advance the remote independently, so the next atomic mirror push is rejected.
	other := filepath.Join(t.TempDir(), "other")
	h.mustGit(transport, "clone", "--branch", "main", remote, other)
	h.mustGit(other, "commit", "--allow-empty", "-m", "remote-only")
	h.mustGit(other, "push", "origin", "main")
	h.mustGit(wt, "commit", "--allow-empty", "-m", "forge-only")
	h.mustGit(wt, "push", h.urlAuth(adminToken, "/proj.git"), "main")
	clk.set("2026-01-01 00:05:00")
	h.forge.Mirrors.ScheduleNow(h.ctx)
	require.Eventually(t, func() bool {
		s, err := h.forge.Mirrors.GetStatus(h.ctx, domain.Root(), h.name("proj"))
		return err == nil && s.Status.State == domain.MirrorPaused
	}, 10*time.Second, 10*time.Millisecond)
	status := run("status", "proj").ok(t).stdout
	assert.Contains(t, status, "paused")
	assert.Contains(t, status, "remote has commits")
	assert.Contains(t, run("status", "proj", "--json").ok(t).stdout, `"failure_class":"diverged"`)
	h.mustGit(wt, "commit", "--allow-empty", "-m", "warning")
	warning := h.mustGit(wt, "push", h.urlAuth(adminToken, "/proj.git"), "main")
	assert.Contains(t, warning, "remote: ermite: mirror")
	assert.Contains(t, warning, "paused")
	token := c.signIn(t)
	page := (browser{h}).do("GET", "/proj", map[string]string{"Cookie": sessionCookie + "=" + token}, "")
	assert.Equal(t, 200, page.status)
	assert.Contains(t, page.body, "Mirror paused")
	assert.Contains(t, page.body, "ermite mirror status proj")

	// Repair the remote, then force a normal (never forced) Git push to recover.
	latest := strings.TrimSpace(h.mustGit(wt, "rev-parse", "main"))
	h.mustGit(wt, "push", "--force", remote, "main") // deliberate remote repair, not mirror behavior
	run("sync", "proj", "--force").ok(t)
	assert.Equal(t, latest, strings.TrimSpace(h.mustGit(transport, "--git-dir="+remote, "rev-parse", "refs/heads/main")))
	assert.Contains(t, run("status", "proj").ok(t).stdout, "ok")
	run("sync", "--all", "--force").ok(t)
	var kinds []string
	for {
		select {
		case event := <-events:
			if event.EventKind() == "mirror.failed" || event.EventKind() == "mirror.recovered" {
				kinds = append(kinds, event.EventKind())
			}
		default:
			assert.Equal(t, []string{"mirror.failed", "mirror.recovered"}, kinds)
			return
		}
	}
}
