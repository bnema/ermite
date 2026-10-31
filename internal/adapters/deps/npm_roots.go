// SPDX-License-Identifier: AGPL-3.0-or-later

package deps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"go.yaml.in/yaml/v3"
)

type npmManifest struct {
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Workspaces      json.RawMessage   `json:"workspaces"`
	PackageManager  string            `json:"packageManager"`
	Engines         struct {
		Node string `json:"node"`
	} `json:"engines"`
}

// parseNPMManifest decodes a package.json, refusing invalid JSON, duplicate
// keys and values of the wrong type.
func parseNPMManifest(content []byte) (npmManifest, error) {
	var m npmManifest
	if _, err := npmJSON(content); err != nil {
		return m, err
	}
	d := json.NewDecoder(bytes.NewReader(content))
	d.UseNumber()
	if err := d.Decode(&m); err != nil {
		return m, fmt.Errorf("%w: package.json: %v", domain.ErrInvalid, err)
	}
	return m, nil
}

type npmRoot struct{ Directory, Manager, Version, NodeVersion, Lockfile string }

// npmManagerDefaults maps a lockfile format to the manager version that
// writes it when packageManager is absent.
var npmManagerDefaults = map[string]string{
	"npm":   "",        // the image's npm
	"pnpm9": "9.15.0",  // lockfileVersion 9.x
	"pnpm6": "8.15.9",  // lockfileVersion 6.x (pnpm 8)
	"yarn1": "1.22.22", // yarn classic
	"yarn3": "3.8.7",   // Berry, __metadata.version < 8
	"yarn4": "4.5.3",   // Berry, __metadata.version >= 8
	"bun":   "1.2.0",
}

// npmLockOrder is the lockfile priority of a directory holding several.
var npmLockOrder = []string{"bun.lock", "bun.lockb", "pnpm-lock.yaml", "yarn.lock", "npm-shrinkwrap.json", "package-lock.json"}

var npmLockManager = map[string]string{"bun.lock": "bun", "bun.lockb": "bun", "pnpm-lock.yaml": "pnpm", "yarn.lock": "yarn", "npm-shrinkwrap.json": "npm", "package-lock.json": "npm"}

var npmPackageManagerRE = regexp.MustCompile(`^(npm|pnpm|yarn|bun)@([0-9]+\.[0-9]+\.[0-9]+(?:\+sha(?:1|224|256|512)\.[0-9a-f]+)?)$`)
var npmNodeRE = regexp.MustCompile(`^(?:>=|\^|~)?([0-9]+)(?:\.x|\.[0-9]+(?:\.[0-9]+)?)?$`)

// Lockfile versions sit at the top of the file: a header is enough.
var pnpmLockVersionRE = regexp.MustCompile(`(?m)^lockfileVersion:[ \t]*['"]?([0-9]+)(?:\.[0-9]+)?['"]?[ \t]*$`)
var yarnMetadataRE = regexp.MustCompile(`(?m)^__metadata:[ \t]*\r?\n(?:[ \t]+.*\r?\n)*?[ \t]+version:[ \t]*['"]?([0-9]+)`)

func npmNode(spec string) string {
	m := npmNodeRE.FindStringSubmatch(strings.TrimSpace(spec))
	if m == nil {
		return "22"
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return "22"
	}
	return strconv.Itoa(min(max(n, 18), 24))
}

func npmFile(files map[domain.TreePath][]byte, dir, name string) []byte {
	p, err := domain.ParseTreePath(path.Join(dir, name))
	if err != nil {
		return nil
	}
	return files[p]
}

// npmManager derives the manager of a root from its packageManager field and
// the lockfile headers. A packageManager naming another tool than the
// existing lockfile, or a lockfile format without a known writer, is refused.
func npmManager(files map[domain.TreePath][]byte, dir string, m npmManifest) (npmRoot, error) {
	root := npmRoot{Directory: dir, Manager: "npm", NodeVersion: npmNode(m.Engines.Node)}
	var pm []string
	if m.PackageManager != "" {
		pm = npmPackageManagerRE.FindStringSubmatch(m.PackageManager)
		if pm == nil {
			return root, fmt.Errorf("%w: unsupported packageManager %q", domain.ErrInvalid, m.PackageManager)
		}
		// npm 6 and older write lockfile v1 and ignore workspaces.
		if major, _, _ := strings.Cut(pm[2], "."); pm[1] == "npm" && len(major) == 1 && major < "7" {
			return root, fmt.Errorf("%w: packageManager %s is older than npm 7", domain.ErrInvalid, m.PackageManager)
		}
	}
	for _, name := range npmLockOrder {
		if npmFile(files, dir, name) == nil {
			continue
		}
		if root.Lockfile == "" || (pm != nil && npmLockManager[name] == pm[1] && npmLockManager[path.Base(root.Lockfile)] != pm[1]) {
			root.Lockfile = path.Join(dir, name)
		}
	}
	lock := npmFile(files, dir, path.Base(root.Lockfile))
	switch path.Base(root.Lockfile) {
	case "bun.lock", "bun.lockb":
		root.Manager, root.Version = "bun", npmManagerDefaults["bun"]
	case "pnpm-lock.yaml":
		root.Manager = "pnpm"
		v := pnpmLockVersionRE.FindSubmatch(lock)
		switch {
		case v != nil && string(v[1]) == "9":
			root.Version = npmManagerDefaults["pnpm9"]
		case v != nil && string(v[1]) == "6":
			root.Version = npmManagerDefaults["pnpm6"]
		case pm == nil:
			return root, fmt.Errorf("%w: unsupported pnpm lockfile version in %s", domain.ErrInvalid, root.Lockfile)
		}
	case "yarn.lock":
		root.Manager, root.Version = "yarn", npmManagerDefaults["yarn1"]
		meta := yarnMetadataRE.FindSubmatch(lock)
		if meta != nil || npmFile(files, dir, ".yarnrc.yml") != nil {
			root.Version = npmManagerDefaults["yarn3"]
			if meta != nil {
				if n, err := strconv.Atoi(string(meta[1])); err == nil && n >= 8 {
					root.Version = npmManagerDefaults["yarn4"]
				}
			}
		}
	}
	if pm != nil {
		if root.Lockfile != "" && root.Manager != pm[1] {
			return root, fmt.Errorf("%w: packageManager %s does not write %s", domain.ErrInvalid, pm[1], root.Lockfile)
		}
		root.Manager, root.Version = pm[1], pm[2]
	}
	return root, nil
}

// npmWorkspaceMatch matches rel against workspace patterns: per path segment
// with path.Match, a trailing /** matching any depth, and !pattern excluding.
func npmWorkspaceMatch(patterns []string, rel string) bool {
	matched := false
	for _, pattern := range patterns {
		negated := strings.HasPrefix(pattern, "!")
		if npmPatternMatch(strings.TrimPrefix(pattern, "!"), rel) {
			if negated {
				return false
			}
			matched = true
		}
	}
	return matched
}

func npmPatternMatch(pattern, rel string) bool {
	a, b := strings.Split(path.Clean(pattern), "/"), strings.Split(rel, "/")
	recursive := len(a) > 0 && a[len(a)-1] == "**"
	if recursive {
		a = a[:len(a)-1]
	}
	if (!recursive && len(a) != len(b)) || len(b) < len(a) {
		return false
	}
	for i, p := range a {
		ok, err := path.Match(p, b[i])
		if err != nil || !ok {
			return false
		}
	}
	return true
}

// npmWorkspaces returns the workspace patterns declared in dir, nil when dir
// is not a workspace root.
func npmWorkspaces(files map[domain.TreePath][]byte, dir string) ([]string, bool, error) {
	var patterns []string
	root := false
	if content := npmFile(files, dir, "package.json"); content != nil {
		m, err := parseNPMManifest(content)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", path.Join(dir, "package.json"), err)
		}
		if len(m.Workspaces) > 0 && string(m.Workspaces) != "null" {
			var list []string
			if err := json.Unmarshal(m.Workspaces, &list); err != nil {
				var object struct {
					Packages []string `json:"packages"`
				}
				if err := json.Unmarshal(m.Workspaces, &object); err != nil {
					return nil, false, fmt.Errorf("%w: workspaces in %s", domain.ErrInvalid, path.Join(dir, "package.json"))
				}
				list = object.Packages
			}
			patterns, root = append(patterns, list...), true
		}
	}
	if content := npmFile(files, dir, "pnpm-workspace.yaml"); content != nil {
		var w struct {
			Packages []string `yaml:"packages"`
		}
		if err := yaml.Unmarshal(content, &w); err != nil {
			return nil, false, fmt.Errorf("%w: %s: %v", domain.ErrInvalid, path.Join(dir, "pnpm-workspace.yaml"), err)
		}
		patterns, root = append(patterns, w.Packages...), true
	}
	return patterns, root, nil
}

// npmRootOf finds the root of the package in dir: dir itself when it is a
// workspace root, else the nearest ancestor whose workspaces match it, else
// dir. It needs only the files of dir and its ancestors.
func npmRootOf(files map[domain.TreePath][]byte, dir string) (npmRoot, error) {
	owner := dir
	for candidate := dir; ; candidate = moduleDir(candidate) {
		patterns, isRoot, err := npmWorkspaces(files, candidate)
		if err != nil {
			return npmRoot{}, err
		}
		if isRoot && (candidate == dir || npmWorkspaceMatch(patterns, strings.TrimPrefix(dir, prefixOf(candidate)))) {
			owner = candidate
			break
		}
		if candidate == "" {
			break
		}
	}
	var m npmManifest
	if content := npmFile(files, owner, "package.json"); content != nil {
		var err error
		if m, err = parseNPMManifest(content); err != nil {
			return npmRoot{}, err
		}
	}
	return npmManager(files, owner, m)
}

func prefixOf(dir string) string {
	if dir == "" {
		return ""
	}
	return dir + "/"
}

// npmAncestors returns dir and its ancestors, the repository root last.
func npmAncestors(dir string) []string {
	out := []string{dir}
	for dir != "" {
		dir = moduleDir(dir)
		out = append(out, dir)
	}
	return out
}
