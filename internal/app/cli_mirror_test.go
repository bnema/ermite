// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	target := s["target"].(map[string]any)
	assert.Equal(t, "MIRROR_SOURCE_OVERRIDE", target["source"])
	assert.Equal(t, false, target["all_branches"])
	assert.Contains(t, run("status").ok(t).stdout, "FAILURES")
	assert.Len(t, decode[[]any](t, run("status", "--json").ok(t).stdout), 1)
	run("remove", "proj").ok(t)
	run("exclude", "proj").ok(t)
	s = decode[map[string]any](t, run("status", "proj", "--json").ok(t).stdout)
	assert.Equal(t, "MIRROR_SOURCE_EXCLUDED", s["target"].(map[string]any)["source"])
	run("include", "proj").ok(t)
	first := run("key", "show").ok(t)
	assert.Empty(t, first.stderr)
	rotated := run("key", "rotate").ok(t)
	assert.NotEqual(t, first.stdout, rotated.stdout)
	assert.Contains(t, rotated.stderr, "remotes refuse pushes until this key is added")
	assert.Equal(t, rotated.stdout, run("key", "show").ok(t).stdout)
	assert.Equal(t, []string{"authorized_key", "created_at", "fingerprint"}, keys(t, run("key", "show", "--json").ok(t).stdout))
}
