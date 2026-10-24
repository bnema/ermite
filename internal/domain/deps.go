// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
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
	Ecosystem DepsEcosystem
	Directory string
	Name      string
	Current   string
	Versions  []DepVersion
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
	Error        string // failure reason, possibly with a bounded log tail
	JobID        string // deps-… while applying
	MR           int64  // open Bernard merge request; zero means none
	MRVersions   []DepRef
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
	if !semver.IsValid(c.Current) || (c.Ecosystem != EcosystemGoMod && c.Ecosystem != EcosystemActions) {
		return DepUpdate{}, false
	}
	floating := floatingMajor(c.Current)
	var best DepUpdate
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
		if best.Version != "" && semver.Compare(v.Version, best.Version) <= 0 {
			continue
		}
		kind := DepPatch
		if semver.Major(v.Version) != semver.Major(c.Current) {
			kind = DepMajor
		} else if semver.MajorMinor(v.Version) != semver.MajorMinor(c.Current) {
			kind = DepMinor
		}
		best = DepUpdate{DepRef: ref, Current: c.Current, Kind: kind}
	}
	return best, best.Version != ""
}

func floatingMajor(v string) bool {
	return v == semver.Major(v)
}
