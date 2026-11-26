// SPDX-License-Identifier: AGPL-3.0-or-later
package deps

import (
	"bnema.dev/ermite/internal/domain"
	"cmp"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func npmFiles(t *testing.T, in map[string]string) map[domain.TreePath][]byte {
	t.Helper()
	out := map[domain.TreePath][]byte{}
	for p, b := range in {
		out[tp(t, p)] = []byte(b)
	}
	return out
}
func npmRootsOf(t *testing.T, files map[domain.TreePath][]byte, dirs ...string) map[string]npmRoot {
	t.Helper()
	out := map[string]npmRoot{}
	for _, d := range dirs {
		r, err := npmRootOf(files, d)
		require.NoError(t, err, d)
		out[d] = r
	}
	return out
}
func TestNPMSpec(t *testing.T) {
	for _, op := range []string{"", "^", "~", ">=", "="} {
		for _, v := range []string{"1.2.3", "v1.2.3", "1.2.3-beta.1"} {
			t.Run(op+v, func(t *testing.T) {
				o, version, ok := npmSpec(op + v)
				require.True(t, ok)
				require.Equal(t, op, o)
				require.Equal(t, "v"+strings.TrimPrefix(v, "v"), version)
			})
		}
	}
	for _, s := range []string{"workspace:^", "file:foo", "link:foo", "portal:x", "patch:x", "catalog:x", "jsr:x", "npm:foo@1.2.3", "github:a/b", "git+https://x", "https://x", "owner/repo", "*", "latest", "1.2.3 || 2.0.0", "1.0.0 - 2.0.0", "1.x", "01.2.3", "1.2"} {
		_, _, ok := npmSpec(s)
		require.False(t, ok, s)
	}
}
func TestNPMEditSpec(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		fail              bool
	}{
		{"format", `{ "dependencies" : {"x" : "^1.2.3"}, "scripts":{"x":"^1.2.3"}, "devDependencies":{"x":"^1.2.3"}}`, `{ "dependencies" : {"x" : "^2.0.0"}, "scripts":{"x":"^1.2.3"}, "devDependencies":{"x":"^2.0.0"}}`, false},
		{"escaped", `{"dependencies":{"\u0078":"^1.2.3"}}`, `{"dependencies":{"\u0078":"^2.0.0"}}`, false},
		{"missing", `{"scripts":{"x":"^1.2.3"}}`, "", true},
		{"conflict", `{"dependencies":{"x":"^1.2.3"},"devDependencies":{"x":"~1.2.3"}}`, "", true},
		{"duplicate", `{"dependencies":{"x":"^1.2.3","x":"^1.2.3"}}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := npmEditSpec([]byte(tc.input), "x", "^1.2.3", "^2.0.0")
			if tc.fail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, string(got))
			}
		})
	}
}
func TestNPMRoots(t *testing.T) {
	for _, workspaces := range []string{`["packages/*","nested/**"]`, `{"packages":["packages/*","nested/**"]}`} {
		files := npmFiles(t, map[string]string{"package.json": `{"workspaces":` + workspaces + `,"packageManager":"pnpm@9.15.0","engines":{"node":">=20"}}`, "packages/a/package.json": `{}`, "nested/root/package.json": `{"workspaces":["children/*"],"packageManager":"yarn@1.22.22"}`, "nested/root/children/a/package.json": `{}`, "other/package.json": `{}`})
		roots := npmRootsOf(t, files, "packages/a", "nested/root/children/a", "other")
		require.Equal(t, "", roots["packages/a"].Directory)
		require.Equal(t, "pnpm", roots["packages/a"].Manager)
		require.Equal(t, "20", roots["packages/a"].NodeVersion)
		require.Equal(t, "nested/root", roots["nested/root/children/a"].Directory)
		require.Equal(t, "other", roots["other"].Directory)
	}
	roots := npmRootsOf(t, npmFiles(t, map[string]string{"pnpm-workspace.yaml": "packages:\n - 'web/*'\n", "web/a/package.json": "{}", "pnpm-lock.yaml": "lockfileVersion: '6.0'\n"}), "web/a")
	require.Equal(t, "", roots["web/a"].Directory)
	require.Equal(t, "8.15.9", roots["web/a"].Version)
	for _, tc := range []struct {
		pattern, rel string
		want         bool
	}{{"a/*", "a/b", true}, {"a/*", "a/b/c", false}, {"a/**", "a/b/c", true}, {"a/[bc]", "a/b", true}, {"a/*", "ab/b", false}} {
		require.Equal(t, tc.want, npmWorkspaceMatch([]string{tc.pattern}, tc.rel))
	}
}
func TestNPMManagersAndNode(t *testing.T) {
	for _, tc := range []struct{ file, content, manager, version string }{{"package-lock.json", "{}", "npm", ""}, {"npm-shrinkwrap.json", "{}", "npm", ""}, {"pnpm-lock.yaml", "lockfileVersion: 9.0", "pnpm", "9.15.0"}, {"pnpm-lock.yaml", "lockfileVersion: '6.0'", "pnpm", "8.15.9"}, {"yarn.lock", "# yarn lockfile v1", "yarn", "1.22.22"}, {"bun.lock", "{}", "bun", "1.2.0"}, {"bun.lockb", "binary", "bun", "1.2.0"}} {
		t.Run(tc.file+tc.version, func(t *testing.T) {
			root, err := npmManager(npmFiles(t, map[string]string{tc.file: tc.content}), "", npmManifest{})
			require.NoError(t, err)
			require.Equal(t, tc.manager, root.Manager)
			require.Equal(t, tc.version, root.Version)
		})
	}
	for spec, want := range map[string]string{">=16": "18", "^20": "20", "~21": "21", "23.x": "23", "24.0.0": "24", "26": "26", "v22.11.0": "22", ">= 20": "20", "^20.17.0 || >=22.9.0": "22", ">=20 <25": "20", ">=100": "26", "lts/*": "26", "bad": "26", "": "26"} {
		require.Equal(t, want, npmNode(nil, npmRoot{Manager: "npm"}, npmManifest{Engines: struct {
			Node string `json:"node"`
		}{spec}}), spec)
	}
	for _, tc := range []struct {
		name, manifest, manager, version, want string
		files                                  map[string]string
	}{
		{name: "nvmrc", manifest: `{"engines":{"node":">=18"}}`, files: map[string]string{".nvmrc": "v24.1.0\n"}, want: "24"},
		{name: "node-version before nvmrc", manifest: `{}`, files: map[string]string{".node-version": "26\n", ".nvmrc": "20"}, want: "26"},
		{name: "nvmrc alias falls through", manifest: `{"engines":{"node":"^20"}}`, files: map[string]string{".nvmrc": "lts/*"}, want: "20"},
		{name: "devEngines object first", manifest: `{"devEngines":{"runtime":{"name":"node","version":">=25"}}}`, files: map[string]string{".nvmrc": "20"}, want: "25"},
		{name: "devEngines list", manifest: `{"devEngines":{"runtime":[{"name":"bun","version":"1"},{"name":"node","version":"^24"}]}}`, want: "24"},
		{name: "devEngines odd shape ignored", manifest: `{"devEngines":{"runtime":"node"},"engines":{"node":"20"}}`, want: "20"},
		{name: "pnpm 11 needs 22", manifest: `{"engines":{"node":">=18"}}`, manager: "pnpm", version: "11.0.0", want: "22"},
		{name: "pnpm 12 keeps the declared Node", manifest: `{"engines":{"node":">=20"}}`, manager: "pnpm", version: "12.15.0", want: "20"},
		{name: "npm 11 needs 20", manifest: `{"engines":{"node":"18"}}`, manager: "npm", version: "11.0.0", want: "20"},
		{name: "npm 12 needs 22", manifest: `{"engines":{"node":"18"}}`, manager: "npm", version: "12.2.3", want: "22"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := parseNPMManifest([]byte(tc.manifest))
			require.NoError(t, err)
			require.Equal(t, tc.want, npmNode(npmFiles(t, tc.files), npmRoot{Manager: cmp.Or(tc.manager, "npm"), Version: tc.version}, m))
		})
	}
	// The Node files sit at the workspace root, which npmRootOf resolves.
	roots := npmRootsOf(t, npmFiles(t, map[string]string{"package.json": `{"workspaces":["app"]}`, ".nvmrc": "24", "app/package.json": `{}`}), "app")
	require.Equal(t, "24", roots["app"].NodeVersion)
	root, err := npmManager(npmFiles(t, map[string]string{"package-lock.json": "{}"}), "", npmManifest{PackageManager: "npm@10.1.0"})
	require.NoError(t, err)
	require.Equal(t, "npm", root.Manager)
	require.Equal(t, "10.1.0", root.Version)
	_, err = npmManager(npmFiles(t, map[string]string{"bun.lock": "{}"}), "", npmManifest{PackageManager: "npm@10.1.0"})
	require.Error(t, err, "packageManager tool differs from the lockfile")
	_, err = npmManager(nil, "", npmManifest{PackageManager: "evil@1.2.3"})
	require.Error(t, err)
}
