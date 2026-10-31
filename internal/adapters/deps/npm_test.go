// SPDX-License-Identifier: AGPL-3.0-or-later
package deps

import (
	"context"
	"fmt"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"testing/synctest"
	"time"
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

// The runner writes `"<range>"` as raw bytes: the server's expected file must
// be byte-identical, so `>=` must not become `\u003e=`.
func TestNPMEditSpecRawBytes(t *testing.T) {
	for _, op := range []string{"", "^", "~", ">=", "="} {
		t.Run(op, func(t *testing.T) {
			got, err := npmEditSpec([]byte(`{"dependencies": {"x": "`+op+`1.0.0"}}`), "x", op+"1.0.0", op+"1.2.0-rc.1")
			require.NoError(t, err)
			require.Equal(t, `{"dependencies": {"x": "`+op+`1.2.0-rc.1"}}`, string(got))
		})
	}
	for _, bad := range []string{`1.0.0"evil`, "1.0.0+build", "<2.0.0", "^1.0"} {
		_, err := npmEditSpec([]byte(`{"dependencies":{"x":"1.0.0"}}`), "x", "1.0.0", bad)
		require.Error(t, err, bad)
	}
}

// One broken manifest is skipped; the other packages are still checked.
func TestNPMDetectSkipsInvalidManifests(t *testing.T) {
	reg := mocks.NewDepsRegistry(t)
	reg.EXPECT().NPMVersions(mock.Anything, "ok", "v1.0.0").Return([]domain.DepVersion{{Version: "v1.1.0"}}, nil).Twice()
	files := npmFiles(t, map[string]string{
		"good/package.json":          `{"dependencies":{"ok":"1.0.0"}}`,
		"invalid/package.json":       `{"dependencies":`,
		"template/package.json":      `{"dependencies":{"x":"{{version}}"},"name":{{name}}}`,
		"dup/package.json":           `{"dependencies":{"x":"1.0.0"},"dependencies":{"x":"1.0.0"}}`,
		"nonstring/package.json":     `{"dependencies":{"x":1}}`,
		"overflow/package.json":      `{"dependencies":{"ok":"1.0.0"},"config":{"n":1e400}}`,
		"latestpm/package.json":      `{"dependencies":{"x":"1.0.0"},"packageManager":"pnpm@latest"}`,
		"oldpnpm/package.json":       `{"dependencies":{"x":"1.0.0"}}`,
		"oldpnpm/pnpm-lock.yaml":     "lockfileVersion: 5.4\n",
		"mismatch/package.json":      `{"dependencies":{"x":"1.0.0"},"packageManager":"yarn@4.5.3"}`,
		"mismatch/package-lock.json": `{}`,
	})
	got, err := (NPM{}).Detect(t.Context(), files, reg)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "good", got[0].Directory)
	require.Equal(t, "overflow", got[1].Directory, "a huge number elsewhere is valid JSON")
	paths := (NPM{}).Manifests(domain.PathList{Paths: []string{"a/testdata/package.json", "fixtures/package.json", "x/__fixtures__/y/package.json", "pkg/package.json"}})
	require.Equal(t, []ports.DepsInput{{Path: tp(t, "pkg/package.json")}}, paths)
}

// Names and versions the runner refuses are never proposed; a registry
// failure skips one package, a done context fails the check.
func TestNPMDetectRunnerRulesAndRegistryErrors(t *testing.T) {
	reg := mocks.NewDepsRegistry(t)
	reg.EXPECT().NPMVersions(mock.Anything, "big", "v1.0.0").Return(nil, domain.ErrResourceExhausted).Once()
	reg.EXPECT().NPMVersions(mock.Anything, "down", "v1.0.0").Return(nil, domain.ErrUnavailable).Once()
	reg.EXPECT().NPMVersions(mock.Anything, "ok", "v1.0.0").Return([]domain.DepVersion{{Version: "v1.2.0+build.5"}, {Version: "v1.1.0"}}, nil).Once()
	files := npmFiles(t, map[string]string{"package.json": `{"dependencies":{"JSONStream":"1.0.0","big":"1.0.0","down":"1.0.0","ok":"^1.0.0"}}`})
	got, err := (NPM{}).Detect(t.Context(), files, reg)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "ok", got[0].Name)
	require.Equal(t, []domain.DepVersion{{Version: "v1.1.0"}}, got[0].Versions)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = (NPM{}).Detect(ctx, files, reg) // done before the first lookup
	require.ErrorIs(t, err, context.Canceled)
}

func TestNPMValidatePackageLock(t *testing.T) {
	original := `{"dependencies":{"x":"^1.0.0"}}`
	updated := strings.Replace(original, "^1.0.0", "^1.1.0", 1)
	lock := func(resolved ...string) string {
		var pkgs []string
		for i, r := range resolved {
			pkgs = append(pkgs, fmt.Sprintf(`"node_modules/p%d":{"version":"1.0.0","resolved":%q}`, i, r))
		}
		return `{"lockfileVersion":3,"packages":{` + strings.Join(pkgs, ",") + `}}`
	}
	old := lock("https://registry.npmjs.org/p0/-/p0-1.0.0.tgz", "git+ssh://git@github.com/a/b.git#abc")
	before := npmFiles(t, map[string]string{"package.json": original, "package-lock.json": old})
	u := update(domain.EcosystemNPM, "", "x", "v1.0.0", "v1.1.0")
	for _, tc := range []struct {
		name, lock string
		valid      bool
	}{
		{"registry", lock("https://registry.npmjs.org/p0/-/p0-1.1.0.tgz", "git+ssh://git@github.com/a/b.git#abc"), true},
		{"new git", lock("https://registry.npmjs.org/p0/-/p0-1.0.0.tgz", "git+ssh://git@github.com/a/b.git#evil"), false},
		{"file", lock("file:../evil"), false},
		{"other registry", lock("https://evil.example/p0.tgz"), false},
		{"v1 version url", `{"lockfileVersion":1,"dependencies":{"p":{"version":"link:../evil"}}}`, false},
		{"invalid", `{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (NPM{}).Validate(before, npmFiles(t, map[string]string{"package.json": updated, "package-lock.json": tc.lock}), []domain.DepUpdate{u})
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, domain.ErrInvalid)
			}
		})
	}
}

func TestNPMWorkspacePatterns(t *testing.T) {
	for _, tc := range []struct {
		patterns []string
		rel      string
		want     bool
	}{
		{[]string{"./packages/*"}, "packages/a", true},
		{[]string{"packages/*/"}, "packages/a", true},
		{[]string{"packages/*", "!packages/private"}, "packages/private", false},
		{[]string{"packages/*", "!packages/private"}, "packages/a", true},
		{[]string{"!packages/a"}, "packages/a", false},
	} {
		require.Equal(t, tc.want, npmWorkspaceMatch(tc.patterns, tc.rel), "%v %s", tc.patterns, tc.rel)
	}
}

func TestNPMManagerRefusesOldNPM(t *testing.T) {
	for pm, ok := range map[string]bool{"npm@6.14.18": false, "npm@0.1.0": false, "npm@7.0.0": true, "npm@10.9.2": true, "pnpm@6.35.1": true} {
		_, err := npmManager(nil, "", npmManifest{PackageManager: pm})
		require.Equal(t, ok, err == nil, pm)
	}
	reg := mocks.NewDepsRegistry(t) // no lookup: the root is skipped
	got, err := (NPM{}).Detect(t.Context(), npmFiles(t, map[string]string{"package.json": `{"dependencies":{"x":"1.0.0"},"packageManager":"npm@6.14.18"}`}), reg)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestNPMManagerLockfiles(t *testing.T) {
	_, err := npmManager(npmFiles(t, map[string]string{"pnpm-lock.yaml": "lockfileVersion: 5.4\n"}), "", npmManifest{})
	require.Error(t, err, "pnpm 5.x lockfiles have no supported writer")
	root, err := npmManager(npmFiles(t, map[string]string{"pnpm-lock.yaml": "lockfileVersion: 5.4\n"}), "", npmManifest{PackageManager: "pnpm@7.33.0"})
	require.NoError(t, err, "an explicit packageManager names the writer")
	require.Equal(t, "7.33.0", root.Version)
	root, err = npmManager(npmFiles(t, map[string]string{"package-lock.json": "{}", "npm-shrinkwrap.json": "{}"}), "", npmManifest{})
	require.NoError(t, err)
	require.Equal(t, "npm-shrinkwrap.json", root.Lockfile)
	root, err = npmManager(npmFiles(t, map[string]string{"yarn.lock": "# yarn lockfile\n\n__metadata:\n  version: 8\n  cacheKey: 10c0\n"}), "", npmManifest{})
	require.NoError(t, err)
	require.Equal(t, "4.5.3", root.Version, "Berry metadata read from a header")
}

// Once the lookup budget is spent, the remaining packages are skipped and the
// candidates found so far kept; cancelling the caller still fails Detect.
// synctest's fake clock makes the three-minute budget instant.
func TestNPMDetectLookupBudget(t *testing.T) {
	files := npmFiles(t, map[string]string{"package.json": `{"dependencies":{"a":"1.0.0","b":"1.0.0","c":"1.0.0"}}`})
	synctest.Test(t, func(t *testing.T) {
		reg := mocks.NewDepsRegistry(t)
		reg.EXPECT().NPMVersions(mock.Anything, "a", "v1.0.0").Return([]domain.DepVersion{{Version: "v1.1.0"}}, nil).Once()
		reg.EXPECT().NPMVersions(mock.Anything, "b", "v1.0.0").RunAndReturn(func(ctx context.Context, _, _ string) ([]domain.DepVersion, error) {
			<-ctx.Done() // a document that never arrives
			return nil, ctx.Err()
		}).Once()
		// The caller allows ten minutes: the lookups get npmLookupBudget.
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
		defer cancel()
		start := time.Now()
		got, err := (NPM{}).Detect(ctx, files, reg)
		require.NoError(t, err)
		require.Equal(t, npmLookupBudget, time.Since(start))
		require.Len(t, got, 1)
		require.Equal(t, "a", got[0].Name)
	})
	synctest.Test(t, func(t *testing.T) {
		reg := mocks.NewDepsRegistry(t)
		reg.EXPECT().NPMVersions(mock.Anything, "a", "v1.0.0").RunAndReturn(func(ctx context.Context, _, _ string) ([]domain.DepVersion, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}).Once()
		// A one-minute caller leaves 30 s of lookups, then its deadline ends it
		// only after Detect returned what it had.
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		start := time.Now()
		got, err := (NPM{}).Detect(ctx, files, reg)
		require.NoError(t, err)
		require.Equal(t, 30*time.Second, time.Since(start))
		require.Empty(t, got)
	})
	synctest.Test(t, func(t *testing.T) {
		reg := mocks.NewDepsRegistry(t)
		ctx, cancel := context.WithCancel(t.Context())
		reg.EXPECT().NPMVersions(mock.Anything, "a", "v1.0.0").RunAndReturn(func(ctx context.Context, _, _ string) ([]domain.DepVersion, error) {
			cancel()
			return nil, ctx.Err()
		}).Once()
		_, err := (NPM{}).Detect(ctx, files, reg)
		require.ErrorIs(t, err, context.Canceled)
	})
}
