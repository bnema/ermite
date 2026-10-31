// SPDX-License-Identifier: AGPL-3.0-or-later

package deps

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"reflect"
	"sort"
	"strconv"
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
func (GoMod) Manifests(paths domain.PathList) []ports.DepsInput {
	all := map[string]bool{}
	for _, p := range paths.Paths {
		all[p] = true
	}
	var out []ports.DepsInput
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
			out = append(out, ports.DepsInput{Path: tp})
		}
	}
	return out
}

// Inputs reads go.mod and go.sum (absent in a module without dependencies)
// of each update directory and probes, by header, the go.mod of every
// directory between an update directory and a changed Go file, so nested
// module boundaries are known without a full listing.
func (GoMod) Inputs(ups []domain.DepUpdate, changed []domain.TreePath) []ports.DepsInput {
	var out []ports.DepsInput
	add := func(p string, in ports.DepsInput) {
		if tp, err := domain.ParseTreePath(p); err == nil {
			in.Path = tp
			out = append(out, in)
		}
	}
	for _, u := range ups {
		if u.Ecosystem != domain.EcosystemGoMod {
			continue
		}
		add(path.Join(u.Directory, "go.mod"), ports.DepsInput{})
		add(path.Join(u.Directory, "go.sum"), ports.DepsInput{Optional: true})
		if u.NewName == "" {
			continue
		}
		for _, c := range changed {
			if !strings.HasSuffix(c.String(), ".go") || !(u.Directory == "" || strings.HasPrefix(c.String(), u.Directory+"/")) {
				continue
			}
			for d := moduleDir(c.String()); d != u.Directory && d != ""; d = moduleDir(d) {
				add(path.Join(d, "go.mod"), ports.DepsInput{Optional: true, Header: true})
			}
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
			// Indirect requirements are not imported: a new major path
			// would never be used.
			var paths []string
			if !r.Indirect {
				paths, err = reg.GoMajorPaths(ctx, r.Mod.Path)
				if err != nil {
					return nil, err
				}
			}
			for _, next := range paths {
				more, err := reg.GoVersions(ctx, next, r.Mod.Version)
				if errors.Is(err, domain.ErrNotFound) {
					continue
				}
				if err != nil {
					return nil, err
				}
				for i := range more {
					more[i].Path = next
				}
				versions = append(versions, more...)
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
		name := u.Name
		if u.NewName != "" {
			name = u.NewName
		}
		dirs[u.Directory][name] = u.Version
	}
	for p, b := range after {
		d := moduleDir(p.String())
		base := path.Base(p.String())
		if strings.HasSuffix(p.String(), ".go") {
			if err := validateImports(p, before, b, ups); err != nil {
				return nil, err
			}
			continue
		}
		if dirs[d] == nil || (base != "go.mod" && base != "go.sum") {
			return nil, fmt.Errorf("%w: unexpected report path %q", domain.ErrInvalid, p.String())
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
			return nil, fmt.Errorf("%w: %v", domain.ErrInvalid, err)
		}
		old, ok := before[p]
		if !ok {
			return nil, fmt.Errorf("%w: missing original go.mod", domain.ErrInvalid)
		}
		content := old
		if b, ok := after[p]; ok {
			content = b
		}
		// Normalize only the old requested path in the comparison baseline.
		f, parseErr := modfile.Parse(p.String(), old, nil)
		if parseErr != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrInvalid, parseErr)
		}
		for _, u := range ups {
			if u.Directory == d && u.NewName != "" {
				var found bool
				for _, r := range f.Require {
					if r.Mod.Path == u.Name {
						found = true
						f.AddNewRequire(u.NewName, u.Version, r.Indirect)
						break
					}
				}
				if !found {
					return nil, fmt.Errorf("%w: original requirement missing", domain.ErrInvalid)
				}
				_ = f.DropRequire(u.Name)
				reported, err := modfile.Parse(p.String(), content, nil)
				if err != nil {
					return nil, fmt.Errorf("%w: %v", domain.ErrInvalid, err)
				}
				newFound := false
				for _, r := range reported.Require {
					if r.Mod.Path == u.NewName {
						newFound = true
					}
					if r.Mod.Path == u.Name && !r.Indirect {
						return nil, fmt.Errorf("%w: old path remains direct", domain.ErrInvalid)
					}
					if r.Mod.Path == u.NewName && r.Mod.Version != u.Version {
						return nil, fmt.Errorf("%w: new path version mismatch", domain.ErrInvalid)
					}
				}
				if !newFound {
					return nil, fmt.Errorf("%w: new path missing", domain.ErrInvalid)
				}
			}
		}
		old, err = f.Format()
		if err != nil {
			return nil, err
		}
		more, err := validateModule(d, old, content, want)
		if err != nil {
			return nil, err
		}
		effects = append(effects, more...)
	}
	sort.Slice(effects, func(i, j int) bool {
		if effects[i].Directory != effects[j].Directory {
			return effects[i].Directory < effects[j].Directory
		}
		return effects[i].Name < effects[j].Name
	})
	return effects, nil
}

func effect(dir, name, from, to string) domain.DepSideEffect {
	return domain.DepSideEffect{Directory: dir, Name: name, From: from, To: to}
}

// validateModule checks the go.mod of one directory against the requested
// versions (module name to version) and returns the changes Go made beyond them.
func validateModule(dir string, old, content []byte, want map[string]string) ([]domain.DepSideEffect, error) {
	file := path.Join(dir, "go.mod")
	a, err := modfile.Parse(file, old, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: original go.mod: %v", domain.ErrInvalid, err)
	}
	b, err := modfile.Parse(file, content, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: reported go.mod: %v", domain.ErrInvalid, err)
	}
	if !reflect.DeepEqual(policy(a), policy(b)) {
		return nil, fmt.Errorf("%w: module directives changed", domain.ErrInvalid)
	}
	prev, err := requirements(a, "original")
	if err != nil {
		return nil, err
	}
	next, err := requirements(b, "reported")
	if err != nil {
		return nil, err
	}
	var effects []domain.DepSideEffect
	for name, v := range want {
		p, r := prev[name], next[name]
		if p != nil && p.Indirect && r == nil {
			effects = append(effects, effect(dir, name, p.Mod.Version, ""))
			continue
		}
		if p == nil || r == nil {
			return nil, fmt.Errorf("%w: requested version missing for %q", domain.ErrInvalid, name)
		}
		cmp := semver.Compare(r.Mod.Version, v)
		switch {
		case cmp < 0:
			return nil, fmt.Errorf("%w: requested version missing for %q", domain.ErrInvalid, name)
		case !p.Indirect && r.Indirect:
			return nil, fmt.Errorf("%w: requested direct requirement %q became indirect", domain.ErrInvalid, name)
		case p.Indirect && !r.Indirect:
			return nil, fmt.Errorf("%w: new direct requirement %q", domain.ErrInvalid, name)
		case cmp == 0 && r.Mod.Version != v:
			return nil, fmt.Errorf("%w: requested version of %q changed to %q", domain.ErrInvalid, name, r.Mod.Version)
		case cmp > 0:
			effects = append(effects, effect(dir, name, v, r.Mod.Version))
		}
	}
	for name, r := range prev {
		if _, requested := want[name]; requested || r.Indirect {
			continue
		}
		n := next[name]
		if n == nil || n.Indirect {
			return nil, fmt.Errorf("%w: unrequested direct requirement %q", domain.ErrInvalid, name)
		}
		cmp := semver.Compare(n.Mod.Version, r.Mod.Version)
		switch {
		case cmp < 0:
			return nil, fmt.Errorf("%w: unrequested direct requirement %q", domain.ErrInvalid, name)
		case cmp == 0 && n.Mod.Version != r.Mod.Version:
			return nil, fmt.Errorf("%w: unrequested direct requirement %q changed from %q to %q", domain.ErrInvalid, name, r.Mod.Version, n.Mod.Version)
		case cmp > 0:
			effects = append(effects, effect(dir, name, r.Mod.Version, n.Mod.Version))
		}
	}
	for name, r := range next {
		if _, requested := want[name]; requested {
			continue
		}
		if !r.Indirect && (prev[name] == nil || prev[name].Indirect) {
			return nil, fmt.Errorf("%w: new direct requirement %q", domain.ErrInvalid, name)
		}
	}
	return effects, nil
}

// requirements indexes the requirements of a go.mod by module path and refuses
// two requirements for one path: the maps of Validate would keep only one.
func requirements(f *modfile.File, which string) (map[string]*modfile.Require, error) {
	m := make(map[string]*modfile.Require, len(f.Require))
	for _, r := range f.Require {
		if _, dup := m[r.Mod.Path]; dup {
			return nil, fmt.Errorf("%w: duplicate requirement for %q in the %s go.mod", domain.ErrInvalid, r.Mod.Path, which)
		}
		m[r.Mod.Path] = r
	}
	return m, nil
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

// ReportAllowed admits module metadata and path-major import candidates.
// Nested module boundaries are checked against original manifests in Validate.
func (GoMod) ReportAllowed(ups []domain.DepUpdate, p domain.TreePath) bool {
	for _, part := range strings.Split(p.String(), "/") {
		if part == "vendor" || part == "testdata" {
			return false
		}
	}
	for _, u := range ups {
		if u.Ecosystem != domain.EcosystemGoMod {
			continue
		}
		if p.String() == path.Join(u.Directory, "go.mod") || p.String() == path.Join(u.Directory, "go.sum") {
			return true
		}
		if u.NewName != "" && strings.HasSuffix(p.String(), ".go") && (u.Directory == "" || strings.HasPrefix(p.String(), u.Directory+"/")) {
			return true
		}
	}
	return false
}

func validateImports(p domain.TreePath, before map[domain.TreePath][]byte, after []byte, ups []domain.DepUpdate) error {
	invalid := func() error { return fmt.Errorf("%w: non-import or unexpected Go change in %s", domain.ErrInvalid, p) }
	if !(GoMod{}).ReportAllowed(ups, p) {
		return invalid()
	}
	var allowed []domain.DepUpdate
	for _, u := range ups {
		if u.NewName == "" || !(u.Directory == "" || strings.HasPrefix(p.String(), u.Directory+"/")) {
			continue
		}
		// Inputs probed the go.mod of every directory between the module and
		// the file: any present one is a nested module boundary.
		nested := false
		for d := moduleDir(p.String()); d != u.Directory && d != ""; d = moduleDir(d) {
			if m, err := domain.ParseTreePath(path.Join(d, "go.mod")); err == nil && before[m] != nil {
				nested = true
			}
		}
		if !nested {
			allowed = append(allowed, u)
		}
	}
	if len(allowed) == 0 {
		return invalid()
	}
	a, ok := before[p]
	if !ok {
		return invalid()
	}
	// Compare all syntax and whitespace except import path literals. This is
	// stricter than ignoring whole declarations: aliases/comments stay intact.
	strip := func(src []byte) ([]byte, []string, error) {
		fs := token.NewFileSet()
		f, err := parser.ParseFile(fs, p.String(), src, parser.ImportsOnly)
		if err != nil {
			return nil, nil, err
		}
		var out []byte
		var imports []string
		end := 0
		for _, decl := range f.Decls {
			g, ok := decl.(*ast.GenDecl)
			if !ok || g.Tok != token.IMPORT {
				continue
			}
			for _, spec := range g.Specs {
				imp := spec.(*ast.ImportSpec)
				start, stop := fs.Position(imp.Path.Pos()).Offset, fs.Position(imp.Path.End()).Offset
				v, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					return nil, nil, err
				}
				out = append(out, src[end:start]...)
				imports = append(imports, v)
				end = stop
			}
		}
		out = append(out, src[end:]...)
		return out, imports, nil
	}
	x, ai, err := strip(a)
	if err != nil {
		return invalid()
	}
	y, bi, err := strip(after)
	if err != nil || !bytes.Equal(x, y) || len(ai) != len(bi) {
		return invalid()
	}
	for i, old := range ai {
		if old == bi[i] {
			continue
		}
		valid := false
		for _, u := range allowed {
			if old != u.Name && !strings.HasPrefix(old, u.Name+"/") {
				continue
			}
			// OLD/v3/... belongs to another major; a path inside another
			// required module nested under OLD is not OLD's.
			suffix := strings.TrimPrefix(old, u.Name)
			if majorSuffix(suffix) || longestRequired(before, u.Directory, old) != u.Name {
				continue
			}
			valid = valid || bi[i] == u.NewName+suffix
		}
		if !valid {
			return invalid()
		}
	}
	return nil
}

// majorSuffix reports whether suffix starts with a /vN path element.
func majorSuffix(suffix string) bool {
	elem, _, _ := strings.Cut(strings.TrimPrefix(suffix, "/"), "/")
	if !strings.HasPrefix(suffix, "/v") || len(elem) < 2 {
		return false
	}
	_, err := strconv.Atoi(elem[1:])
	return err == nil
}

// longestRequired returns the longest module required by the go.mod of dir
// that holds the import path imp, or "" when none does.
func longestRequired(before map[domain.TreePath][]byte, dir, imp string) string {
	p, err := domain.ParseTreePath(path.Join(dir, "go.mod"))
	if err != nil {
		return ""
	}
	f, err := modfile.ParseLax(p.String(), before[p], nil)
	if err != nil {
		return ""
	}
	best := ""
	for _, r := range f.Require {
		if (imp == r.Mod.Path || strings.HasPrefix(imp, r.Mod.Path+"/")) && len(r.Mod.Path) > len(best) {
			best = r.Mod.Path
		}
	}
	return best
}
