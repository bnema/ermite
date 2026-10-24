// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

var goVersionPattern = regexp.MustCompile(`^1\.\d+(\.\d+)?$`)

// GoVersionOf returns the Go version a module needs to be built: the highest
// of the go and toolchain directives of its go.mod, without the "go" prefix.
// A toolchain of "default" or "local" counts for nothing. A result that is not
// 1.N or 1.N.P (a release candidate, say) is ErrInvalid.
func GoVersionOf(goMod []byte) (string, error) {
	f, err := modfile.Parse("go.mod", goMod, nil)
	if err != nil {
		return "", fmt.Errorf("%w: go.mod: %v", ErrInvalid, err)
	}
	var best string
	consider := func(v string) {
		v = strings.TrimPrefix(v, "go")
		if v == "" || v == "default" || v == "local" {
			return
		}
		if best == "" || !semver.IsValid("v"+v) || (semver.IsValid("v"+best) && semver.Compare("v"+v, "v"+best) > 0) {
			best = v
		}
	}
	if f.Go != nil {
		consider(f.Go.Version)
	}
	if f.Toolchain != nil {
		consider(f.Toolchain.Name)
	}
	if best == "" {
		return "", fmt.Errorf("%w: go.mod has no go directive", ErrInvalid)
	}
	if !goVersionPattern.MatchString(best) {
		return "", fmt.Errorf("%w: unsupported Go version %q in go.mod", ErrInvalid, best)
	}
	return best, nil
}
