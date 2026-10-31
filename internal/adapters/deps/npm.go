// SPDX-License-Identifier: AGPL-3.0-or-later

package deps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
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

func (NPM) Manifests(paths domain.PathList) []domain.TreePath {
	var out []domain.TreePath
	for _, p := range paths.Paths {
		skip := false
		for _, part := range strings.Split(p, "/") {
			if part == "node_modules" {
				skip = true
			}
		}
		if skip {
			continue
		}
		base := path.Base(p)
		if base != "package.json" && base != "pnpm-workspace.yaml" && base != ".yarnrc.yml" && !npmLockfiles[base] {
			continue
		}
		if tp, err := domain.ParseTreePath(p); err == nil {
			out = append(out, tp)
		}
	}
	return out
}

func npmDependencies(content []byte) (map[string]string, error) {
	if _, err := npmJSON(content); err != nil {
		return nil, err
	}
	var m npmManifest
	if err := json.Unmarshal(content, &m); err != nil {
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

// Detect skips conflicting and unsupported dependency ranges.
//
//hexcheck:ignore suspicious-business-logic-in-adapter parses package syntax and resolves versions through the registry port; update policy stays in domain.PickUpdates
func (NPM) Detect(ctx context.Context, files map[domain.TreePath][]byte, reg ports.DepsRegistry) ([]domain.DepCandidate, error) {
	var out []domain.DepCandidate
	for _, p := range filePaths(files) {
		if path.Base(p.String()) != "package.json" {
			continue
		}
		deps, err := npmDependencies(files[p])
		if err != nil {
			return nil, err
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
			versions, err := reg.NPMVersions(ctx, name, current)
			if errors.Is(err, domain.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			out = append(out, domain.DepCandidate{Ecosystem: domain.EcosystemNPM, Directory: moduleDir(p.String()), Name: name, Current: current, Versions: versions})
		}
	}
	return out, nil
}

// RunnerDetails derives immutable job inputs from the manifests at its commit.
func (NPM) RunnerDetails(files map[domain.TreePath][]byte, ups []domain.DepUpdate) ([]domain.CIDepsUpdate, string, error) {
	roots, err := npmRoots(files)
	if err != nil {
		return nil, "", err
	}
	var details []domain.CIDepsUpdate
	node := 0
	for _, u := range ups {
		root, ok := roots[u.Directory]
		if !ok {
			return nil, "", fmt.Errorf("%w: npm package missing", domain.ErrInvalid)
		}
		deps, err := npmDependencies(npmFile(files, u.Directory, "package.json"))
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
		if n > node {
			node = n
		}
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

// Validate accepts exactly the requested range edits; lockfiles are opaque.
func (n NPM) Validate(before, after map[domain.TreePath][]byte, ups []domain.DepUpdate) ([]domain.DepSideEffect, error) {
	details, _, err := n.RunnerDetails(before, ups)
	if err != nil {
		return nil, err
	}
	roots, err := npmRoots(before)
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
		root := roots[u.Directory]
		if root.Lockfile != "" {
			allowedLocks[root.Lockfile] = true
		}
	}
	for p, content := range after {
		if want, ok := expected[p]; ok {
			if !bytes.Equal(want, content) {
				return nil, fmt.Errorf("%w: unexpected package.json edit %s", domain.ErrInvalid, p)
			}
		} else if !allowedLocks[p.String()] || before[p] == nil {
			return nil, fmt.Errorf("%w: unexpected npm report path %s", domain.ErrInvalid, p)
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
