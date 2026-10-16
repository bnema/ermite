// SPDX-License-Identifier: AGPL-3.0-or-later

package config_test

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/config"
)

const token = "0123456789abcdef0123456789abcdef" // 32 chars

func lookup(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

func valid(overrides map[string]string) map[string]string {
	env := map[string]string{
		config.EnvDataDir:    "/var/lib/ermite",
		config.EnvPublicURL:  "https://git.example.com",
		config.EnvAdminToken: token,
	}
	for k, v := range overrides {
		if v == "\x00unset" {
			delete(env, k)
			continue
		}
		env[k] = v
	}
	return env
}

func TestLoadDefaults(t *testing.T) {
	c, err := config.Load(lookup(valid(nil)))
	require.NoError(t, err)
	assert.Equal(t, ":7341", c.Addr)
	assert.Equal(t, "/var/lib/ermite", c.DataDir)
	assert.Equal(t, "https://git.example.com", c.PublicURL)
	assert.Equal(t, "git", c.Git)
	assert.Equal(t, "ermite-git", c.GitReader, "ermite-git reads by default")
	assert.Equal(t, "https://git.example.com/ermite", c.SourceURL)
	assert.Equal(t, "info", c.LogLevel)
	assert.Equal(t, "json", c.LogFormat)
	assert.Empty(t, c.TrustedProxies)
	assert.Equal(t, sha256.Sum256([]byte(token)), c.AdminTokenHash)
}

func TestLoadAllValues(t *testing.T) {
	c, err := config.Load(lookup(valid(map[string]string{
		config.EnvAddr:           "127.0.0.1:8080",
		config.EnvDataDir:        "/srv/../srv/data/",
		config.EnvPublicURL:      "http://localhost:7341/",
		config.EnvTrustedProxies: "10.1.2.3/8, fd00::1/8",
		config.EnvGit:            "/usr/local/bin/git",
		config.EnvGitReader:      " CLI ",
		config.EnvSourceURL:      "https://forge.example.org/src/",
		config.EnvLogLevel:       "DEBUG",
		config.EnvLogFormat:      "Console",
	})))
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:8080", c.Addr)
	assert.Equal(t, "/srv/data", c.DataDir)
	assert.Equal(t, "http://localhost:7341", c.PublicURL)
	assert.Equal(t, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}, c.TrustedProxies)
	assert.Equal(t, "/usr/local/bin/git", c.Git)
	assert.Equal(t, "cli", c.GitReader)
	assert.Equal(t, "https://forge.example.org/src", c.SourceURL)
	assert.Equal(t, "debug", c.LogLevel)
	assert.Equal(t, "console", c.LogFormat)
}

func TestLoadSourceURLFollowsPublicURL(t *testing.T) {
	c, err := config.Load(lookup(valid(map[string]string{config.EnvPublicURL: "http://127.0.0.1:7341/"})))
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:7341/ermite", c.SourceURL)
}

func TestLoadProblems(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string // variable named in the error
		msg  string // fragment of its message
	}{
		{"missing data dir", map[string]string{config.EnvDataDir: "\x00unset"}, config.EnvDataDir, "required"},
		{"empty data dir", map[string]string{config.EnvDataDir: ""}, config.EnvDataDir, "required"},
		{"relative data dir", map[string]string{config.EnvDataDir: "data"}, config.EnvDataDir, "absolute"},
		{"missing public url", map[string]string{config.EnvPublicURL: "\x00unset"}, config.EnvPublicURL, "required"},
		{"public url scheme", map[string]string{config.EnvPublicURL: "ftp://x.org"}, config.EnvPublicURL, "scheme"},
		{"public url no scheme", map[string]string{config.EnvPublicURL: "git.example.com"}, config.EnvPublicURL, "scheme"},
		{"public url no host", map[string]string{config.EnvPublicURL: "https://"}, config.EnvPublicURL, "host"},
		{"public url path", map[string]string{config.EnvPublicURL: "https://x.org/forge"}, config.EnvPublicURL, "path"},
		{"public url query", map[string]string{config.EnvPublicURL: "https://x.org?a=b"}, config.EnvPublicURL, "query"},
		{"public url fragment", map[string]string{config.EnvPublicURL: "https://x.org#top"}, config.EnvPublicURL, "fragment"},
		{"public url credentials", map[string]string{config.EnvPublicURL: "https://u:p@x.org"}, config.EnvPublicURL, "credentials"},
		{"missing token", map[string]string{config.EnvAdminToken: "\x00unset"}, config.EnvAdminToken, "required"},
		{"short token", map[string]string{config.EnvAdminToken: strings.Repeat("a", 31)}, config.EnvAdminToken, "at least 32"},
		{"bad addr", map[string]string{config.EnvAddr: "7341"}, config.EnvAddr, "host:port"},
		{"bad port", map[string]string{config.EnvAddr: ":http"}, config.EnvAddr, "port"},
		{"port zero", map[string]string{config.EnvAddr: ":0"}, config.EnvAddr, "port"},
		{"port too big", map[string]string{config.EnvAddr: ":70000"}, config.EnvAddr, "port"},
		{"bad cidr", map[string]string{config.EnvTrustedProxies: "10.0.0.1"}, config.EnvTrustedProxies, "CIDR"},
		{"empty cidr entry", map[string]string{config.EnvTrustedProxies: "10.0.0.0/8,,"}, config.EnvTrustedProxies, "empty"},
		{"bad git reader", map[string]string{config.EnvGitReader: "libgit2"}, config.EnvGitReader, "cli or ermite-git"},
		{"git reader typo", map[string]string{config.EnvGitReader: "ermite_git"}, config.EnvGitReader, `"ermite_git"`},
		{"bad source url", map[string]string{config.EnvSourceURL: "nope"}, config.EnvSourceURL, "scheme"},
		{"bad level", map[string]string{config.EnvLogLevel: "loud"}, config.EnvLogLevel, "trace, debug"},
		{"bad format", map[string]string{config.EnvLogFormat: "xml"}, config.EnvLogFormat, "json or console"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(lookup(valid(tt.env)))
			require.Error(t, err)
			var ce *config.Error
			require.True(t, errors.As(err, &ce))
			require.Len(t, ce.Problems, 1, err.Error())
			assert.True(t, strings.HasPrefix(ce.Problems[0], tt.want+":"), ce.Problems[0])
			assert.Contains(t, ce.Problems[0], tt.msg)
		})
	}
}

func TestLoadReportsEveryProblemTogether(t *testing.T) {
	_, err := config.Load(lookup(map[string]string{
		config.EnvAddr:      "bad",
		config.EnvLogLevel:  "loud",
		config.EnvLogFormat: "xml",
	}))
	require.Error(t, err)
	msg := err.Error()
	for _, name := range []string{
		config.EnvAddr, config.EnvDataDir, config.EnvPublicURL, config.EnvAdminToken, config.EnvLogLevel, config.EnvLogFormat,
	} {
		assert.Contains(t, msg, name)
	}
	var ce *config.Error
	require.True(t, errors.As(err, &ce))
	assert.Len(t, ce.Problems, 6)
}

func TestErrorNeverContainsToken(t *testing.T) {
	short := "secret-but-too-short"
	_, err := config.Load(lookup(valid(map[string]string{config.EnvAdminToken: short, config.EnvAddr: "bad"})))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), short)
}

func TestTokenLengthCountsCharacters(t *testing.T) {
	_, err := config.Load(lookup(valid(map[string]string{config.EnvAdminToken: strings.Repeat("é", 32)})))
	assert.NoError(t, err)
	_, err = config.Load(lookup(valid(map[string]string{config.EnvAdminToken: strings.Repeat("é", 31)})))
	assert.Error(t, err)
}

func TestLoadCommitterDefaults(t *testing.T) {
	c, err := config.Load(lookup(valid(nil)))
	require.NoError(t, err)
	assert.Equal(t, "ermite", c.CommitterName)
	assert.Equal(t, "ermite@git.example.com", c.CommitterEmail, "the host of the public URL, without port or path")

	c, err = config.Load(lookup(valid(map[string]string{config.EnvPublicURL: "http://localhost:7341/"})))
	require.NoError(t, err)
	assert.Equal(t, "ermite@localhost", c.CommitterEmail)
}

func TestLoadCommitterFromTheEnvironment(t *testing.T) {
	c, err := config.Load(lookup(valid(map[string]string{
		config.EnvCommitterName:  " Forge Bot ",
		config.EnvCommitterEmail: "bot@example.org",
	})))
	require.NoError(t, err)
	assert.Equal(t, "Forge Bot", c.CommitterName)
	assert.Equal(t, "bot@example.org", c.CommitterEmail)
}

func TestLoadRejectsBadCommitter(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"name with an angle bracket":  {config.EnvCommitterName: "Bot <x>"},
		"name with a line break":      {config.EnvCommitterName: "Bot\nCo-authored-by: x"},
		"name not utf-8":              {config.EnvCommitterName: "\xff"},
		"email without an @":          {config.EnvCommitterEmail: "bot"},
		"email with a space":          {config.EnvCommitterEmail: "a b@example.org"},
		"email with an angle bracket": {config.EnvCommitterEmail: "<bot@example.org>"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := config.Load(lookup(valid(env)))
			var ce *config.Error
			require.ErrorAs(t, err, &ce)
			assert.Contains(t, ce.Problems[0], "ERMITE_COMMITTER_")
		})
	}
}

func TestLoadGitReader(t *testing.T) {
	for in, want := range map[string]string{
		"": config.GitReaderErmiteGit, "ermite-git": config.GitReaderErmiteGit, "ERMITE-GIT": config.GitReaderErmiteGit,
		"cli": config.GitReaderCLI, "Cli": config.GitReaderCLI,
	} {
		c, err := config.Load(lookup(valid(map[string]string{config.EnvGitReader: in})))
		require.NoError(t, err, in)
		assert.Equal(t, want, c.GitReader, in)
	}
}

func TestOwnerAndCIRetention(t *testing.T) {
	base := map[string]string{
		config.EnvDataDir: "/data", config.EnvPublicURL: "https://forge.example.org",
		config.EnvAdminToken: "0123456789abcdef0123456789abcdef",
	}
	load := func(extra map[string]string) (config.Config, error) {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		return config.Load(func(k string) string { return env[k] })
	}

	c, err := load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Owner != "owner" || c.CIRetention != 0 {
		t.Fatalf("defaults (retention is left to the use case): owner %q, retention %v", c.Owner, c.CIRetention)
	}

	c, err = load(map[string]string{config.EnvOwner: "bnema", config.EnvCILogRetention: "30d"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Owner != "bnema" || c.CIRetention != 30*24*time.Hour {
		t.Fatalf("set: owner %q, retention %v", c.Owner, c.CIRetention)
	}
	if c, err = load(map[string]string{config.EnvCILogRetention: "48h"}); err != nil || c.CIRetention != 48*time.Hour {
		t.Fatalf("go duration: %v, %v", c.CIRetention, err)
	}

	for _, extra := range []map[string]string{
		{config.EnvCILogRetention: "soon"}, {config.EnvCILogRetention: "0d"}, {config.EnvCILogRetention: "10m"},
	} {
		if _, err := load(extra); err == nil {
			t.Errorf("%v: want an error", extra)
		}
	}
}

func TestCIPollTimeout(t *testing.T) {
	load := func(extra map[string]string) (config.Config, error) {
		env := map[string]string{
			config.EnvDataDir: "/data", config.EnvPublicURL: "https://forge.example.org",
			config.EnvAdminToken: "0123456789abcdef0123456789abcdef",
		}
		for k, v := range extra {
			env[k] = v
		}
		return config.Load(func(k string) string { return env[k] })
	}

	c, err := load(nil)
	require.NoError(t, err)
	assert.Zero(t, c.CIPollTimeout, "unset: the default is left to the CI use case")

	for in, want := range map[string]time.Duration{
		"25s": 25 * time.Second, " 30s ": 30 * time.Second, "1s": time.Second, "55s": 55 * time.Second, "1500ms": 1500 * time.Millisecond,
	} {
		c, err = load(map[string]string{config.EnvCIPollTimeout: in})
		require.NoError(t, err, in)
		assert.Equal(t, want, c.CIPollTimeout, in)
	}

	for _, in := range []string{"soon", "25", "0s", "-5s", "999ms", "56s", "2m"} {
		_, err := load(map[string]string{config.EnvCIPollTimeout: in})
		var ce *config.Error
		require.True(t, errors.As(err, &ce), in)
		assert.Contains(t, ce.Problems[0], config.EnvCIPollTimeout, in)
	}
}

func TestSecretsKey(t *testing.T) {
	key := make([]byte, config.SecretsKeyLen)
	for i := range key {
		key[i] = byte(i + 1)
	}
	std := base64.StdEncoding.EncodeToString(key)

	cfg, err := config.Load(lookup(valid(nil)))
	require.NoError(t, err)
	assert.Nil(t, cfg.SecretsKey, "unset: secrets are off")

	for name, enc := range map[string]string{
		"std": std, "raw": base64.RawStdEncoding.EncodeToString(key),
		"url": base64.URLEncoding.EncodeToString(key), "rawurl": base64.RawURLEncoding.EncodeToString(key), "spaces": " " + std + "\n",
	} {
		cfg, err = config.Load(lookup(valid(map[string]string{config.EnvSecretsKey: enc})))
		require.NoError(t, err, name)
		assert.Equal(t, key, cfg.SecretsKey, name)
	}

	for name, bad := range map[string]string{
		"short":      base64.StdEncoding.EncodeToString(key[:16]),
		"long":       base64.StdEncoding.EncodeToString(append(key, 1)),
		"not base64": "not base64 at all!!",
	} {
		_, err = config.Load(lookup(valid(map[string]string{config.EnvSecretsKey: bad})))
		var ce *config.Error
		require.ErrorAs(t, err, &ce, name)
		assert.Contains(t, err.Error(), config.EnvSecretsKey, name)
		assert.NotContains(t, err.Error(), bad, "the value is never repeated: "+name)
	}
}

func TestCIArtifacts(t *testing.T) {
	c, err := config.Load(lookup(valid(nil)))
	require.NoError(t, err)
	assert.True(t, c.CIArtifacts, "on by default")
	assert.Zero(t, c.CIArtifactRetention, "unset: the defaults are left to the CI use case")
	assert.Zero(t, c.CIArtifactQuota)
	assert.Zero(t, c.CIArtifactMax)

	c, err = config.Load(lookup(valid(map[string]string{
		config.EnvCIArtifacts: "0", config.EnvCIArtifactRetention: "30d",
		config.EnvCIArtifactQuota: "20GiB", config.EnvCIArtifactMax: "512 MiB",
	})))
	require.NoError(t, err)
	assert.False(t, c.CIArtifacts)
	assert.Equal(t, 30*24*time.Hour, c.CIArtifactRetention)
	assert.EqualValues(t, 20<<30, c.CIArtifactQuota)
	assert.EqualValues(t, 512<<20, c.CIArtifactMax)

	for in, want := range map[string]int64{"1048576": 1 << 20, "2048KiB": 2 << 20, "1TiB": 1 << 40, "3 GiB": 3 << 30, "5368709120B": 5 << 30} {
		c, err := config.Load(lookup(valid(map[string]string{config.EnvCIArtifactMax: in})))
		require.NoError(t, err, in)
		assert.Equal(t, want, c.CIArtifactMax, in)
	}

	for env, in := range map[string]string{
		config.EnvCIArtifacts:         "maybe",
		config.EnvCIArtifactRetention: "12h",
		config.EnvCIArtifactQuota:     "10GB",
		config.EnvCIArtifactMax:       "1KiB",
	} {
		_, err := config.Load(lookup(valid(map[string]string{env: in})))
		var ce *config.Error
		require.True(t, errors.As(err, &ce), env)
		assert.Contains(t, ce.Problems[0], env)
	}
	for _, in := range []string{"0", "-1GiB", "2048TiB", "9223372036854775807GiB", "GiB"} {
		_, err := config.Load(lookup(valid(map[string]string{config.EnvCIArtifactQuota: in})))
		require.Error(t, err, in)
	}
}
