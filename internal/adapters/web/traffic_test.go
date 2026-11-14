// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type trafficHit struct {
	repo    domain.RepoName
	kind    domain.TrafficKind
	ip      netip.Addr
	ua, ref string
}
type trafficRecorder struct{ hits []trafficHit }

func (f *trafficRecorder) Hit(repo domain.RepoName, kind domain.TrafficKind, ip netip.Addr, ua, ref string) {
	f.hits = append(f.hits, trafficHit{repo, kind, ip, ua, ref})
}

func TestTrafficSummary(t *testing.T) {
	for _, tc := range []struct {
		name, path, method string
		signed, hx         bool
		want               int
	}{
		{name: "anonymous", path: "/r", method: "GET", want: 1},
		{name: "signed", path: "/r", method: "GET", signed: true},
		{name: "hx", path: "/r", method: "GET", hx: true},
		{name: "raw", path: "/r/raw/main/a", method: "GET"},
		{name: "missing", path: "/missing", method: "GET"},
		{name: "head", path: "/r", method: "HEAD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &trafficRecorder{}
			browse := mocks.NewBrowseService(t)
			browse.EXPECT().ListRefs(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.RefPage{}, nil).Maybe()
			browse.EXPECT().OpenBlob(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(domain.BlobInfo{}, nil, domain.ErrNotFound).Maybe()
			s := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "r")), Browse: browse, Traffic: f})
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.RemoteAddr = "192.0.2.1:1234"
			r.Header.Set("User-Agent", "visitor")
			r.Header.Set("Referer", "https://example.org/")
			if tc.hx {
				r.Header.Set("HX-Request", "true")
			}
			if tc.signed {
				r = r.WithContext(context.WithValue(r.Context(), signedInKey{}, struct{}{}))
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			require.Len(t, f.hits, tc.want)
			if tc.want > 0 {
				require.Equal(t, http.StatusOK, w.Code)
				require.Equal(t, trafficHit{repoName(t, "r"), domain.TrafficView, netip.MustParseAddr("192.0.2.1"), "visitor", "https://example.org/"}, f.hits[0])
				// Revalidation is not a successful 200 view.
				r.Header.Set("If-None-Match", w.Header().Get("ETag"))
				s.ServeHTTP(httptest.NewRecorder(), r)
				require.Len(t, f.hits, 1)
			}
		})
	}
}
