// SPDX-License-Identifier: AGPL-3.0-or-later

package client_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/client"
)

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestConfigFromEnv(t *testing.T) {
	cfg, err := client.ConfigFromEnv(env(map[string]string{
		client.EnvURL: " HTTPS://Git.Example.com/base/ ", client.EnvToken: " ermite_abc\n",
	}))
	require.NoError(t, err)
	assert.Equal(t, "https://Git.Example.com/base", cfg.URL)
	assert.Equal(t, "ermite_abc", cfg.Token)
}

func TestConfigFromEnvRejects(t *testing.T) {
	tests := []struct {
		name, url, want string
	}{
		{"unset", "", "ERMITE_URL is not set"},
		{"no scheme", "git.example.com", "not an http(s) URL"},
		{"ftp", "ftp://git.example.com", "not an http(s) URL"},
		{"no host", "https://", "has no host"},
		{"credentials", "https://user:s3cret@git.example.com", "must not contain credentials"},
		{"query", "https://git.example.com/?a=b", "query or a fragment"},
		{"fragment", "https://git.example.com/#x", "query or a fragment"},
		{"garbage", "http://[::1", "not a valid URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := client.ConfigFromEnv(env(map[string]string{client.EnvURL: tt.url}))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.NotContains(t, err.Error(), "s3cret", "credentials must never be echoed")
		})
	}
}

func TestInsecureTokenWarning(t *testing.T) {
	tests := []struct {
		url, token string
		warn       bool
	}{
		{"http://git.example.com", "t", true},
		{"http://10.0.0.5:7341", "t", true},
		{"http://git.example.com", "", false}, // nothing secret to leak
		{"https://git.example.com", "t", false},
		{"http://localhost:7341", "t", false},
		{"http://127.0.0.1:7341", "t", false},
		{"http://[::1]:7341", "t", false},
		{"http://dev.localhost", "t", false},
	}
	for _, tt := range tests {
		w := client.Config{URL: tt.url, Token: tt.token}.InsecureTokenWarning()
		assert.Equal(t, tt.warn, w != "", "%s token=%q: %q", tt.url, tt.token, w)
		assert.NotContains(t, w, tt.token+"_", "never echo the token")
	}
}

func TestParseURLErrorsDoNotEchoInput(t *testing.T) {
	for _, raw := range []string{"user:s3cret@host", "s3cret.example.com", "http://", "https://h/?k=s3cret", "http://[s3cret"} {
		_, err := client.ParseURL(raw)
		require.Error(t, err, raw)
		assert.NotContains(t, err.Error(), "s3cret", raw)
		assert.NotContains(t, err.Error(), "user:", raw)
	}
}
