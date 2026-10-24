// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/config"
)

func TestLoadConfigChecksTheDepsCron(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		config.EnvDataDir: "/data", config.EnvPublicURL: "https://forge.example.org",
		config.EnvAdminToken: "0123456789abcdef0123456789abcdef", config.EnvDepsCheckAt: "not a cron",
	}
	_, err := loadConfig(func(k string) string { return env[k] })
	var ce *config.Error
	require.True(t, errors.As(err, &ce))
	assert.Contains(t, ce.Problems[0], config.EnvDepsCheckAt)

	env[config.EnvDepsCheckAt] = "30 5 * * 1"
	cfg, err := loadConfig(func(k string) string { return env[k] })
	require.NoError(t, err)
	assert.Equal(t, "30 5 * * 1", cfg.DepsCheckAt)
}

func TestLoadConfigChecksTheOwnerName(t *testing.T) {
	t.Parallel()
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
		return loadConfig(func(k string) string { return env[k] })
	}

	cfg, err := load(map[string]string{config.EnvOwner: " bnema "})
	require.NoError(t, err)
	assert.Equal(t, "bnema", cfg.Owner)
	cfg, err = load(nil)
	require.NoError(t, err)
	assert.Equal(t, config.DefaultOwner, cfg.Owner)

	for _, owner := range []string{"Bad Name", ".dot", "a/b", "x.git"} {
		_, err := load(map[string]string{config.EnvOwner: owner})
		var ce *config.Error
		require.True(t, errors.As(err, &ce), owner)
		assert.Contains(t, ce.Problems[0], config.EnvOwner, owner)
	}

	// The problem is reported with the others, in one error.
	_, err = load(map[string]string{config.EnvOwner: "Bad", config.EnvCILogRetention: "soon"})
	var ce *config.Error
	require.True(t, errors.As(err, &ce))
	assert.Len(t, ce.Problems, 2)
}
