// SPDX-License-Identifier: AGPL-3.0-or-later

package depsregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

func TestGoVersions(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "ermite-bernard" {
			t.Error("missing user agent")
		}
		if !strings.HasPrefix(r.URL.Path, "/example.com/!module/@v/") {
			t.Errorf("unescaped path: %s", r.URL.Path)
		}
		if strings.HasSuffix(r.URL.Path, "list") {
			_, _ = fmt.Fprint(w, "v1.0.0\nv1.1.0\nv1.2.0\ninvalid\nv1.1.0\n")
			return
		}
		calls++
		_, _ = fmt.Fprint(w, `{"Time":"2026-01-01T00:00:00Z"}`)
	}))
	defer s.Close()
	reg := New(Config{GoProxyURL: s.URL})
	vs, err := reg.GoVersions(t.Context(), "example.com/Module", "v1.0.0")
	if err != nil || len(vs) != 2 || calls != 2 {
		t.Fatalf("%v %v calls=%d", vs, err, calls)
	}
	if vs[0].Version != "v1.2.0" || vs[0].Published.IsZero() {
		t.Fatal(vs)
	}
}

func TestGoVersionCap(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "list") {
			for i := 0; i < 100; i++ {
				_, _ = fmt.Fprintf(w, "v1.0.%d\n", i)
			}
			return
		}
		calls++
		_, _ = fmt.Fprint(w, `{"Time":"2026-01-01T00:00:00Z"}`)
	}))
	defer s.Close()
	vs, err := New(Config{GoProxyURL: s.URL}).GoVersions(t.Context(), "example.com/mod", "v1.0.0")
	if err != nil || len(vs) != 50 || calls != 50 || vs[0].Version != "v1.0.99" {
		t.Fatalf("%v %v %d", vs, err, calls)
	}
}

func TestHTTPFailures(t *testing.T) {
	for _, status := range []int{404, 410} {
		for _, info := range []bool{false, true} {
			t.Run(fmt.Sprint(status, info), func(t *testing.T) {
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if info && strings.HasSuffix(r.URL.Path, "list") {
						_, _ = fmt.Fprint(w, "v1.1.0")
						return
					}
					w.WriteHeader(status)
				}))
				defer s.Close()
				_, err := New(Config{GoProxyURL: s.URL}).GoVersions(t.Context(), "example.com/mod", "v1.0.0")
				if !errors.Is(err, domain.ErrNotFound) {
					t.Fatal(err)
				}
			})
		}
	}
	t.Run("oversize", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, strings.Repeat("x", (1<<20)+1)) }))
		defer s.Close()
		_, err := New(Config{GoProxyURL: s.URL}).GoVersions(t.Context(), "example.com/mod", "v1.0.0")
		if !errors.Is(err, domain.ErrResourceExhausted) {
			t.Fatal(err)
		}
	})
	t.Run("cross host", func(t *testing.T) {
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect followed") }))
		defer target.Close()
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
		defer s.Close()
		_, err := New(Config{GoProxyURL: s.URL}).GoVersions(t.Context(), "example.com/mod", "v1.0.0")
		if err == nil {
			t.Fatal("redirect accepted")
		}
	})
	t.Run("same host bounded", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/done" {
				_, _ = fmt.Fprint(w, "")
				return
			}
			http.Redirect(w, r, "/done", http.StatusFound)
		}))
		defer s.Close()
		_, err := New(Config{GoProxyURL: s.URL}).GoVersions(t.Context(), "example.com/mod", "v1.0.0")
		if err != nil {
			t.Fatal(err)
		}
		loop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", http.StatusFound) }))
		defer loop.Close()
		_, err = New(Config{GoProxyURL: loop.URL}).GoVersions(t.Context(), "example.com/mod", "v1.0.0")
		if err == nil {
			t.Fatal("unbounded redirect")
		}
	})
}

func TestOSV(t *testing.T) {
	for _, eco := range []domain.DepsEcosystem{domain.EcosystemGoMod, domain.EcosystemActions} {
		for _, id := range []string{"MAL-123", "GO-123", "GHSA-123"} {
			t.Run(string(eco)+id, func(t *testing.T) {
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != "POST" || r.URL.Path != "/v1/query" || r.Header.Get("Content-Type") != "application/json" {
						t.Error("bad request")
					}
					var body struct {
						Package struct{ Ecosystem, Name string }
						Version string
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					want := "Go"
					if eco == domain.EcosystemActions {
						want = "GitHub Actions"
					}
					if body.Package.Ecosystem != want || body.Package.Name != "example" || body.Version != "v1" {
						t.Errorf("%+v", body)
					}
					_, _ = fmt.Fprintf(w, `{"vulns":[{"id":%q}]}`, id)
				}))
				defer s.Close()
				got, err := New(Config{OSVURL: s.URL}).Malicious(t.Context(), eco, "example", "v1")
				if err != nil || got != strings.HasPrefix(id, "MAL-") {
					t.Fatalf("%v %v", got, err)
				}
			})
		}
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer s.Close()
	if _, err := New(Config{OSVURL: s.URL}).Malicious(t.Context(), domain.EcosystemGoMod, "x", "v1"); err == nil {
		t.Fatal("500 accepted")
	}
	s.Close()
	if _, err := New(Config{OSVURL: s.URL}).Malicious(t.Context(), domain.EcosystemGoMod, "x", "v1"); err == nil {
		t.Fatal("transport error accepted")
	}
}

func TestActionTags(t *testing.T) {
	remote := mocks.NewGitRemote(t)
	remote.EXPECT().ListRefs(t.Context(), "owner", "repo").Return([]ports.RemoteRef{
		{Name: "refs/tags/v1", ID: "tag", Peeled: "commit"}, {Name: "refs/tags/v2", ID: "plain"}, {Name: "refs/tags/release", ID: "other"}, {Name: "refs/heads/v3", ID: "branch"},
	}, nil).Once()
	reg := New(Config{GitRemote: remote})
	tags, err := reg.ActionTags(t.Context(), "owner", "repo")
	if err != nil || len(tags) != 2 || tags[0].Name != "v1" || tags[0].SHA != "commit" || tags[1].SHA != "plain" {
		t.Fatalf("%v %v", tags, err)
	}
	remote.EXPECT().ListRefs(t.Context(), "owner", "missing").Return(nil, domain.ErrNotFound).Once()
	if _, err := reg.ActionTags(t.Context(), "owner", "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal(err)
	}
	for _, name := range []string{"", "..", "a/b", "a b", "a@b"} {
		if _, err := reg.ActionTags(t.Context(), name, "repo"); !errors.Is(err, domain.ErrInvalid) {
			t.Fatal(name, err)
		}
	}
}

func TestCache(t *testing.T) {
	for _, status := range []int{200, 404, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(status)
				if status == 200 {
					_, _ = fmt.Fprint(w, `{}`)
				}
			}))
			defer s.Close()
			c := NewCached(New(Config{GoProxyURL: s.URL, OSVURL: s.URL}))
			for i := 0; i < 2; i++ {
				_, _ = c.GoVersions(t.Context(), "example.com/mod", "v1.0.0")
				_, _ = c.Malicious(t.Context(), domain.EcosystemGoMod, "mod", "v1")
			}
			want := 2
			if status == 404 {
				want = 3
			}
			if status == 500 {
				want = 4
			}
			if calls != want {
				t.Fatalf("calls %d want %d", calls, want)
			}
		})
	}
	remote := mocks.NewGitRemote(t)
	remote.EXPECT().ListRefs(context.Background(), "o", "r").Return([]ports.RemoteRef{{Name: "refs/tags/v1", ID: "sha"}}, nil).Once()
	c := NewCached(New(Config{GitRemote: remote}))
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Go(func() {
			tags, err := c.ActionTags(context.Background(), "o", "r")
			if err != nil {
				t.Error(err)
			}
			tags[0].SHA = "modified"
		})
	}
	wg.Wait()
	tags, _ := c.ActionTags(context.Background(), "o", "r")
	if tags[0].SHA != "sha" {
		t.Fatal("cache mutated")
	}
	remote.EXPECT().ListRefs(context.Background(), "o", "missing").Return(nil, domain.ErrNotFound).Once()
	for i := 0; i < 2; i++ {
		_, _ = c.ActionTags(context.Background(), "o", "missing")
	}
	remote.EXPECT().ListRefs(context.Background(), "o", "error").Return(nil, domain.ErrUnavailable).Twice()
	for i := 0; i < 2; i++ {
		_, _ = c.ActionTags(context.Background(), "o", "error")
	}
}
