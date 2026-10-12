// SPDX-License-Identifier: AGPL-3.0-or-later

// Package boundary_test enforces the import rules between the layers of
// ermite. The module root holds no Go package (the binary lives in cmd/ermite),
// so the check lives in this test-only package and walks the module from its
// root. It parses import clauses only; it never loads packages.
package boundary_test

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "bnema.dev/ermite"

// moduleRoot is the module root relative to this package's directory.
const moduleRoot = "../.."

type packageLayer string

const (
	layerCommand     packageLayer = "command"
	layerApp         packageLayer = "app"
	layerUsecase     packageLayer = "usecase"
	layerAdapter     packageLayer = "adapter"
	layerClientLimit packageLayer = "clientlimit"
	layerConfig      packageLayer = "config"
	layerPorts       packageLayer = "ports"
	layerDomain      packageLayer = "domain"
	layerGenerated   packageLayer = "generated"
	layerScript      packageLayer = "script"
	layerBoundary    packageLayer = "boundary"
	layerTestSupport packageLayer = "test-support"
)

// productionDependencies lists, per layer, the layers its production files
// may import.
//
//   - cmd/ermite is the entry point: it only calls internal/app.
//   - internal/app is the composition root and the CLI: it wires everything.
//   - internal/usecase holds the use cases: domain and ports only.
//   - adapters implement ports and never import use cases or app; the
//     per-package rule in packageImportDenied also keeps them from importing
//     each other.
//   - internal/clientlimit is infrastructure shared by the HTTP adapters.
//   - internal/config parses the environment into plain values.
var productionDependencies = map[packageLayer]map[packageLayer]bool{
	layerCommand:     {layerApp: true},
	layerApp:         {layerUsecase: true, layerAdapter: true, layerClientLimit: true, layerConfig: true, layerPorts: true, layerDomain: true, layerGenerated: true},
	layerUsecase:     {layerUsecase: true, layerPorts: true, layerDomain: true},
	layerAdapter:     {layerAdapter: true, layerClientLimit: true, layerPorts: true, layerDomain: true, layerGenerated: true},
	layerClientLimit: {layerPorts: true},
	layerConfig:      {},
	layerPorts:       {layerDomain: true},
	layerDomain:      {layerDomain: true},
	layerGenerated:   {layerGenerated: true},
	layerScript:      {},
	layerBoundary:    {},
	layerTestSupport: {layerPorts: true, layerDomain: true},
}

func TestImportBoundaries(t *testing.T) {
	violations, packages := inspectRepositoryDependencies(t)
	if len(violations) != 0 {
		t.Fatalf("invalid package dependencies:\n%s", strings.Join(violations, "\n"))
	}
	for _, want := range []string{"cmd/ermite", "internal/app", "internal/usecase", "internal/ports", "internal/domain"} {
		if _, ok := packages[modulePath+"/"+want]; !ok {
			t.Fatalf("package inventory misses %s: the walk is not reading the module", want)
		}
	}
}

func TestImportBoundaryFixtures(t *testing.T) {
	p := func(s string) string { return modulePath + "/" + s }
	tests := []struct {
		name, source, target string
		testFile             bool
		want                 bool
	}{
		{"command accepts app", p("cmd/ermite"), p("internal/app"), false, true},
		{"command rejects usecase", p("cmd/ermite"), p("internal/usecase"), false, false},
		{"command rejects adapter", p("cmd/ermite"), p("internal/adapters/web"), false, false},
		{"command rejects domain", p("cmd/ermite"), p("internal/domain"), false, false},
		{"app accepts usecase", p("internal/app"), p("internal/usecase"), false, true},
		{"app accepts adapter", p("internal/app"), p("internal/adapters/gitcli"), false, true},
		{"app accepts config", p("internal/app"), p("internal/config"), false, true},
		{"app accepts clientlimit", p("internal/app"), p("internal/clientlimit"), false, true},
		{"app rejects command", p("internal/app"), p("cmd/ermite"), false, false},
		{"app test may import command", p("internal/app"), p("cmd/ermite"), true, true},
		{"usecase accepts domain", p("internal/usecase"), p("internal/domain"), false, true},
		{"usecase accepts ports", p("internal/usecase"), p("internal/ports"), false, true},
		{"usecase rejects adapter", p("internal/usecase"), p("internal/adapters/kvstore"), false, false},
		{"usecase rejects app", p("internal/usecase"), p("internal/app"), false, false},
		{"usecase rejects config", p("internal/usecase"), p("internal/config"), false, false},
		{"usecase rejects clientlimit", p("internal/usecase"), p("internal/clientlimit"), false, false},
		{"usecase test may import adapter", p("internal/usecase"), p("internal/adapters/kvstore"), true, true},
		{"adapter accepts ports", p("internal/adapters/rpc"), p("internal/ports"), false, true},
		{"adapter accepts domain", p("internal/adapters/rpc"), p("internal/domain"), false, true},
		{"adapter accepts clientlimit", p("internal/adapters/githttp"), p("internal/clientlimit"), false, true},
		{"adapter accepts own subpackage", p("internal/adapters/web/sub"), p("internal/adapters/web"), false, true},
		{"adapter production rejects generated mocks", p("internal/adapters/web"), p("internal/adapters/web/mocks"), false, false},
		{"adapter test may import generated mocks", p("internal/adapters/web"), p("internal/adapters/web/mocks"), true, true},
		{"adapter rejects usecase", p("internal/adapters/rpc"), p("internal/usecase"), false, false},
		{"adapter rejects app", p("internal/adapters/rpc"), p("internal/app"), false, false},
		{"adapter rejects config", p("internal/adapters/rpc"), p("internal/config"), false, false},
		{"adapter rejects sibling adapter", p("internal/adapters/web"), p("internal/adapters/gitcli"), false, false},
		{"adapter test may import sibling adapter", p("internal/adapters/ermitegit"), p("internal/adapters/gitcli"), true, true},
		{"clientlimit accepts ports", p("internal/clientlimit"), p("internal/ports"), false, true},
		{"clientlimit rejects usecase", p("internal/clientlimit"), p("internal/usecase"), false, false},
		{"config rejects domain", p("internal/config"), p("internal/domain"), false, false},
		{"ports accepts domain", p("internal/ports"), p("internal/domain"), false, true},
		{"ports rejects usecase", p("internal/ports"), p("internal/usecase"), false, false},
		{"ports rejects adapter", p("internal/ports"), p("internal/adapters/web"), false, false},
		{"ports mocks accept ports", p("internal/ports/mocks"), p("internal/ports"), false, true},
		{"ports mocks reject usecase", p("internal/ports/mocks"), p("internal/usecase"), false, false},
		{"reader conformance accepts ports", p("internal/readertest"), p("internal/ports"), false, true},
		{"reader conformance rejects adapter", p("internal/readertest"), p("internal/adapters/gitcli"), false, false},
		{"domain accepts domain", p("internal/domain"), p("internal/domain/x"), false, true},
		{"domain rejects ports", p("internal/domain"), p("internal/ports"), false, false},
		{"domain test may import anything", p("internal/domain"), p("internal/ports"), true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := dependencyAllowed(tt.source, tt.target, tt.testFile)
			if err != nil {
				t.Fatalf("dependencyAllowed: %v", err)
			}
			if got != tt.want {
				t.Fatalf("dependencyAllowed(%q, %q, test=%t) = %t, want %t", tt.source, tt.target, tt.testFile, got, tt.want)
			}
		})
	}
}

func inspectRepositoryDependencies(t *testing.T) ([]string, map[string]struct{}) {
	t.Helper()
	var violations []string
	packages := make(map[string]struct{})
	fset := token.NewFileSet()
	err := filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			name := entry.Name()
			if name == ".git" || name == ".worktrees" || name == "vendor" || name == "testdata" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(moduleRoot, path)
		if err != nil {
			return err
		}
		source := importPathForFile(rel)
		packages[source] = struct{}{}
		if _, err := classifyPackage(source); err != nil {
			violations = append(violations, rel+": "+err.Error())
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		testFile := strings.HasSuffix(path, "_test.go")
		for _, spec := range file.Imports {
			target, err := strconv.Unquote(spec.Path.Value)
			if err != nil || (target != modulePath && !strings.HasPrefix(target, modulePath+"/")) {
				continue
			}
			allowed, err := dependencyAllowed(source, target, testFile)
			if err != nil {
				violations = append(violations, fmt.Sprintf("%s: %v", rel, err))
				continue
			}
			if !allowed {
				violations = append(violations, fmt.Sprintf("%s: %s may not import %s", rel, source, target))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking module: %v", err)
	}
	sort.Strings(violations)
	return violations, packages
}

// dependencyAllowed reports whether a file of package source may import
// target. Test files are exempt, so that composition tests can wire adapters
// and use cases together; their packages must still be classified.
func dependencyAllowed(source, target string, testFile bool) (bool, error) {
	sourceLayer, err := classifyPackage(source)
	if err != nil {
		return false, err
	}
	targetLayer, err := classifyPackage(target)
	if err != nil {
		return false, err
	}
	if testFile {
		return true, nil
	}
	if packageImportDenied(source, target) {
		return false, nil
	}
	return productionDependencies[sourceLayer][targetLayer], nil
}

// packageImportDenied narrows the layer rules to packages: an adapter may
// import only packages under its own directory (its mocks, for instance),
// never a sibling adapter.
func packageImportDenied(source, target string) bool {
	const adapters = modulePath + "/internal/adapters/"
	if strings.HasPrefix(source, adapters) && strings.HasPrefix(target, adapters) {
		return adapterRoot(source) != adapterRoot(target)
	}
	return false
}

// adapterRoot is the first path element under internal/adapters.
func adapterRoot(path string) string {
	rest := strings.TrimPrefix(path, modulePath+"/internal/adapters/")
	root, _, _ := strings.Cut(rest, "/")
	return root
}

func under(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+"/")
}

func classifyPackage(path string) (packageLayer, error) {
	p := func(s string) string { return modulePath + "/" + s }
	switch {
	case under(path, p("cmd")):
		return layerCommand, nil
	case under(path, p("scripts")):
		return layerScript, nil
	case under(path, p("internal/boundary")):
		return layerBoundary, nil
	case under(path, p("internal/app")):
		return layerApp, nil
	case under(path, p("internal/usecase")):
		return layerUsecase, nil
	case path == p("internal/ports/mocks"), under(path, p("internal/adapters/web/mocks")), under(path, p("internal/readertest")):
		return layerTestSupport, nil
	case under(path, p("internal/adapters")):
		return layerAdapter, nil
	case under(path, p("internal/clientlimit")):
		return layerClientLimit, nil
	case under(path, p("internal/config")):
		return layerConfig, nil
	case path == p("internal/ports"):
		return layerPorts, nil
	case under(path, p("internal/domain")):
		return layerDomain, nil
	case under(path, p("gen")):
		return layerGenerated, nil
	default:
		return "", fmt.Errorf("unclassified package %q", path)
	}
}

func importPathForFile(rel string) string {
	dir := filepath.ToSlash(filepath.Dir(rel))
	if dir == "." {
		return modulePath
	}
	return modulePath + "/" + dir
}
