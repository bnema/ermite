// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"crypto/sha256"
	"errors"
	"fmt"
	goversion "go/version"
	"slices"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// ErrModulePathMismatch means a Go module version's go.mod declares another
// module path than the one it is fetched as (vanity move, case change).
var ErrModulePathMismatch = errors.New("go.mod declares another module path")

// DepsCacheRef isolates the deps module cache from workflow caches both ways.
const DepsCacheRef = "refs/ermite/deps"

// DepsEcosystem identifies a supported dependency ecosystem.
type DepsEcosystem string

// Supported dependency ecosystems.
const (
	EcosystemGoMod   DepsEcosystem = "gomod"
	EcosystemNPM     DepsEcosystem = "npm"
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
	Path      string // module path holding the version; empty means candidate name
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

// DepName identifies a dependency across its family batches.
type DepName struct {
	Ecosystem DepsEcosystem
	Name      string
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
	NewName string // new module path for path-major updates
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
	DepsQueued  // Waiting for the server to bump manifests, push and open a draft MR.
	DepsDrafted // Draft MR pushed; waiting for a deps-run slot.
)

// RepoDeps holds Bernard's repository record, apart from ignores and first-seen.
type RepoDeps struct {
	Repo         RepoName
	Batches      []DepBatch
	CheckedAt    time.Time
	SnoozedUntil time.Time
	// AutoOpenDisabled opts out of automatic patch/minor merge requests.
	AutoOpenDisabled bool
	// SplitOut holds majors pulled out of their family batch.
	SplitOut []DepName
}

// Base returns the base batch, installing it first when absent.
func (d *RepoDeps) Base() *DepBatch {
	for i := range d.Batches {
		if d.Batches[i].Key == "" {
			return &d.Batches[i]
		}
	}
	d.Batches = append([]DepBatch{{}}, d.Batches...)
	return &d.Batches[0]
}

// Refs returns the proposed versions in update order.
func (d DepBatch) Refs() []DepRef {
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
// cooled down and is not ignored. Updates may cross majors. Stable versions never opt into prereleases.
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
	if !semver.IsValid(c.Current) {
		return nil
	}
	floating := floatingMajor(c.Current)
	var out []DepUpdate
	for _, v := range c.Versions {
		if !semver.IsValid(v.Version) || semver.Compare(v.Version, c.Current) <= 0 {
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
		newName := ""
		if v.Path != "" && v.Path != c.Name {
			newName = v.Path
		}
		out = append(out, DepUpdate{DepRef: ref, Current: c.Current, Kind: kind, NewName: newName})
	}
	slices.SortStableFunc(out, func(a, b DepUpdate) int { return semver.Compare(b.Version, a.Version) })
	return out
}

func floatingMajor(v string) bool {
	return v == semver.Major(v)
}

// DepSideEffect describes a Go-selected change beyond the requested versions.
// An empty To means tidy removed the module.
type DepSideEffect struct {
	Directory, Name, From, To string
}

// DepBatchKey identifies the base batch (empty) or a dependency major.
type DepBatchKey string

// DepBatch holds independently applied updates and their run and merge request.
type DepBatch struct {
	Key        DepBatchKey
	Updates    []DepUpdate
	State      DepsState
	Error      string
	RunID      int64
	RunUpdates []DepUpdate
	MR         int64
	MRVersions []DepRef
	// MRUpdates are the updates the merge request holds, with the versions
	// they update from.
	MRUpdates []DepUpdate
	// Branch is stored when first written; empty uses the key-derived branch.
	Branch string
	// Head is the last commit Bernard wrote on the branch.
	Head ObjectID
	// Adopted means a human pushed to the branch; Bernard never rewrites it again.
	Adopted bool
	// QueuedAt records when the batch entered the queue.
	QueuedAt time.Time
	// Manual puts the batch before automatic batches, then ordered by QueuedAt.
	Manual bool
	// Repro holds local reproduction commands for a failure.
	Repro []string
	// Drafts are the draft heads Bernard pushed on the branch, newest last,
	// at most MaxDepDrafts: CI holds their runs whenever it plans them.
	Drafts []DepDraft
}

// MaxDepDrafts bounds DepBatch.Drafts.
const MaxDepDrafts = 16

// DepDraft is a manifest-only head Bernard pushed on a batch branch. Created
// means the push created the branch.
type DepDraft struct {
	Sha     ObjectID
	Created bool
}

// AddDraft records a draft head, dropping the oldest past MaxDepDrafts.
func (b *DepBatch) AddDraft(sha ObjectID, created bool) {
	b.Drafts = append(slices.DeleteFunc(slices.Clone(b.Drafts), func(d DepDraft) bool { return d.Sha == sha }), DepDraft{Sha: sha, Created: created})
	if n := len(b.Drafts) - MaxDepDrafts; n > 0 {
		b.Drafts = slices.Delete(b.Drafts, 0, n)
	}
}

// Draft returns the recorded draft head sha.
func (b DepBatch) Draft(sha ObjectID) (DepDraft, bool) {
	for _, d := range b.Drafts {
		if d.Sha == sha {
			return d, true
		}
	}
	return DepDraft{}, false
}

// RemoveDraft forgets the draft head sha.
func (b *DepBatch) RemoveDraft(sha ObjectID) {
	b.Drafts = slices.DeleteFunc(slices.Clone(b.Drafts), func(d DepDraft) bool { return d.Sha == sha })
	if len(b.Drafts) == 0 {
		b.Drafts = nil
	}
}

// BranchName returns the stored branch or the key-derived branch when unset.
func (b DepBatch) BranchName() string {
	if b.Branch != "" {
		return b.Branch
	}
	return b.Key.Branch()
}

// Outdated reports whether the batch has a merge request that no longer
// matches its proposed updates: other versions, or the same versions from
// other current versions (an overlapping batch was merged meanwhile).
func (d DepBatch) Outdated() bool {
	if d.MR == 0 {
		return false
	}
	if !SameRefs(d.Refs(), d.MRVersions) {
		return true
	}
	current := make(map[DepRef]string, len(d.MRUpdates))
	for _, u := range d.MRUpdates {
		current[u.DepRef] = u.Current
	}
	for _, u := range d.Updates {
		if c, ok := current[u.DepRef]; ok && c != u.Current {
			return true
		}
	}
	return false
}

// Batch finds a batch and its index, or returns index -1 when absent.
func (d RepoDeps) Batch(k DepBatchKey) (DepBatch, int, bool) {
	for i, b := range d.Batches {
		if b.Key == k {
			return b, i, true
		}
	}
	return DepBatch{}, -1, false
}

// Visible reports whether the batch has something to show: proposed updates,
// an open merge request or a run that is applying it.
func (d DepBatch) Visible() bool {
	return len(d.Updates) != 0 || d.MR != 0 || d.State == DepsApplying
}

// Pending reports whether any batch has proposed updates.
func (d RepoDeps) Pending() bool {
	return slices.ContainsFunc(d.Batches, func(b DepBatch) bool { return len(b.Updates) != 0 })
}

// majorKeyPrefix starts every major batch key: "major:<ecosystem>:<family>"
// or, for a dependency split out of its family, "major:<ecosystem>:<name>".
const majorKeyPrefix = "major:"

// multiOwnerGoHosts are Go module hosts where the first path element is an
// owner: their family is host/owner/.
var multiOwnerGoHosts = map[string]bool{
	"github.com": true, "gitlab.com": true, "codeberg.org": true, "bitbucket.org": true,
	"git.bnema.dev": true, "gitea.com": true, "git.sr.ht": true, "salsa.debian.org": true,
}

// FamilyOf returns the family a major update is grouped with. Families end
// with "/"; a dependency without a family returns its own name.
//   - npm: a scoped package "@scope/x" belongs to "@scope/".
//   - Actions: "owner/repo[/path]" belongs to "owner/".
//   - Go: on a multi-owner host (github.com, gitlab.com, codeberg.org, ...)
//     "host/owner/"; "golang.org/x/..." is "golang.org/x/"; gopkg.in has no
//     family; any other host is a vanity domain with one owner, so "host/"
//     (go.opentelemetry.io/, k8s.io/).
//
// Anything else (unscoped npm, single-segment names) has no family.
func FamilyOf(u DepUpdate) string {
	name := u.Name
	segs := strings.Split(name, "/")
	switch u.Ecosystem {
	case EcosystemNPM:
		if strings.HasPrefix(name, "@") && len(segs) >= 2 && segs[0] != "@" {
			return segs[0] + "/"
		}
	case EcosystemActions:
		if len(segs) >= 2 && segs[0] != "" && !strings.Contains(name, "://") {
			return segs[0] + "/"
		}
	case EcosystemGoMod:
		host := segs[0]
		switch {
		case len(segs) < 2 || !strings.Contains(host, ".") || host == "gopkg.in":
		case multiOwnerGoHosts[host]:
			if len(segs) >= 3 && segs[1] != "" {
				return host + "/" + segs[1] + "/"
			}
		case host == "golang.org" && segs[1] == "x" && len(segs) >= 3:
			return "golang.org/x/"
		default:
			return host + "/"
		}
	}
	return name
}

// FamilyBatchKey is the key of the major batch of u: its family across all
// directories, or the dependency alone when it is in split.
func FamilyBatchKey(u DepUpdate, split []DepName) DepBatchKey {
	group := FamilyOf(u)
	if slices.Contains(split, DepName{Ecosystem: u.Ecosystem, Name: u.Name}) {
		group = u.Name
	}
	return DepBatchKey(majorKeyPrefix + string(u.Ecosystem) + ":" + group)
}

// Branch returns a safe, stable branch name with a collision-resistant suffix.
func (k DepBatchKey) Branch() string {
	if k == "" {
		return "bernard/deps"
	}
	var slug strings.Builder
	for _, r := range strings.ToLower(string(k)) {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			r = '-'
		}
		if r == '-' && (slug.Len() == 0 || strings.HasSuffix(slug.String(), "-")) {
			continue
		}
		slug.WriteRune(r)
	}
	s := slug.String()
	if len(s) > 80 {
		s = s[:80]
	}
	s = strings.Trim(s, "-")
	sum := sha256.Sum256([]byte(k))
	return fmt.Sprintf("bernard/deps-major/%s-%x", s, sum[:4])
}

// BotSubject describes a batch. A major batch names its package and major
// when it holds one package, its family otherwise. Without updates it takes
// the names from its merge request or run versions, and from its key last.
func BotSubject(b DepBatch) string {
	if b.Key == "" {
		return BotCommitSubject(len(b.Updates))
	}
	var refs []DepRef
	switch {
	case len(b.Updates) != 0:
		refs = b.Refs()
	case len(b.MRVersions) != 0:
		refs = b.MRVersions
	case len(b.RunUpdates) != 0:
		for _, u := range b.RunUpdates {
			refs = append(refs, u.DepRef)
		}
	}
	if len(refs) == 0 {
		if rest, ok := strings.CutPrefix(string(b.Key), majorKeyPrefix); ok {
			_, group, _ := strings.Cut(rest, ":")
			if strings.HasSuffix(group, "/") {
				return "build(deps): update " + group + "* to new majors"
			}
			return "build(deps): update " + group + " to a new major"
		}
		name, major := parseMajorBatchKey(b.Key)
		return "build(deps): update " + name + " to " + major
	}
	names := map[string]bool{}
	majors := map[string]bool{}
	for _, r := range refs {
		names[r.Name] = true
		majors[semver.Major(r.Version)] = true
	}
	if len(names) == 1 && len(majors) == 1 {
		return "build(deps): update " + refs[0].Name + " to " + semver.Major(refs[0].Version)
	}
	if len(names) == 1 {
		return "build(deps): update " + refs[0].Name + " to new majors"
	}
	family := FamilyOf(DepUpdate{DepRef: refs[0]})
	return "build(deps): update " + family + "* to new majors"
}

// parseMajorBatchKey reverses the per-dependency keys of schema 12
// ("<eco>/<dir>/<name>@<major>"), which batches with an open merge request
// may keep after the family upgrade. The major follows the last @,
// which keeps scoped npm names whole. The directory may hold slashes, so the
// name is cut by ecosystem: the owner/repo after an action file, the last
// segment (two for a scoped npm package) for npm, and from the first segment
// with a dot (the module host) for Go.
func parseMajorBatchKey(k DepBatchKey) (name, major string) {
	eco, rest, _ := strings.Cut(string(k), "/")
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest, major = rest[:i], rest[i+1:]
	}
	segs := strings.Split(rest, "/")
	start := len(segs) - 1
	switch DepsEcosystem(eco) {
	case EcosystemActions:
		start = max(len(segs)-2, 0)
		for i, s := range segs {
			if strings.HasSuffix(s, ".yml") || strings.HasSuffix(s, ".yaml") {
				start = i + 1
			}
		}
	case EcosystemNPM:
		if start > 0 && strings.HasPrefix(segs[start-1], "@") {
			start--
		}
	case EcosystemGoMod:
		if i := slices.IndexFunc(segs, func(s string) bool { return strings.Contains(s, ".") }); i >= 0 {
			start = i
		}
	}
	return strings.Join(segs[min(start, len(segs)-1):], "/"), major
}

// SplitBatches selects the highest same-major and cross-major version per
// dependency, and groups the cross-major ones into batches by FamilyBatchKey
// (sorted by key, updates by ecosystem, directory and name). It does not
// modify the input or consult the update's Kind.
func SplitBatches(ups []DepUpdate, split []DepName) (base []DepUpdate, majors []DepBatch) {
	type identity struct {
		eco       DepsEcosystem
		dir, name string
	}
	same, cross := map[identity]DepUpdate{}, map[identity]DepUpdate{}
	for _, u := range ups {
		k := identity{u.Ecosystem, u.Directory, u.Name}
		target := same
		if semver.Major(u.Version) != semver.Major(u.Current) {
			target = cross
		}
		if old, ok := target[k]; !ok || semver.Compare(u.Version, old.Version) > 0 {
			target[k] = u
		}
	}
	byName := func(a, b DepUpdate) int {
		if c := strings.Compare(string(a.Ecosystem), string(b.Ecosystem)); c != 0 {
			return c
		}
		if c := strings.Compare(a.Directory, b.Directory); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	}
	for _, u := range same {
		base = append(base, u)
	}
	slices.SortFunc(base, byName)
	groups := map[DepBatchKey][]DepUpdate{}
	for _, u := range cross {
		k := FamilyBatchKey(u, split)
		groups[k] = append(groups[k], u)
	}
	for k, g := range groups {
		slices.SortFunc(g, byName)
		majors = append(majors, DepBatch{Key: k, Updates: g})
	}
	slices.SortFunc(majors, func(a, b DepBatch) int { return strings.Compare(string(a.Key), string(b.Key)) })
	return base, majors
}
