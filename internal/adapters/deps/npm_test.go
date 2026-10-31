// SPDX-License-Identifier: AGPL-3.0-or-later
package deps

import (
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestNPMDetect(t *testing.T) {
	reg := mocks.NewDepsRegistry(t)
	reg.EXPECT().NPMVersions(mock.Anything, "x", "v1.0.0").Return([]domain.DepVersion{{Version: "v1.1.0"}}, nil).Once()
	files := npmFiles(t, map[string]string{"package.json": `{"dependencies":{"x":"^1.0.0","conflict":"1.0.0","local":"workspace:*"},"devDependencies":{"x":"^1.0.0","conflict":"2.0.0"}}`})
	got, err := (NPM{}).Detect(t.Context(), files, reg)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "x", got[0].Name)
	require.Equal(t, "v1.0.0", got[0].Current)
	paths := (NPM{}).Manifests(domain.PathList{Paths: []string{"package.json", "pnpm-lock.yaml", "node_modules/x/package.json", "nested/node_modules/y/yarn.lock", "web/package.json", ".yarnrc.yml", "README.md"}})
	require.Len(t, paths, 4)
}

func TestNPMValidate(t *testing.T) {
	original := `{"dependencies":{"x":"^1.0.0"},"scripts":{"postinstall":"echo unchanged"}}`
	updated := strings.Replace(original, "^1.0.0", "^1.1.0", 1)
	before := npmFiles(t, map[string]string{"package.json": `{"workspaces":["web/*"],"packageManager":"pnpm@9.15.0"}`, "web/a/package.json": original, "pnpm-lock.yaml": "lockfileVersion: '9.0'", "web/a/yarn.lock": "old"})
	u := update(domain.EcosystemNPM, "web/a", "x", "v1.0.0", "v1.1.0")
	for _, tc := range []struct {
		name    string
		changes map[string]string
		valid   bool
	}{
		{"valid", map[string]string{"web/a/package.json": updated, "pnpm-lock.yaml": "opaque"}, true},
		{"script", map[string]string{"web/a/package.json": strings.Replace(updated, "echo unchanged", "evil", 1)}, false},
		{"format", map[string]string{"web/a/package.json": updated + "\n"}, false},
		{"missing update", map[string]string{"pnpm-lock.yaml": "opaque"}, false},
		{"member lock", map[string]string{"web/a/package.json": updated, "web/a/yarn.lock": "opaque"}, false},
		{"new lock", map[string]string{"web/a/package.json": updated, "package-lock.json": "opaque"}, false},
		{"unrelated package", map[string]string{"web/a/package.json": updated, "package.json": "{}"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			effects, err := (NPM{}).Validate(before, npmFiles(t, tc.changes), []domain.DepUpdate{u})
			if tc.valid {
				require.NoError(t, err)
				require.Empty(t, effects)
			} else {
				require.Error(t, err)
			}
		})
	}
	details, node, err := (NPM{}).RunnerDetails(before, []domain.DepUpdate{u})
	require.NoError(t, err)
	require.Equal(t, "22", node)
	require.Equal(t, "^1.1.0", details[0].Range)
	require.Equal(t, "pnpm", details[0].Manager)
	require.Equal(t, "", details[0].Root)
}
