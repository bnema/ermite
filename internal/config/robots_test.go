// SPDX-License-Identifier: AGPL-3.0-or-later

package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/config"
)

func TestRobotsTxtFileUnset(t *testing.T) {
	c, err := config.Load(lookup(valid(nil)))
	require.NoError(t, err)
	assert.Nil(t, c.RobotsTxt, "nil serves the default")
}

func TestRobotsTxtFileRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "robots.txt")
	body := "User-agent: *\nDisallow: /\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	c, err := config.Load(lookup(valid(map[string]string{config.EnvRobotsTxtFile: path})))
	require.NoError(t, err)
	assert.Equal(t, body, string(c.RobotsTxt), "the content is kept as is")
}

func TestRobotsTxtFileProblems(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))
	big := filepath.Join(dir, "big")
	require.NoError(t, os.WriteFile(big, []byte(strings.Repeat("#\n", config.MaxRobotsTxtSize/2+1)), 0o600))

	for name, tc := range map[string]struct{ path, want string }{
		"missing":   {filepath.Join(dir, "nope"), "cannot read"},
		"directory": {dir, "is not a regular file"},
		"empty":     {empty, "is empty"},
		"too large": {big, "larger than 500 KiB"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := config.Load(lookup(valid(map[string]string{config.EnvRobotsTxtFile: tc.path})))
			require.Error(t, err)
			assert.Contains(t, err.Error(), config.EnvRobotsTxtFile+": ")
			assert.Contains(t, err.Error(), tc.want)
			assert.Contains(t, err.Error(), tc.path, "the operator sees which file")
		})
	}
}
