// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"crypto/sha256"
	"fmt"
	"strings"
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
	d := RepoDeps{Batches: []DepBatch{{Updates: []DepUpdate{{DepRef: a}, {DepRef: b}}}}}
	refs := d.Base().Refs()
	assert.Equal(t, []DepRef{a, b}, refs)
	refs[0] = b
	assert.Equal(t, a, d.Base().Updates[0].DepRef)
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

func TestBatchLookupAndPending(t *testing.T) {
	u := DepUpdate{DepRef: DepRef{Name: "pkg"}}
	for _, tc := range []struct {
		name           string
		d              RepoDeps
		key            DepBatchKey
		index          int
		found, pending bool
	}{
		{"empty", RepoDeps{}, "", -1, false, false},
		{"base", RepoDeps{Batches: []DepBatch{{}}}, "", 0, true, false},
		{"major", RepoDeps{Batches: []DepBatch{{}, {Key: "major", Updates: []DepUpdate{u}}}}, "major", 1, true, true},
		{"missing", RepoDeps{Batches: []DepBatch{{Updates: []DepUpdate{u}}}}, "missing", -1, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, i, ok := tc.d.Batch(tc.key)
			assert.Equal(t, tc.index, i)
			assert.Equal(t, tc.found, ok)
			assert.Equal(t, tc.pending, tc.d.Pending())
			if ok {
				assert.Equal(t, tc.d.Batches[i], b)
			} else {
				assert.Equal(t, DepBatch{}, b)
			}
		})
	}
}

func TestMajorBatchKey(t *testing.T) {
	for _, tc := range []struct {
		eco                      DepsEcosystem
		dir, name, version, want string
	}{
		{"npm", "web", "@scope/pkg", "v19.2.0", "npm/web/@scope/pkg@v19"},
		{EcosystemGoMod, "", "gopkg.in/yaml.v2", "v3.0.1+incompatible", "gomod//gopkg.in/yaml.v2@v3"},
		{EcosystemActions, ".github/workflows/ci.yml", "actions/checkout", "v5", "github-actions/.github/workflows/ci.yml/actions/checkout@v5"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			assert.Equal(t, DepBatchKey(tc.want), MajorBatchKey(DepUpdate{DepRef: DepRef{Ecosystem: tc.eco, Directory: tc.dir, Name: tc.name, Version: tc.version}, NewName: "ignored"}))
		})
	}
}

func TestDepBatchBranch(t *testing.T) {
	for _, tc := range []struct{ key, slug string }{
		{"", ""}, {"npm/web/@scope/pkg@v19", "npm-web-scope-pkg-v19"},
		{"gomod//gopkg.in/yaml.v2@v3", "gomod-gopkg-in-yaml-v2-v3"},
		{"---A__B...C---", "a__b-c"}, {"日本語/É/包", ""},
		{strings.Repeat("A", 200), strings.Repeat("a", 80)},
	} {
		t.Run(tc.key, func(t *testing.T) {
			key := DepBatchKey(tc.key)
			got := key.Branch()
			_, err := ParseBranchName(got)
			require.NoError(t, err)
			if key == "" {
				assert.Equal(t, "bernard/deps", got)
			} else {
				sum := sha256.Sum256([]byte(key))
				assert.Equal(t, fmt.Sprintf("bernard/deps-major/%s-%x", tc.slug, sum[:4]), got)
			}
			assert.Equal(t, got, key.Branch())
		})
	}
	assert.NotEqual(t, DepBatchKey("a/b").Branch(), DepBatchKey("a.b").Branch())
}

func TestBotSubject(t *testing.T) {
	for _, tc := range []struct {
		name  string
		batch DepBatch
		want  string
	}{
		{"empty base", DepBatch{}, "build(deps): update 0 dependencies"},
		{"base", DepBatch{Updates: make([]DepUpdate, 2)}, "build(deps): update 2 dependencies"},
		{"major", DepBatch{Key: "npm/web/react@v19", Updates: []DepUpdate{{DepRef: DepRef{Name: "react", Version: "v19.2.0"}}}}, "build(deps): update react to v19"},
		{"stale scoped", DepBatch{Key: "npm/web/@scope/pkg@v19"}, "build(deps): update @scope/pkg to v19"},
		{"stale Go", DepBatch{Key: "gomod//gopkg.in/yaml.v2@v3"}, "build(deps): update gopkg.in/yaml.v2 to v3"},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, BotSubject(tc.batch)) })
	}
}

func TestSplitBatches(t *testing.T) {
	u := func(eco DepsEcosystem, dir, name, current, version string) DepUpdate {
		return DepUpdate{DepRef: DepRef{Ecosystem: eco, Directory: dir, Name: name, Version: version}, Current: current}
	}
	a := u("npm", "", "a", "v1.0.0", "v1.9.0")
	a2 := u("npm", "", "a", "v1.0.0", "v3.2.0")
	b := u("npm", "web", "a", "v0.1.0", "v0.9.0")
	c := u(EcosystemGoMod, "", "z", "v1.0.0", "v2.0.0+incompatible")
	d := u("npm", "", "b", "v1.0.0", "v1.1.0")
	e := u(EcosystemGoMod, "", "z", "v1.0.0", "v1.2.0")
	for _, tc := range []struct {
		name                string
		input, base, majors []DepUpdate
	}{
		{"empty", nil, nil, nil},
		{"base only", []DepUpdate{b, d, a, e}, []DepUpdate{e, a, d, b}, nil},
		{"major only sorted", []DepUpdate{a2, c}, nil, []DepUpdate{c, a2}},
		{"highest per identity", []DepUpdate{a2, b, a, c, d, e, u("npm", "", "a", "v1.0.0", "v2.9.0"), u("npm", "", "a", "v1.0.0", "v1.1.0")}, []DepUpdate{e, a, d, b}, []DepUpdate{c, a2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := append([]DepUpdate(nil), tc.input...)
			base, majors := SplitBatches(tc.input)
			assert.Equal(t, tc.base, base)
			assert.Equal(t, tc.majors, majors)
			assert.Equal(t, before, tc.input)
		})
	}
}

func TestRepoDepsBase(t *testing.T) {
	for _, initial := range [][]DepBatch{nil, {{Key: "major"}}, {{}, {Key: "major"}}} {
		d := RepoDeps{Batches: initial}
		base := d.Base()
		require.Empty(t, base.Key)
		base.MR = 7
		require.Equal(t, int64(7), d.Base().MR)
		require.Equal(t, base, d.Base())
		count := 0
		for _, b := range d.Batches {
			if b.Key == "" {
				count++
			}
		}
		require.Equal(t, 1, count)
	}
}
