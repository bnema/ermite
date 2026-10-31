// SPDX-License-Identifier: AGPL-3.0-or-later

package depsregistry

import (
	"encoding/json"
	"fmt"
	"io"
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
	// One document per name, whatever the current versions of the callers.
	reg.EXPECT().NPMVersions(mock.Anything, "react", "v0.0.0-0").Return([]domain.DepVersion{{Version: "v2.0.0"}, {Version: "v1.5.0"}, {Version: "v0.0.0-rc.2"}}, nil).Once()
	reg.EXPECT().NPMVersions(mock.Anything, "missing", "v0.0.0-0").Return(nil, domain.ErrNotFound).Once()
	c := NewCached(reg)
	first, err := c.NPMVersions(t.Context(), "react", "v1.0.0")
	require.NoError(t, err)
	require.Len(t, first, 2)
	first[0].Version = "mutated"
	second, err := c.NPMVersions(t.Context(), "react", "v1.5.0")
	require.NoError(t, err)
	require.Equal(t, []domain.DepVersion{{Version: "v2.0.0"}}, second)
	pre, err := c.NPMVersions(t.Context(), "react", "v0.0.0-rc.1")
	require.NoError(t, err)
	require.Len(t, pre, 3, "a 0.0.0-x prerelease newer than current is kept")
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

// Large documents stream: only versions.*.deprecated and time are kept, and
// odd shapes (unpublished packages, boolean deprecations) do not fail.
func TestNPMVersionsDocumentShapes(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"unpublished", `{"name":"gone","time":{"created":"2026-01-01T00:00:00Z","unpublished":{"time":"2026-02-01T00:00:00Z","versions":["1.0.0"]}}}`, nil},
		{"deprecated kinds", `{"versions":{"1.1.0":{"deprecated":true},"1.2.0":{"deprecated":false},"1.3.0":{"deprecated":""},"1.4.0":{"deprecated":"no"}},"time":{"1.1.0":"2026-01-01T00:00:00Z","1.2.0":"2026-01-01T00:00:00Z","1.3.0":"2026-01-01T00:00:00Z","1.4.0":"2026-01-01T00:00:00Z"}}`, []string{"v1.3.0", "v1.2.0"}},
		{"skipped members", `{"readme":"` + strings.Repeat("x", 1<<20) + `","versions":{"1.1.0":{"dist":{"a":[1,{"b":2}]}}},"users":{"a":true},"time":{"1.1.0":"2026-01-01T00:00:00Z","modified":{"odd":1}}}`, []string{"v1.1.0"}},
		{"versions null", `{"versions":null,"time":"bad"}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, tc.body) }))
			defer s.Close()
			got, err := New(Config{NPMRegistryURL: s.URL}).NPMVersions(t.Context(), "x", "v1.0.0")
			require.NoError(t, err)
			var versions []string
			for _, v := range got {
				versions = append(versions, v.Version)
			}
			require.Equal(t, tc.want, versions)
		})
	}
}

func TestNPMVersionsByteCap(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"versions":{`)
		chunk := strings.Repeat(`"0.0.1-x":{},`, 4096)
		for written := 0; written <= npmDocBytes; written += len(chunk) {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	}))
	defer s.Close()
	_, err := New(Config{NPMRegistryURL: s.URL}).NPMVersions(t.Context(), "huge", "v1.0.0")
	require.ErrorIs(t, err, domain.ErrResourceExhausted)
}

// The cache's lower bound sorts below 0.0.0 prereleases in the real client.
func TestNPMVersionsLowestBound(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"versions":{"0.0.0-rc.1":{}},"time":{"0.0.0-rc.1":"2026-01-01T00:00:00Z"}}`)
	}))
	defer s.Close()
	got, err := New(Config{NPMRegistryURL: s.URL}).NPMVersions(t.Context(), "x", npmLowest)
	require.NoError(t, err)
	require.Len(t, got, 1)
}
