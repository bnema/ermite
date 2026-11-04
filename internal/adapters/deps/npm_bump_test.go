// SPDX-License-Identifier: AGPL-3.0-or-later
package deps

import (
	"bytes"
	"maps"
	"testing"

	"bnema.dev/ermite/internal/domain"
	"github.com/stretchr/testify/require"
)

func cloneFiles(in map[domain.TreePath][]byte) map[domain.TreePath][]byte {
	out := map[domain.TreePath][]byte{}
	for k, v := range in {
		out[k] = bytes.Clone(v)
	}
	return out
}

func TestNPMBump(t *testing.T) {
	root := "{\n  \"name\": \"app\",\n  \"workspaces\": [\"web\"],\n  \"packageManager\": \"pnpm@9.15.0\",\n" +
		"  \"dependencies\": {\n    \"a\":   \"^1.2.0\",\n    \"b\" : \"~2.0.1\",\n    \"c\": \"3.0.0\"\n  },\n" +
		"  \"devDependencies\": {\t\"d\": \">=0.1.0\" },\n  \"scripts\": {\"a\": \"^1.2.0\"}\n}\n"
	web := `{"dependencies":{"a":"^1.2.0"}}`
	files := npmFiles(t, map[string]string{"package.json": root, "web/package.json": web, "pnpm-lock.yaml": "lockfileVersion: '9.0'"})
	orig := cloneFiles(files)
	ups := []domain.DepUpdate{
		update(domain.EcosystemNPM, "", "a", "v1.2.0", "v1.9.0"),
		update(domain.EcosystemNPM, "", "b", "v2.0.1", "v2.1.0"),
		update(domain.EcosystemNPM, "", "c", "v3.0.0", "v3.0.4"),
		update(domain.EcosystemNPM, "", "d", "v0.1.0", "v0.2.0-rc.1"),
		update(domain.EcosystemNPM, "web", "a", "v1.2.0", "v1.3.0"),
	}
	got, err := (NPM{}).Bump(files, ups)
	require.NoError(t, err)
	require.Equal(t, orig, files, "input mutated")
	wantRoot := "{\n  \"name\": \"app\",\n  \"workspaces\": [\"web\"],\n  \"packageManager\": \"pnpm@9.15.0\",\n" +
		"  \"dependencies\": {\n    \"a\":   \"^1.9.0\",\n    \"b\" : \"~2.1.0\",\n    \"c\": \"3.0.4\"\n  },\n" +
		"  \"devDependencies\": {\t\"d\": \">=0.2.0-rc.1\" },\n  \"scripts\": {\"a\": \"^1.2.0\"}\n}\n"
	require.Equal(t, map[domain.TreePath][]byte{
		tp(t, "package.json"):     []byte(wantRoot),
		tp(t, "web/package.json"): []byte(`{"dependencies":{"a":"^1.3.0"}}`),
	}, got)

	// The manifest-only commit is exactly what Validate expects from a runner.
	effects, err := (NPM{}).Validate(files, got, ups)
	require.NoError(t, err)
	require.Empty(t, effects)

	// A no-op bump returns no changed file and leaves its input alone.
	before := maps.Clone(got)
	same, err := (NPM{}).Bump(got, []domain.DepUpdate{update(domain.EcosystemNPM, "", "a", "v1.9.0", "v1.9.0")})
	require.NoError(t, err)
	require.Empty(t, same)
	require.Equal(t, before, got)
}

func TestNPMBumpErrors(t *testing.T) {
	files := npmFiles(t, map[string]string{"package.json": `{"dependencies":{"a":"^1.0.0","g":"github:x/y"}}`})
	for name, u := range map[string]domain.DepUpdate{
		"missing package":  update(domain.EcosystemNPM, "", "zz", "v1.0.0", "v1.1.0"),
		"missing manifest": update(domain.EcosystemNPM, "web", "a", "v1.0.0", "v1.1.0"),
		"not editable":     update(domain.EcosystemNPM, "", "g", "v1.0.0", "v1.1.0"),
		"current changed":  update(domain.EcosystemNPM, "", "a", "v0.9.0", "v1.1.0"),
		"bad version":      update(domain.EcosystemNPM, "", "a", "v1.0.0", "v1.1"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := (NPM{}).Bump(files, []domain.DepUpdate{u})
			require.ErrorIs(t, err, domain.ErrPreconditionFailed)
		})
	}
}
