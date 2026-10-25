// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-api/gen/ermite"
)

func journalFeatures(f *fakeAPI, features ...string) *fakeAPI {
	f.getVersion = func(*ermite.GetVersionRequest) (*ermite.GetVersionResponse, error) {
		return &ermite.GetVersionResponse{Features: features}, nil
	}
	return f
}

func activityRequests(t *testing.T, f *fakeAPI) []*ermite.ListActivityRequest {
	t.Helper()
	calls, reqs := f.snapshot()
	var out []*ermite.ListActivityRequest
	for i, c := range calls {
		if c == "ListActivity" {
			out = append(out, reqs[i].(*ermite.ListActivityRequest))
		}
	}
	return out
}

var (
	entryMerged = &ermite.ActivityEntry{Id: "0002", Kind: ermite.ActivityKind_ACTIVITY_KIND_MR_MERGED, Repo: "app", At: ts("2026-10-10T11:00:00Z"), Actor: "owner", Number: 4, Ref: "main", Title: "Add login"}
	entryPush   = &ermite.ActivityEntry{Id: "0001", Kind: ermite.ActivityKind_ACTIVITY_KIND_PUSH, Repo: "app", At: ts("2026-10-10T10:00:00Z"), Actor: "claude", Ref: "main", CommitCount: 100, CommitsCapped: true, Commit: "abc", Summary: "fix: typo"}
	entryTag    = &ermite.ActivityEntry{Id: "0000", Kind: ermite.ActivityKind_ACTIVITY_KIND_TAG_CREATED, Repo: "lib", At: ts("2026-10-09T09:00:00Z"), Ref: "v1.2.0"}
)

func TestJournalTableAndDefaults(t *testing.T) {
	f := journalFeatures(&fakeAPI{listActivity: func(*ermite.ListActivityRequest) (*ermite.ListActivityResponse, error) {
		return &ermite.ListActivityResponse{Entries: []*ermite.ActivityEntry{entryMerged, entryPush, entryTag}}, nil
	}}, "journal")
	res := runCLI(t, envFor(f.serve(t)), "", "journal").ok(t)
	assert.Equal(t, ""+
		"WHEN              REPO  KIND         DETAIL                         ACTOR\n"+
		"2026-10-10 11:00  app   mr_merged    !4 Add login                   owner\n"+
		"2026-10-10 10:00  app   push         main: 100+ commits, fix: typo  claude\n"+
		"2026-10-09 09:00  lib   tag_created  v1.2.0                         \n", res.stdout)
	reqs := activityRequests(t, f)
	require.Len(t, reqs, 1)
	assert.True(t, reqs[0].GetSince().AsTime().Equal(testNow.Add(-24*time.Hour)), "24h by default")
	assert.Nil(t, reqs[0].Repo)
	assert.Empty(t, reqs[0].GetKinds())
	assert.EqualValues(t, 100, reqs[0].GetPageSize())
}

func TestJournalFiltersAndJSON(t *testing.T) {
	f := journalFeatures(&fakeAPI{listActivity: func(*ermite.ListActivityRequest) (*ermite.ListActivityResponse, error) {
		return &ermite.ListActivityResponse{Entries: []*ermite.ActivityEntry{entryPush}}, nil
	}}, "journal")
	res := runCLI(t, envFor(f.serve(t)), "", "journal", "--since", "7d", "--repo", "app", "--kind", "push,mr_merged", "--kind", "push", "--json").ok(t)
	assert.JSONEq(t, `[{"id":"0001","kind":"push","repo":"app","at":"2026-10-10T10:00:00Z","actor":"claude","number":0,
		"ref":"main","title":"","commit_count":100,"commits_capped":true,"commit":"abc","summary":"fix: typo"}]`, res.stdout)
	req := activityRequests(t, f)[0]
	assert.True(t, req.GetSince().AsTime().Equal(testNow.Add(-7*24*time.Hour)))
	assert.Equal(t, "app", req.GetRepo())
	assert.Equal(t, []ermite.ActivityKind{ermite.ActivityKind_ACTIVITY_KIND_PUSH, ermite.ActivityKind_ACTIVITY_KIND_MR_MERGED}, req.GetKinds())

	res = runCLI(t, envFor(f.serve(t)), "", "journal", "--since", "2026-10-01").ok(t)
	assert.Contains(t, res.stdout, "push")
	reqs := activityRequests(t, f)
	assert.True(t, reqs[len(reqs)-1].GetSince().AsTime().Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)))

	res = runCLI(t, envFor(f.serve(t)), "", "journal", "--json")
	require.NoError(t, res.err)
}

// The pages are followed until --limit; each asks for what is left.
func TestJournalFollowsPagesUpToTheLimit(t *testing.T) {
	page := 0
	f := journalFeatures(&fakeAPI{listActivity: func(r *ermite.ListActivityRequest) (*ermite.ListActivityResponse, error) {
		page++
		var out ermite.ListActivityResponse
		for i := range int(r.GetPageSize()) {
			out.Entries = append(out.Entries, &ermite.ActivityEntry{Id: fmt.Sprintf("%d-%d", page, i), Kind: ermite.ActivityKind_ACTIVITY_KIND_TAG_CREATED, Repo: "app", Ref: "v1"})
		}
		out.NextPageToken = fmt.Sprintf("p%d", page)
		return &out, nil
	}}, "journal")
	res := runCLI(t, envFor(f.serve(t)), "", "journal", "--limit", "700", "--json").ok(t)
	got := decode[[]jsonActivity](t, res.stdout)
	assert.Len(t, got, 700)
	reqs := activityRequests(t, f)
	require.Len(t, reqs, 2)
	assert.EqualValues(t, 500, reqs[0].GetPageSize())
	assert.Empty(t, reqs[0].GetPageToken())
	assert.EqualValues(t, 200, reqs[1].GetPageSize())
	assert.Equal(t, "p1", reqs[1].GetPageToken())
}

// A page token the server already sent stops the paging with an error,
// whether it comes back at once or after other empty pages.
func TestJournalStopsOnARepeatedPageToken(t *testing.T) {
	for name, tokens := range map[string][]string{
		"consecutive": {"A", "A"},
		"cycle":       {"A", "B", "A"},
	} {
		t.Run(name, func(t *testing.T) {
			call := 0
			f := journalFeatures(&fakeAPI{listActivity: func(*ermite.ListActivityRequest) (*ermite.ListActivityResponse, error) {
				if call >= len(tokens) {
					return nil, fmt.Errorf("called %d times", call+1)
				}
				call++
				return &ermite.ListActivityResponse{NextPageToken: tokens[call-1]}, nil
			}}, "journal")
			res := runCLI(t, envFor(f.serve(t)), "", "journal")
			require.Error(t, res.err)
			assert.Contains(t, res.err.Error(), `page token "A" twice`)
			assert.Len(t, activityRequests(t, f), len(tokens), "no request after the repeat")
		})
	}
}

func TestJournalRefusesBadInputBeforeCalling(t *testing.T) {
	f := journalFeatures(&fakeAPI{}, "journal")
	vars := envFor(f.serve(t))
	for _, args := range [][]string{
		{"journal", "--since", "tomorrow"},
		{"journal", "--since", "2030-01-01"},
		{"journal", "--kind", "merged"},
		{"journal", "--limit", "0"},
		{"journal", "extra"},
	} {
		res := runCLI(t, vars, "", args...)
		assert.Error(t, res.err, "%v", args)
	}
	calls, _ := f.snapshot()
	assert.Empty(t, calls)
}

func TestJournalNeedsTheFeature(t *testing.T) {
	f := journalFeatures(&fakeAPI{}, "releases")
	vars := envFor(f.serve(t))
	res := runCLI(t, vars, "", "journal")
	require.ErrorIs(t, res.err, errNoJournal)
	res = runCLI(t, vars, "", "release", "pending")
	require.ErrorIs(t, res.err, errNoJournal)
	calls, _ := f.snapshot()
	assert.Equal(t, []string{"GetVersion", "GetVersion"}, calls)
}

func TestJournalDescribesServerErrors(t *testing.T) {
	f := journalFeatures(&fakeAPI{listActivity: func(*ermite.ListActivityRequest) (*ermite.ListActivityResponse, error) {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("reading the journal needs owner rights or the journal:read scope"))
	}}, "journal")
	res := runCLI(t, envFor(f.serve(t)), "", "journal")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "journal:read")
}

func TestReleasePending(t *testing.T) {
	f := journalFeatures(&fakeAPI{listUnreleased: func(r *ermite.ListUnreleasedRequest) (*ermite.ListUnreleasedResponse, error) {
		return &ermite.ListUnreleasedResponse{Repos: []*ermite.UnreleasedRepo{
			{Repo: "ermite-api", LatestTag: "v0.12.0", LatestTagAt: ts("2026-10-07T11:00:00Z"), CommitsSince: 4, DefaultBranch: "main"},
			{Repo: "big", CommitsSince: 500, CommitsCapped: true, DefaultBranch: "trunk"},
			{Repo: "one", LatestTag: "v1", LatestTagAt: ts("2026-10-10T09:00:00Z"), CommitsSince: 1, DefaultBranch: "main"},
		}}, nil
	}}, "journal")
	vars := envFor(f.serve(t))
	res := runCLI(t, vars, "", "release", "pending").ok(t)
	assert.Equal(t, ""+
		"ermite-api  4 commits since v0.12.0 (3 days ago)\n"+
		"big         500+ commits since the latest tag\n"+
		"one         1 commit since v1 (3 hours ago)\n", res.stdout)
	_, reqs := f.snapshot()
	assert.Nil(t, reqs[len(reqs)-1].(*ermite.ListUnreleasedRequest).Repo)

	res = runCLI(t, vars, "", "release", "pending", "ermite-api", "--json").ok(t)
	assert.Contains(t, res.stdout, `"latest_tag_at": "2026-10-07T11:00:00Z"`)
	got := decode[[]jsonUnreleased](t, res.stdout)
	require.Len(t, got, 3)
	assert.Equal(t, jsonUnreleased{Repo: "big", CommitsSince: 500, CommitsCapped: true, DefaultBranch: "trunk"}, got[1])
	_, reqs = f.snapshot()
	assert.Equal(t, "ermite-api", reqs[len(reqs)-1].(*ermite.ListUnreleasedRequest).GetRepo())
}

func TestReleasePendingEmpty(t *testing.T) {
	f := journalFeatures(&fakeAPI{listUnreleased: func(*ermite.ListUnreleasedRequest) (*ermite.ListUnreleasedResponse, error) {
		return &ermite.ListUnreleasedResponse{}, nil
	}}, "journal")
	vars := envFor(f.serve(t))
	assert.Equal(t, "every tagged repository is released\n", runCLI(t, vars, "", "release", "pending").ok(t).stdout)
	assert.JSONEq(t, `[]`, runCLI(t, vars, "", "release", "pending", "--json").ok(t).stdout)
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.FixedZone("CET", 3600))
	for in, want := range map[string]time.Time{
		"24h":                  now.Add(-24 * time.Hour),
		"90m":                  now.Add(-90 * time.Minute),
		"7d":                   now.Add(-7 * 24 * time.Hour),
		"2w":                   now.Add(-14 * 24 * time.Hour),
		"0d":                   now,
		"36500d":               now.Add(-36500 * 24 * time.Hour),
		"5214w":                now.Add(-5214 * 7 * 24 * time.Hour),
		"2026-10-01":           time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		"2026-10-24T08:00:00Z": time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC),
	} {
		got, err := parseSince(in, now)
		require.NoError(t, err, in)
		assert.True(t, got.Equal(want), "%s: %v", in, got)
		assert.Equal(t, time.UTC, got.Location(), in)
	}
	for _, in := range []string{"", "x", "-1d", "-2h", "1y", "2026-10-26", "2026-10-25T12:00:01+01:00", "36501d", "5215w", "36500w", "99999999999999w"} {
		_, err := parseSince(in, now)
		assert.Error(t, err, in)
	}
}

func TestTokenJournalReadFeatureGuard(t *testing.T) {
	for _, supported := range []bool{false, true} {
		t.Run(fmt.Sprint(supported), func(t *testing.T) {
			features := []string{"issues", "releases"}
			if supported {
				features = append(features, "journal")
			}
			f := journalFeatures(&fakeAPI{createToken: func(r *ermite.CreateTokenRequest) (*ermite.CreateTokenResponse, error) {
				return &ermite.CreateTokenResponse{Token: &ermite.Token{Id: "t", Scopes: r.GetScopes()}, Secret: "s"}, nil
			}}, features...)
			vars := envFor(f.serve(t))
			res := runCLI(t, vars, "", "token", "create", "daily", "--preset", "cli").ok(t)
			got := lastCreateToken(t, f).GetScopes()
			if supported {
				assert.Contains(t, got, ermite.Scope_SCOPE_JOURNAL_READ)
			} else {
				assert.NotContains(t, got, ermite.Scope_SCOPE_JOURNAL_READ)
				assert.Contains(t, res.stderr, "the server has no journal feature, so the token has no journal:read scope")
			}
			res = runCLI(t, vars, "", "token", "create", "reader", "--scope", "journal:read")
			if supported {
				res.ok(t)
				assert.Equal(t, []ermite.Scope{ermite.Scope_SCOPE_JOURNAL_READ}, lastCreateToken(t, f).GetScopes())
			} else {
				require.ErrorIs(t, res.err, errNoJournal)
			}
		})
	}
}
