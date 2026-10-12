// SPDX-License-Identifier: AGPL-3.0-or-later

package githttp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/githttp"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

const (
	pathUpload  = "/pub.git/git-upload-pack"
	pathRefs    = "/pub.git/info/refs?service=git-upload-pack"
	pathReceive = "/pub.git/git-receive-pack"
)

// held is a request kept inside the backend until the gate opens.
type held struct{ path, client string }

// gatedRig is a rig whose backend blocks every request that reaches it until
// open is called, so a test decides exactly which slots are taken.
type gatedRig struct {
	*rig
	entered chan struct{} // a request reached the backend
	served  chan struct{} // a request left the handler, its slots released
	gate    chan struct{}
	done    sync.WaitGroup
}

func newGatedRig(t *testing.T, mut func(*githttp.Config)) *gatedRig {
	t.Helper()
	g := &gatedRig{entered: make(chan struct{}, 64), served: make(chan struct{}, 64), gate: make(chan struct{})}
	script := cgiScript(t)
	be := mocks.NewGitBackend(t)
	be.EXPECT().HTTPBackend(mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, _ bool) (ports.HTTPBackend, error) {
			g.entered <- struct{}{}
			select {
			case <-g.gate:
			case <-ctx.Done():
			}
			return ports.HTTPBackend{Path: script, Dir: filepath.Dir(script), Env: []string{"PATH=/usr/bin:/bin"}}, nil
		}).Maybe()
	g.rig = newRig(t, func(c *githttp.Config) {
		c.Backend = be
		c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
		if mut != nil {
			mut(c)
		}
	})
	// A client sees the response before the handler returns and gives its
	// slots back: serve through a wrapper that tells when it has.
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { g.served <- struct{}{} }()
		g.h.ServeHTTP(w, r)
	}))
	t.Cleanup(g.srv.Close)
	t.Cleanup(func() { g.open(); g.done.Wait() })
	return g
}

// waitServed waits until n requests have left the handler.
func (g *gatedRig) waitServed(n int) {
	for range n {
		<-g.served
	}
}

// hold starts the requests and returns once all of them are inside the backend.
func (g *gatedRig) hold(t *testing.T, reqs []held) {
	t.Helper()
	for _, h := range reqs {
		method := http.MethodPost
		if h.path == pathRefs {
			method = http.MethodGet
		}
		g.done.Go(func() {
			req, err := http.NewRequest(method, g.srv.URL+h.path, nil)
			if err != nil {
				return
			}
			req.SetBasicAuth("u", "root-secret")
			req.Header.Set("X-Forwarded-For", h.client)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		})
	}
	for range reqs {
		<-g.entered
	}
}

func (g *gatedRig) open() {
	select {
	case <-g.gate:
	default:
		close(g.gate)
	}
}

func TestHandlerBackendLimits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		cfg   func(*githttp.Config)
		hold  []held
		probe held
		want  int
	}{
		{
			name:  "free upload slot",
			cfg:   func(c *githttp.Config) { c.MaxUploadPacks = 2 },
			hold:  []held{{pathUpload, "198.51.100.1"}},
			probe: held{pathUpload, "198.51.100.2"},
			want:  http.StatusOK,
		},
		{
			name:  "upload slots full",
			cfg:   func(c *githttp.Config) { c.MaxUploadPacks = 2 },
			hold:  []held{{pathUpload, "198.51.100.1"}, {pathRefs, "198.51.100.2"}},
			probe: held{pathUpload, "198.51.100.3"},
			want:  http.StatusServiceUnavailable,
		},
		{
			name:  "ref advertisement counts as an upload",
			cfg:   func(c *githttp.Config) { c.MaxUploadPacks = 1 },
			hold:  []held{{pathUpload, "198.51.100.1"}},
			probe: held{pathRefs, "198.51.100.2"},
			want:  http.StatusServiceUnavailable,
		},
		{
			name:  "receive slots full",
			cfg:   func(c *githttp.Config) { c.MaxReceivePacks = 1 },
			hold:  []held{{pathReceive, "198.51.100.1"}},
			probe: held{pathReceive, "198.51.100.2"},
			want:  http.StatusServiceUnavailable,
		},
		{
			name:  "pushes do not take upload slots",
			cfg:   func(c *githttp.Config) { c.MaxUploadPacks = 1; c.MaxReceivePacks = 1 },
			hold:  []held{{pathUpload, "198.51.100.1"}},
			probe: held{pathReceive, "198.51.100.2"},
			want:  http.StatusOK,
		},
		{
			name:  "fetches do not take receive slots",
			cfg:   func(c *githttp.Config) { c.MaxUploadPacks = 1; c.MaxReceivePacks = 1 },
			hold:  []held{{pathReceive, "198.51.100.1"}},
			probe: held{pathUpload, "198.51.100.2"},
			want:  http.StatusOK,
		},
		{
			name:  "client at its cap",
			cfg:   func(c *githttp.Config) { c.MaxPerClient = 2 },
			hold:  []held{{pathUpload, "198.51.100.1"}, {pathReceive, "198.51.100.1"}},
			probe: held{pathUpload, "198.51.100.1"},
			want:  http.StatusTooManyRequests,
		},
		{
			name:  "another client is not affected by the cap",
			cfg:   func(c *githttp.Config) { c.MaxPerClient = 2 },
			hold:  []held{{pathUpload, "198.51.100.1"}, {pathUpload, "198.51.100.1"}},
			probe: held{pathUpload, "198.51.100.2"},
			want:  http.StatusOK,
		},
		{
			name:  "addresses of one IPv6 /64 are one client",
			cfg:   func(c *githttp.Config) { c.MaxPerClient = 1 },
			hold:  []held{{pathUpload, "2001:db8::1"}},
			probe: held{pathUpload, "2001:db8::2"},
			want:  http.StatusTooManyRequests,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := newGatedRig(t, func(c *githttp.Config) {
				c.MaxUploadPacks, c.MaxReceivePacks, c.MaxPerClient = 8, 8, 8
				tt.cfg(c)
			})
			g.hold(t, tt.hold)
			if tt.want == http.StatusOK {
				g.open() // the probe has to get through the backend gate too
			}
			code, _, hdr := g.do(t, http.MethodPost, tt.probe.path, "u", "root-secret", nil,
				map[string]string{"X-Forwarded-For": tt.probe.client})
			if tt.probe.path == pathRefs {
				code, _, hdr = g.do(t, http.MethodGet, tt.probe.path, "u", "root-secret", nil,
					map[string]string{"X-Forwarded-For": tt.probe.client})
			}
			assert.Equal(t, tt.want, code)
			if tt.want != http.StatusOK {
				assert.NotEmpty(t, hdr.Get("Retry-After"))
			}
		})
	}
}

func TestHandlerBackendSlotsAreReleased(t *testing.T) {
	t.Parallel()
	g := newGatedRig(t, func(c *githttp.Config) {
		c.MaxUploadPacks, c.MaxReceivePacks, c.MaxPerClient = 1, 1, 1
	})
	hdr := map[string]string{"X-Forwarded-For": "198.51.100.1"}

	g.hold(t, []held{{pathUpload, "198.51.100.1"}})
	code, _, _ := g.do(t, http.MethodPost, pathUpload, "u", "root-secret", nil, hdr)
	require.Equal(t, http.StatusTooManyRequests, code)
	g.waitServed(1)

	g.open()
	g.waitServed(1)
	for _, p := range []string{pathUpload, pathReceive, pathUpload} {
		code, _, _ = g.do(t, http.MethodPost, p, "u", "root-secret", nil, hdr)
		assert.Equal(t, http.StatusOK, code, "the slots came back: %s", p)
		g.waitServed(1)
	}
}

func TestHandlerBackendFailureReleasesSlots(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(c *githttp.Config) { c.MaxUploadPacks, c.MaxPerClient = 1, 1 })
	r.backendErr = context.DeadlineExceeded
	for range 3 {
		code, _, _ := r.do(t, http.MethodPost, pathUpload, "", "", nil, nil)
		assert.Equal(t, http.StatusInternalServerError, code, "never a refusal: the slot is released on failure")
	}
}

// deadlineRecorder is a ResponseWriter that supports the read deadline, as
// the writer of net/http does, and records it.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (d *deadlineRecorder) SetReadDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}

func TestHandlerBodyReadDeadline(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
		max  time.Duration
		min  time.Duration
	}{
		{"fetch body", pathUpload, time.Minute, 30 * time.Second},
		{"push body is bounded by idleness", pathReceive, 5 * time.Minute, time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, nil)
			req := httptest.NewRequest(http.MethodPost, tt.path, stringsReader("payload"))
			req.SetBasicAuth("u", "root-secret")
			w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			start := time.Now()
			r.h.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code)

			require.GreaterOrEqual(t, len(w.deadlines), 2)
			first, last := w.deadlines[0], w.deadlines[len(w.deadlines)-1]
			assert.WithinRange(t, first, start.Add(tt.min), time.Now().Add(tt.max))
			assert.True(t, last.IsZero(), "the deadline is lifted once the body is read")
		})
	}
}

func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }
