// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPickUpdate(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	cooldown := 72 * time.Hour
	old := now.Add(-4 * 24 * time.Hour)
	versions := func(vs ...string) []DepVersion {
		out := make([]DepVersion, len(vs))
		for i, v := range vs {
			out[i] = DepVersion{Version: v, Published: old}
		}
		return out
	}
	for _, tc := range []struct {
		name     string
		eco      DepsEcosystem
		current  string
		versions []DepVersion
		seen     time.Time
		ignore   string
		want     string
		kind     DepKind
	}{
		{name: "highest patch unordered", current: "v1.2.0", versions: versions("v1.2.3", "v1.2.1", "v1.2.2"), want: "v1.2.3", kind: DepPatch},
		{name: "minor", current: "v1.2.0", versions: versions("v1.3.0"), want: "v1.3.0", kind: DepMinor},
		{name: "Go never crosses major", current: "v1.2.0", versions: versions("v2.0.0", "v1.3.0"), want: "v1.3.0", kind: DepMinor},
		{name: "Go v2 never crosses major", current: "v2.0.0", versions: versions("v3.0.0")},
		{name: "Go v0 stays v0", current: "v0.1.0", versions: versions("v1.0.0", "v0.2.0"), want: "v0.2.0", kind: DepMinor},
		{name: "equal older invalid", current: "v1.2.0", versions: versions("v1.2.0", "v1.1.9", "garbage")},
		{name: "invalid current", current: "garbage", versions: versions("v1.2.0")},
		{name: "unknown ecosystem", eco: "unknown", current: "v1.0.0", versions: versions("v1.2.0")},
		{name: "stable skips prerelease", current: "v1.0.0", versions: versions("v1.2.0-rc.1", "v1.1.0"), want: "v1.1.0", kind: DepMinor},
		{name: "prerelease accepts prerelease", current: "v1.0.0-beta.1", versions: versions("v1.0.0-rc.1"), want: "v1.0.0-rc.1", kind: DepPatch},
		{name: "prerelease accepts stable", current: "v1.0.0-rc.1", versions: versions("v1.0.0"), want: "v1.0.0", kind: DepPatch},
		{name: "incompatible", current: "v2.0.0+incompatible", versions: versions("v2.1.0+incompatible", "v3.0.0+incompatible"), want: "v2.1.0+incompatible", kind: DepMinor},
		{name: "metadata not newer", current: "v1.0.0", versions: versions("v1.0.0+incompatible")},
		{name: "ignored highest", current: "v1.0.0", versions: versions("v1.2.0", "v1.1.0"), ignore: "v1.2.0", want: "v1.1.0", kind: DepMinor},
		{name: "all ignored", current: "v1.0.0", versions: versions("v1.1.0"), ignore: "v1.1.0"},
		{name: "young highest falls back", current: "v1.0.0", versions: []DepVersion{{Version: "v1.2.0", Published: now.Add(-time.Hour)}, {Version: "v1.1.0", Published: old}}, want: "v1.1.0", kind: DepMinor},
		{name: "exact cooldown boundary", current: "v1.0.0", versions: []DepVersion{{Version: "v1.1.0", Published: now.Add(-cooldown)}}, want: "v1.1.0", kind: DepMinor},
		{name: "future publish", current: "v1.0.0", versions: []DepVersion{{Version: "v1.1.0", Published: now.Add(time.Hour)}}},
		{name: "unknown time fails closed", current: "v1.0.0", versions: []DepVersion{{Version: "v1.1.0"}}},
		{name: "first seen fallback", current: "v1.0.0", versions: []DepVersion{{Version: "v1.1.0", SHA: "commit"}}, seen: old, want: "v1.1.0", kind: DepMinor},
		{name: "young first seen", current: "v1.0.0", versions: []DepVersion{{Version: "v1.1.0"}}, seen: now.Add(-time.Hour)},
		{name: "publish wins over old seen", current: "v1.0.0", versions: []DepVersion{{Version: "v1.1.0", Published: now}}, seen: old},
		{name: "floating major", eco: EcosystemActions, current: "v3", versions: versions("v4", "v4.2.1", "v5.0.0"), want: "v4", kind: DepMajor},
		{name: "floating ignores exact", eco: EcosystemActions, current: "v4", versions: versions("v4.2.1", "v5.0.0")},
		{name: "floating next major", eco: EcosystemActions, current: "v4", versions: versions("v4", "v4.2.1", "v5"), want: "v5", kind: DepMajor},
		{name: "exact highest exact major", eco: EcosystemActions, current: "v4.0.0", versions: versions("v6", "v5.2.1", "v4.9.0"), want: "v5.2.1", kind: DepMajor},
		{name: "exact action minor", eco: EcosystemActions, current: "v4.0.0", versions: versions("v4", "v4.2.1"), want: "v4.2.1", kind: DepMinor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eco := tc.eco
			if eco == "" {
				eco = EcosystemGoMod
			}
			c := DepCandidate{Ecosystem: eco, Directory: "sub", Name: "example/dependency", Current: tc.current, Versions: tc.versions}
			got, ok := PickUpdate(c, now, cooldown, func(v DepVersion) time.Time { require.NotEmpty(t, v.Version); return tc.seen }, func(ref DepRef) bool {
				assert.Equal(t, eco, ref.Ecosystem)
				assert.Equal(t, c.Directory, ref.Directory)
				assert.Equal(t, c.Name, ref.Name)
				return ref.Version == tc.ignore
			})
			require.Equal(t, tc.want != "", ok)
			if !ok {
				assert.Equal(t, DepUpdate{}, got)
				return
			}
			assert.Equal(t, DepUpdate{DepRef: DepRef{Ecosystem: eco, Directory: c.Directory, Name: c.Name, Version: tc.want}, Current: tc.current, Kind: tc.kind}, got)
		})
	}
}

func TestPickUpdatesHighestFirst(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	old := now.Add(-96 * time.Hour)
	c := DepCandidate{Ecosystem: EcosystemGoMod, Name: "module", Current: "v1.0.0", Versions: []DepVersion{
		{Version: "v1.1.0", Published: old}, {Version: "v1.3.0", Published: old}, {Version: "v1.4.0", Published: now},
		{Version: "v1.2.0", Published: old}, {Version: "v1.3.0", Published: old}, {Version: "v2.0.0", Published: old},
	}}
	var got []string
	for _, u := range PickUpdates(c, now, 72*time.Hour, nil, func(r DepRef) bool { return r.Version == "v1.2.0" }) {
		got = append(got, u.Version)
	}
	assert.Equal(t, []string{"v1.3.0", "v1.1.0"}, got, "young, ignored, duplicate and new-major versions are left out")
	assert.Empty(t, PickUpdates(DepCandidate{Ecosystem: EcosystemGoMod, Current: "garbage", Versions: c.Versions}, now, 0, nil, nil))
}

func TestGoDirectiveFits(t *testing.T) {
	for _, tc := range []struct {
		dep, repo string
		want      bool
	}{
		{"1.25.3", "1.25.3", true},
		{"1.25.0", "1.25.3", true},
		{"1.25", "1.25.3", true},
		{"1.24", "1.25", true},
		{"1.25.0", "1.25", true}, // go get only rewrites 1.25 to 1.25.0
		{"1.25.1", "1.25", false},
		{"1.25.4", "1.25.3", false},
		{"1.26rc1", "1.25.3", false},
		{"1.26rc1", "1.26.0", true},
		{"1.26.0", "1.26rc1", false},
		{"1.26.0", "1.25.3", false},
		{"", "1.25.3", true}, // no go line
		{"1.30", "", false},  // a go.mod without a go line is go 1.16
		{"1.16", "", true},
		{"", "", true},
		{"junk", "1.25.3", false},
	} {
		assert.Equal(t, tc.want, GoDirectiveFits(tc.dep, tc.repo), "dep %q repo %q", tc.dep, tc.repo)
	}
}

func TestDependencyRefs(t *testing.T) {
	a := DepRef{Ecosystem: EcosystemGoMod, Name: "module", Version: "v1.1.0"}
	b := DepRef{Ecosystem: EcosystemActions, Directory: "workflow.yml", Name: "owner/action", Version: "v5"}
	d := RepoDeps{Updates: []DepUpdate{{DepRef: a}, {DepRef: b}}}
	refs := d.Refs()
	assert.Equal(t, []DepRef{a, b}, refs)
	refs[0] = b
	assert.Equal(t, a, d.Updates[0].DepRef)
	assert.True(t, SameRefs([]DepRef{a, b}, []DepRef{b, a}))
	assert.True(t, SameRefs(nil, []DepRef{}))
	assert.False(t, SameRefs([]DepRef{a}, nil))
	assert.False(t, SameRefs([]DepRef{a, a}, []DepRef{a, b}))
	for _, changed := range []DepRef{
		{Ecosystem: EcosystemActions, Name: a.Name, Version: a.Version},
		{Ecosystem: a.Ecosystem, Directory: "other", Name: a.Name, Version: a.Version},
		{Ecosystem: a.Ecosystem, Name: "other", Version: a.Version},
		{Ecosystem: a.Ecosystem, Name: a.Name, Version: "v1.2.0"},
	} {
		assert.False(t, SameRefs([]DepRef{a}, []DepRef{changed}))
	}
}

func TestPickUpdateFirstSeenUsesTagSHA(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	c := DepCandidate{Ecosystem: EcosystemActions, Name: "owner/action", Current: "v4", Versions: []DepVersion{{Version: "v5", SHA: "repointed"}, {Version: "v6", SHA: "old"}}}
	got, ok := PickUpdate(c, now, 72*time.Hour, func(v DepVersion) time.Time {
		if v.SHA == "repointed" {
			return now
		}
		return now.Add(-96 * time.Hour)
	}, nil)
	require.True(t, ok)
	assert.Equal(t, "v6", got.Version)
	c.Versions = c.Versions[:1]
	_, ok = PickUpdate(c, now, 72*time.Hour, func(DepVersion) time.Time { return now }, nil)
	assert.False(t, ok, "a repointed tag starts its cooldown again")
}

func TestPickUpdateNilCallbacksAndPureInput(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	c := DepCandidate{Ecosystem: EcosystemGoMod, Name: "module", Current: "v1.0.0", Versions: []DepVersion{{Version: "v1.1.0", Published: now.Add(-96 * time.Hour)}}}
	before := append([]DepVersion(nil), c.Versions...)
	_, ok := PickUpdate(c, now, 72*time.Hour, nil, nil)
	assert.True(t, ok)
	assert.Equal(t, before, c.Versions)
	c.Versions[0].Published = time.Time{}
	_, ok = PickUpdate(c, now, 72*time.Hour, nil, nil)
	assert.False(t, ok)
}
