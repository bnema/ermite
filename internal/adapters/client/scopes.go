// SPDX-License-Identifier: AGPL-3.0-or-later

package client

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"bnema.dev/ermite-api/gen/ermite"
)

// Token presets (ADR 0006). The API knows scopes only; a preset is a client
// shortcut that expands to scopes.
const (
	presetCLI   = "cli"
	presetAgent = "agent"
	presetCI    = "ci"
	// presetRunner is for a CI runner host: it only registers runners.
	presetRunner = "runner"
)

// PresetNames lists the token presets, see PresetScopes.
var PresetNames = []string{presetCLI, presetAgent, presetCI, presetRunner}

// ScopeName is the name of a scope as users type it and as the docs write
// it: SCOPE_REPO_READ is "repo:read".
func ScopeName(s ermite.Scope) string {
	name, ok := ermite.Scope_name[int32(s)]
	if !ok {
		return strconv.Itoa(int(s))
	}
	return strings.Replace(strings.ToLower(strings.TrimPrefix(name, "SCOPE_")), "_", ":", 1)
}

// AllScopes lists every scope the contract defines that this server accepts, in
// enum order. Deriving it keeps the `cli` preset ("every scope") true when the
// contract grows. SCOPE_MR_REVIEW is in the contract but not yet implemented
// here, and the server rejects it like any unknown scope, so it is left out.
func AllScopes() []ermite.Scope {
	var scopes []ermite.Scope
	for n := range ermite.Scope_name {
		if s := ermite.Scope(n); s != ermite.Scope_SCOPE_UNSPECIFIED && s != ermite.Scope_SCOPE_MR_REVIEW {
			scopes = append(scopes, s)
		}
	}
	slices.Sort(scopes)
	return scopes
}

// ParseScope reads a scope as users type it, for example "repo:read".
func ParseScope(name string) (ermite.Scope, error) {
	want := strings.ToLower(strings.TrimSpace(name))
	for _, s := range AllScopes() {
		if ScopeName(s) == want {
			return s, nil
		}
	}
	return 0, fmt.Errorf("unknown scope %q: use one of %s", name, strings.Join(ScopeNames(AllScopes()), ", "))
}

// ScopeNames maps ScopeName over scopes.
func ScopeNames(scopes []ermite.Scope) []string {
	names := make([]string, len(scopes))
	for i, s := range scopes {
		names[i] = ScopeName(s)
	}
	return names
}

// PresetScopes expands a preset to its scopes.
func PresetScopes(name string) ([]ermite.Scope, error) {
	switch strings.ToLower(name) {
	case presetCLI:
		return AllScopes(), nil
	case presetAgent:
		return []ermite.Scope{ermite.Scope_SCOPE_REPO_WRITE, ermite.Scope_SCOPE_MR_WRITE}, nil
	case presetCI:
		return []ermite.Scope{ermite.Scope_SCOPE_REPO_READ}, nil
	case presetRunner:
		return []ermite.Scope{ermite.Scope_SCOPE_RUNNER_REGISTER}, nil
	default:
		return nil, fmt.Errorf("unknown preset %q: use one of %s", name, strings.Join(PresetNames, ", "))
	}
}

// MergeScopes merges a preset and explicit scopes into a sorted, unique list.
// Neither given is an error: a token's power is never chosen by default.
func MergeScopes(preset string, scopeFlags []string) ([]ermite.Scope, error) {
	var scopes []ermite.Scope
	if preset != "" {
		s, err := PresetScopes(preset)
		if err != nil {
			return nil, err
		}
		scopes = append(scopes, s...)
	}
	for _, f := range scopeFlags {
		s, err := ParseScope(f)
		if err != nil {
			return nil, err
		}
		scopes = append(scopes, s)
	}
	if len(scopes) == 0 {
		return nil, fmt.Errorf("choose what the token may do: --preset %s, or one or more --scope", strings.Join(PresetNames, "|"))
	}
	slices.Sort(scopes)
	return slices.Compact(scopes), nil
}

// ParseExpiry reads a lifetime such as "30d", "2w", "12h", "90m" or "1h30m"
// (days and weeks are 24 h and 7 d), or an absolute date, "2027-01-31" (end of
// that day, UTC) or an RFC 3339 instant. The result must lie in the future.
func ParseExpiry(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	bad := fmt.Errorf("invalid --expires %q: use a duration like 30d, 12h or 2w, or a date like 2027-01-31", s)
	var at time.Time
	switch {
	case s == "":
		return time.Time{}, bad
	case strings.HasSuffix(s, "d") || strings.HasSuffix(s, "w"):
		n, err := strconv.Atoi(s[:len(s)-1])
		if err != nil || n <= 0 {
			return time.Time{}, bad
		}
		unit := 24 * time.Hour
		if strings.HasSuffix(s, "w") {
			unit *= 7
		}
		if n > 100*365 {
			return time.Time{}, bad
		}
		at = now.Add(time.Duration(n) * unit)
	default:
		if d, err := time.ParseDuration(s); err == nil {
			at = now.Add(d)
		} else if t, err := time.Parse(time.RFC3339, s); err == nil {
			at = t
		} else if t, err := time.Parse(time.DateOnly, s); err == nil {
			at = t.Add(24*time.Hour - time.Second)
		} else {
			return time.Time{}, bad
		}
	}
	if !at.After(now) {
		return time.Time{}, fmt.Errorf("invalid --expires %q: it is in the past", s)
	}
	return at.UTC().Truncate(time.Second), nil
}
