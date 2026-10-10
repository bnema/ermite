// SPDX-License-Identifier: AGPL-3.0-or-later

package config_test

import (
	"crypto/sha256"
	"errors"
	"net/netip"
	"strings"
	"testing"

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
