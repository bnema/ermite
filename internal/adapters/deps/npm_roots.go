// SPDX-License-Identifier: AGPL-3.0-or-later

package deps

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
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

type npmRoot struct{ Directory, Manager, Version, NodeVersion, Lockfile string }

var npmManagerDefaults = map[string]string{"npm": "", "pnpm9": "9.15.0", "pnpm6": "8.15.9", "yarn1": "1.22.22", "yarn3": "3.8.7", "yarn4": "4.5.3", "bun": "1.2.0"}
var npmPackageManagerRE = regexp.MustCompile(`^(npm|pnpm|yarn|bun)@([0-9]+\.[0-9]+\.[0-9]+(?:\+sha(?:1|224|256|512)\.[0-9a-f]+)?)$`)
var npmNodeRE = regexp.MustCompile(`^(?:>=|\^|~)?([0-9]+)(?:\.x|\.[0-9]+(?:\.[0-9]+)?)?$`)

func npmNode(spec string) string {
	m := npmNodeRE.FindStringSubmatch(strings.TrimSpace(spec))
	if m == nil {
		return "22"
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return "22"
	}
	if n < 18 {
		n = 18
	}
	if n > 24 {
		n = 24
	}
	return strconv.Itoa(n)
}

func npmFile(files map[domain.TreePath][]byte, dir, name string) []byte {
	p, err := domain.ParseTreePath(path.Join(dir, name))
	if err != nil {
		return nil
	}
	return files[p]
}

func npmManager(files map[domain.TreePath][]byte, dir string, m npmManifest) (npmRoot, error) {
	root := npmRoot{Directory: dir, Manager: "npm", NodeVersion: npmNode(m.Engines.Node)}
	// Lockfile priority is deterministic even when a repository contains several.
	for _, name := range []string{"bun.lock", "bun.lockb", "pnpm-lock.yaml", "yarn.lock", "package-lock.json", "npm-shrinkwrap.json"} {
		if npmFile(files, dir, name) != nil {
			root.Lockfile = path.Join(dir, name)
			break
		}
	}
	switch path.Base(root.Lockfile) {
	case "bun.lock", "bun.lockb":
		root.Manager = "bun"
		root.Version = npmManagerDefaults["bun"]
	case "pnpm-lock.yaml":
		root.Manager = "pnpm"
		root.Version = npmManagerDefaults["pnpm9"]
		var lock struct {
			Version string `yaml:"lockfileVersion"`
		}
		if err := yaml.Unmarshal(npmFile(files, dir, "pnpm-lock.yaml"), &lock); err != nil {
			return root, err
		}
		if strings.HasPrefix(lock.Version, "6") {
			root.Version = npmManagerDefaults["pnpm6"]
		}
	case "yarn.lock":
		root.Manager = "yarn"
		root.Version = npmManagerDefaults["yarn1"]
		lock := npmFile(files, dir, "yarn.lock")
		if npmFile(files, dir, ".yarnrc.yml") != nil || strings.Contains(string(lock), "__metadata:") {
			root.Version = npmManagerDefaults["yarn3"]
			var meta struct {
				Metadata struct {
					Version int `yaml:"version"`
				} `yaml:"__metadata"`
			}
			if err := yaml.Unmarshal(lock, &meta); err == nil && meta.Metadata.Version >= 8 {
				root.Version = npmManagerDefaults["yarn4"]
			}
		}
	}
	if m.PackageManager != "" {
		match := npmPackageManagerRE.FindStringSubmatch(m.PackageManager)
		if match == nil {
			return root, fmt.Errorf("%w: unsupported packageManager %q", domain.ErrInvalid, m.PackageManager)
		}
		root.Manager = match[1]
		root.Version = match[2]
	}
	return root, nil
}

func npmWorkspaceMatch(pattern, rel string) bool {
	a, b := strings.Split(strings.TrimSuffix(pattern, "/"), "/"), strings.Split(rel, "/")
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

// npmRoots maps each package directory to its nearest matching workspace root.
func npmRoots(files map[domain.TreePath][]byte) (map[string]npmRoot, error) {
	manifests := map[string]npmManifest{}
	patterns := map[string][]string{}
	for _, p := range filePaths(files) {
		if path.Base(p.String()) != "package.json" {
			continue
		}
		var m npmManifest
		if err := json.Unmarshal(files[p], &m); err != nil {
			return nil, err
		}
		dir := moduleDir(p.String())
		manifests[dir] = m
		if len(m.Workspaces) > 0 && string(m.Workspaces) != "null" {
			var list []string
			if err := json.Unmarshal(m.Workspaces, &list); err != nil {
				var object struct {
					Packages []string `json:"packages"`
				}
				if err := json.Unmarshal(m.Workspaces, &object); err != nil {
					return nil, err
				}
				list = object.Packages
			}
			patterns[dir] = list
		}
	}
	for _, p := range filePaths(files) {
		if path.Base(p.String()) != "pnpm-workspace.yaml" {
			continue
		}
		var w struct {
			Packages []string `yaml:"packages"`
		}
		if err := yaml.Unmarshal(files[p], &w); err != nil {
			return nil, err
		}
		patterns[moduleDir(p.String())] = w.Packages
	}
	var roots []string
	for dir := range patterns {
		roots = append(roots, dir)
	}
	sort.Slice(roots, func(i, j int) bool { return len(roots[i]) > len(roots[j]) })
	out := map[string]npmRoot{}
	for dir, m := range manifests {
		owner := dir
		for _, candidate := range roots {
			if candidate == dir {
				owner = dir
				break
			}
			prefix := candidate + "/"
			if candidate == "" {
				prefix = ""
			}
			if !strings.HasPrefix(dir, prefix) {
				continue
			}
			rel := strings.TrimPrefix(dir, prefix)
			matched := false
			for _, pattern := range patterns[candidate] {
				if npmWorkspaceMatch(pattern, rel) {
					matched = true
				}
			}
			if matched {
				owner = candidate
				break
			}
		}
		if owner != dir {
			m = manifests[owner]
		}
		root, err := npmManager(files, owner, m)
		if err != nil {
			return nil, err
		}
		out[dir] = root
	}
	return out, nil
}
