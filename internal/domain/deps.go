// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	goversion "go/version"
	"slices"
	"time"

	"golang.org/x/mod/semver"
)

// DepsEcosystem identifies a supported dependency ecosystem.
type DepsEcosystem string

// Supported dependency ecosystems.
const (
	EcosystemGoMod   DepsEcosystem = "gomod"
	EcosystemActions DepsEcosystem = "github-actions"
)

// DepKind is the semantic size of an update.
type DepKind int

// Dependency update kinds.
const (
	DepPatch DepKind = iota + 1
	DepMinor
	DepMajor
)

// DepVersion is an available version. A zero Published uses first-seen instead.
type DepVersion struct {
	Version   string
	Published time.Time
	SHA       string // peeled action commit; empty for Go
}

// DepTag is an action tag and its peeled commit.
type DepTag struct{ Name, SHA string }

// DepRef identifies one proposed version, for expected versions and ignores.
type DepRef struct {
	Ecosystem DepsEcosystem
	Directory string // empty for repository root; action workflow/file path otherwise
	Name      string // Go module path or action owner/repo
	Version   string
}

// DepCandidate is an installed dependency and its available versions.
type DepCandidate struct {
	Ecosystem   DepsEcosystem
	Directory   string
	Name        string
	Current     string
	Versions    []DepVersion
	GoDirective string // go directive of the module in Directory; Go only, empty when absent
}

// GoDirectiveFits reports whether a dependency whose go.mod declares dep can
// be required by a module that declares repo, without the go directive having
// to move. Versions compare as Go does: 1.25 < 1.25.0 < 1.26rc1 < 1.26.0. A
// repo on 1.N also fits a dep on exactly 1.N.0, since `go get` only rewrites
// the directive to that form. An empty dep (no go line) fits; a go.mod
// without a go line is go 1.16; a dep that is not a Go version does not.
func GoDirectiveFits(dep, repo string) bool {
	if dep == "" {
		return true
	}
	if repo == "" {
		repo = "1.16"
	}
	if !goversion.IsValid("go" + dep) {
		return false
	}
	return goversion.Compare("go"+dep, "go"+repo) <= 0 || (goversion.Lang("go"+repo) == "go"+repo && dep == repo+".0")
}

// DepUpdate is a proposed update; Version is the latest proposed version.
type DepUpdate struct {
	DepRef
	Current string
	Kind    DepKind
	New     bool // absent from the previous check
}

// DepsState is the state of applying dependency updates.
type DepsState int

// Dependency application states.
const (
	DepsIdle DepsState = iota
	DepsApplying
	DepsFailed
)

// RepoDeps holds Bernard's repository record, apart from ignores and first-seen.
type RepoDeps struct {
	Repo         RepoName
	Updates      []DepUpdate // sorted by ecosystem, directory, name
	CheckedAt    time.Time
	SnoozedUntil time.Time // zero means not snoozed
	State        DepsState
	Error        string // short failure reason, one line; the log is on the run
	JobID string // deps-… while applying (replaced by RunID)
	// RunID is the CI run applying the updates, or the last one that tried
	// (kept after a failure or a success, so that the record links to its log);
	// zero means none. The next run overwrites it.
	RunID int64
	// RunUpdates is the snapshot of every pending update when the run started,
	// what the run commits: the daily check rewrites Updates meanwhile.
	RunUpdates []DepUpdate
	MR         int64 // open Bernard merge request; zero means none
	MRVersions []DepRef
}

// Refs returns the proposed versions in update order, independently of Updates.
func (d RepoDeps) Refs() []DepRef {
	refs := make([]DepRef, len(d.Updates))
	for i, u := range d.Updates {
		refs[i] = u.DepRef
	}
	return refs
}

// SameRefs compares proposed versions without regard to order, retaining multiplicity.
func SameRefs(a, b []DepRef) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[DepRef]int, len(a))
	for _, r := range a {
		counts[r]++
	}
	for _, r := range b {
		if counts[r] == 0 {
			return false
		}
		counts[r]--
	}
	return true
}

// PickUpdate selects the highest newer version of the same tag style that has
// cooled down and is not ignored. Go updates stay within their current major;
// actions may cross majors. Stable versions never opt into prereleases.
func PickUpdate(c DepCandidate, now time.Time, cooldown time.Duration, firstSeen func(DepVersion) time.Time, ignored func(DepRef) bool) (DepUpdate, bool) {
	ups := PickUpdates(c, now, cooldown, firstSeen, ignored)
	if len(ups) == 0 {
		return DepUpdate{}, false
	}
	return ups[0], true
}

// PickUpdates returns every update PickUpdate could select, highest first and
// without duplicate versions, so a caller can fall back to an older one.
func PickUpdates(c DepCandidate, now time.Time, cooldown time.Duration, firstSeen func(DepVersion) time.Time, ignored func(DepRef) bool) []DepUpdate {
	if !semver.IsValid(c.Current) || (c.Ecosystem != EcosystemGoMod && c.Ecosystem != EcosystemActions) {
		return nil
	}
	floating := floatingMajor(c.Current)
	var out []DepUpdate
	for _, v := range c.Versions {
		if !semver.IsValid(v.Version) || semver.Compare(v.Version, c.Current) <= 0 {
			continue
		}
		if c.Ecosystem == EcosystemGoMod && semver.Major(v.Version) != semver.Major(c.Current) {
			continue
		}
		if c.Ecosystem == EcosystemActions && floatingMajor(v.Version) != floating {
			continue
		}
		if semver.Prerelease(c.Current) == "" && semver.Prerelease(v.Version) != "" {
			continue
		}
		at := v.Published
		if at.IsZero() && firstSeen != nil {
			at = firstSeen(v)
		}
		if at.IsZero() || now.Sub(at) < cooldown {
			continue
		}
		ref := DepRef{Ecosystem: c.Ecosystem, Directory: c.Directory, Name: c.Name, Version: v.Version}
		if ignored != nil && ignored(ref) {
			continue
		}
		if slices.ContainsFunc(out, func(u DepUpdate) bool { return semver.Compare(u.Version, v.Version) == 0 }) {
			continue
		}
		kind := DepPatch
		if semver.Major(v.Version) != semver.Major(c.Current) {
			kind = DepMajor
		} else if semver.MajorMinor(v.Version) != semver.MajorMinor(c.Current) {
			kind = DepMinor
		}
		out = append(out, DepUpdate{DepRef: ref, Current: c.Current, Kind: kind})
	}
	slices.SortStableFunc(out, func(a, b DepUpdate) int { return semver.Compare(b.Version, a.Version) })
	return out
}

func floatingMajor(v string) bool {
	return v == semver.Major(v)
}
