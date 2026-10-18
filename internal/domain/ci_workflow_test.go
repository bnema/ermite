// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseWorkflowPath(t *testing.T) {
	for in, want := range map[string]string{
		"release.yml":                    ".github/workflows/release.yml",
		" release.yaml ":                 ".github/workflows/release.yaml",
		".github/workflows/release.yml":  ".github/workflows/release.yml",
		".github/workflows/a.b-c_d.yaml": ".github/workflows/a.b-c_d.yaml",
	} {
		got, err := ParseWorkflowPath(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{
		"", "  ", "release", "release.txt", ".yml", ".github/workflows/.yml",
		"sub/release.yml", ".github/workflows/sub/release.yml", ".github/workflows/../release.yml",
		"/.github/workflows/release.yml", ".github/other/release.yml",
		".github/workflows//release.yml", "./.github/workflows/release.yml", ".github/workflows/sub/../release.yml",
		".github/workflows/./release.yml", ".github//workflows/release.yml", "github/workflows/release.yml",
		".github/workflows/release.yml/", "..", "../release.yml", ".github/workflows/..", ".github/workflows/.", "x\\y.yml", "a\x00.yml", ".github/workflows/",
	} {
		_, err := ParseWorkflowPath(in)
		assert.ErrorIs(t, err, ErrInvalid, "%q", in)
	}
}
