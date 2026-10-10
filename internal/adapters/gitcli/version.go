// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
)

// Version is a git release number.
type Version struct{ Major, Minor, Patch int }

// MinVersion is the oldest supported git.
var MinVersion = Version{Major: 2, Minor: 45}

// String returns "major.minor.patch".
func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Less reports whether v is older than o.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

var versionRE = regexp.MustCompile(`^git version (\d+)\.(\d+)(?:\.(\d+))?`)

// ParseVersion parses the output of `git version`, such as
// "git version 2.45.0", "git version 2.39.5 (Apple Git-154)" or
// "git version 2.46.0.rc1".
func ParseVersion(out string) (Version, error) {
	m := versionRE.FindStringSubmatch(out)
	if m == nil {
		return Version{}, fmt.Errorf("unrecognized git version output %q", out)
	}
	var v Version
	for i, dst := range []*int{&v.Major, &v.Minor, &v.Patch} {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return Version{}, fmt.Errorf("unrecognized git version output %q: %w", out, err)
		}
		*dst = n
	}
	return v, nil
}

// CheckVersion runs `git version` and fails if git is missing, unreadable or
// older than MinVersion. It returns the version found.
func (g *Git) CheckVersion(ctx context.Context) (Version, error) {
	out, err := g.run(ctx, "version")
	if err != nil {
		return Version{}, fmt.Errorf("run %q: %w", g.bin, err)
	}
	v, err := ParseVersion(out)
	if err != nil {
		return Version{}, err
	}
	if v.Less(MinVersion) {
		return v, fmt.Errorf("git %s is older than the required %s", v, MinVersion)
	}
	return v, nil
}
