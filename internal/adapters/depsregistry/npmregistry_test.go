// SPDX-License-Identifier: AGPL-3.0-or-later

package depsregistry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestNPMVersions(t *testing.T) {
	for _, name := range []string{"react", "@scope/package"} {
		t.Run(name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want := "/react"
				if strings.HasPrefix(name, "@") {
					want = "/@scope%2fpackage"
				}
				require.Equal(t, want, r.RequestURI)
				require.Equal(t, "ermite-bernard", r.Header.Get("User-Agent"))
				_, _ = fmt.Fprint(w, `{"versions":{"1.0.0":{},"1.1.0":{},"2.0.0":{"deprecated":"unsafe"},"3.0.0":{},"4.0.0":{},"invalid":{}},"time":{"1.0.0":"2026-01-01T00:00:00Z","1.1.0":"2026-02-01T00:00:00.123Z","2.0.0":"2026-03-01T00:00:00Z","4.0.0":"invalid"}}`)
			}))
			defer s.Close()
			got, err := New(Config{NPMRegistryURL: s.URL}).NPMVersions(t.Context(), name, "v1.0.0")
			require.NoError(t, err)
			require.Len(t, got, 1)
			require.Equal(t, "v1.1.0", got[0].Version)
			require.Equal(t, time.Date(2026, 2, 1, 0, 0, 0, 123000000, time.UTC), got[0].Published)
		})
	}
}

func TestNPMVersionsFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"missing", 404, "", domain.ErrNotFound},
		{"oversize", 200, strings.Repeat(" ", (32<<20)+1), domain.ErrResourceExhausted},
		{"unavailable", 503, "", domain.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); _, _ = fmt.Fprint(w, tc.body) }))
			defer s.Close()
			_, err := New(Config{NPMRegistryURL: s.URL}).NPMVersions(t.Context(), "react", "v1.0.0")
			require.ErrorIs(t, err, tc.want)
		})
	}
}

func TestNPMVersionsCache(t *testing.T) {
	reg := mocks.NewDepsRegistry(t)
	reg.EXPECT().NPMVersions(mock.Anything, "react", "v1.0.0").Return([]domain.DepVersion{{Version: "v2.0.0"}}, nil).Once()
	reg.EXPECT().NPMVersions(mock.Anything, "missing", "v1.0.0").Return(nil, domain.ErrNotFound).Once()
	c := NewCached(reg)
	first, err := c.NPMVersions(t.Context(), "react", "v1.0.0")
	require.NoError(t, err)
	first[0].Version = "mutated"
	second, err := c.NPMVersions(t.Context(), "react", "v1.0.0")
	require.NoError(t, err)
	require.Equal(t, "v2.0.0", second[0].Version)
	for range 2 {
		_, err := c.NPMVersions(t.Context(), "missing", "v1.0.0")
		require.ErrorIs(t, err, domain.ErrNotFound)
	}
}

func TestNPMOSV(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Package struct{ Ecosystem, Name string }
			Version string
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		require.Equal(t, "npm", payload.Package.Ecosystem)
		require.Equal(t, "@scope/package", payload.Package.Name)
		require.Equal(t, "2.0.0", payload.Version)
		_, _ = fmt.Fprint(w, `{"vulns":[{"id":"MAL-123"}]}`)
	}))
	defer s.Close()
	got, err := New(Config{OSVURL: s.URL}).Malicious(t.Context(), domain.EcosystemNPM, "@scope/package", "v2.0.0")
	require.NoError(t, err)
	require.True(t, got)
}
