// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import "slices"

// The permissions of a job repository token (CI-3, D3) are the keys and levels
// of the "permissions:" of a GitHub workflow. They are computed once, when the
// job is planned, and stored as a map from key to level; a key that is absent
// from the map is "none". What they allow on ermite is in RepoTokenScopes.

// Permission levels, as a workflow spells them.
const (
	PermissionNone  = "none"
	PermissionRead  = "read"
	PermissionWrite = "write"
)

// Shorthands of a "permissions:" that is a string.
const (
	PermissionsReadAll  = "read-all"
	PermissionsWriteAll = "write-all"
)

// Permission keys ermite acts on.
const (
	PermContents     = "contents"
	PermPullRequests = "pull-requests"
	// PermMetadata is always read: a workflow cannot set it.
	PermMetadata = "metadata"
)

// permissionKey describes a key of "permissions:" (workflow-v1.0.json,
// permissions-mapping).
type permissionKey struct {
	name string
	// levels tell what the key accepts besides none: "read", "write" or both.
	canRead, canWrite bool
	// writeAll is the level "write-all" grants.
	writeAll string
}

// permissionKeys are the keys a workflow can set. "write-all" keeps models and
// vulnerability-alerts at read, as GitHub does; "read-all" grants read to the
// keys that have one.
var permissionKeys = []permissionKey{
	{"actions", true, true, PermissionWrite},
	{"artifact-metadata", true, true, PermissionWrite},
	{"attestations", true, true, PermissionWrite},
	{"checks", true, true, PermissionWrite},
	{"code-quality", true, true, PermissionWrite},
	{PermContents, true, true, PermissionWrite},
	{"copilot-requests", false, true, PermissionWrite},
	{"deployments", true, true, PermissionWrite},
	{"discussions", true, true, PermissionWrite},
	{"drives", true, true, PermissionWrite},
	{"id-token", false, true, PermissionWrite},
	{"issues", true, true, PermissionWrite},
	{"models", true, false, PermissionRead},
	{"packages", true, true, PermissionWrite},
	{"pages", true, true, PermissionWrite},
	{PermPullRequests, true, true, PermissionWrite},
	{"repository-projects", true, true, PermissionWrite},
	{"security-events", true, true, PermissionWrite},
	{"statuses", true, true, PermissionWrite},
	{"vulnerability-alerts", true, false, PermissionRead},
}

func findPermissionKey(name string) (permissionKey, bool) {
	for _, k := range permissionKeys {
		if k.name == name {
			return k, true
		}
	}
	return permissionKey{}, false
}

// PermissionSpec is a "permissions:" as written in a workflow or a job: a
// shorthand (read-all, write-all) or a level per key. A nil *PermissionSpec is
// no "permissions:" at all; an empty spec is "permissions: {}".
type PermissionSpec struct {
	All    string
	Levels map[string]string
}

// DefaultTokenPermissions are the permissions of a token whose workflow says
// nothing: GitHub's restricted default (ermite offers no permissive setting).
func DefaultTokenPermissions() map[string]string {
	return map[string]string{PermContents: PermissionRead, PermMetadata: PermissionRead, "packages": PermissionRead}
}

// ResolvePermissions returns the permissions a job asks for: the job's
// "permissions:" replaces the workflow's as a whole (GitHub does not merge
// them), and metadata is always read. It returns nil when neither says anything:
// the job then gets the default (TokenPermissions). A key or level ermite does
// not know is none. The trust rule of the run is not applied here: it is applied
// when the token is issued (TokenPermissions), so that a re-run of the same job
// by another caller lowers the token without planning again.
func ResolvePermissions(workflow, job *PermissionSpec) map[string]string {
	spec := job
	if spec == nil {
		spec = workflow
	}
	if spec == nil {
		return nil
	}
	out := map[string]string{PermMetadata: PermissionRead}
	switch spec.All {
	case PermissionsReadAll:
		for _, k := range permissionKeys {
			if k.canRead {
				out[k.name] = PermissionRead
			}
		}
	case PermissionsWriteAll:
		for _, k := range permissionKeys {
			out[k.name] = k.writeAll
		}
	default:
		for name, level := range spec.Levels {
			k, ok := findPermissionKey(name)
			switch {
			case !ok, level == PermissionNone:
			case level == PermissionRead && k.canRead, level == PermissionWrite && k.canWrite:
				out[name] = level
			}
		}
	}
	return out
}

// TokenPermissions returns the permissions a job token is issued with, given
// those stored with the job: the default when none are stored (a job planned
// before they existed), and, for a run that is not trusted (D1), every write
// lowered to read (to none for a key that has no read). It copies its input.
func TokenPermissions(stored map[string]string, trusted bool) map[string]string {
	if len(stored) == 0 {
		stored = DefaultTokenPermissions()
	}
	out := make(map[string]string, len(stored)+1)
	for name, level := range stored {
		if level == PermissionWrite && !trusted {
			if k, ok := findPermissionKey(name); ok && !k.canRead {
				continue
			}
			level = PermissionRead
		}
		if level == PermissionRead || level == PermissionWrite {
			out[name] = level
		}
	}
	out[PermMetadata] = PermissionRead
	return out
}

// RepoTokenScopes maps the permissions of a job token to the scopes of the
// principal on the run's repository: contents read is repo:read, contents write
// adds repo:write, pull-requests write is mr:write. Never repo:admin, mr:merge,
// secrets, tokens or CI scopes, whatever the permissions say. Permissions
// without an effect on ermite grant nothing.
func RepoTokenScopes(perms map[string]string) []Scope {
	if len(perms) == 0 {
		perms = DefaultTokenPermissions()
	}
	var scopes []Scope
	switch perms[PermContents] {
	case PermissionWrite:
		scopes = append(scopes, ScopeRepoRead, ScopeRepoWrite)
	case PermissionRead:
		scopes = append(scopes, ScopeRepoRead)
	}
	if perms[PermPullRequests] == PermissionWrite {
		scopes = append(scopes, ScopeMRWrite)
	}
	return scopes
}

// permissionRank orders the levels: none, read, write.
func permissionRank(level string) int {
	switch level {
	case PermissionRead:
		return 1
	case PermissionWrite:
		return 2
	}
	return 0
}

// PermissionViolations lists the keys that requested asks for above what max
// allows (an absent key is none): a reusable workflow can only keep or lower the
// permissions of its caller. The two slices hold "key: level" in the same
// order, sorted by key, as GitHub's error message shows them.
func PermissionViolations(requested, max map[string]string) (asked, allowed []string) {
	keys := make([]string, 0, len(requested))
	for k := range requested {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if permissionRank(requested[k]) > permissionRank(max[k]) {
			lv := max[k]
			if lv == "" {
				lv = PermissionNone
			}
			asked = append(asked, k+": "+requested[k])
			allowed = append(allowed, k+": "+lv)
		}
	}
	return asked, allowed
}
