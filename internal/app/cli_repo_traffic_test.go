// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bnema.dev/ermite-api/gen/ermite"
	"connectrpc.com/connect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestRepoTraffic(t *testing.T) {
	for _, tc := range []struct {
		name, window string
		r            ermite.TrafficRange
		json         bool
	}{
		{"default", "", ermite.TrafficRange_TRAFFIC_RANGE_30D, false},
		{"daily", "30d", ermite.TrafficRange_TRAFFIC_RANGE_30D, false},
		{"hourly", "24h", ermite.TrafficRange_TRAFFIC_RANGE_24H, false},
		{"json", "24h", ermite.TrafficRange_TRAFFIC_RANGE_24H, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeAPI{getRepoTraffic: func(r *ermite.GetRepoTrafficRequest) (*ermite.GetRepoTrafficResponse, error) {
				assert.Equal(t, "alpha", r.GetRepo())
				assert.Equal(t, tc.r, r.GetRange())
				return &ermite.GetRepoTrafficResponse{Totals: &ermite.TrafficCounts{Views: 7, ViewUniques: 3, Clones: 2, CloneUniques: 1, Partial: true}, Points: []*ermite.TrafficPoint{{Start: ts("2026-06-01T12:00:00Z"), Counts: &ermite.TrafficCounts{Views: 7, Clones: 2}}}, Referrers: []*ermite.TrafficReferrer{{Domain: "example.test", Count: 4}}}, nil
			}}
			args := []string{"repo", "traffic", "alpha"}
			if tc.window != "" {
				args = append(args, "--range", tc.window)
			}
			if tc.json {
				args = append(args, "--json")
			}
			res := runCLI(t, envFor(f.serve(t)), "", args...).ok(t)
			assert.Empty(t, res.stderr)
			if tc.json {
				assert.JSONEq(t, `{"totals":{"views":7,"view_uniques":3,"clones":2,"clone_uniques":1,"partial":true},"points":[{"start":"2026-06-01T12:00:00Z","counts":{"views":7,"view_uniques":0,"clones":2,"clone_uniques":0,"partial":false}}],"referrers":[{"domain":"example.test","count":4}]}`, res.stdout)
			} else {
				assert.Contains(t, res.stdout, "views 7 · 3 unique\nclones 2 · 1 unique\ncounts are partial\n")
				assert.Contains(t, res.stdout, "referrers (30d)\nDOMAIN")
				assert.Contains(t, res.stdout, "example.test")
				if tc.window == "24h" {
					assert.Contains(t, res.stdout, "HOUR (UTC)")
					assert.Contains(t, res.stdout, "2026-06-01 12:00")
				} else {
					assert.Contains(t, res.stdout, "DATE (UTC)")
					assert.NotContains(t, res.stdout, "12:00")
				}
			}
		})
	}
}
func TestRepoTrafficEmptyAndErrors(t *testing.T) {
	f := &fakeAPI{getRepoTraffic: func(*ermite.GetRepoTrafficRequest) (*ermite.GetRepoTrafficResponse, error) {
		return &ermite.GetRepoTrafficResponse{}, nil
	}}
	url := f.serve(t)
	res := runCLI(t, envFor(url), "", "repo", "traffic", "alpha", "--json").ok(t)
	assert.JSONEq(t, `{"totals":{"views":0,"view_uniques":0,"clones":0,"clone_uniques":0,"partial":false},"points":[],"referrers":[]}`, res.stdout)
	res = runCLI(t, envFor(url), "", "repo", "traffic", "alpha").ok(t)
	assert.Contains(t, res.stdout, "referrers (30d)\nnone\n")
	for _, args := range [][]string{{"repo", "traffic"}, {"repo", "traffic", "a", "b"}, {"repo", "traffic", "a", "--range", "7d"}} {
		res := runCLI(t, nil, "", args...)
		require.Error(t, res.err)
		assert.Empty(t, res.stdout)
	}
	for _, code := range []connect.Code{connect.CodeNotFound, connect.CodeUnimplemented} {
		f := &fakeAPI{getRepoTraffic: func(*ermite.GetRepoTrafficRequest) (*ermite.GetRepoTrafficResponse, error) {
			return nil, connect.NewError(code, "traffic unavailable")
		}}
		res := runCLI(t, envFor(f.serve(t)), "", "repo", "traffic", "alpha")
		require.Error(t, res.err)
		assert.Empty(t, res.stdout)
	}
}
func TestRepoTrafficHelp(t *testing.T) {
	res := runCLI(t, nil, "", "repo", "traffic", "--help").ok(t)
	for _, s := range []string{"--range", "30d", "--json", "view_uniques", "clone_uniques", "partial", "referrers"} {
		assert.Contains(t, res.stdout, s)
	}
}
