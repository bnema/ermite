// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestInboxTraffic(t *testing.T) {
	t.Parallel()
	for _, rng := range []string{"24h", "30d"} {
		t.Run(rng, func(t *testing.T) {
			t.Parallel()
			svc := mocks.NewInboxService(t)
			expectInbox(svc, inboxFixture{})
			srv := inboxServer(t, svc)
			f := mocks.NewTrafficService(t)
			data := ports.TrafficForge{
				Totals: domain.TrafficCounts{Views: 1204, ViewUniques: 312, Clones: 87, CloneUniques: 14, Bots: 9},
				Repos: []domain.TrafficSummary{
					{Repo: repoName(t, "alpha"), TrafficCounts: domain.TrafficCounts{Views: 3, ViewUniques: 2, Clones: 1, CloneUniques: 1, Bots: 1}},
					{Repo: repoName(t, "beta"), TrafficCounts: domain.TrafficCounts{Views: 1, ViewUniques: 1, Clones: 20, CloneUniques: 12, Bots: 8}},
				},
				Referrers: []ports.TrafficReferrer{{Domain: "example.org", Count: 42}, {Domain: "<untrusted>", Count: 1}},
			}
			window := 24 * time.Hour
			if rng == "30d" {
				window = 30 * 24 * time.Hour
			}
			f.EXPECT().Forge(mock.Anything, window).Return(data, nil).Times(2)
			srv.cfg.Traffic = f
			path := inboxPath + "?tab=traffic&range=" + rng
			rec := inboxGet(srv, path, true)
			require.Equal(t, 200, rec.Code, rec.Body.String())
			body := rec.Body.String()
			assert.Contains(t, body, `href="/-/inbox?tab=traffic" aria-current="page">Traffic</a>`)
			assert.Contains(t, body, "1\u202f204 views · 312 unique · 87 clones · 14 unique · 9 bots")
			assert.Contains(t, body, `class="list" aria-label="Repository traffic"`)
			assert.Contains(t, body, `<th class="c-main">Repository</th>`)
			for _, header := range []string{"Views", "View uniques", "Clones", "Clone uniques", "Bots", "Count"} {
				assert.Contains(t, body, `<th class="num">`+header+`</th>`)
			}
			assert.Contains(t, body, `<th class="c-main">Domain</th>`)
			assert.Contains(t, body, `href="/alpha">alpha</a>`)
			assert.Less(t, strings.Index(body, `href="/beta">beta</a>`), strings.Index(body, `href="/alpha">alpha</a>`))
			assert.Contains(t, body, `<td class="num">20</td><td class="num">12</td><td class="num">8</td>`)
			assert.Contains(t, body, `Top referrers`)
			assert.Contains(t, body, `<td class="c-main">example.org</td><td class="num">42</td>`)
			assert.Contains(t, body, "&lt;untrusted&gt;")
			assert.Contains(t, body, `range=30d&amp;tab=traffic#inbox-traffic`)
			assert.NotContains(t, body, "7 days")
			assert.Equal(t, "alpha", data.Repos[0].Repo.String(), "sorting must not mutate reader data")
			q := url.Values{"view": {"inbox"}, "tab": {"traffic"}, "range": {rng}}
			assert.True(t, srv.ownerStreamOK(withOwner(t.Context()), q))
			assert.False(t, srv.ownerStreamOK(t.Context(), q))
			fragments, err := (&liveStream{s: srv, view: "inbox", tab: "traffic", rng: rng}).inbox(withOwner(t.Context()), ports.WatchEvent{Event: domain.JournalUpdated{}})
			require.NoError(t, err)
			require.Len(t, fragments, 1)
			assert.Contains(t, fragments[0].html, `id="inbox-traffic"`)
			assert.Contains(t, fragments[0].html, "example.org")
			assert.Equal(t, 404, inboxGet(srv, path, false).Code)
		})
	}
}

func TestInboxTrafficUnavailableAndEmpty(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"nil", "recorder only", "empty", "failure"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			svc := mocks.NewInboxService(t)
			expectInbox(svc, inboxFixture{})
			srv := inboxServer(t, svc)
			switch mode {
			case "recorder only":
				srv.cfg.Traffic = mocks.NewTrafficRecorder(t)
			case "empty":
				f := mocks.NewTrafficService(t)
				f.EXPECT().Forge(mock.Anything, 24*time.Hour).Return(ports.TrafficForge{}, nil).Once()
				srv.cfg.Traffic = f
			case "failure":
				f := mocks.NewTrafficService(t)
				f.EXPECT().Forge(mock.Anything, 24*time.Hour).Return(ports.TrafficForge{}, errors.New("unavailable")).Once()
				srv.cfg.Traffic = f
			}
			rec := inboxGet(srv, inboxPath+"?tab=traffic", true)
			switch mode {
			case "nil", "recorder only":
				assert.Equal(t, 404, rec.Code)
				body := inboxGet(srv, inboxPath, true).Body.String()
				assert.NotContains(t, body, `tab=traffic`)
				assert.False(t, srv.ownerStreamOK(withOwner(t.Context()), url.Values{"view": {"inbox"}, "tab": {"traffic"}}))
			case "empty":
				require.Equal(t, 200, rec.Code)
				assert.Contains(t, rec.Body.String(), "no traffic yet.")
			case "failure":
				assert.Equal(t, 500, rec.Code)
			}
		})
	}
}

func TestInboxTrafficRanges(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		tab, raw string
		valid    bool
	}{
		{"activity", "24h", true}, {"activity", "7d", true}, {"activity", "30d", false},
		{"traffic", "", true}, {"traffic", "24h", true}, {"traffic", "30d", true}, {"traffic", "7d", false},
	} {
		_, err := activityRange(tc.raw, tc.tab)
		assert.Equal(t, tc.valid, err == nil, "tab=%s range=%s", tc.tab, tc.raw)
	}
}
