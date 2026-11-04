// SPDX-License-Identifier: AGPL-3.0-or-later

package deps_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/deps"
	"bnema.dev/ermite/internal/adapters/depsregistry"
	"bnema.dev/ermite/internal/domain"
)

// A module whose proxy lists a version it cannot describe stays a
// candidate, with its other versions and its new major path, as
// github.com/hashicorp/golang-lru (v1.0.0 listed, no v1.0.0.info).
func TestGoModDetectKeepsAModuleWithAnUndescribedVersion(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/example.org/lru/@v/list":
			_, _ = fmt.Fprint(w, "v0.5.4\nv0.6.0\nv1.0.0\n")
		case "/example.org/lru/v2/@v/list":
			_, _ = fmt.Fprint(w, "v2.0.7\n")
		case "/example.org/lru/@v/v1.0.0.info":
			http.NotFound(w, r)
		default:
			if strings.HasSuffix(r.URL.Path, ".info") {
				_, _ = fmt.Fprint(w, `{"Time":"2026-01-01T00:00:00Z"}`)
				return
			}
			http.NotFound(w, r) // no /v3
		}
	}))
	defer s.Close()
	reg := depsregistry.NewCached(depsregistry.New(depsregistry.Config{GoProxyURL: s.URL}))
	gomod, err := domain.ParseTreePath("go.mod")
	require.NoError(t, err)
	got, _, err := (deps.GoMod{}).Detect(t.Context(), map[domain.TreePath][]byte{gomod: []byte("module example.org/app\ngo 1.24\nrequire example.org/lru v0.5.4\n")}, reg)
	require.NoError(t, err)
	require.Len(t, got, 1)
	var versions []string
	for _, v := range got[0].Versions {
		versions = append(versions, v.Version)
	}
	assert.Equal(t, []string{"v0.6.0", "v2.0.7"}, versions)
	assert.Equal(t, "example.org/lru/v2", got[0].Versions[1].Path)
}
