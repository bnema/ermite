// SPDX-License-Identifier: AGPL-3.0-or-later

package deps

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"reflect"
	"sort"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// GoMod detects Go module requirements and validates runner reports.
type GoMod struct{}

var _ ports.DepsEcosystem = GoMod{}

// Name identifies the ecosystem.
func (GoMod) Name() domain.DepsEcosystem { return domain.EcosystemGoMod }

// ApplyInServer reports that Go updates require a runner.
func (GoMod) ApplyInServer() bool { return false }

// Apply refuses server-side Go edits.
func (GoMod) Apply(map[domain.TreePath][]byte, []domain.DepUpdate) (map[domain.TreePath][]byte, error) {
	return nil, fmt.Errorf("%w: Go updates require a runner", domain.ErrPreconditionFailed)
}

// Manifests selects non-vendored modules outside vendor and testdata trees.
func (GoMod) Manifests(paths domain.PathList) []domain.TreePath {
	all := map[string]bool{}
	for _, p := range paths.Paths {
		all[p] = true
	}
	var out []domain.TreePath
	for _, p := range paths.Paths {
		if path.Base(p) != "go.mod" {
			continue
		}
		skip := false
		for _, part := range strings.Split(p, "/") {
			if part == "vendor" || part == "testdata" {
				skip = true
			}
		}
		if skip || all[path.Join(path.Dir(p), "vendor/modules.txt")] {
			continue
		}
		if tp, err := domain.ParseTreePath(p); err == nil {
			out = append(out, tp)
		}
	}
	return out
}

func moduleDir(p string) string {
	d := path.Dir(p)
	if d == "." {
		return ""
	}
	return d
}

// Detect reads requirements, skipping replaced and unresolvable modules.
//
//hexcheck:ignore suspicious-business-logic-in-adapter parses module syntax and resolves versions through the registry port; update policy stays in domain.PickUpdate
func (GoMod) Detect(ctx context.Context, files map[domain.TreePath][]byte, reg ports.DepsRegistry) ([]domain.DepCandidate, error) {
	var out []domain.DepCandidate
	for _, p := range filePaths(files) {
		if path.Base(p.String()) != "go.mod" {
			continue
		}
		f, err := modfile.ParseLax(p.String(), files[p], nil)
		if err != nil {
			return nil, err
		}
		var goDirective string
		if f.Go != nil {
			goDirective = f.Go.Version
		}
		replaced := map[string]bool{}
		// ParseLax omits main-module-only replace directives from File.Replace.
		// Its syntax tree still contains them, including grouped replacements.
		for _, stmt := range f.Syntax.Stmt {
			switch s := stmt.(type) {
			case *modfile.Line:
				if len(s.Token) > 1 && s.Token[0] == "replace" {
					replaced[strings.Trim(s.Token[1], "\"")] = true
				}
			case *modfile.LineBlock:
				if len(s.Token) == 1 && s.Token[0] == "replace" {
					for _, line := range s.Line {
						if len(line.Token) > 0 {
							replaced[strings.Trim(line.Token[0], "\"")] = true
						}
					}
				}
			}
		}
		for _, r := range f.Require {
			if replaced[r.Mod.Path] {
				continue
			}
			versions, err := reg.GoVersions(ctx, r.Mod.Path, r.Mod.Version)
			if errors.Is(err, domain.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			out = append(out, domain.DepCandidate{Ecosystem: domain.EcosystemGoMod, Directory: moduleDir(p.String()), Name: r.Mod.Path, Current: r.Mod.Version, Versions: versions, GoDirective: goDirective})
		}
	}
	return out, nil
}

type modulePolicy struct {
	Module           string
	Go, Toolchain    string
	Replace          [][4]string
	Exclude, Retract [][2]string
	Godebug          [][2]string
	Tool, Ignore     []string
}

// canonicalGo maps a two-part "1.N" go directive to "1.N.0". `go get` rewrites
// "go 1.N" to "go 1.N.0" when a dependency declares "go 1.N.0" (Go orders
// 1.N < 1.N.0); that normalisation is not a policy change. Nothing else is
// touched, so any other change of the directive is still refused.
func canonicalGo(v string) string {
	if rest, ok := strings.CutPrefix(v, "1."); ok && rest != "" && strings.Trim(rest, "0123456789") == "" {
		return v + ".0"
	}
	return v
}

func policy(f *modfile.File) modulePolicy {
	p := modulePolicy{}
	if f.Module != nil {
		p.Module = f.Module.Mod.Path
	}
	if f.Go != nil {
		p.Go = canonicalGo(f.Go.Version)
	}
	if f.Toolchain != nil {
		p.Toolchain = f.Toolchain.Name
	}
	for _, r := range f.Replace {
		p.Replace = append(p.Replace, [4]string{r.Old.Path, r.Old.Version, r.New.Path, r.New.Version})
	}
	for _, r := range f.Exclude {
		p.Exclude = append(p.Exclude, [2]string{r.Mod.Path, r.Mod.Version})
	}
	for _, r := range f.Retract {
		p.Retract = append(p.Retract, [2]string{r.Low, r.High})
	}
	for _, g := range f.Godebug {
		p.Godebug = append(p.Godebug, [2]string{g.Key, g.Value})
	}
	for _, t := range f.Tool {
		p.Tool = append(p.Tool, t.Path)
	}
	for _, i := range f.Ignore {
		p.Ignore = append(p.Ignore, i.Path)
	}
	return p
}

// Validate confines reports to requested module files and policy-preserving
// upgrades, allowing higher versions selected by Go and indirect tidy removals.
func (GoMod) Validate(before, after map[domain.TreePath][]byte, ups []domain.DepUpdate) ([]domain.DepSideEffect, error) {
	var effects []domain.DepSideEffect
	dirs := map[string]map[string]string{}
	for _, u := range ups {
		if u.Ecosystem != domain.EcosystemGoMod || (u.Directory != "" && (path.Clean(u.Directory) != u.Directory || strings.HasPrefix(u.Directory, "/") || u.Directory == ".." || strings.HasPrefix(u.Directory, "../"))) {
			return nil, fmt.Errorf("%w: invalid Go update", domain.ErrInvalid)
		}
		if dirs[u.Directory] == nil {
			dirs[u.Directory] = map[string]string{}
		}
		if v, ok := dirs[u.Directory][u.Name]; ok && v != u.Version {
			return nil, fmt.Errorf("%w: conflicting Go updates", domain.ErrInvalid)
		}
		dirs[u.Directory][u.Name] = u.Version
	}
	for p, b := range after {
		d := moduleDir(p.String())
		base := path.Base(p.String())
		if dirs[d] == nil || (base != "go.mod" && base != "go.sum") {
			return nil, fmt.Errorf("%w: unexpected report path %s", domain.ErrInvalid, p.String())
		}
		if base == "go.sum" {
			if err := validSum(b); err != nil {
				return nil, err
			}
		}
	}
	for d, want := range dirs {
		p, err := domain.ParseTreePath(path.Join(d, "go.mod"))
		if err != nil {
			return nil, err
		}
		old, ok := before[p]
		if !ok {
			return nil, fmt.Errorf("%w: missing original go.mod", domain.ErrInvalid)
		}
		content := old
		if b, ok := after[p]; ok {
			content = b
		}
		a, err := modfile.Parse(p.String(), old, nil)
		if err != nil {
			return nil, err
		}
		b, err := modfile.Parse(p.String(), content, nil)
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(policy(a), policy(b)) {
			return nil, fmt.Errorf("%w: module directives changed", domain.ErrInvalid)
		}
		prev, next := map[string]*modfile.Require{}, map[string]*modfile.Require{}
		for _, r := range a.Require {
			prev[r.Mod.Path] = r
		}
		for _, r := range b.Require {
			next[r.Mod.Path] = r
		}
		for name, v := range want {
			r := next[name]
			if prev[name] != nil && prev[name].Indirect && r == nil {
				effects = append(effects, domain.DepSideEffect{Ecosystem: domain.EcosystemGoMod, Directory: d, Name: name, From: prev[name].Mod.Version})
				continue
			}
			if prev[name] == nil || r == nil || semver.Compare(r.Mod.Version, v) < 0 {
				return nil, fmt.Errorf("%w: requested version missing for %s", domain.ErrInvalid, name)
			}
			if semver.Compare(r.Mod.Version, v) > 0 {
				effects = append(effects, domain.DepSideEffect{Ecosystem: domain.EcosystemGoMod, Directory: d, Name: name, From: v, To: r.Mod.Version})
			}
		}
		for name, r := range prev {
			if _, requested := want[name]; requested {
				continue
			}
			n := next[name]
			if !r.Indirect && (n == nil || n.Indirect || semver.Compare(n.Mod.Version, r.Mod.Version) < 0) {
				return nil, fmt.Errorf("%w: unrequested direct requirement %s", domain.ErrInvalid, name)
			}
			if !r.Indirect && semver.Compare(n.Mod.Version, r.Mod.Version) > 0 {
				effects = append(effects, domain.DepSideEffect{Ecosystem: domain.EcosystemGoMod, Directory: d, Name: name, From: r.Mod.Version, To: n.Mod.Version})
			}
		}
		for name, r := range next {
			if _, requested := want[name]; requested {
				continue
			}
			if !r.Indirect && (prev[name] == nil || prev[name].Indirect) {
				return nil, fmt.Errorf("%w: new direct requirement %s", domain.ErrInvalid, name)
			}
		}
	}
	sort.Slice(effects, func(i, j int) bool {
		if effects[i].Directory != effects[j].Directory {
			return effects[i].Directory < effects[j].Directory
		}
		return effects[i].Name < effects[j].Name
	})
	return effects, nil
}

func validSum(b []byte) error {
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			return fmt.Errorf("%w: malformed go.sum line", domain.ErrInvalid)
		}
		v := strings.TrimSuffix(f[1], "/go.mod")
		hash, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(f[2], "h1:"))
		if module.CheckPath(f[0]) != nil || !semver.IsValid(v) || !strings.HasPrefix(f[2], "h1:") || err != nil || len(hash) != 32 {
			return fmt.Errorf("%w: malformed go.sum entry", domain.ErrInvalid)
		}
	}
	return nil
}
