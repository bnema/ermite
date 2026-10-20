// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/config"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/usecase"
)

func TestMirrorCLIArguments(t *testing.T) {
	for _, args := range [][]string{
		{"mirror", "sync"}, {"mirror", "sync", "repo", "--all"}, {"mirror", "sync", "a", "b"},
		{"mirror", "add", "repo"}, {"mirror", "remove"}, {"mirror", "exclude"}, {"mirror", "include"},
		{"mirror", "key", "show", "extra"}, {"mirror", "key", "rotate", "extra"}, {"mirror", "status", "a", "b"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			r := runCLI(t, nil, "", args...)
			require.Error(t, r.err)
			assert.NotContains(t, r.err.Error(), "ERMITE_URL", "arguments must be rejected before loading credentials")
		})
	}
	r := runCLI(t, nil, "", "mirror", "sync", "--help").ok(t)
	assert.Contains(t, r.stdout, "never force-pushes")
	assert.Contains(t, r.stdout, "lifts a pause")
}

func TestMirrorCLIConfiguration(t *testing.T) {
	h := newSecretsCLI(t)
	run := func(args ...string) result { return h.cli(t, adminToken, append([]string{"mirror"}, args...)...) }
	run("config", "--enabled", "--url", "git@example.test:owner/{repo}.git", "--cron", "*/5 * * * *", "--all-branches").ok(t)
	r := run("config", "show", "--json").ok(t)
	g := decode[map[string]any](t, r.stdout)
	assert.Equal(t, true, g["enabled"])
	assert.Equal(t, true, g["all_branches"])
	assert.Equal(t, "git@example.test:owner/{repo}.git", g["url_template"])
	run("config", "--enabled=false", "--all-branches=false").ok(t)
	g = decode[map[string]any](t, run("config", "show", "--json").ok(t).stdout)
	assert.Equal(t, false, g["enabled"])
	assert.Equal(t, false, g["all_branches"])
	assert.Equal(t, "*/5 * * * *", g["cron"], "omitted fields are not cleared")
	run("add", "proj", "git@example.test:owner/proj.git", "--cron", "*/10 * * * *", "--all-branches=false").ok(t)
	r = run("status", "proj", "--json").ok(t)
	s := decode[map[string]any](t, r.stdout)
	assert.Equal(t, "override", s["source"])
	assert.Equal(t, false, s["all_branches"])
	assert.Equal(t, "ok", s["state"])
	assert.Equal(t, "proj", s["repo"])
	assert.Contains(t, run("status").ok(t).stdout, "FAILURES")
	assert.Len(t, decode[[]any](t, run("status", "--json").ok(t).stdout), 1)
	run("remove", "proj").ok(t)
	run("exclude", "proj").ok(t)
	s = decode[map[string]any](t, run("status", "proj", "--json").ok(t).stdout)
	assert.Equal(t, "excluded", s["source"])
	run("include", "proj").ok(t)
	first := run("key", "show").ok(t)
	assert.Empty(t, first.stderr)
	firstFingerprint := decode[map[string]any](t, run("key", "show", "--json").ok(t).stdout)["fingerprint"].(string)
	rotated := run("key", "rotate").ok(t)
	assert.Contains(t, rotated.stderr, "replaced key "+firstFingerprint)
	assert.NotEqual(t, first.stdout, rotated.stdout)
	assert.Contains(t, rotated.stderr, "remotes refuse pushes until this key is added")
	assert.Equal(t, rotated.stdout, run("key", "show").ok(t).stdout)
	assert.Equal(t, []string{"authorized_key", "created_at", "fingerprint"}, keys(t, run("key", "show", "--json").ok(t).stdout))
}

func TestMirrorCLISyncHelpDocumentsBackgroundSync(t *testing.T) {
	r := runCLI(t, nil, "", "mirror", "sync", "--help").ok(t)
	assert.Contains(t, r.stdout, "queued")
	assert.Contains(t, r.stdout, "ermite mirror status")
}

type blockingPusher struct{ entered, release chan struct{} }

func (p blockingPusher) Push(ctx context.Context, _ ports.MirrorPush) (ports.MirrorPushResult, error) {
	p.entered <- struct{}{}
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	return ports.MirrorPushResult{}, nil
}

// A push that outlasts the server's wait bound answers "queued"; the CLI says it continues.
func TestMirrorCLISyncReportsBackgroundSync(t *testing.T) {
	p := blockingPusher{entered: make(chan struct{}, 4), release: make(chan struct{})}
	h := newHarnessWith(t, harnessOpts{env: map[string]string{config.EnvSecretsKey: secretsKeyA}}, func(o *options) {
		o.mirrors = func(c *usecase.MirrorsConfig) {
			c.ValidateURL = func(string) error { return nil }
			c.Pusher = p
			c.SyncWait = 50 * time.Millisecond
		}
	})
	h.createRepo("proj", domain.VisibilityPrivate)
	c := &cliHarness{harness: h, url: h.srv.URL}
	wt := h.workTree("main", "README.md")
	h.mustGit(wt, "push", h.urlAuth(adminToken, "/proj.git"), "main")
	c.cli(t, adminToken, "mirror", "add", "proj", "file:///nowhere", "--cron", "*/5 * * * *").ok(t)
	for _, args := range [][]string{{"sync", "proj", "--force"}, {"sync", "--all", "--force"}} {
		r := c.cli(t, adminToken, append([]string{"mirror"}, args...)...).ok(t)
		assert.Contains(t, r.stdout, "proj")
		assert.Contains(t, r.stdout, "true", "QUEUED column")
		assert.Contains(t, r.stderr, "continue in the background")
		assert.Contains(t, r.stderr, "ermite mirror status")
	}
	close(p.release)
	h.forge.Mirrors.Wait(h.ctx)
}
