// SPDX-License-Identifier: AGPL-3.0-or-later

package depsregistry

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"bnema.dev/ermite/internal/domain"
)

func TestGoMajorPaths(t *testing.T) {
	for _, tc := range []struct {
		name, prefix string
		start, count int
		empty        bool
	}{
		{"example.org/m", "example.org/m/v", 2, 2, false},
		{"example.org/m/v3", "example.org/m/v", 4, 1, true},
		{"gopkg.in/yaml.v1", "gopkg.in/yaml.v", 2, 2, false},
		{"gopkg.in/yaml.v2", "gopkg.in/yaml.v", 3, 0, true},
		{"example.org/m", "example.org/m/v", 2, 5, false},
	} {
		t.Run(tc.name+fmt.Sprint(tc.count), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want := fmt.Sprintf("/%s%d/@v/list", tc.prefix, tc.start+calls)
				if r.URL.Path != want {
					t.Errorf("path %s, want %s", r.URL.Path, want)
				}
				calls++
				if calls > tc.count {
					if !tc.empty {
						http.NotFound(w, r)
					}
					return
				}
				fmt.Fprintln(w, "v2.0.0")
			}))
			defer server.Close()
			got, err := New(Config{GoProxyURL: server.URL}).GoMajorPaths(t.Context(), tc.name)
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			for i := 0; i < tc.count; i++ {
				want = append(want, fmt.Sprintf("%s%d", tc.prefix, tc.start+i))
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v want %v", got, want)
			}
			expected := tc.count + 1
			if expected > 5 {
				expected = 5
			}
			if calls != expected {
				t.Fatalf("%d probes", calls)
			}
		})
	}
}

func TestRequestCustomCap(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat("x", 33)) }))
	defer s.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, s.URL, nil)
	_, err := New(Config{}).request(req, true, 32, time.Second)
	if !errors.Is(err, domain.ErrResourceExhausted) {
		t.Fatalf("got %v", err)
	}
}

func TestGoMajorPathsCache(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if strings.Contains(r.URL.Path, "/v2/") {
			fmt.Fprint(w, "v2.0.0")
		} else {
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	c := NewCached(New(Config{GoProxyURL: s.URL}))
	paths, err := c.GoMajorPaths(t.Context(), "example.org/m")
	if err != nil {
		t.Fatal(err)
	}
	paths[0] = "mutated"
	paths, err = c.GoMajorPaths(t.Context(), "example.org/m")
	if err != nil || len(paths) != 1 || paths[0] != "example.org/m/v2" || calls != 2 {
		t.Fatalf("%v %v calls=%d", paths, err, calls)
	}
}
