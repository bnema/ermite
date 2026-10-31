// SPDX-License-Identifier: AGPL-3.0-or-later

package deps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"github.com/bnema/zerowrap"
)

// NPM detects JavaScript dependencies and verifies runner manifest edits.
type NPM struct{}

var _ ports.DepsEcosystem = NPM{}

func (NPM) Name() domain.DepsEcosystem { return domain.EcosystemNPM }
func (NPM) ApplyInServer() bool        { return false }
func (NPM) Apply(map[domain.TreePath][]byte, []domain.DepUpdate) (map[domain.TreePath][]byte, error) {
	return nil, fmt.Errorf("%w: npm updates require a runner", domain.ErrPreconditionFailed)
}

var npmLockfiles = map[string]bool{"package-lock.json": true, "npm-shrinkwrap.json": true, "pnpm-lock.yaml": true, "yarn.lock": true, "bun.lock": true, "bun.lockb": true}

// npmRegistryBase is the only registry new package-lock.json resolutions may
// point to; the runner installs from the public registry.
const npmRegistryBase = "https://registry.npmjs.org/"

// The runner's input rules: names and ranges it refuses are never proposed.
var (
	npmNameRE  = regexp.MustCompile(`^(@[a-z0-9~-][a-z0-9._~-]*/)?[a-z0-9~-][a-z0-9._~-]*$`)
	npmRangeRE = regexp.MustCompile(`^(\^|~|>=|=)?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
)

// npmSkippedDir reports directories whose manifests are not real packages.
func npmSkippedDir(p string) bool {
	for _, part := range strings.Split(p, "/") {
		switch part {
		case "node_modules", "testdata", "fixtures", "__fixtures__":
			return true
		}
	}
	return false
}

// Manifests reads package.json and pnpm-workspace.yaml in full; lockfiles
// and .yarnrc.yml only matter by presence and version header.
func (NPM) Manifests(paths domain.PathList) []ports.DepsInput {
	var out []ports.DepsInput
	for _, p := range paths.Paths {
		if npmSkippedDir(p) {
			continue
		}
		base := path.Base(p)
		header := npmLockfiles[base] || base == ".yarnrc.yml"
		if base != "package.json" && base != "pnpm-workspace.yaml" && !header {
			continue
		}
		if tp, err := domain.ParseTreePath(p); err == nil {
			out = append(out, ports.DepsInput{Path: tp, Header: header})
		}
	}
	return out
}

// Inputs reads the package.json of each update directory and, in it and its
// ancestors, the files that decide its workspace root and manager.
func (NPM) Inputs(ups []domain.DepUpdate, _ []domain.TreePath) []ports.DepsInput {
	var out []ports.DepsInput
	add := func(p string, in ports.DepsInput) {
		if tp, err := domain.ParseTreePath(p); err == nil {
			in.Path = tp
			out = append(out, in)
		}
	}
	for _, u := range ups {
		if u.Ecosystem != domain.EcosystemNPM {
			continue
		}
		add(path.Join(u.Directory, "package.json"), ports.DepsInput{})
		for _, d := range npmAncestors(u.Directory) {
			if d != u.Directory {
				add(path.Join(d, "package.json"), ports.DepsInput{Optional: true})
			}
			add(path.Join(d, "pnpm-workspace.yaml"), ports.DepsInput{Optional: true})
			add(path.Join(d, ".yarnrc.yml"), ports.DepsInput{Optional: true, Header: true})
			for _, lock := range npmLockOrder {
				add(path.Join(d, lock), ports.DepsInput{Optional: true, Header: true})
			}
		}
	}
	return out
}

// npmDependencies returns the dependency specs of a package.json; a name in
// both sections with different specs maps to "".
func npmDependencies(content []byte) (map[string]string, error) {
	m, err := parseNPMManifest(content)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for name, spec := range m.Dependencies {
		out[name] = spec
	}
	for name, spec := range m.DevDependencies {
		if prev, ok := out[name]; ok && prev != spec {
			out[name] = ""
		} else {
			out[name] = spec
		}
	}
	return out, nil
}

// Detect skips conflicting and unsupported dependency ranges, invalid
// manifests, packages whose root cannot be updated and registry failures.
//
//hexcheck:ignore suspicious-business-logic-in-adapter parses package syntax and resolves versions through the registry port; update policy stays in domain.PickUpdates
func (NPM) Detect(ctx context.Context, files map[domain.TreePath][]byte, reg ports.DepsRegistry) ([]domain.DepCandidate, error) {
	log := zerowrap.FromCtx(ctx)
	var out []domain.DepCandidate
	for _, p := range filePaths(files) {
		if path.Base(p.String()) != "package.json" {
			continue
		}
		deps, err := npmDependencies(files[p])
		if err != nil {
			log.Warn().Err(err).Str("path", p.String()).Msg("bernard: invalid package.json skipped")
			continue
		}
		if len(deps) == 0 {
			continue
		}
		dir := moduleDir(p.String())
		if _, err := npmRootOf(files, dir); err != nil {
			log.Warn().Err(err).Str("path", p.String()).Msg("bernard: npm package without a usable root skipped")
			continue
		}
		var names []string
		for name := range deps {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			_, current, ok := npmSpec(deps[name])
			if !ok {
				continue
			}
			if !npmNameRE.MatchString(name) {
				log.Debug().Str("path", p.String()).Str("name", name).Msg("bernard: npm name the runner refuses skipped")
				continue
			}
			versions, err := reg.NPMVersions(ctx, name, current)
			if errors.Is(err, domain.ErrNotFound) {
				continue
			}
			if err != nil {
				if cerr := ctx.Err(); cerr != nil {
					return nil, cerr
				}
				log.Warn().Err(err).Str("path", p.String()).Str("name", name).Msg("bernard: npm registry lookup failed, package skipped")
				continue
			}
			kept := versions[:0:0]
			for _, v := range versions {
				if npmRangeRE.MatchString(strings.TrimPrefix(v.Version, "v")) {
					kept = append(kept, v)
				} else {
					log.Debug().Str("name", name).Str("version", v.Version).Msg("bernard: npm version the runner refuses skipped")
				}
			}
			out = append(out, domain.DepCandidate{Ecosystem: domain.EcosystemNPM, Directory: dir, Name: name, Current: current, Versions: kept})
		}
	}
	return out, nil
}

// RunnerDetails derives immutable job inputs from the manifests at its commit.
func (NPM) RunnerDetails(files map[domain.TreePath][]byte, ups []domain.DepUpdate) ([]domain.CIDepsUpdate, string, error) {
	var details []domain.CIDepsUpdate
	node := 0
	for _, u := range ups {
		content := npmFile(files, u.Directory, "package.json")
		if content == nil {
			return nil, "", fmt.Errorf("%w: npm package missing", domain.ErrInvalid)
		}
		root, err := npmRootOf(files, u.Directory)
		if err != nil {
			return nil, "", err
		}
		deps, err := npmDependencies(content)
		if err != nil {
			return nil, "", err
		}
		_, current, ok := npmSpec(deps[u.Name])
		if !ok || current != u.Current {
			return nil, "", fmt.Errorf("%w: npm dependency changed", domain.ErrInvalid)
		}
		spec, err := npmRange(deps[u.Name], u.Version)
		if err != nil {
			return nil, "", err
		}
		details = append(details, domain.CIDepsUpdate{Ref: u.DepRef, Range: spec, Manager: root.Manager, ManagerVersion: root.Version, Root: root.Directory})
		n, _ := strconv.Atoi(root.NodeVersion)
		node = max(node, n)
	}
	return details, strconv.Itoa(node), nil
}

// ReportAllowed admits candidates; Validate checks snapshot-dependent root and
// pre-existing-lockfile restrictions against the original manifests.
func (NPM) ReportAllowed(ups []domain.DepUpdate, p domain.TreePath) bool {
	for _, part := range strings.Split(p.String(), "/") {
		if part == "node_modules" {
			return false
		}
	}
	for _, u := range ups {
		if u.Ecosystem != domain.EcosystemNPM {
			continue
		}
		if p.String() == path.Join(u.Directory, "package.json") {
			return true
		}
		d := moduleDir(p.String())
		if npmLockfiles[path.Base(p.String())] && (d == u.Directory || d == "" || strings.HasPrefix(u.Directory, d+"/")) {
			return true
		}
	}
	return false
}

// Validate accepts exactly the requested range edits and the root's existing
// lockfile. package-lock.json and npm-shrinkwrap.json may only gain
// resolutions from the public registry; other lockfiles are opaque.
func (n NPM) Validate(before, after map[domain.TreePath][]byte, ups []domain.DepUpdate) ([]domain.DepSideEffect, error) {
	details, _, err := n.RunnerDetails(before, ups)
	if err != nil {
		return nil, err
	}
	expected := map[domain.TreePath][]byte{}
	allowedLocks := map[string]bool{}
	for i, u := range ups {
		p, err := domain.ParseTreePath(path.Join(u.Directory, "package.json"))
		if err != nil {
			return nil, err
		}
		content, ok := expected[p]
		if !ok {
			content = before[p]
		}
		deps, err := npmDependencies(content)
		if err != nil {
			return nil, err
		}
		content, err = npmEditSpec(content, u.Name, deps[u.Name], details[i].Range)
		if err != nil {
			return nil, err
		}
		expected[p] = content
		root, err := npmRootOf(before, u.Directory)
		if err != nil {
			return nil, err
		}
		if root.Lockfile != "" {
			allowedLocks[root.Lockfile] = true
		}
	}
	for p, content := range after {
		if want, ok := expected[p]; ok {
			if !bytes.Equal(want, content) {
				return nil, fmt.Errorf("%w: unexpected package.json edit %s", domain.ErrInvalid, p)
			}
			continue
		}
		if !allowedLocks[p.String()] || before[p] == nil {
			return nil, fmt.Errorf("%w: unexpected npm report path %s", domain.ErrInvalid, p)
		}
		if base := path.Base(p.String()); base == "package-lock.json" || base == "npm-shrinkwrap.json" {
			if err := checkNPMLock(before[p], content); err != nil {
				return nil, fmt.Errorf("%w: %s: %v", domain.ErrInvalid, p, err)
			}
		}
	}
	for p, want := range expected {
		got, ok := after[p]
		if !ok {
			got = before[p]
		}
		if !bytes.Equal(want, got) {
			return nil, fmt.Errorf("%w: requested npm update missing in %s", domain.ErrInvalid, p)
		}
	}
	return nil, nil
}

// checkNPMLock refuses a package-lock.json whose new or changed resolutions
// leave the public registry (git, file, link, tarball URLs elsewhere). An
// original that does not parse (a header of a larger file) counts as empty,
// which only makes the check stricter.
func checkNPMLock(before, after []byte) error {
	old := map[string]bool{}
	if v, err := decodeNPMLock(before); err == nil {
		collectNPMResolutions(v, func(s string) { old[s] = true })
	}
	v, err := decodeNPMLock(after)
	if err != nil {
		return fmt.Errorf("invalid lockfile: %v", err)
	}
	var bad string
	collectNPMResolutions(v, func(s string) {
		if bad == "" && !old[s] && !strings.HasPrefix(s, npmRegistryBase) {
			bad = s
		}
	})
	if bad != "" {
		return fmt.Errorf("resolution outside the registry: %q", bad)
	}
	return nil
}

func decodeNPMLock(content []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(content))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// collectNPMResolutions calls fn with every "resolved" string, and every
// "version" string that is a URL or a protocol spec (lockfile v1).
func collectNPMResolutions(v any, fn func(string)) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if s, ok := child.(string); ok && (k == "resolved" || (k == "version" && strings.Contains(s, ":"))) {
				fn(s)
				continue
			}
			collectNPMResolutions(child, fn)
		}
	case []any:
		for _, child := range t {
			collectNPMResolutions(child, fn)
		}
	}
}
